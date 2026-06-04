package poller

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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
//   1. /interface/bridge/print + monitor → bridge state
//   2. /interface/bridge/port/print + monitor → port roles
//   3. /log/print filtered to bridge,info → loop / mac flap events
//
// Anomaly detection writes rows to loop_events:
//   - stp_disabled: bridge has STP off and >1 non-edge port
//   - tcn_storm:    topology-changes counter rose by >= tcn_storm_threshold in one cycle
//   - loop_detected / mac_flap / bpdu_on_edge: directly from parsed bridge log
type NetworkHealthPoller struct {
	db       *sql.DB
	pool     *routeros.Pool
	hub      *ws.Hub
	interval time.Duration

	// previous topology_changes per bridge — used to compute deltas.
	prevTC map[string]int // key = device_id|bridge_name
	// log fingerprints already processed per device.
	seenLogs       map[string]map[string]time.Time
	lastLogPruneAt time.Time
	// per-device per-interface state for port-deactivation / flap detection.
	ports *portMonitor
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

// tcnStormSeverity decides whether a bridge's per-cycle topology-change delta is a
// TCN storm and at what severity. Topology-change notifications are an STP concept,
// so only an STP-running bridge can storm — a non-STP "bridge" (incl. any interface
// mis-reported as a bridge) never fires, which is the definitive guard against the
// false positives. Returns the delta for the event message.
func tcnStormSeverity(b routeros.BridgeInfo, prev, threshold int) (severity string, delta int, fire bool) {
	if !b.STPEnabled() {
		return "", 0, false
	}
	delta = b.TopologyChanges - prev
	if delta < threshold {
		return "", delta, false
	}
	if delta >= tcnStormCriticalDelta {
		return "critical", delta, true
	}
	return "warn", delta, true
}

func NewNetworkHealthPoller(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, interval time.Duration) *NetworkHealthPoller {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	return &NetworkHealthPoller{
		db:       db,
		pool:     pool,
		hub:      hub,
		interval: interval,
		prevTC:   make(map[string]int),
		seenLogs: make(map[string]map[string]time.Time),
		ports:    newPortMonitor(),
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

	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if dev.Status != "online" {
			continue
		}
		client := n.pool.Get(dev.ID)
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

			bridges, err := routeros.GetBridges(client)
			if err != nil {
				return
			}
			// Keep only genuine bridges — some RouterOS versions report
			// non-bridge interfaces via /interface/bridge/print, which would
			// otherwise be tracked as bridges and drive false TCN storms. Use
			// cached interface types (a live fetch can fail on 7.23).
			bridges = filterRealBridges(bridges, nonBridgeInterfaceNames(n.db, dev.ID))
			ports, err := routeros.GetBridgePorts(client)
			if err != nil {
				ports = nil
			}

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
				}
				_ = queries.UpsertBridgeStatus(n.db, bs)

				key := dev.ID + "|" + b.Name
				prev, hadPrev := n.prevTC[key]
				n.prevTC[key] = b.TopologyChanges

				// stp_disabled: bridge has multiple *non-edge* ports but no STP.
				// A loop needs at least two non-edge ports to physically form, so
				// counting only non-edge ports stops access-only bridges (all
				// edge ports) and single-uplink bridges from false-firing. Loopback
				// bridges never carry external traffic, so skip them entirely.
				lowerName := strings.ToLower(b.Name)
				if !b.STPEnabled() && lowerName != "lo" && lowerName != "loopback" {
					nonEdgeCount := 0
					for _, p := range portsByBridge[b.Name] {
						if !p.Edge {
							nonEdgeCount++
						}
					}
					if nonEdgeCount > 1 {
						if n.recordEvent(dev.ID, "stp_disabled", "warn", b.Name, "", "",
							fmt.Sprintf("bridge %q runs %d non-edge ports with STP disabled (protocol=%s)",
								b.Name, nonEdgeCount, nonempty(b.ProtocolMode, "none"))) {
							evCount++
						}
					}
				}

				// tcn_storm: topology changes rising fast within one cycle. Only
				// STP-running bridges can storm (see tcnStormSeverity); the
				// threshold is runtime-tunable and escalates to critical past a
				// hard ceiling regardless of the configured warn threshold.
				if hadPrev {
					if severity, delta, fire := tcnStormSeverity(b, prev, tcnThreshold); fire {
						if n.recordEvent(dev.ID, "tcn_storm", severity, b.Name, "", "",
							fmt.Sprintf("bridge %q topology-changes rose by %d in last poll (now %d)",
								b.Name, delta, b.TopologyChanges)) {
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

			// Bridge log events: loop / mac flap / bpdu on edge.
			logEvents, err := routeros.GetBridgeLogEvents(client)
			if err == nil {
				added := n.processBridgeLogs(dev.ID, logEvents, cycleStart)
				evCount += added
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
