package poller

import (
	"context"
	"database/sql"
	"fmt"
	"log"
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
//   - stp_disabled: bridge has STP off and >1 forwarding port
//   - tcn_storm:    topology-changes counter rose by >tcnStormDelta in one cycle
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

// Below this many topology changes per cycle the bridge is considered stable.
// Anything above suggests an unstable L2 — e.g. a flapping link or active loop.
const tcnStormDelta = 5

const bridgeLogFingerprintTTL = 30 * time.Minute

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
					log.Printf("network health: panic on %s: %v", dev.Identity, r)
				}
			}()

			bridges, err := routeros.GetBridges(client)
			if err != nil {
				return
			}
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

				// stp_disabled: bridge has multiple ports but no STP. Any
				// untrusted second port could form a loop.
				if !b.STPEnabled() && len(portsByBridge[b.Name]) > 1 {
					if n.recordEvent(dev.ID, "stp_disabled", "warn", b.Name, "", "",
						fmt.Sprintf("bridge %q runs %d ports with STP disabled (protocol=%s)",
							b.Name, len(portsByBridge[b.Name]), nonempty(b.ProtocolMode, "none"))) {
						evCount++
					}
				}

				// tcn_storm: topology changes rising fast within one cycle.
				if hadPrev && b.TopologyChanges-prev >= tcnStormDelta {
					if n.recordEvent(dev.ID, "tcn_storm", "warn", b.Name, "", "",
						fmt.Sprintf("bridge %q topology-changes rose by %d in last poll (now %d)",
							b.Name, b.TopologyChanges-prev, b.TopologyChanges)) {
						evCount++
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

func nonempty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
