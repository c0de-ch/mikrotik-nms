package poller

import (
	"context"
	"database/sql"
	"log"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/flow"
	"github.com/mikrotik-nms/backend/internal/resolver"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/topology"
	"github.com/mikrotik-nms/backend/internal/ws"
)

type Manager struct {
	db         *sql.DB
	pool       *routeros.Pool
	hub        *ws.Hub
	cfg        *config.Config
	cancel     context.CancelFunc
	lastVacuum time.Time

	// portStats is built in NewManager (not Start) because main reads it via
	// PortStats() right after launching Start asynchronously.
	portStats *PortStatsCollector

	// flows is the NetFlow/IPFIX/sFlow collector (disabled unless
	// MIKROTIK_NMS_FLOW_LISTEN is set), built in NewManager for the same
	// reason as portStats.
	flows *flow.Collector

	// ipRejectSeen rate-limits auto-follow rejection audit rows by proposed
	// move + failure category (see ipRejectionTTL / recordIPRejection). Accessed
	// only from the topology goroutine today, but guarded so it stays safe if
	// that changes.
	ipRejectMu   sync.Mutex
	ipRejectSeen map[string]time.Time

	// cadence is the per-class schedule every loop shares (NewCadence).
	cadence CadenceSource
	// inventory schedules Tier 6 (the info refresh) per device, topoClock
	// the topology loop's neighbor and route reads; classes holds the class
	// probe's pending changes (hysteresis).
	inventory *tierClock
	topoClock *tierClock
	classes   *classGate
	// infoBusy guards against two concurrent info refreshes of one device
	// (the info loop and the liveness poll's enrichment of a new device).
	infoBusy sync.Map
	// lastBuild is when the topology loop last rebuilt the link graph.
	lastBuild time.Time
	// Test seams, all optional: runner is the topology loop's device link
	// (nil = no pooled connection; the pool when unset), now the clock of
	// the per-device loops, infoRefresh the info refresh the info loop runs.
	// stagger spaces the devices one topology tick reads (NewManager: a
	// second).
	runner      func(deviceID string) routeros.Runner
	now         func() time.Time
	infoRefresh func(dev queries.Device)
	stagger     time.Duration
}

// deviceRunner is deviceID's device link for the topology loop.
func (m *Manager) deviceRunner(deviceID string) routeros.Runner {
	if m.runner != nil {
		return m.runner(deviceID)
	}
	return poolRunner(m.pool, deviceID)
}

// clock is the loops' time.
func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

func NewManager(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, cfg *config.Config) *Manager {
	flowCfg := flow.Config{Listen: cfg.FlowListen, CaptureDir: cfg.FlowCaptureDir}
	if !cfg.PollersEnabled() {
		// Polling off means no UDP I/O either: the collector stays disabled
		// even when a listen address is configured.
		flowCfg.Listen = nil
	}
	m := &Manager{
		db:           db,
		pool:         pool,
		hub:          hub,
		cfg:          cfg,
		ipRejectSeen: make(map[string]time.Time),
		portStats:    NewPortStatsCollector(db, pool, hub),
		flows:        flow.New(db, pool, hub, flowCfg),
		cadence:      NewCadence(db, cfg),
		inventory:    newTierClock(Schedule()),
		topoClock:    newTierClock(Schedule()),
		classes:      newClassGate(),
		stagger:      time.Second,
	}
	// One schedule for every loop: the class cadences, floored by the
	// fleet-wide settings and env vars, and the per-device pause.
	m.portStats.UseCadence(m.cadence)
	// The port-stats collector's Tier 0 read is the one interface print:
	// the flow ifIndex sync (here), the port monitor and the uplink pass
	// (Start, topology) read its snapshot instead of printing again; its
	// stacking read (Tier 3c) also serves the sync's VLAN rows.
	m.flows.UseIfaceSource(m.portStats)
	// The flow plan and point facing judge egress rows as the map does.
	m.flows.UseUplinkAge(UplinkAge(m.cadence))
	return m
}

// PortStats returns the fleet-wide port-counter collector (non-nil right
// after NewManager) for the API's snapshot and history endpoints.
func (m *Manager) PortStats() *PortStatsCollector {
	return m.portStats
}

// Flows returns the flow collector (non-nil right after NewManager; disabled
// when no listen address is configured) for the API's /flows endpoints.
func (m *Manager) Flows() *flow.Collector {
	return m.flows
}

// Cadence returns the schedule every loop shares (non-nil right after
// NewManager), so the API judges link, egress and FDB staleness by the same
// per-device cadences, floors and pause as the pollers that wrote the rows.
func (m *Manager) Cadence() CadenceSource {
	return m.cadence
}

// Start launches the background loops. Database-only jobs — retention and
// the port-stats hourly rollup — run on every instance; everything that
// reaches a device or binds UDP runs only with MIKROTIK_NMS_POLLERS=local, so
// an instance started against a copy of the production database still shows
// and maintains its history without contacting a single device.
func (m *Manager) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	go m.retentionLoop(ctx)

	if !m.cfg.PollersEnabled() {
		m.portStats.startupRollup()
		log.Println("poller: device polling is off (MIKROTIK_NMS_POLLERS=off) — only database maintenance runs")
		return
	}

	go m.healthLoop(ctx)
	go m.infoLoop(ctx)
	go m.topologyLoop(ctx)
	go m.firmwareLoop(ctx)

	trafficMgr := NewTrafficManager(m.db, m.pool, m.hub)
	go trafficMgr.Run(ctx)

	liveTraffic := NewLiveTrafficCollector(m.db, m.portStats, m.hub)
	liveTraffic.UseCadence(m.cadence)
	go liveTraffic.Run(ctx)

	go m.portStats.Run(ctx)

	go m.flows.Run(ctx)

	// One /log/print per device per window, shared by the wifi and
	// network-health pollers, and one capability probe per device for all.
	logs := NewLogReader()
	caps := NewCapabilityCache()

	wifiTracker := NewWifiTracker(m.db, m.pool, m.hub, 30*time.Second)
	wifiTracker.UseLogReader(logs)
	wifiTracker.UseCapabilities(caps)
	wifiTracker.UseCadence(m.cadence)
	go wifiTracker.Run(ctx)

	clientDisc := NewClientDiscoveryPoller(m.db, m.pool, resolver.New(m.db), 15*time.Minute)
	clientDisc.UseCadence(m.cadence)
	go clientDisc.Run(ctx)

	netHealth := NewNetworkHealthPoller(m.db, m.pool, m.hub, m.cfg.NetworkHealthInterval)
	netHealth.UseLogReader(logs)
	netHealth.UseInterfaceSnapshots(m.portStats)
	netHealth.UseCadence(m.cadence)
	go netHealth.Run(ctx)

	connectivity := NewConnectivityPoller(m.db, m.pool, m.hub, defaultConnectivityInterval, m.cfg.ROSTLSVerify)
	go connectivity.Run(ctx)

	speedTests := NewSpeedTestPoller(m.db, m.hub, m.cfg.ROSTLSVerify, defaultSpeedTestInterval)
	go speedTests.Run(ctx)

	log.Println("poller: started all pollers")
}

func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	log.Println("poller: stopped")
}

func (m *Manager) healthLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.HealthInterval)
	defer ticker.Stop()

	// Run immediately on start
	m.pollAllDevices(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollAllDevices(ctx)
		}
	}
}

func (m *Manager) pollAllDevices(ctx context.Context) {
	devices, err := queries.ListDevices(m.db)
	if err != nil {
		log.Printf("poller: list devices: %v", err)
		return
	}
	// Deleted devices leave the schedule view and the per-device clocks.
	keep := deviceIDs(devices)
	Schedule().Retain(keep)
	m.inventory.retain(keep)
	m.topoClock.retain(keep)
	m.classes.forget(keep)

	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return
		default:
			m.safePollDevice(dev)
		}
	}
}

func (m *Manager) safePollDevice(dev queries.Device) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller: panic polling %s (%s): %v\n%s", dev.Identity, dev.Address, r, debug.Stack())
			m.pool.Close(dev.ID)
		}
	}()
	m.pollDevice(dev)
}

// pollDevice is the lightweight liveness check run on the frequent health
// interval, for every device — paused or not. It only answers "is the device
// up?" via a cheap TCP ping that bypasses the RouterOS API session, so it
// can't contend with the heavier pollers and doesn't flap on transient API
// slowness. The full stats/version/interface refresh is the infoLoop's job
// (see refreshDeviceInfo).
func (m *Manager) pollDevice(dev queries.Device) {
	if err := m.ping(dev); err != nil {
		m.markUnreachable(dev, err)
		return
	}

	_ = queries.MarkDeviceOnline(m.db, dev.ID)
	m.hub.Publish("device.health", map[string]interface{}{
		"device_id": dev.ID,
		"status":    "online",
		"last_seen": time.Now().UTC().Format(time.RFC3339),
	})

	// A device we've never gathered details from (freshly added) gets enriched
	// right away instead of waiting up to a full info interval — unless its
	// polling is paused: then only this ping runs. (A device that has details
	// but no class yet is picked up by the info loop's next tick.)
	if (dev.Board == "" || dev.ROSVersion == "") && !m.cadence.Paused(dev, time.Now()) {
		m.refreshDeviceInfo(dev)
	}
}

// ping does a couple of quick TCP dials so a single dropped packet doesn't
// register as the device being down.
func (m *Manager) ping(dev queries.Device) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if err = routeros.Ping(dev.Address, dev.APIPort, 3*time.Second); err == nil {
			return nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	return err
}

// defaultOfflineThreshold is how long a device may stay unreachable before it's
// reported offline, when the admin hasn't configured offline_threshold_seconds.
const defaultOfflineThreshold = 120 * time.Second

// offlineThreshold reads the runtime-tunable grace period from app_settings,
// falling back to the default. Read each failed poll so Settings changes apply
// without a backend restart.
func (m *Manager) offlineThreshold() time.Duration {
	v, err := queries.GetSetting(m.db, "offline_threshold_seconds")
	if err != nil {
		return defaultOfflineThreshold
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultOfflineThreshold
	}
	return time.Duration(n) * time.Second
}

// statusAfterFailedPoll decides what status to record for a device whose poll
// just failed. While still within the grace period (measured from its last
// successful contact) it reports "unknown" — surfaced in the UI as the gray
// "not responding" state — and only flips to "offline" once the grace period
// elapses (or if the device has never been reached).
func statusAfterFailedPoll(lastSeen *time.Time, threshold time.Duration, now time.Time) string {
	if lastSeen != nil && now.Sub(*lastSeen) < threshold {
		return "unknown"
	}
	return "offline"
}

// markUnreachable records a failed poll. To avoid flapping a device to offline
// on a single missed poll, it's only reported "offline" once it has been out of
// contact (last_seen) for longer than the configured grace period. Within that
// window it's reported "unknown" ("not responding" in the UI).
func (m *Manager) markUnreachable(dev queries.Device, pollErr error) {
	errStr := pollErr.Error()
	status := statusAfterFailedPoll(dev.LastSeen, m.offlineThreshold(), time.Now())
	_ = queries.MarkDeviceUnreachable(m.db, dev.ID, status, &errStr)

	payload := map[string]interface{}{
		"device_id": dev.ID,
		"status":    status,
		"error":     errStr,
	}
	if dev.LastSeen != nil {
		payload["last_seen"] = dev.LastSeen.UTC().Format(time.RFC3339)
	}
	m.hub.Publish("device.health", payload)
}

// defaultInfoInterval is how often the full device info (cpu/memory/uptime,
// version/board/platform/arch and the CPU class) is refreshed when the admin
// hasn't configured info_interval — the floor of Tier 6, whose class cadence
// is an hour too. Static details rarely change, so the frequent liveness poll
// just pings while this heavier refresh runs rarely.
const defaultInfoInterval = 60 * time.Minute

// classProbeRetry is how soon a device that is online but has never been
// classified gets its resource print again after an attempt that did not
// classify it (no connection, a failing print), instead of an hour later.
const classProbeRetry = 5 * time.Minute

func (m *Manager) infoLoop(ctx context.Context) {
	// Every device is due right after startup so cached stats are fresh on
	// boot, then on its Tier 6 cadence (info_interval as the floor).
	if !sleepCtx(ctx, 8*time.Second) {
		return
	}
	for {
		m.refreshDueInfo(ctx, m.clock())
		if !sleepCtx(ctx, pollerTick) {
			return
		}
	}
}

// refreshDueInfo runs the info refresh of every device whose Tier 6 is due at
// now, offline ones included: dialling the pooled connection here is what
// lets the other loops reach a device at all. A device whose polling is
// paused is skipped. A device that was never classified is also due once
// online, every classProbeRetry, so a freshly added device is classified
// within a tick of its first successful ping.
func (m *Manager) refreshDueInfo(ctx context.Context, now time.Time) {
	devices, err := queries.ListDevices(m.db)
	if err != nil {
		log.Printf("poller: info refresh list devices: %v", err)
		return
	}
	for _, dev := range devices {
		if ctx.Err() != nil {
			return
		}
		iv := runInterval(m.cadence, dev, devclass.TierInventory, now)
		due := m.inventory.due(dev.ID, devclass.TierInventory, iv, now)
		if !due && iv > 0 && dev.DeviceClass == "" && dev.Status == "online" {
			due = now.Sub(m.inventory.lastEnd(dev.ID, devclass.TierInventory)) >= classProbeRetry
		}
		if !due {
			continue
		}
		if m.infoRefresh != nil {
			m.infoRefresh(dev)
		} else {
			m.safeRefreshInfo(dev)
		}
	}
}

func (m *Manager) safeRefreshInfo(dev queries.Device) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller: panic refreshing info for %s (%s): %v\n%s", dev.Identity, dev.Address, r, debug.Stack())
			m.pool.Close(dev.ID)
		}
	}()
	m.refreshDeviceInfo(dev)
}

// refreshDeviceInfo does the heavy lifting: a full RouterOS API session to read
// system resources (cpu/memory/uptime) and static details (version/board/
// platform/arch), caching them in the DB, and the CPU class derived from the
// same print (probeClass). The interface inventory is not read here: the
// port-stats collector's Tier 0 print keeps the interfaces table current
// (PortStatsCollector.syncInventory). Dialling the pooled connection here is
// still what lets that collector reach a freshly added device. Status stays
// owned by the liveness poll — on a connection failure here we leave status
// alone. The attempt counts as the device's Tier 6 run either way.
func (m *Manager) refreshDeviceInfo(dev queries.Device) {
	if _, busy := m.infoBusy.LoadOrStore(dev.ID, true); busy {
		return
	}
	defer m.infoBusy.Delete(dev.ID)
	defer func() {
		end := m.clock()
		m.inventory.ran(dev.ID, devclass.TierInventory, runInterval(m.cadence, dev, devclass.TierInventory, end), end)
	}()

	client, err := m.pool.EnsureConnection(
		dev.ID, dev.Address, dev.APIPort, dev.Username, dev.PasswordEnc, dev.UseTLS,
	)
	if err != nil {
		return
	}

	res, err := routeros.GetSystemResource(client)
	if err != nil {
		m.pool.Close(dev.ID)
		return
	}
	m.probeClass(dev, res.Fields, m.clock())

	memUsed := res.MemoryTotal - res.MemoryFree
	_ = queries.UpdateDeviceHealth(m.db, dev.ID, "online",
		&res.CPULoad, &memUsed, &res.MemoryTotal, &res.Uptime, nil)
	_ = queries.UpdateDeviceInfo(m.db, dev.ID,
		res.Platform, res.Board, res.Version, "", res.Architecture)

	memPct := 0
	if res.MemoryTotal > 0 {
		memPct = int(memUsed * 100 / res.MemoryTotal)
	}
	m.hub.Publish("device.health", map[string]interface{}{
		"device_id":  dev.ID,
		"status":     "online",
		"cpu_load":   res.CPULoad,
		"memory_pct": memPct,
		"uptime":     res.Uptime,
		"last_seen":  time.Now().UTC().Format(time.RFC3339),
	})
}

func (m *Manager) topologyLoop(ctx context.Context) {
	// Wait a bit for initial health poll to populate device info
	if !sleepCtx(ctx, 5*time.Second) {
		return
	}
	for {
		m.safePollTopology(ctx)
		if !sleepCtx(ctx, pollerTick) {
			return
		}
	}
}

func (m *Manager) safePollTopology(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller: panic in topology poll: %v", r)
		}
	}()
	m.pollTopology(ctx)
}

// pollTopology is one tick of the topology loop: the neighbor (Tier 3d) and
// default-route (Tier 3e) reads of every device for which they are due, then
// — once per MIKROTIK_NMS_TOPOLOGY_INTERVAL, the loop's former cadence — the
// gateway attachment, IP auto-follow and the link-graph rebuild, which read
// only the database.
func (m *Manager) pollTopology(ctx context.Context) {
	devices, err := queries.ListDevices(m.db)
	if err != nil {
		log.Printf("poller topology: list devices: %v", err)
		return
	}

	now := m.clock()
	polled := 0
	for _, dev := range devices {
		if ctx.Err() != nil {
			return
		}
		var nIv, rIv time.Duration
		if dev.Status == "online" {
			nIv = runInterval(m.cadence, dev, devclass.TierNeighbors, now)
			rIv = runInterval(m.cadence, dev, devclass.TierRoutes, now)
		}
		nDue := m.topoClock.due(dev.ID, devclass.TierNeighbors, nIv, now)
		rDue := m.topoClock.due(dev.ID, devclass.TierRoutes, rIv, now)
		if !nDue && !rDue {
			continue
		}

		run := m.deviceRunner(dev.ID)
		if run == nil {
			retry := now.Add(noClientRetry)
			if nDue {
				m.topoClock.retryAt(dev.ID, devclass.TierNeighbors, retry)
			}
			if rDue {
				m.topoClock.retryAt(dev.ID, devclass.TierRoutes, retry)
			}
			continue
		}
		if polled > 0 {
			time.Sleep(m.stagger) // stagger between devices
		}
		polled++
		if nDue {
			m.collectNeighbors(dev, run)
			m.topoClock.ran(dev.ID, devclass.TierNeighbors, nIv, m.clock())
		}
		if rDue {
			m.collectUplinks(dev, run)
			m.topoClock.ran(dev.ID, devclass.TierRoutes, rIv, m.clock())
		}
	}

	if !m.lastBuild.IsZero() && now.Sub(m.lastBuild) < m.cfg.TopologyInterval {
		return
	}
	m.lastBuild = now

	// Gateway MACs (from the uplink rows) to look up in the devices' bridge
	// FDBs — locates the switch+port a gateway is physically attached to
	// instead of guessing.
	m.resolveGatewayHosts(m.gatewayObservations(devices, m.gatewayMACs(devices, now), now))

	// Auto-follow IP changes: opt-in reconcile of devices that moved to a new IP,
	// using the neighbors upserted by the reads. Runs before the rebuild so a
	// committed address change is reflected in this same cycle's graph.
	m.reconcileAddresses(ctx)

	// Rebuild topology from neighbors, each judged by its reporting device's
	// own neighbor cadence (egress rows by its route cadence).
	graph, err := NewTopologyBuilder(m.db, m.cadence).Build()
	if err != nil {
		log.Printf("poller topology: build graph: %v", err)
		return
	}

	m.hub.Publish("topology.update", graph)
	if polled > 0 {
		log.Printf("poller: topology discovery complete — %d nodes, %d edges", len(graph.Nodes), len(graph.Edges))
	}
}

// collectNeighbors upserts the device's /ip/neighbor rows (Tier 3d).
func (m *Manager) collectNeighbors(dev queries.Device, run routeros.Runner) {
	neighbors, err := routeros.ReadNeighbors(run)
	if err != nil {
		log.Printf("poller topology: get neighbors for %s: %v", dev.Identity, err)
		return
	}
	for _, n := range neighbors {
		if n.NeighborMAC == "" {
			continue
		}
		_ = queries.UpsertNeighbor(m.db, &queries.Neighbor{
			ID:                dev.ID + ":" + n.LocalInterface + ":" + n.NeighborMAC,
			DeviceID:          dev.ID,
			LocalInterface:    n.LocalInterface,
			NeighborAddress:   n.NeighborAddress,
			NeighborMAC:       n.NeighborMAC,
			NeighborIdentity:  n.NeighborIdentity,
			NeighborPlatform:  n.NeighborPlatform,
			NeighborBoard:     n.NeighborBoard,
			NeighborVersion:   n.NeighborVersion,
			NeighborInterface: n.NeighborInterface,
			DiscoveredBy:      n.DiscoveredBy,
		})
	}
}

// neighborWindowReads is how many of a device's own neighbor reads its rows
// stay current for (link staleness).
const neighborWindowReads = 3

// NeighborAge is the topology builder's staleness rule on cs: a reporting
// device's neighbor rows stay current for three of its effective neighbor
// reads, and do not age while the device is paused — a link it reported
// stays up while nobody reads it.
func NeighborAge(cs CadenceSource) topology.NeighborAge {
	return func(dev queries.Device, now time.Time) (time.Duration, bool) {
		iv := cs.Interval(dev, devclass.TierNeighbors, now)
		if iv <= 0 {
			iv = defaultTopologyFloor
		}
		return neighborWindowReads * iv, cs.Paused(dev, now)
	}
}

// uplinkWindowReads is how many of a device's own route reads its egress
// rows (device_uplinks) stay current for.
const uplinkWindowReads = 3

// UplinkAge is the device_uplinks staleness rule on cs: a device's egress
// rows stay current for three of its route reads (Tier 3e, floored by
// MIKROTIK_NMS_TOPOLOGY_INTERVAL), at least queries.DefaultUplinkWindow —
// the fixed window every reader used before, under which a weak device read
// every 5 min lost its egress after two reads, one failed read and a little
// jitter — and do not age while the device is paused.
func UplinkAge(cs CadenceSource) queries.UplinkAge {
	return func(dev queries.Device, now time.Time) (time.Duration, bool) {
		window := uplinkWindowReads * cs.Interval(dev, devclass.TierRoutes, now)
		return max(window, queries.DefaultUplinkWindow), cs.Paused(dev, now)
	}
}

// FreshUplinks returns the device_uplinks rows current at now by
// UplinkAge(cs). cs nil = the fleet-wide schedule.
func FreshUplinks(db *sql.DB, cs CadenceSource, now time.Time) ([]queries.DeviceUplink, error) {
	if cs == nil {
		cs = newFleetCadence(db)
	}
	return queries.ListFreshDeviceUplinks(db, UplinkAge(cs), now)
}

// NewTopologyBuilder is the link-graph builder on cs's staleness rules —
// neighbor rows by NeighborAge, egress rows by UplinkAge — which the
// topology loop and GET /topology share, so both draw the same graph.
func NewTopologyBuilder(db *sql.DB, cs CadenceSource) *topology.Builder {
	if cs == nil {
		cs = newFleetCadence(db)
	}
	return topology.NewBuilder(db).UseNeighborAge(NeighborAge(cs)).UseUplinkAge(UplinkAge(cs))
}

// gatewayMACs returns MAC→IP for the unmanaged private gateways currently in
// device_uplinks (previous cycle) whose MAC the client cache knows. These are
// the addresses whose physical attachment the FDB pass locates.
func (m *Manager) gatewayMACs(devices []queries.Device, now time.Time) map[string]string {
	managed := make(map[string]bool, len(devices))
	for _, d := range devices {
		managed[d.Address] = true
	}
	ups, err := FreshUplinks(m.db, m.cadence, now)
	if err != nil {
		return nil
	}
	out := make(map[string]string)
	for _, u := range ups {
		if u.Kind != "default-route" {
			continue
		}
		gw := u.GatewayIP
		if gw == "" || managed[gw] || !topology.IsPrivateIP(gw) {
			continue
		}
		if mac := queries.MACForIP(m.db, gw); mac != "" {
			out[strings.ToUpper(mac)] = gw
		}
	}
	return out
}

// gatewayObservations locates the gateway MACs in the bridge FDB dumps the
// port-stats collector stores in port_hosts — each online device's latest
// dump only, while current by that device's own dump cadence (a paused
// device's last dump stays current) — so attachment costs no device command
// of its own (it used to be one FDB query per gateway MAC per device per
// cycle). A gateway whose MAC no dump holds yet (a new gateway, or a cold
// start before the first dump) stays unattached until the next dump;
// resolveGatewayHosts keeps the previous rows meanwhile.
func (m *Manager) gatewayObservations(devices []queries.Device, gwMACs map[string]string, now time.Time) []queries.DeviceUplink {
	if len(gwMACs) == 0 {
		return nil
	}
	online := make(map[string]bool, len(devices))
	for _, d := range devices {
		if d.Status == "online" {
			online[d.ID] = true
		}
	}
	macs := make([]string, 0, len(gwMACs))
	for mac := range gwMACs {
		macs = append(macs, mac)
	}
	newest, err := queries.LatestPortHostsDumps(m.db)
	if err != nil {
		log.Printf("poller topology: gateway hosts from port_hosts: %v", err)
		return nil
	}
	cut := portHostsCutoffs(m.cadence, devices, newest, now)
	hosts, err := queries.ListLatestPortHostsForMACs(m.db, earliest(cut, now), macs)
	if err != nil {
		log.Printf("poller topology: gateway hosts from port_hosts: %v", err)
		return nil
	}
	var out []queries.DeviceUplink
	for _, h := range hosts {
		ip, ok := gwMACs[strings.ToUpper(h.MACAddress)]
		if c, known := cut[h.DeviceID]; !ok || !online[h.DeviceID] || !known || h.LastSeen.Before(c) {
			continue
		}
		out = append(out, queries.DeviceUplink{DeviceID: h.DeviceID, Interface: h.InterfaceName, GatewayIP: ip})
	}
	return out
}

// resolveGatewayHosts picks, per gateway, the FDB observation that is NOT on
// an inter-switch link port — the device that learned the gateway's MAC on an
// edge port is its physical attachment. Ties go to the best-connected device.
// Keeps previous rows when this cycle produced no observations (transient
// failures must not flap the map); genuinely stale rows age out via the
// builder's freshness window.
func (m *Manager) resolveGatewayHosts(observations []queries.DeviceUplink) {
	if len(observations) == 0 {
		return
	}
	links, err := queries.ListLinks(m.db)
	if err != nil {
		return
	}
	interSwitch := interSwitchPorts(links)
	linkDegree := make(map[string]int)
	for _, l := range links {
		linkDegree[l.DeviceAID]++
		linkDegree[l.DeviceBID]++
	}

	best := make(map[string]queries.DeviceUplink)
	for _, o := range observations {
		if interSwitch[o.DeviceID+":"+o.Interface] {
			continue // learned via an uplink → transit, not the attachment point
		}
		cur, ok := best[o.GatewayIP]
		if !ok || linkDegree[o.DeviceID] > linkDegree[cur.DeviceID] ||
			(linkDegree[o.DeviceID] == linkDegree[cur.DeviceID] && o.DeviceID < cur.DeviceID) {
			best[o.GatewayIP] = o
		}
	}
	if len(best) == 0 {
		return
	}
	rows := make([]queries.DeviceUplink, 0, len(best))
	for _, o := range best {
		rows = append(rows, o)
	}
	if err := queries.ReplaceGatewayHosts(m.db, rows); err != nil {
		log.Printf("poller topology: store gateway hosts: %v", err)
	}
}

// interSwitchPorts returns the set of "deviceID:port" keys that are endpoints
// of an inter-switch link. Link interfaces come straight from neighbor
// discovery and may list several names ("ether1,bridge"), while the FDB
// reports a single physical port, so each comma-separated name is keyed on
// its own.
func interSwitchPorts(links []queries.Link) map[string]bool {
	out := make(map[string]bool, len(links)*2)
	add := func(deviceID, ifaces string) {
		for _, name := range strings.Split(ifaces, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out[deviceID+":"+name] = true
			}
		}
	}
	for _, l := range links {
		add(l.DeviceAID, l.InterfaceA)
		add(l.DeviceBID, l.InterfaceB)
	}
	return out
}

// collectUplinks refreshes the device's egress rows: its active IPv4 default
// routes (one cheap /ip/route query) plus running VPN tunnel interfaces. The
// interface types and running state come from the port-stats collector's
// interface snapshot (at most a few counter intervals old), the interfaces
// table only when there is none — no interface print of its own.
func (m *Manager) collectUplinks(dev queries.Device, run routeros.Runner) {
	routes, err := routeros.GetDefaultRoutes(run)
	if err != nil {
		// Keep the previous rows: a transient API failure must not flap the
		// map's egress edges. Rows age out via the builder's freshness window
		// once the device is genuinely unreachable.
		log.Printf("poller topology: default routes for %s: %v", dev.Identity, err)
		return
	}

	typeByName := make(map[string]string)
	var vpnNames []string
	add := func(name, typ string, running, disabled bool) {
		typeByName[name] = typ
		if running && !disabled && topology.IsVPNIfaceType(typ) {
			vpnNames = append(vpnNames, name)
		}
	}
	if rows, _, ok := m.portStats.DeviceSnapshot(dev.ID); ok {
		for _, i := range rows {
			add(i.Name, i.Type, i.Running, i.Disabled)
		}
	} else if cached, cerr := queries.ListInterfacesByDevice(m.db, dev.ID); cerr == nil {
		for _, i := range cached {
			add(i.Name, i.Type, i.Running, i.Disabled)
		}
	}

	var ups []queries.DeviceUplink
	for _, r := range routes {
		ups = append(ups, queries.DeviceUplink{
			DeviceID: dev.ID, Kind: "default-route",
			Interface: r.Interface, IfaceType: typeByName[r.Interface], GatewayIP: r.Gateway,
		})
	}
	for _, name := range vpnNames {
		ups = append(ups, queries.DeviceUplink{
			DeviceID: dev.ID, Kind: "vpn", Interface: name, IfaceType: typeByName[name],
		})
	}

	if err := queries.ReplaceDeviceUplinks(m.db, dev.ID, ups); err != nil {
		log.Printf("poller topology: store uplinks for %s: %v", dev.Identity, err)
	}
}

func (m *Manager) firmwareLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.FirmwareInterval)
	defer ticker.Stop()

	// Initial check after 30s delay
	time.Sleep(30 * time.Second)
	m.safeCheckFirmware(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.safeCheckFirmware(ctx)
		}
	}
}

func (m *Manager) safeCheckFirmware(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller: panic in firmware check: %v", r)
		}
	}()
	m.checkFirmware(ctx)
}

func (m *Manager) checkFirmware(ctx context.Context) {
	RunFirmwareCheck(ctx, m.db, m.pool, m.hub)
}

// RunFirmwareCheck polls every online device for RouterOS update availability
// and upserts firmware_status. Shared by the periodic firmware poller and the
// on-demand "Check All" API endpoint — both use the same connection pool.
// A device whose polling is paused is skipped (logged once per pause): the
// check is a fleet sweep, not an action on that device.
//
// A device whose check doesn't return a latest-version (async fetch still
// pending, or it can't reach MikroTik's update servers) is left as-is:
// UpsertFirmwareStatus preserves the last-known version rather than blanking it.
func RunFirmwareCheck(ctx context.Context, db *sql.DB, pool *routeros.Pool, hub *ws.Hub) {
	devices, err := queries.ListDevices(db)
	if err != nil {
		return
	}

	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if dev.Status != "online" {
			continue
		}
		if dev.PollingPaused(time.Now()) {
			pauseLog.skip("firmware check", dev)
			continue
		}

		client := pool.GetLive(dev.ID)
		if client == nil {
			continue
		}

		var fw *routeros.FirmwareInfo
		var rb *routeros.RouterboardInfo
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("poller firmware: panic checking %s: %v", dev.Identity, r)
					pool.Close(dev.ID)
				}
			}()
			var err error
			fw, err = routeros.CheckFirmwareUpdate(client)
			if err != nil {
				log.Printf("poller firmware: check %s: %v", dev.Identity, err)
				return
			}
			rb, _ = routeros.GetRouterboardInfo(client)
		}()
		if fw == nil {
			continue
		}

		fws := &queries.FirmwareStatus{
			ID:               dev.ID + ":fw",
			DeviceID:         dev.ID,
			Channel:          fw.Channel,
			InstalledVersion: fw.InstalledVersion,
			UpdateAvailable:  fw.UpdateAvailable,
		}
		if fw.LatestVersion != "" {
			fws.LatestVersion = &fw.LatestVersion
		}
		if rb != nil {
			fws.RouterboardCurrent = &rb.CurrentFirmware
			fws.RouterboardUpgrade = &rb.UpgradeFirmware
		}

		_ = queries.UpsertFirmwareStatus(db, fws)

		if fw.UpdateAvailable {
			hub.Publish("firmware.update", map[string]interface{}{
				"device_id":        dev.ID,
				"installed":        fw.InstalledVersion,
				"latest":           fw.LatestVersion,
				"update_available": true,
			})
		}

		time.Sleep(2 * time.Second) // Stagger between devices
	}
}

func (m *Manager) retentionLoop(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.RetentionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().AddDate(0, 0, -m.cfg.RetentionDays)
			if n, err := queries.DeleteOldTrafficSamples(m.db, cutoff); err != nil {
				log.Printf("poller retention: traffic: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d old traffic samples", n)
			}

			m.sweepNeighbors(time.Now())

			wifiCutoff := time.Now().AddDate(0, 0, -m.cfg.RetentionDays)
			if n, err := queries.DeleteOldWifiHistory(m.db, wifiCutoff); err != nil {
				log.Printf("poller retention: wifi history: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d old wifi history entries", n)
			}

			if n, err := queries.DeleteOldClientHistory(m.db, wifiCutoff); err != nil {
				log.Printf("poller retention: client history: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d old client history entries", n)
			}

			if n, err := queries.DeleteOldLoopEvents(m.db, wifiCutoff); err != nil {
				log.Printf("poller retention: loop events: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d old loop events", n)
			}

			if n, err := queries.DeleteOldPingSamples(m.db, wifiCutoff); err != nil {
				log.Printf("poller retention: ping samples: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d old ping samples", n)
			}

			if n, err := queries.DeleteOldClientSignalSamples(m.db, wifiCutoff); err != nil {
				log.Printf("poller retention: client signal samples: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d old client signal samples", n)
			}

			// Speed samples (~4 rows/day) and traceroute runs are sparse series
			// and the UI offers a 30d trend, so keep them at least 90 days
			// regardless of the general retention setting.
			speedDays := m.cfg.RetentionDays
			if speedDays < 90 {
				speedDays = 90
			}
			speedCutoff := time.Now().AddDate(0, 0, -speedDays)
			if n, err := queries.DeleteOldSpeedSamples(m.db, speedCutoff); err != nil {
				log.Printf("poller retention: speed samples: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d old speed samples", n)
			}

			if n, err := queries.DeleteOldTracerouteRuns(m.db, speedCutoff); err != nil {
				log.Printf("poller retention: traceroute runs: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d old traceroute runs", n)
			}

			if n, err := queries.DeleteStaleMACLookups(m.db, wifiCutoff); err != nil {
				log.Printf("poller retention: mac lookup: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d stale mac-lookup entries", n)
			}

			// Backstop the request-reset handler's lazy cleanup: sweep reset
			// tokens whose expiry passed over 24h ago.
			resetCutoff := time.Now().Add(-24 * time.Hour)
			if n, err := queries.DeleteExpiredResetTokens(m.db, resetCutoff); err != nil {
				log.Printf("poller retention: reset tokens: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d expired reset tokens", n)
			}

			sweepPortStats(m.db, time.Now())

			flow.Sweep(m.db, time.Now())

			m.reclaimSpace()
		}
	}
}

// neighborRetention is how long a neighbor row a device no longer reports is
// kept (the device page lists them; links stop using them after three of the
// device's neighbor reads).
const neighborRetention = 24 * time.Hour

// sweepNeighbors deletes, per device, the neighbor rows it has not reported
// for neighborRetention — or three of its neighbor reads, when an admin set
// a floor longer than that. A paused device's rows are kept: nothing ages
// while nobody reads it.
func (m *Manager) sweepNeighbors(now time.Time) {
	devices, err := queries.ListDevices(m.db)
	if err != nil {
		log.Printf("poller retention: neighbors: %v", err)
		return
	}
	var total int64
	for _, dev := range devices {
		if m.cadence.Paused(dev, now) {
			continue
		}
		keep := max(neighborRetention, neighborWindowReads*m.cadence.Interval(dev, devclass.TierNeighbors, now))
		n, err := queries.DeleteStaleNeighbors(m.db, dev.ID, now.Add(-keep))
		if err != nil {
			log.Printf("poller retention: neighbors of %s: %v", nonempty(dev.Identity, dev.Address), err)
			continue
		}
		total += n
	}
	if total > 0 {
		log.Printf("poller retention: deleted %d stale neighbors", total)
	}
}

// sweepPortStats applies the traffic-analytics retention settings, re-read on
// every sweep: port_stats_1m_days (default 2, clamp 1..90 — the UI reads at
// most 24 h of 1-minute data, and the collector's hourly re-roll relies on at
// least one day of it), port_stats_1h_days (default 365, clamp 7..1825) and
// port_hosts_stale_days (default 7, clamp 1..365; a paused device's rows are
// kept). Deletes run in bounded chunks, so lowering a retention never locks
// the database for long.
func sweepPortStats(db *sql.DB, now time.Time) {
	days1m := settingIntClamped(db, "port_stats_1m_days", 2, 1, 90)
	if n, err := queries.DeleteOldPortStats(db, queries.PortStats1m, now.AddDate(0, 0, -days1m)); err != nil {
		log.Printf("poller retention: port stats 1m: %v", err)
	} else if n > 0 {
		log.Printf("poller retention: deleted %d old 1-minute port stats", n)
	}

	days1h := settingIntClamped(db, "port_stats_1h_days", 365, 7, 1825)
	if n, err := queries.DeleteOldPortStats(db, queries.PortStats1h, now.AddDate(0, 0, -days1h)); err != nil {
		log.Printf("poller retention: port stats 1h: %v", err)
	} else if n > 0 {
		log.Printf("poller retention: deleted %d old hourly port stats", n)
	}

	staleDays := settingIntClamped(db, "port_hosts_stale_days", 7, 1, 365)
	if n, err := queries.DeleteStalePortHosts(db, now.AddDate(0, 0, -staleDays), now); err != nil {
		log.Printf("poller retention: port hosts: %v", err)
	} else if n > 0 {
		log.Printf("poller retention: deleted %d stale port hosts", n)
	}
}

// reclaimSpace bounds the WAL on every sweep and compacts the database file
// roughly once a day. DELETEs alone never return freed pages to the OS, so the
// file would otherwise only ever grow.
func (m *Manager) reclaimSpace() {
	if _, err := m.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("poller retention: wal checkpoint: %v", err)
	}
	now := time.Now()
	if now.Sub(m.lastVacuum) < 24*time.Hour {
		return
	}
	if _, err := m.db.Exec(`VACUUM`); err != nil {
		log.Printf("poller retention: vacuum: %v", err)
		return
	}
	m.lastVacuum = now
	log.Println("poller retention: vacuumed database")
}
