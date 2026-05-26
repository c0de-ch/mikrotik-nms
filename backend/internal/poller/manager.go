package poller

import (
	"context"
	"database/sql"
	"log"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/resolver"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/topology"
	"github.com/mikrotik-nms/backend/internal/ws"
)

type Manager struct {
	db     *sql.DB
	pool   *routeros.Pool
	hub    *ws.Hub
	cfg    *config.Config
	cancel context.CancelFunc
}

func NewManager(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, cfg *config.Config) *Manager {
	return &Manager{
		db:   db,
		pool: pool,
		hub:  hub,
		cfg:  cfg,
	}
}

func (m *Manager) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	go m.healthLoop(ctx)
	go m.infoLoop(ctx)
	go m.topologyLoop(ctx)
	go m.firmwareLoop(ctx)
	go m.retentionLoop(ctx)

	trafficMgr := NewTrafficManager(m.db, m.pool, m.hub)
	go trafficMgr.Run(ctx)

	wifiTracker := NewWifiTracker(m.db, m.pool, m.hub, 30*time.Second)
	go wifiTracker.Run(ctx)

	clientDisc := NewClientDiscoveryPoller(m.db, m.pool, resolver.New(m.db), 15*time.Minute)
	go clientDisc.Run(ctx)

	netHealth := NewNetworkHealthPoller(m.db, m.pool, m.hub, m.cfg.NetworkHealthInterval)
	go netHealth.Run(ctx)

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
// interval. It only answers "is the device up?" via a cheap TCP ping that
// bypasses the RouterOS API session, so it can't contend with the heavier
// pollers and doesn't flap on transient API slowness. The full stats/version/
// interface refresh is the infoLoop's job (see refreshDeviceInfo).
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
	// right away instead of waiting up to a full info interval.
	if dev.Board == "" || dev.ROSVersion == "" {
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
// version/board/platform/arch and interfaces) is refreshed when the admin
// hasn't configured info_interval. Static details rarely change, so the
// frequent liveness poll just pings while this heavier refresh runs rarely.
const defaultInfoInterval = 60 * time.Minute

// infoInterval reads the runtime-tunable info-refresh period from app_settings.
// Picked up on the next cycle without a backend restart.
func (m *Manager) infoInterval() time.Duration {
	v, err := queries.GetSetting(m.db, "info_interval")
	if err != nil {
		return defaultInfoInterval
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return defaultInfoInterval
	}
	return time.Duration(n) * time.Second
}

func (m *Manager) infoLoop(ctx context.Context) {
	// Refresh shortly after startup so cached stats are fresh on boot, then on
	// the configured (long) interval. A fresh timer each iteration lets a
	// Settings change to info_interval take effect on the next cycle.
	select {
	case <-ctx.Done():
		return
	case <-time.After(8 * time.Second):
	}
	m.refreshAllInfo(ctx)

	for {
		timer := time.NewTimer(m.infoInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			m.refreshAllInfo(ctx)
		}
	}
}

func (m *Manager) refreshAllInfo(ctx context.Context) {
	devices, err := queries.ListDevices(m.db)
	if err != nil {
		log.Printf("poller: info refresh list devices: %v", err)
		return
	}
	for _, dev := range devices {
		select {
		case <-ctx.Done():
			return
		default:
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
// system resources (cpu/memory/uptime), static details (version/board/platform/
// arch) and the interface list, caching them in the DB. Status stays owned by
// the liveness poll — on a connection failure here we leave status alone.
func (m *Manager) refreshDeviceInfo(dev queries.Device) {
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

	memUsed := res.MemoryTotal - res.MemoryFree
	_ = queries.UpdateDeviceHealth(m.db, dev.ID, "online",
		&res.CPULoad, &memUsed, &res.MemoryTotal, &res.Uptime, nil)
	_ = queries.UpdateDeviceInfo(m.db, dev.ID,
		res.Platform, res.Board, res.Version, "", res.Architecture)

	if ifaces, err := routeros.GetInterfaces(client); err == nil {
		for _, iface := range ifaces {
			mtu := iface.MTU
			_ = queries.UpsertInterface(m.db, &queries.Interface{
				ID:         dev.ID + ":" + iface.Name,
				DeviceID:   dev.ID,
				Name:       iface.Name,
				Type:       iface.Type,
				MACAddress: iface.MACAddress,
				MTU:        &mtu,
				Running:    iface.Running,
				Disabled:   iface.Disabled,
				Comment:    iface.Comment,
			})
		}
	}

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
	ticker := time.NewTicker(m.cfg.TopologyInterval)
	defer ticker.Stop()

	// Wait a bit for initial health poll to populate device info
	time.Sleep(5 * time.Second)
	m.safePollTopology(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.safePollTopology(ctx)
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

func (m *Manager) pollTopology(ctx context.Context) {
	devices, err := queries.ListDevices(m.db)
	if err != nil {
		log.Printf("poller topology: list devices: %v", err)
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

		client := m.pool.Get(dev.ID)
		if client == nil {
			continue
		}

		neighbors, err := routeros.GetNeighbors(client)
		if err != nil {
			log.Printf("poller topology: get neighbors for %s: %v", dev.Identity, err)
			continue
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

		// Stagger between devices
		time.Sleep(time.Second)
	}

	// Rebuild topology from neighbors
	builder := topology.NewBuilder(m.db)
	graph, err := builder.Build()
	if err != nil {
		log.Printf("poller topology: build graph: %v", err)
		return
	}

	m.hub.Publish("topology.update", graph)
	log.Printf("poller: topology discovery complete — %d nodes, %d edges", len(graph.Nodes), len(graph.Edges))
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

		client := pool.Get(dev.ID)
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

			neighborCutoff := time.Now().Add(-24 * time.Hour)
			if n, err := queries.DeleteStaleNeighbors(m.db, neighborCutoff); err != nil {
				log.Printf("poller retention: neighbors: %v", err)
			} else if n > 0 {
				log.Printf("poller retention: deleted %d stale neighbors", n)
			}

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
		}
	}
}
