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

// WifiTracker periodically polls CAPsMAN/WiFi registrations and records history.
type WifiTracker struct {
	db       *sql.DB
	pool     *routeros.Pool
	hub      *ws.Hub
	interval time.Duration
	lastSeen map[string]string // mac -> ap_name (last known position)
}

func NewWifiTracker(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, interval time.Duration) *WifiTracker {
	return &WifiTracker{
		db:       db,
		pool:     pool,
		hub:      hub,
		interval: interval,
		lastSeen: make(map[string]string),
	}
}

func (wt *WifiTracker) Run(ctx context.Context) {
	ticker := time.NewTicker(wt.interval)
	defer ticker.Stop()

	// Restore last-known client positions from DB to avoid false "join" events on restart
	wt.restoreState()

	// Initial delay for device connections
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

	// Collect current wifi clients from all devices
	currentClients := make(map[string]*wifiSnapshot) // mac -> snapshot

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
				if mac == "" {
					continue
				}
				apName := reg.AP
				if apName == "" && reg.Interface != "" {
					apName = reg.Interface
					if idx := strings.Index(apName, "/"); idx > 0 {
						apName = apName[:idx]
					}
				}
				currentClients[mac] = &wifiSnapshot{
					mac:    mac,
					ap:     apName,
					ssid:   reg.SSID,
					band:   reg.Band,
					channel: reg.Channel,
					signal: reg.Signal,
					txRate: reg.TxRate,
					rxRate: reg.RxRate,
					devID:  dev.ID,
				}
			}
		}()
	}

	// Load MAC lookup for hostname resolution
	macLookups, _ := queries.GetAllMACLookups(wt.db)

	// Compare with last known state to detect roaming/join/leave
	now := time.Now()
	currentMACs := make(map[string]bool)

	for mac, snap := range currentClients {
		currentMACs[mac] = true

		// Resolve hostname from MAC lookup
		hostname := ""
		ip := ""
		if lookup, ok := macLookups[mac]; ok {
			hostname = lookup.HostName
			if hostname == "" {
				hostname = lookup.DNSName
			}
			ip = lookup.IPAddress
		}
		prevAP, wasSeen := wt.lastSeen[mac]

		var event string
		if !wasSeen {
			event = "join"
		} else if prevAP != snap.ap {
			event = "roam"
		} else {
			event = "seen"
		}

		wt.lastSeen[mac] = snap.ap

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

		// Broadcast roam/join events
		if event == "roam" || event == "join" {
			wt.hub.Publish("wifi.event", map[string]interface{}{
				"mac":    mac,
				"ap":     snap.ap,
				"event":  event,
				"prev_ap": prevAP,
				"signal": snap.signal,
				"time":   now.Format(time.RFC3339),
			})
		}
	}

	// Detect leaves
	for mac, ap := range wt.lastSeen {
		if !currentMACs[mac] {
			_ = queries.InsertWifiHistory(wt.db, &queries.WifiHistoryEntry{
				MACAddress: mac,
				APName:     ap,
				Event:      "leave",
			})
			wt.hub.Publish("wifi.event", map[string]interface{}{
				"mac":   mac,
				"ap":    ap,
				"event": "leave",
				"time":  now.Format(time.RFC3339),
			})
			delete(wt.lastSeen, mac)
		}
	}
}

// restoreState loads the last known AP for each client from the DB
// so that the first poll after restart doesn't generate false "join" events.
func (wt *WifiTracker) restoreState() {
	clients, err := queries.GetWifiClientsCurrentAP(wt.db)
	if err != nil {
		log.Printf("wifi tracker: failed to restore state: %v", err)
		return
	}
	for _, c := range clients {
		wt.lastSeen[c.MACAddress] = c.APName
	}
	if len(clients) > 0 {
		log.Printf("wifi tracker: restored %d client positions from DB", len(clients))
	}
}

type wifiSnapshot struct {
	mac, ap, ssid, band, channel, signal, txRate, rxRate, devID string
}
