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
	AP       string
	Uptime   string
	MissedPolls int // how many consecutive polls the client was absent
}

// WifiTracker periodically polls CAPsMAN/WiFi registrations and records history.
// Uses uptime field to distinguish real join/leave from poll artifacts.
type WifiTracker struct {
	db       *sql.DB
	pool     *routeros.Pool
	hub      *ws.Hub
	interval time.Duration
	clients  map[string]*clientState // mac -> state
}

// Number of consecutive missed polls before declaring a client as "left".
// At 30s intervals, 3 misses = 90s grace period.
const missedPollThreshold = 3

func NewWifiTracker(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, interval time.Duration) *WifiTracker {
	return &WifiTracker{
		db:       db,
		pool:     pool,
		hub:      hub,
		interval: interval,
		clients:  make(map[string]*clientState),
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

				// Skip non-wireless entries
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
					uptime:  reg.Uptime,
					devID:   dev.ID,
				}
			}
		}()
	}

	macLookups, _ := queries.GetAllMACLookups(wt.db)
	now := time.Now()
	seenMACs := make(map[string]bool)

	for mac, snap := range currentClients {
		seenMACs[mac] = true

		hostname := ""
		ip := ""
		if lookup, ok := macLookups[mac]; ok {
			hostname = lookup.HostName
			if hostname == "" {
				hostname = lookup.DNSName
			}
			ip = lookup.IPAddress
		}

		prev, wasSeen := wt.clients[mac]

		var event string
		if !wasSeen {
			// Truly new client — never seen before (or after restart restore)
			event = "join"
		} else if prev.AP != snap.ap {
			// AP changed — real roam
			event = "roam"
		} else if prev.Uptime != "" && snap.uptime != "" && uptimeReset(prev.Uptime, snap.uptime) {
			// Same AP but uptime went backwards — client reconnected
			event = "join"
		} else {
			// Same AP, uptime incrementing — just a periodic "seen", don't record
			// Update state and skip DB write to reduce noise
			prev.Uptime = snap.uptime
			prev.MissedPolls = 0
			continue
		}

		// Update state
		wt.clients[mac] = &clientState{
			AP:     snap.ap,
			Uptime: snap.uptime,
		}

		entry := &queries.WifiHistoryEntry{
			MACAddress:   mac,
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
		}
		_ = queries.InsertWifiHistory(wt.db, entry)

		if event == "roam" || event == "join" {
			prevAP := ""
			if prev != nil {
				prevAP = prev.AP
			}
			wt.hub.Publish("wifi.event", map[string]interface{}{
				"mac":     mac,
				"ap":      snap.ap,
				"event":   event,
				"prev_ap": prevAP,
				"signal":  snap.signal,
				"time":    now.Format(time.RFC3339),
			})
		}
	}

	// Handle clients not seen in this poll
	for mac, state := range wt.clients {
		if seenMACs[mac] {
			continue
		}

		state.MissedPolls++

		// Only declare "leave" after multiple consecutive missed polls
		if state.MissedPolls >= missedPollThreshold {
			_ = queries.InsertWifiHistory(wt.db, &queries.WifiHistoryEntry{
				MACAddress: mac,
				APName:     state.AP,
				Event:      "leave",
			})
			wt.hub.Publish("wifi.event", map[string]interface{}{
				"mac":   mac,
				"ap":    state.AP,
				"event": "leave",
				"time":  now.Format(time.RFC3339),
			})
			delete(wt.clients, mac)
		}
		// Otherwise keep the state — client might reappear next poll
	}
}

func (wt *WifiTracker) restoreState() {
	clients, err := queries.GetWifiClientsCurrentAP(wt.db)
	if err != nil {
		log.Printf("wifi tracker: failed to restore state: %v", err)
		return
	}
	for _, c := range clients {
		wt.clients[c.MACAddress] = &clientState{
			AP:     c.APName,
			Uptime: "", // unknown after restart, will be filled on first poll
		}
	}
	if len(clients) > 0 {
		log.Printf("wifi tracker: restored %d client positions from DB", len(clients))
	}
}

// uptimeReset returns true if the new uptime is shorter than the previous,
// indicating the client disconnected and reconnected.
func uptimeReset(prev, curr string) bool {
	prevSec := parseUptimeSeconds(prev)
	currSec := parseUptimeSeconds(curr)
	if prevSec == 0 || currSec == 0 {
		return false
	}
	// If current uptime is significantly less than previous, it reset
	return currSec < prevSec-60 // allow 60s jitter
}

// parseUptimeSeconds parses RouterOS uptime strings like "2h57m3s", "5m30s", "45s"
func parseUptimeSeconds(s string) int {
	if s == "" {
		return 0
	}
	total := 0
	num := 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int(c-'0')
		case c == 'd':
			total += num * 86400
			num = 0
		case c == 'h':
			total += num * 3600
			num = 0
		case c == 'm':
			total += num * 60
			num = 0
		case c == 's':
			total += num
			num = 0
		case c == 'w':
			total += num * 604800
			num = 0
		}
	}
	return total
}

type wifiSnapshot struct {
	mac, ap, ssid, band, channel, signal, txRate, rxRate, uptime, devID string
}
