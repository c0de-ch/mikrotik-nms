package poller

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// clientState tracks the last known state of a wifi client.
type clientState struct {
	AP          string
	SSID        string
	Signal      string
	MissedPolls int // how many consecutive polls the client was absent
}

// WifiTracker periodically polls CAPsMAN/WiFi registrations and records history.
//
// Sources of truth, in priority order:
//  1. RouterOS wireless log entries (authoritative for join/leave/roam) —
//     parsed via routeros.GetWirelessLogEvents.
//  2. Registration table snapshots — used to enrich state with signal/rate
//     and to catch clients that the log missed.
//  3. Absence from the registration table for >= missedPollThreshold cycles —
//     a safety net only, in case logging is disabled on the controller.
type WifiTracker struct {
	db       *sql.DB
	pool     *routeros.Pool
	hub      *ws.Hub
	interval time.Duration
	clients  map[string]*clientState // mac -> state
	// seenLogs tracks log fingerprints we have already processed, per device.
	// Pruned periodically to bound memory.
	seenLogs        map[string]map[string]time.Time // devID -> fingerprint -> firstSeen
	lastLogPruneAt  time.Time
}

// Number of consecutive missed polls before declaring a client as "left" via
// the safety-net path. With log-based detection as the primary source, we
// raise this so absence-based leaves only fire when logging is broken.
// At 30s intervals, 10 misses = 5 minute grace period.
const missedPollThreshold = 10

// How long to keep log fingerprints in memory before pruning. Must be larger
// than the poll interval so a fingerprint stays cached across consecutive
// cycles.
const logFingerprintTTL = 30 * time.Minute

func NewWifiTracker(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, interval time.Duration) *WifiTracker {
	return &WifiTracker{
		db:       db,
		pool:     pool,
		hub:      hub,
		interval: interval,
		clients:  make(map[string]*clientState),
		seenLogs: make(map[string]map[string]time.Time),
	}
}

func (wt *WifiTracker) Run(ctx context.Context) {
	ticker := time.NewTicker(wt.interval)
	defer ticker.Stop()

	wt.restoreState()

	time.Sleep(15 * time.Second)
	wt.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			wt.safePoll(ctx)
		}
	}
}

func (wt *WifiTracker) safePoll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("wifi tracker: panic: %v", r)
		}
	}()
	wt.poll(ctx)
}

func (wt *WifiTracker) poll(ctx context.Context) {
	devices, err := queries.ListDevices(wt.db)
	if err != nil {
		return
	}

	macLookups, _ := queries.GetAllMACLookups(wt.db)
	now := time.Now()

	// Phase 1: drain wireless logs from each controller. This is the
	// authoritative source for join / leave / roam events.
	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if dev.Status != "online" {
			continue
		}
		client := wt.pool.Get(dev.ID)
		if client == nil {
			continue
		}
		func() {
			defer func() { recover() }()
			events, err := routeros.GetWirelessLogEvents(client)
			if err != nil {
				return
			}
			wt.processDeviceLogs(dev.ID, events, macLookups, now)
		}()
	}

	// Phase 2: snapshot registration tables (for signal/rate enrichment and
	// the absence safety net).
	currentClients := make(map[string]*wifiSnapshot)
	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if dev.Status != "online" {
			continue
		}
		client := wt.pool.Get(dev.ID)
		if client == nil {
			continue
		}
		func() {
			defer func() { recover() }()
			regs, err := routeros.GetCAPsMANRegistrations(client)
			if err != nil {
				return
			}
			for _, reg := range regs {
				mac := strings.ToUpper(reg.MAC)
				if mac == "" || mac == "00:00:00:00:00:00" {
					continue
				}
				// Skip non-wireless entries.
				if reg.SSID == "" && reg.Band == "" && reg.Signal == "" {
					iface := strings.ToLower(reg.Interface)
					if strings.Contains(iface, "ether") || strings.Contains(iface, "bridge") ||
						strings.Contains(iface, "vlan") || strings.Contains(iface, "pppoe") ||
						strings.Contains(iface, "l2tp") || strings.Contains(iface, "ovpn") {
						continue
					}
				}
				apName := reg.AP
				if apName == "" && reg.Interface != "" {
					apName = reg.Interface
					if idx := strings.Index(apName, "/"); idx > 0 {
						apName = apName[:idx]
					}
				}
				currentClients[mac] = &wifiSnapshot{
					mac:     mac,
					ap:      apName,
					ssid:    reg.SSID,
					band:    reg.Band,
					channel: reg.Channel,
					signal:  reg.Signal,
					txRate:  reg.TxRate,
					rxRate:  reg.RxRate,
					devID:   dev.ID,
				}
			}
		}()
	}

	// Phase 3: enrich state from snapshot. If a client appears in the
	// registration table that the log never told us about, emit a join from
	// the snapshot — covers controllers where wireless logging is disabled.
	seenMACs := make(map[string]bool)
	for mac, snap := range currentClients {
		seenMACs[mac] = true
		prev, exists := wt.clients[mac]
		if !exists {
			wt.emitFromSnapshot(snap, "join", "", macLookups, now)
			wt.clients[mac] = &clientState{
				AP: snap.ap, SSID: snap.ssid, Signal: snap.signal,
			}
			continue
		}
		// Already known — refresh enrichment fields and reset miss counter.
		prev.Signal = snap.signal
		prev.SSID = snap.ssid
		prev.MissedPolls = 0
	}

	// Phase 4: absence safety net. Only fires when a client has been missing
	// from registration tables for missedPollThreshold cycles AND the log
	// never recorded a leave for it (which would have removed it from
	// wt.clients already).
	for mac, state := range wt.clients {
		if seenMACs[mac] {
			continue
		}
		state.MissedPolls++
		if state.MissedPolls >= missedPollThreshold {
			wt.emitLeave(mac, state.AP, state.SSID, state.Signal, "", macLookups, now)
			delete(wt.clients, mac)
		}
	}

	// Phase 5: prune log fingerprint cache.
	if now.Sub(wt.lastLogPruneAt) > 5*time.Minute {
		wt.pruneLogFingerprints(now)
		wt.lastLogPruneAt = now
	}
}

// processDeviceLogs applies a batch of wireless log events from one
// controller, deduping against fingerprints already seen in this process.
//
// On the very first poll for a device, all current log entries are marked as
// seen WITHOUT emitting events — otherwise a backend restart would replay
// the entire in-memory log buffer into wifi_history.
func (wt *WifiTracker) processDeviceLogs(devID string, events []routeros.WirelessLogEvent, macLookups map[string]*queries.MACLookup, now time.Time) {
	seen, exists := wt.seenLogs[devID]
	firstRun := !exists
	if !exists {
		seen = make(map[string]time.Time)
		wt.seenLogs[devID] = seen
	}

	for i := range events {
		ev := &events[i]
		fp := ev.Fingerprint()
		if _, already := seen[fp]; already {
			seen[fp] = now // refresh so it stays alive while in source buffer
			continue
		}
		seen[fp] = now
		if firstRun {
			continue
		}
		wt.handleLogEvent(ev, devID, macLookups, now)
	}
}

// handleLogEvent applies one parsed wireless log event to the in-memory
// state, the wifi_history table, and the websocket hub.
func (wt *WifiTracker) handleLogEvent(ev *routeros.WirelessLogEvent, devID string, macLookups map[string]*queries.MACLookup, now time.Time) {
	hostname, ip := lookupHostnameIP(ev.MAC, macLookups)

	switch ev.Event {
	case "connected", "reconnecting":
		prev := wt.clients[ev.MAC]
		prevAP := ""
		event := "join"
		if prev != nil {
			prevAP = prev.AP
			if prev.AP != "" && prev.AP != ev.AP {
				event = "roam"
			}
		}
		wt.clients[ev.MAC] = &clientState{
			AP: ev.AP, SSID: ev.SSID, Signal: ev.Signal,
		}
		_ = queries.InsertWifiHistory(wt.db, &queries.WifiHistoryEntry{
			MACAddress: ev.MAC, IPAddress: ip, HostName: hostname,
			APName: ev.AP, SSID: ev.SSID, Signal: ev.Signal,
			Event: event, ControllerID: devID,
		})
		wt.hub.Publish("wifi.event", map[string]interface{}{
			"mac":     ev.MAC,
			"ap":      ev.AP,
			"event":   event,
			"prev_ap": prevAP,
			"signal":  ev.Signal,
			"time":    now.Format(time.RFC3339),
		})

	case "disconnected":
		ap := ev.AP
		ssid := ev.SSID
		if prev := wt.clients[ev.MAC]; prev != nil {
			if ap == "" {
				ap = prev.AP
			}
			if ssid == "" {
				ssid = prev.SSID
			}
		}
		wt.emitLeave(ev.MAC, ap, ssid, ev.Signal, ev.Reason, macLookups, now)
		delete(wt.clients, ev.MAC)

	case "roamed":
		prev := wt.clients[ev.MAC]
		prevAP := ""
		if prev != nil {
			prevAP = prev.AP
		}
		wt.clients[ev.MAC] = &clientState{
			AP: ev.ToAP, SSID: ev.ToSSID, Signal: ev.Signal,
		}
		_ = queries.InsertWifiHistory(wt.db, &queries.WifiHistoryEntry{
			MACAddress: ev.MAC, IPAddress: ip, HostName: hostname,
			APName: ev.ToAP, SSID: ev.ToSSID, Signal: ev.Signal,
			Event: "roam", ControllerID: devID,
		})
		wt.hub.Publish("wifi.event", map[string]interface{}{
			"mac":     ev.MAC,
			"ap":      ev.ToAP,
			"event":   "roam",
			"prev_ap": prevAP,
			"signal":  ev.Signal,
			"time":    now.Format(time.RFC3339),
		})
	}
}

func (wt *WifiTracker) emitFromSnapshot(snap *wifiSnapshot, event, prevAP string, macLookups map[string]*queries.MACLookup, now time.Time) {
	hostname, ip := lookupHostnameIP(snap.mac, macLookups)
	_ = queries.InsertWifiHistory(wt.db, &queries.WifiHistoryEntry{
		MACAddress:   snap.mac,
		IPAddress:    ip,
		HostName:     hostname,
		APName:       snap.ap,
		SSID:         snap.ssid,
		Band:         snap.band,
		Channel:      snap.channel,
		Signal:       snap.signal,
		TxRate:       snap.txRate,
		RxRate:       snap.rxRate,
		Event:        event,
		ControllerID: snap.devID,
	})
	wt.hub.Publish("wifi.event", map[string]interface{}{
		"mac":     snap.mac,
		"ap":      snap.ap,
		"event":   event,
		"prev_ap": prevAP,
		"signal":  snap.signal,
		"time":    now.Format(time.RFC3339),
	})
}

func (wt *WifiTracker) emitLeave(mac, ap, ssid, signal, reason string, macLookups map[string]*queries.MACLookup, now time.Time) {
	hostname, ip := lookupHostnameIP(mac, macLookups)
	_ = queries.InsertWifiHistory(wt.db, &queries.WifiHistoryEntry{
		MACAddress: mac, IPAddress: ip, HostName: hostname,
		APName: ap, SSID: ssid, Signal: signal,
		Event: "leave",
	})
	payload := map[string]interface{}{
		"mac":    mac,
		"ap":     ap,
		"event":  "leave",
		"signal": signal,
		"time":   now.Format(time.RFC3339),
	}
	if reason != "" {
		payload["reason"] = reason
	}
	wt.hub.Publish("wifi.event", payload)
}

func (wt *WifiTracker) pruneLogFingerprints(now time.Time) {
	cutoff := now.Add(-logFingerprintTTL)
	for devID, seen := range wt.seenLogs {
		for fp, t := range seen {
			if t.Before(cutoff) {
				delete(seen, fp)
			}
		}
		if len(seen) == 0 {
			delete(wt.seenLogs, devID)
		}
	}
}

func lookupHostnameIP(mac string, macLookups map[string]*queries.MACLookup) (hostname, ip string) {
	if lookup, ok := macLookups[mac]; ok {
		hostname = lookup.HostName
		if hostname == "" {
			hostname = lookup.DNSName
		}
		ip = lookup.IPAddress
	}
	return
}

func (wt *WifiTracker) restoreState() {
	clients, err := queries.GetWifiClientsCurrentAP(wt.db)
	if err != nil {
		log.Printf("wifi tracker: failed to restore state: %v", err)
		return
	}
	for _, c := range clients {
		wt.clients[c.MACAddress] = &clientState{
			AP: c.APName,
		}
	}
	if len(clients) > 0 {
		log.Printf("wifi tracker: restored %d client positions from DB", len(clients))
	}
}

type wifiSnapshot struct {
	mac, ap, ssid, band, channel, signal, txRate, rxRate, devID string
}
