package poller

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// NetworkHealthPoller tracks bridge / STP state and surfaces L2 loop signals.
//
// Every pollerTick it visits each online, unpaused device and runs what is due
// on the device's own cadence (MIKROTIK_NMS_NETWORK_HEALTH_INTERVAL is the
// floor):
//  1. Tier 2a–2c: /interface/bridge/print → the bridges (proplisted), then
//     /interface/bridge/port/print → ports with their STP runtime state on
//     RouterOS >= 7.24, else one batched (or, last resort, per-port) monitor;
//     one batched /interface/bridge/monitor for the bridge summary
//     (routeros.ReadBridgeState learns what each device accepts)
//  2. Tier 3a: /interface/bridge/vlan/print → the VLAN table
//  3. TierLogs: the shared log read → loop / mac flap events, login lines
//  4. the port monitor over the port-stats collector's interface snapshot
//     (Tier 0 + the Tier 4a loop-protect state) — no command of its own, run
//     per device every max(floor, its counter cadence)
//
// Rows of bridges, ports, VLANs and interfaces a device no longer reports are
// pruned once they are older than 3× that device's cadence for them, at
// least 30 min; a paused device's rows are never pruned, nor within that
// window after its pause ended.
//
// Anomaly detection writes rows to loop_events:
//   - stp_disabled: bridge has STP off and >1 non-edge port
//   - tcn_storm:    the ports' topology-changes counters rose by >= tcn_storm_threshold per minute
//   - loop_detected / mac_flap / bpdu_on_edge: directly from parsed bridge log
//   - port_link_down / port_disabled / port_link_flap / port_loop_protect: the port monitor
type NetworkHealthPoller struct {
	db       *sql.DB
	pool     *routeros.Pool
	hub      *ws.Hub
	interval time.Duration

	// previous per-port topology_changes (key device|bridge|interface) and the
	// time of each bridge's previous sample: TCN deltas are summed per bridge
	// and normalised per elapsed minute, so a slower per-device cadence does
	// not read as a storm.
	prevPortTC map[string]int
	prevTCAt   map[string]time.Time // key = device_id|bridge_name
	// per-device RouterOS capabilities learned by the bridge reader (which
	// print / monitor forms give STP state), and the path last logged so a
	// change is reported once.
	bridgeCaps map[string]routeros.BridgeCaps
	bridgePath map[string]string
	// bridges ever observed running STP — used to suppress false stp_disabled
	// when RouterOS 7.23 intermittently reports an rstp bridge as protocol=none.
	bridgeSTPSeen map[string]bool // key = device_id|bridge_name
	// log fingerprints already processed per device.
	seenLogs       map[string]map[string]time.Time
	lastLogPruneAt time.Time
	// per-device per-interface state for port-deactivation / flap detection.
	ports *portMonitor
	// ifaces serves the port monitor's input (UseInterfaceSnapshots).
	ifaces InterfaceSnapshots

	// logs is the shared per-device log read (UseLogReader); logins turns
	// its account lines into device_findings.
	logs   *LogReader
	logins *LoginWatch

	// cadence and clock schedule the tiers per device (UseCadence); runner
	// is the device link (nil = no pooled connection) and now the clock
	// (test seams).
	cadence CadenceSource
	clock   *tierClock
	runner  func(deviceID string) routeros.Runner
	now     func() time.Time
	// lastPortScan is when the port monitor last ran per device.
	lastPortScan map[string]time.Time
	// The once-per-floor pass: prunes and the network.health summary, with
	// the per-device bridge/port counts of the latest visits and the events
	// recorded since the last summary.
	lastMaint    time.Time
	bridgeCounts map[string]int
	portCounts   map[string]int
	newEvents    int
	// pausedSeen is when maintain last found each device paused: after the
	// pause the device's rows are as old as the pause, and the prunes give
	// it a stale window from there to be read again.
	pausedSeen map[string]time.Time
}

// UseCadence schedules the poller's tiers by cs. Call before Run.
func (n *NetworkHealthPoller) UseCadence(cs CadenceSource) {
	if cs != nil {
		n.cadence = cs
	}
}

// InterfaceSnapshots serves each device's latest scheduled interface read:
// PortStatsCollector.DeviceSnapshot.
type InterfaceSnapshots interface {
	DeviceSnapshot(deviceID string) (rows []routeros.InterfaceSnapshot, at time.Time, ok bool)
}

// UseLogReader shares one log reader with the other pollers, so a device's
// buffer is read once per window for all of them.
func (n *NetworkHealthPoller) UseLogReader(r *LogReader) {
	if r != nil {
		n.logs = r
	}
}

// UseInterfaceSnapshots feeds the port monitor from src instead of a device
// read. Without a source the port monitor does not run: it never prints the
// interface table itself.
func (n *NetworkHealthPoller) UseInterfaceSnapshots(src InterfaceSnapshots) {
	n.ifaces = src
}

// defaultTCNStormThreshold is the topology-changes-per-cycle delta above which a
// bridge is considered to be in a TCN storm (unstable L2 — a flapping link or
// active loop) when the admin hasn't configured tcn_storm_threshold. The default
// is deliberately high: STP normally emits a handful of topology changes during
// routine port up/down, so a low threshold produced a lot of false positives.
const defaultTCNStormThreshold = 30

// tcnStormCriticalDelta is the per-cycle topology-changes delta at or above which
// a tcn_storm event is escalated from "warn" to "critical".
const tcnStormCriticalDelta = 100

const bridgeLogFingerprintTTL = 30 * time.Minute

// filterRealBridges drops entries that are not genuinely bridges. RouterOS 7.23's
// /interface/bridge/print can return non-bridge interfaces (ethernet ports, the
// loopback, WireGuard/veth, VLANs), which would otherwise be tracked as bridges
// and drive bogus topology-change "storms". nonBridgeNames is the set of interface
// names known (from their type) NOT to be bridges. Any entry in that set is
// removed; names not positively identified as non-bridge are kept (so real bridges
// are never lost), and the function is a no-op when the set is empty.
func filterRealBridges(bridges []routeros.BridgeInfo, nonBridgeNames map[string]bool) []routeros.BridgeInfo {
	if len(nonBridgeNames) == 0 {
		return bridges
	}
	out := make([]routeros.BridgeInfo, 0, len(bridges))
	for _, b := range bridges {
		if !nonBridgeNames[b.Name] {
			out = append(out, b)
		}
	}
	return out
}

func countNonEdgePorts(ports []routeros.BridgePortInfo) int {
	n := 0
	for _, p := range ports {
		if !p.Edge {
			n++
		}
	}
	return n
}

// shouldFlagSTPDisabled reports whether a bridge with STP off and multiple
// non-edge ports is a genuine loop risk worth a warning. everSTP suppresses the
// warning for any bridge we've ever seen running STP: RouterOS 7.23 intermittently
// reports an rstp bridge's protocol as "none", and trusting the "STP on" read over
// the flickering "off" read avoids a flood of false stp_disabled warnings. The
// loopback is always skipped (it never carries external traffic).
func shouldFlagSTPDisabled(b routeros.BridgeInfo, everSTP bool, nonEdgeCount int) bool {
	if b.STPEnabled() || everSTP {
		return false
	}
	switch strings.ToLower(b.Name) {
	case "lo", "loopback":
		return false
	}
	return nonEdgeCount > 1
}

// nonBridgeInterfaceNames returns the set of a device's interface names that are
// known (by cached type) not to be bridges. It reads the stored interfaces table
// rather than a live query, because on RouterOS 7.23 the live interface fetch can
// fail (go-routeros parse errors) while the cached types stay valid — interface
// types effectively never change.
func nonBridgeInterfaceNames(db *sql.DB, deviceID string) map[string]bool {
	ifaces, err := queries.ListInterfacesByDevice(db, deviceID)
	if err != nil {
		return nil
	}
	set := make(map[string]bool, len(ifaces))
	for _, i := range ifaces {
		if i.Type != "" && !strings.EqualFold(i.Type, "bridge") {
			set[i.Name] = true
		}
	}
	return set
}

// tcnStormSeverity decides whether a bridge's topology-change rate (the ports'
// counter deltas per minute, see bridgeTCNDelta) is a TCN storm and at what
// severity. Topology-change notifications are an STP concept, so only an
// STP-running bridge can storm — a non-STP "bridge" (incl. any interface
// mis-reported as a bridge) never fires, which is the definitive guard against
// the false positives.
func tcnStormSeverity(b routeros.BridgeInfo, deltaPerMinute, threshold int) (severity string, fire bool) {
	if !b.STPEnabled() {
		return "", false
	}
	if deltaPerMinute < threshold {
		return "", false
	}
	if deltaPerMinute >= tcnStormCriticalDelta {
		return "critical", true
	}
	return "warn", true
}

// normalizeTCNDelta converts a raw topology-change delta into a per-minute
// rate, so tcn_storm_threshold keeps its meaning at any poll cadence. Gaps
// up to a minute count as one minute: a fast cycle must not inflate a
// handful of changes into a storm.
func normalizeTCNDelta(delta int, elapsed time.Duration) int {
	if delta <= 0 {
		return 0
	}
	if minutes := elapsed.Minutes(); minutes > 1 {
		return int(math.Round(float64(delta) / minutes))
	}
	return delta
}

// bridgeTCNDelta sums the member ports' topology-changes deltas since the
// previous cycle and normalises the sum per elapsed minute. ok is false on a
// bridge's first sample (nothing to compare) and when no port carries a
// counter (RouterOS without the field). A port seen for the first time
// contributes nothing, so a port joining with a large counter is not a storm;
// neither is a counter that went backwards (a reboot).
func (n *NetworkHealthPoller) bridgeTCNDelta(key string, ports []routeros.BridgePortInfo, now time.Time) (perMinute int, ok bool) {
	sum, compared := 0, false
	for i := range ports {
		p := &ports[i]
		if !p.HasTopologyChanges {
			continue
		}
		pk := key + "|" + p.Interface
		prev, had := n.prevPortTC[pk]
		n.prevPortTC[pk] = p.TopologyChanges
		if !had {
			continue
		}
		compared = true
		if p.TopologyChanges > prev {
			sum += p.TopologyChanges - prev
		}
	}
	last, hadLast := n.prevTCAt[key]
	n.prevTCAt[key] = now
	if !hadLast || !compared {
		return 0, false
	}
	return normalizeTCNDelta(sum, now.Sub(last)), true
}

// pruneTCState forgets the TCN samples and reader capabilities of devices
// that no longer exist.
func (n *NetworkHealthPoller) pruneTCState(active map[string]struct{}) {
	deviceOf := func(key string) string {
		if i := strings.IndexByte(key, '|'); i >= 0 {
			return key[:i]
		}
		return key
	}
	for k := range n.prevPortTC {
		if _, ok := active[deviceOf(k)]; !ok {
			delete(n.prevPortTC, k)
		}
	}
	for k := range n.prevTCAt {
		if _, ok := active[deviceOf(k)]; !ok {
			delete(n.prevTCAt, k)
		}
	}
	for id := range n.bridgeCaps {
		if _, ok := active[id]; !ok {
			delete(n.bridgeCaps, id)
			delete(n.bridgePath, id)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func NewNetworkHealthPoller(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, interval time.Duration) *NetworkHealthPoller {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	n := &NetworkHealthPoller{
		db:            db,
		pool:          pool,
		hub:           hub,
		interval:      interval,
		prevPortTC:    make(map[string]int),
		prevTCAt:      make(map[string]time.Time),
		bridgeCaps:    make(map[string]routeros.BridgeCaps),
		bridgePath:    make(map[string]string),
		bridgeSTPSeen: make(map[string]bool),
		seenLogs:      make(map[string]map[string]time.Time),
		ports:         newPortMonitor(),
		logs:          NewLogReader(),
		logins:        NewLoginWatch(db),
		cadence:       newFleetCadence(db),
		clock:         newTierClock(Schedule()),
		now:           time.Now,
		lastPortScan:  make(map[string]time.Time),
		bridgeCounts:  make(map[string]int),
		portCounts:    make(map[string]int),
		pausedSeen:    make(map[string]time.Time),
	}
	n.runner = func(deviceID string) routeros.Runner { return poolRunner(n.pool, deviceID) }
	return n
}

func (n *NetworkHealthPoller) Run(ctx context.Context) {
	// Seed in-memory port state from the DB so the first poll after a restart
	// only suppresses transitions for interfaces we've genuinely never seen.
	// Without this, every existing disabled/down port would stay invisible
	// until something changed on the device.
	if err := n.ports.RestoreFromDB(n.db); err != nil {
		log.Printf("network health: restore port state: %v", err)
	}

	// Seed "has run STP" from stored bridge state so a flickering protocol read
	// right after a restart doesn't produce false stp_disabled warnings.
	if bridges, err := queries.ListBridgeStatus(n.db); err == nil {
		for _, b := range bridges {
			switch strings.ToLower(b.Protocol) {
			case "stp", "rstp", "mstp":
				n.bridgeSTPSeen[b.DeviceID+"|"+b.BridgeName] = true
			}
		}
	}

	// Stagger after the topology poller's initial run.
	if !sleepCtx(ctx, 20*time.Second) {
		return
	}
	for {
		n.safePoll(ctx)
		if !sleepCtx(ctx, pollerTick) {
			return
		}
	}
}

func (n *NetworkHealthPoller) safePoll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("network health: panic: %v", r)
		}
	}()
	n.poll(ctx)
}

// poll is one tick: every device's due work, then — once per floor interval —
// the prunes and the network.health summary.
func (n *NetworkHealthPoller) poll(ctx context.Context) {
	devices, err := queries.ListDevices(n.db)
	if err != nil {
		return
	}
	now := n.now()

	activeDevices := make(map[string]struct{}, len(devices))
	for _, dev := range devices {
		activeDevices[dev.ID] = struct{}{}
	}
	n.ports.pruneDevices(activeDevices)
	n.pruneTCState(activeDevices)
	n.logs.Prune(activeDevices)
	n.clock.retain(deviceIDs(devices))
	for id := range n.lastPortScan {
		if _, ok := activeDevices[id]; !ok {
			delete(n.lastPortScan, id)
		}
	}
	for _, m := range []map[string]int{n.bridgeCounts, n.portCounts} {
		for id := range m {
			if _, ok := activeDevices[id]; !ok {
				delete(m, id)
			}
		}
	}
	for id := range n.pausedSeen {
		if _, ok := activeDevices[id]; !ok {
			delete(n.pausedSeen, id)
		}
	}

	var settings *healthSettings
	for _, dev := range devices {
		if ctx.Err() != nil {
			return
		}
		n.visit(dev, now, &settings)
	}
	n.maintain(devices, now)
}

// healthSettings are the runtime-tunable knobs a visit reads, loaded at most
// once per tick and only when some device has work to do.
type healthSettings struct {
	ports PortMonitorSettings
	tcn   int
}

func (n *NetworkHealthPoller) settingsOnce(s **healthSettings) *healthSettings {
	if *s == nil {
		*s = &healthSettings{ports: loadPortMonitorSettings(n.db), tcn: n.tcnStormThreshold()}
	}
	return *s
}

// portScanInterval is how often the port monitor runs over dev's snapshot:
// every floor interval, or every counter read when that is slower. The
// snapshot only changes on a counter read; interface_state rows are written
// per scan.
func (n *NetworkHealthPoller) portScanInterval(dev queries.Device, now time.Time) time.Duration {
	return max(n.interval, n.cadence.Interval(dev, devclass.TierCounters, now))
}

// visit runs dev's work that is due at now: the port monitor, then the bridge
// (Tier 2a–2c), VLAN (3a) and log reads on one pooled client. A device whose
// bridge read fails is not asked for the rest this tick.
func (n *NetworkHealthPoller) visit(dev queries.Device, now time.Time, s **healthSettings) {
	online := dev.Status == "online"
	paused := n.cadence.Paused(dev, now)
	if !online {
		delete(n.bridgeCounts, dev.ID)
		delete(n.portCounts, dev.ID)
	}
	var bpIv, vlIv, lgIv time.Duration
	if online {
		bpIv = runInterval(n.cadence, dev, devclass.TierBridgePorts, now)
		vlIv = runInterval(n.cadence, dev, devclass.TierBridgeVLAN, now)
		lgIv = runInterval(n.cadence, dev, devclass.TierLogs, now)
	}
	bpDue := n.clock.due(dev.ID, devclass.TierBridgePorts, bpIv, now)
	vlDue := n.clock.due(dev.ID, devclass.TierBridgeVLAN, vlIv, now)
	lgDue := n.clock.due(dev.ID, devclass.TierLogs, lgIv, now)

	// Port monitoring: link down / admin disable / flap / loop-protect on
	// every matching interface. This is independent of bridge state because
	// it surfaces issues on uplink interfaces and unbridged WAN ports too,
	// and it reads the shared interface snapshot — no device command, so
	// neither a pooled connection nor a successful bridge read is needed. A
	// paused device has no new snapshot to look at.
	if online && !paused && now.Sub(n.lastPortScan[dev.ID]) >= n.portScanInterval(dev, now) {
		if set := n.settingsOnce(s); set.ports.Enabled {
			added, ports := n.monitorPorts(dev, set.ports)
			n.newEvents += added
			n.portCounts[dev.ID] = ports
		}
		n.lastPortScan[dev.ID] = now
	}

	if !bpDue && !vlDue && !lgDue {
		return
	}
	run := n.runner(dev.ID)
	if run == nil {
		retry := now.Add(noClientRetry)
		for _, t := range []struct {
			tier devclass.Tier
			due  bool
		}{{devclass.TierBridgePorts, bpDue}, {devclass.TierBridgeVLAN, vlDue}, {devclass.TierLogs, lgDue}} {
			if t.due {
				n.clock.retryAt(dev.ID, t.tier, retry)
			}
		}
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("network health: panic on %s: %v\n%s", dev.Identity, r, debug.Stack())
		}
	}()
	if bpDue {
		ok := n.readBridges(dev, run, now, n.settingsOnce(s).tcn)
		n.clock.ran(dev.ID, devclass.TierBridgePorts, bpIv, n.now())
		if !ok {
			return
		}
	}
	if vlDue {
		n.readVLANs(dev, run)
		n.clock.ran(dev.ID, devclass.TierBridgeVLAN, vlIv, n.now())
	}
	if lgDue {
		n.readLogs(dev, run, now, lgIv)
		n.clock.ran(dev.ID, devclass.TierLogs, lgIv, n.now())
	}
}

// readBridges is Tier 2a–2c: the bridges, their ports' STP state and the
// bridge summary, upserted, with the stp_disabled and tcn_storm detectors.
// It reports whether the device answered.
func (n *NetworkHealthPoller) readBridges(dev queries.Device, run routeros.Runner, cycleStart time.Time, tcnThreshold int) bool {
	bridges, err := routeros.ListBridges(run)
	if err != nil {
		return false
	}
	// Keep only genuine bridges — some RouterOS versions report
	// non-bridge interfaces via /interface/bridge/print, which would
	// otherwise be tracked as bridges, monitored, and drive false TCN
	// storms. Use cached interface types (a live fetch can fail on 7.23).
	bridges = filterRealBridges(bridges, nonBridgeInterfaceNames(n.db, dev.ID))
	state, err := routeros.ReadBridgeState(run, bridges, n.bridgeCaps[dev.ID], dev.ROSVersion, cycleStart)
	if err != nil {
		return false
	}
	n.bridgeCaps[dev.ID] = state.Caps
	if prev := n.bridgePath[dev.ID]; prev != state.Path {
		n.bridgePath[dev.ID] = state.Path
		log.Printf("network health: %s: STP state via %s (%d commands for %d bridges, %d ports)",
			nonempty(dev.Identity, dev.Address), state.Path, 1+state.Commands, len(state.Bridges), len(state.Ports))
	}
	bridges, ports := state.Bridges, state.Ports

	// Group ports by bridge for port_count and stp_disabled detection.
	portsByBridge := make(map[string][]routeros.BridgePortInfo)
	for _, p := range ports {
		portsByBridge[p.Bridge] = append(portsByBridge[p.Bridge], p)
	}

	n.bridgeCounts[dev.ID] = len(bridges)
	for _, b := range bridges {
		bs := &queries.BridgeStatus{
			ID:                 dev.ID + ":" + b.Name,
			DeviceID:           dev.ID,
			BridgeName:         b.Name,
			Protocol:           b.ProtocolMode,
			STPEnabled:         b.STPEnabled(),
			BridgeID:           b.BridgeID,
			RootBridgeID:       b.RootBridgeID,
			RootPathCost:       b.RootPathCost,
			RootPort:           b.RootPort,
			TopologyChanges:    b.TopologyChanges,
			LastTopologyChange: b.LastTopologyChange,
			PortCount:          len(portsByBridge[b.Name]),
			STPSource:          state.Path,
		}
		_ = queries.UpsertBridgeStatus(n.db, bs)

		key := dev.ID + "|" + b.Name
		// The bridge monitor has no topology-changes field: the TCN
		// rate is the sum of the ports' counter deltas, per minute.
		tcnDelta, hadPrev := n.bridgeTCNDelta(key, portsByBridge[b.Name], cycleStart)

		// Remember bridges that run STP so a later flickering "none" read
		// (RouterOS 7.23) doesn't produce a false stp_disabled warning.
		if b.STPEnabled() {
			n.bridgeSTPSeen[key] = true
		}

		// stp_disabled: bridge has multiple *non-edge* ports but no STP.
		// A loop needs at least two non-edge ports to physically form, so
		// counting only non-edge ports stops access-only bridges (all
		// edge ports) and single-uplink bridges from false-firing.
		nonEdgeCount := countNonEdgePorts(portsByBridge[b.Name])
		if shouldFlagSTPDisabled(b, n.bridgeSTPSeen[key], nonEdgeCount) {
			if n.recordEvent(dev.ID, "stp_disabled", "warn", b.Name, "", "",
				fmt.Sprintf("bridge %q runs %d non-edge ports with STP disabled (protocol=%s)",
					b.Name, nonEdgeCount, nonempty(b.ProtocolMode, "none"))) {
				n.newEvents++
			}
		}

		// tcn_storm: topology changes rising fast. Only STP-running
		// bridges can storm (see tcnStormSeverity); the threshold is
		// runtime-tunable, per minute, and escalates to critical past
		// a hard ceiling regardless of the configured warn threshold.
		if hadPrev {
			if severity, fire := tcnStormSeverity(b, tcnDelta, tcnThreshold); fire {
				if n.recordEvent(dev.ID, "tcn_storm", severity, b.Name, "", "",
					fmt.Sprintf("bridge %q topology-changes rose by %d/min since last poll (now %d)",
						b.Name, tcnDelta, b.TopologyChanges)) {
					n.newEvents++
				}
			}
		}
	}

	for _, p := range ports {
		ps := &queries.BridgePortStatus{
			ID:               dev.ID + ":" + p.Bridge + ":" + p.Interface,
			DeviceID:         dev.ID,
			BridgeName:       p.Bridge,
			PortInterface:    p.Interface,
			Role:             p.Role,
			Status:           p.Status,
			Edge:             p.Edge,
			PointToPoint:     p.PointToPoint,
			PathCost:         p.PathCost,
			DesignatedBridge: p.DesignatedBridge,
			Inactive:         ptr(p.Inactive),
			Disabled:         ptr(p.Disabled),
			HWOffloadGroup:   p.HWOffloadGroup,
			PortState:        p.PortState,
			EdgeConfig:       p.EdgeConfig,
		}
		// Unknown stays NULL: a device that reports no runtime state
		// or counter must not look like "not forwarding, 0 changes".
		if p.HasRuntime {
			ps.Forwarding, ps.Learning = ptr(p.Forwarding), ptr(p.Learning)
		}
		if p.HasTopologyChanges {
			ps.TopologyChanges = ptr(p.TopologyChanges)
		}
		if p.HWOffloadReported {
			ps.HWOffload = ptr(p.HWOffload)
		}
		if p.PVID > 0 {
			ps.PVID = ptr(p.PVID)
		}
		_ = queries.UpsertBridgePortStatus(n.db, ps)
	}
	return true
}

// readVLANs is Tier 3a: the bridge VLAN table — which VLANs exist and where
// they're tagged / untagged. Best-effort: devices without bridge VLAN
// filtering return an empty table.
func (n *NetworkHealthPoller) readVLANs(dev queries.Device, run routeros.Runner) {
	vlans, err := routeros.GetBridgeVLANs(run)
	if err != nil {
		return
	}
	for _, v := range vlans {
		bv := &queries.BridgeVLAN{
			ID:              dev.ID + ":" + v.Bridge + ":" + v.VLANIDs,
			DeviceID:        dev.ID,
			BridgeName:      v.Bridge,
			VLANIDs:         v.VLANIDs,
			Tagged:          v.Tagged,
			Untagged:        v.Untagged,
			CurrentTagged:   v.CurrentTagged,
			CurrentUntagged: v.CurrentUntagged,
			Comment:         v.Comment,
		}
		_ = queries.UpsertBridgeVLAN(n.db, bv)
	}
}

// readLogs is TierLogs: bridge log events (loop / mac flap / bpdu on edge) and
// account lines from the shared per-device log read, whose window follows the
// device's log cadence.
func (n *NetworkHealthPoller) readLogs(dev queries.Device, run routeros.Runner, now time.Time, interval time.Duration) {
	entries, err := n.logs.Entries(logConsumerBridge, dev.ID, run, now, logReadWindow(interval))
	if err != nil {
		return
	}
	n.newEvents += n.processBridgeLogs(dev.ID, routeros.BridgeLogEventsFrom(entries), now)
	n.logins.Process(&dev, run, entries, now)
}

// staleReads is how many of its own reads a row may miss before it is
// pruned as gone from the device.
const staleReads = 3

// minStaleRowAge is the shortest a row outlives its device's last report: the
// fleet-wide window the prunes used before per-device cadences. Three reads
// of a C3 router are only minutes, and a device that reboots or drops off for
// a few minutes — the moment an operator opens the network-health page or the
// VLAN matrix to look into it — must not vanish from them.
const minStaleRowAge = 30 * time.Minute

// staleAge is how old a row read every interval may get before the prune
// takes it as gone from the device.
func staleAge(interval time.Duration) time.Duration {
	return max(staleReads*interval, minStaleRowAge)
}

// pausedSeenKeep bounds how long the end of a pause is remembered: longer
// than any stale window.
const pausedSeenKeep = 24 * time.Hour

// maintain runs once per floor interval: it drops the rows of bridges, ports,
// VLANs and interfaces a device stopped reporting — rows older than 3× that
// device's cadence for them, at least 30 min (staleAge), so a weak switch
// read every 15 min is not pruned on a router's clock — prunes the log
// fingerprints and publishes the network.health summary. A paused device's
// rows are never pruned, and once its pause ends each tier gets a whole stale
// window to be read again before its rows count as gone: the rows are as old
// as the pause, and a first read that fails (no pooled connection yet, a
// session lost during the pause) must not wipe the device off the page.
func (n *NetworkHealthPoller) maintain(devices []queries.Device, now time.Time) {
	if !n.lastMaint.IsZero() && now.Sub(n.lastMaint) < n.interval {
		return
	}
	n.lastMaint = now

	for _, dev := range devices {
		if n.cadence.Paused(dev, now) {
			n.pausedSeen[dev.ID] = now
			continue
		}
		resumed, wasPaused := n.pausedSeen[dev.ID]
		if wasPaused && now.Sub(resumed) > pausedSeenKeep {
			delete(n.pausedSeen, dev.ID)
			wasPaused = false
		}
		// prune reports whether rows read every iv may be pruned now.
		prune := func(iv time.Duration) bool {
			return iv > 0 && !(wasPaused && now.Sub(resumed) <= staleAge(iv))
		}
		if iv := n.cadence.Interval(dev, devclass.TierBridgePorts, now); prune(iv) {
			if err := queries.DeleteStaleBridgeRows(n.db, dev.ID, now.Add(-staleAge(iv))); err != nil {
				log.Printf("network health: stale bridge cleanup: %v", err)
			}
		}
		if iv := n.cadence.Interval(dev, devclass.TierBridgeVLAN, now); prune(iv) {
			if err := queries.DeleteStaleBridgeVLANs(n.db, dev.ID, now.Add(-staleAge(iv))); err != nil {
				log.Printf("network health: stale bridge vlan cleanup: %v", err)
			}
		}
		if iv := n.portScanInterval(dev, now); prune(iv) {
			if err := queries.DeleteStaleInterfaceStates(n.db, dev.ID, now.Add(-staleAge(iv))); err != nil {
				log.Printf("network health: stale interface cleanup: %v", err)
			}
		}
	}

	if now.Sub(n.lastLogPruneAt) > 5*time.Minute {
		n.pruneLogFingerprints(now)
		n.lastLogPruneAt = now
	}

	// Broadcast a summary so the UI can refresh: the bridges and ports of
	// each device's latest visit, and the events since the last summary.
	totalBridges, totalPorts := 0, 0
	for _, c := range n.bridgeCounts {
		totalBridges += c
	}
	for _, c := range n.portCounts {
		totalPorts += c
	}
	since := now.Add(-24 * time.Hour)
	warn, crit, _ := queries.CountRecentLoopEvents(n.db, since)
	n.hub.Publish("network.health", map[string]interface{}{
		"bridges":      totalBridges,
		"ports":        totalPorts,
		"new_events":   n.newEvents,
		"warn_24h":     warn,
		"critical_24h": crit,
		"polled_at":    now.Format(time.RFC3339),
	})

	if n.newEvents > 0 {
		log.Printf("network health: %d bridges polled, %d new loop events", totalBridges, n.newEvents)
	}
	n.newEvents = 0
}

// processBridgeLogs applies new bridge log events from one device. Returns
// how many new events were recorded. Every line the shared LogReader hands
// out is new — the buffer it found on its first read of the device is handed
// to nobody, so a backend restart does not replay old loops — and the
// fingerprints only guard against a line delivered twice.
func (n *NetworkHealthPoller) processBridgeLogs(devID string, events []routeros.BridgeLogEvent, now time.Time) int {
	seen := n.seenLogs[devID]
	if seen == nil {
		seen = make(map[string]time.Time)
		n.seenLogs[devID] = seen
	}

	added := 0
	for i := range events {
		ev := &events[i]
		fp := ev.Fingerprint()
		if _, ok := seen[fp]; ok {
			seen[fp] = now
			continue
		}
		seen[fp] = now

		severity := "warn"
		switch ev.Kind {
		case "loop_detected", "mac_flap":
			severity = "critical"
		}
		message := ev.Message
		if message == "" {
			message = ev.Kind
		}
		port := ev.Port
		if ev.OtherPort != "" {
			port = ev.Port + " ↔ " + ev.OtherPort
		}
		if n.recordEvent(devID, ev.Kind, severity, ev.Bridge, port, ev.MAC, message) {
			added++
		}
	}
	return added
}

// monitorPorts runs the port monitor over dev's latest interface snapshot —
// the port-stats collector's Tier 0 read with the Tier 4a loop-protect state
// merged in — and returns how many events it recorded and how many ports it
// looked at. The rows are processed as of the snapshot's read time, so a
// snapshot seen on two cycles counts its link-downs once. A device without a
// fresh snapshot (offline, or not read since startup) is skipped this cycle,
// as a failed interface print was before.
func (n *NetworkHealthPoller) monitorPorts(dev queries.Device, settings PortMonitorSettings) (events, ports int) {
	if n.ifaces == nil {
		return 0, 0
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("network health: panic monitoring ports of %s: %v\n%s", dev.Identity, r, debug.Stack())
		}
	}()
	rows, at, ok := n.ifaces.DeviceSnapshot(dev.ID)
	if !ok {
		return 0, 0
	}
	n.ports.rememberFirstRun(dev.ID)
	for _, iface := range rows {
		if !settings.includeInterface(iface) {
			continue
		}
		pevents, state := n.ports.processInterface(dev.ID, iface, settings, at)
		_ = queries.UpsertInterfaceState(n.db, state)
		for _, pe := range pevents {
			if n.recordEvent(dev.ID, pe.Kind, pe.Severity, "", pe.Interface, "", pe.Message) {
				events++
			}
		}
		ports++
	}
	n.ports.markDeviceProcessed(dev.ID)
	return events, ports
}

// recordEvent writes a loop_events row and broadcasts it.
// Returns true if the event was successfully inserted.
func (n *NetworkHealthPoller) recordEvent(devID, kind, severity, bridge, port, mac, message string) bool {
	ev := &queries.LoopEvent{
		DeviceID:      devID,
		EventType:     kind,
		Severity:      severity,
		BridgeName:    bridge,
		PortInterface: port,
		MACAddress:    strings.ToUpper(mac),
		Message:       message,
	}
	id, err := queries.InsertLoopEvent(n.db, ev)
	if err != nil {
		log.Printf("network health: insert loop_event: %v", err)
		return false
	}
	n.hub.Publish("network.health.event", map[string]interface{}{
		"id":             id,
		"device_id":      devID,
		"event_type":     kind,
		"severity":       severity,
		"bridge_name":    bridge,
		"port_interface": port,
		"mac_address":    ev.MACAddress,
		"message":        message,
		"recorded_at":    time.Now().Format(time.RFC3339),
	})
	return true
}

func (n *NetworkHealthPoller) pruneLogFingerprints(now time.Time) {
	cutoff := now.Add(-bridgeLogFingerprintTTL)
	for devID, seen := range n.seenLogs {
		for fp, t := range seen {
			if t.Before(cutoff) {
				delete(seen, fp)
			}
		}
		if len(seen) == 0 {
			delete(n.seenLogs, devID)
		}
	}
}

// tcnStormThreshold reads the runtime-tunable TCN-storm delta from app_settings,
// falling back to the default. Read each poll so Settings changes apply without a
// backend restart.
func (n *NetworkHealthPoller) tcnStormThreshold() int {
	v, err := queries.GetSetting(n.db, "tcn_storm_threshold")
	if err != nil {
		return defaultTCNStormThreshold
	}
	t, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || t <= 0 {
		return defaultTCNStormThreshold
	}
	return t
}

func nonempty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
