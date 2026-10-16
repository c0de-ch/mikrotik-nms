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
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// NetworkHealthPoller tracks bridge / STP state and surfaces L2 loop signals.
//
// Per cycle it does, for each online device:
//  1. /interface/bridge/print → the bridges (proplisted)
//  2. /interface/bridge/port/print → ports with their STP runtime state on
//     RouterOS >= 7.24, else one batched (or, last resort, per-port) monitor;
//     one batched /interface/bridge/monitor for the bridge summary
//     (routeros.ReadBridgeState learns what each device accepts)
//  3. /log/print filtered to bridge,info → loop / mac flap events
//
// Anomaly detection writes rows to loop_events:
//   - stp_disabled: bridge has STP off and >1 non-edge port
//   - tcn_storm:    the ports' topology-changes counters rose by >= tcn_storm_threshold per minute
//   - loop_detected / mac_flap / bpdu_on_edge: directly from parsed bridge log
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

	// logs is the shared per-device log read (UseLogReader); logins turns
	// its account lines into device_findings.
	logs   *LogReader
	logins *LoginWatch
}

// UseLogReader shares one log reader with the other pollers, so a device's
// buffer is read once per window for all of them.
func (n *NetworkHealthPoller) UseLogReader(r *LogReader) {
	if r != nil {
		n.logs = r
	}
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
	return &NetworkHealthPoller{
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
	}
}

func (n *NetworkHealthPoller) Run(ctx context.Context) {
	ticker := time.NewTicker(n.interval)
	defer ticker.Stop()

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
	time.Sleep(20 * time.Second)
	n.safePoll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.safePoll(ctx)
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

func (n *NetworkHealthPoller) poll(ctx context.Context) {
	devices, err := queries.ListDevices(n.db)
	if err != nil {
		return
	}

	cycleStart := time.Now()
	totalBridges := 0
	totalEvents := 0
	totalPorts := 0

	portSettings := loadPortMonitorSettings(n.db)
	tcnThreshold := n.tcnStormThreshold()

	activeDevices := make(map[string]struct{}, len(devices))
	for _, dev := range devices {
		activeDevices[dev.ID] = struct{}{}
	}
	n.ports.pruneDevices(activeDevices)
	n.pruneTCState(activeDevices)
	n.logs.Prune(activeDevices)

	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if dev.Status != "online" {
			continue
		}
		client := n.pool.GetLive(dev.ID)
		if client == nil {
			continue
		}

		evCount := 0
		brCount := 0
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("network health: panic on %s: %v\n%s", dev.Identity, r, debug.Stack())
				}
			}()

			bridges, err := routeros.ListBridges(client)
			if err != nil {
				return
			}
			// Keep only genuine bridges — some RouterOS versions report
			// non-bridge interfaces via /interface/bridge/print, which would
			// otherwise be tracked as bridges, monitored, and drive false TCN
			// storms. Use cached interface types (a live fetch can fail on 7.23).
			bridges = filterRealBridges(bridges, nonBridgeInterfaceNames(n.db, dev.ID))
			state, err := routeros.ReadBridgeState(client, bridges, n.bridgeCaps[dev.ID], dev.ROSVersion, cycleStart)
			if err != nil {
				return
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

			brCount = len(bridges)
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
						evCount++
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
							evCount++
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

			// Bridge VLAN table: which VLANs exist and where they're
			// tagged / untagged. Best-effort — devices without bridge VLAN
			// filtering return an empty table.
			vlans, err := routeros.GetBridgeVLANs(client)
			if err == nil {
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

			// Bridge log events (loop / mac flap / bpdu on edge) and account
			// lines, from the shared per-device log read.
			run := routeros.ClientRunner{Client: client}
			entries, firstSight, err := n.logs.Entries(logConsumerBridge, dev.ID, run, cycleStart)
			if err == nil {
				added := n.processBridgeLogs(dev.ID, routeros.BridgeLogEventsFrom(entries), cycleStart)
				evCount += added
				n.logins.Process(&dev, run, entries, firstSight, cycleStart)
			}

			// Port monitoring: link down / admin disable / flap on every
			// matching interface. This is independent of bridge state because
			// it surfaces issues on uplink interfaces and unbridged WAN
			// ports too.
			if portSettings.Enabled {
				ifaces, err := routeros.GetInterfacesDetail(client)
				if err == nil {
					n.ports.rememberFirstRun(dev.ID)
					for _, iface := range ifaces {
						if !portSettings.includeInterface(iface) {
							continue
						}
						pevents, state := n.ports.processInterface(dev.ID, iface, portSettings, cycleStart)
						_ = queries.UpsertInterfaceState(n.db, state)
						for _, pe := range pevents {
							if n.recordEvent(dev.ID, pe.Kind, pe.Severity, "", pe.Interface, "", pe.Message) {
								evCount++
							}
						}
						totalPorts++
					}
					n.ports.markDeviceProcessed(dev.ID)
				}
			}
		}()

		totalBridges += brCount
		totalEvents += evCount
	}

	// Drop rows for bridges / interfaces that vanished from devices in the
	// last 30min so the UI doesn't keep showing ghost ports forever.
	staleCutoff := time.Now().Add(-30 * time.Minute)
	if err := queries.DeleteStaleBridgeRows(n.db, staleCutoff); err != nil {
		log.Printf("network health: stale bridge cleanup: %v", err)
	}
	if err := queries.DeleteStaleInterfaceStates(n.db, staleCutoff); err != nil {
		log.Printf("network health: stale interface cleanup: %v", err)
	}
	if err := queries.DeleteStaleBridgeVLANs(n.db, staleCutoff); err != nil {
		log.Printf("network health: stale bridge vlan cleanup: %v", err)
	}

	if cycleStart.Sub(n.lastLogPruneAt) > 5*time.Minute {
		n.pruneLogFingerprints(cycleStart)
		n.lastLogPruneAt = cycleStart
	}

	// Broadcast a summary so the UI can refresh.
	since := time.Now().Add(-24 * time.Hour)
	warn, crit, _ := queries.CountRecentLoopEvents(n.db, since)
	n.hub.Publish("network.health", map[string]interface{}{
		"bridges":      totalBridges,
		"ports":        totalPorts,
		"new_events":   totalEvents,
		"warn_24h":     warn,
		"critical_24h": crit,
		"polled_at":    cycleStart.Format(time.RFC3339),
	})

	if totalEvents > 0 {
		log.Printf("network health: %d bridges polled, %d new loop events", totalBridges, totalEvents)
	}
}

// processBridgeLogs applies new bridge log events from one device. Returns
// how many new events were recorded. On the very first poll for a device, the
// existing log buffer is marked seen without firing events — otherwise a
// backend restart would replay the entire log buffer as new events.
func (n *NetworkHealthPoller) processBridgeLogs(devID string, events []routeros.BridgeLogEvent, now time.Time) int {
	seen, exists := n.seenLogs[devID]
	firstRun := !exists
	if !exists {
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
		if firstRun {
			continue
		}

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
