package poller

import (
	"context"
	"database/sql"
	"log"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
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
	go m.topologyLoop(ctx)
	go m.firmwareLoop(ctx)
	go m.retentionLoop(ctx)

	trafficMgr := NewTrafficManager(m.db, m.pool, m.hub)
	go trafficMgr.Run(ctx)

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
			log.Printf("poller: panic polling %s (%s): %v", dev.Identity, dev.Address, r)
			m.pool.Close(dev.ID)
		}
	}()
	m.pollDevice(dev)
}

func (m *Manager) pollDevice(dev queries.Device) {
	client, err := m.pool.EnsureConnection(
		dev.ID, dev.Address, dev.APIPort, dev.Username, dev.PasswordEnc, dev.UseTLS,
	)
	if err != nil {
		errStr := err.Error()
		_ = queries.UpdateDeviceHealth(m.db, dev.ID, "offline", nil, nil, nil, nil, &errStr)
		m.hub.Publish("device.health", map[string]interface{}{
			"device_id": dev.ID,
			"status":    "offline",
			"error":     errStr,
		})
		return
	}

	res, err := routeros.GetSystemResource(client)
	if err != nil {
		errStr := err.Error()
		_ = queries.UpdateDeviceHealth(m.db, dev.ID, "offline", nil, nil, nil, nil, &errStr)
		m.pool.Close(dev.ID)
		return
	}

	memUsed := res.MemoryTotal - res.MemoryFree
	_ = queries.UpdateDeviceHealth(m.db, dev.ID, "online",
		&res.CPULoad, &memUsed, &res.MemoryTotal, &res.Uptime, nil)
	_ = queries.UpdateDeviceInfo(m.db, dev.ID,
		res.Platform, res.Board, res.Version, "", res.Architecture)

	// Update interfaces
	ifaces, err := routeros.GetInterfaces(client)
	if err == nil {
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
	devices, err := queries.ListDevices(m.db)
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

		client := m.pool.Get(dev.ID)
		if client == nil {
			continue
		}

		var fw *routeros.FirmwareInfo
		var rb *routeros.RouterboardInfo
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("poller firmware: panic checking %s: %v", dev.Identity, r)
					m.pool.Close(dev.ID)
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

		_ = queries.UpsertFirmwareStatus(m.db, fws)

		if fw.UpdateAvailable {
			m.hub.Publish("firmware.update", map[string]interface{}{
				"device_id":     dev.ID,
				"installed":     fw.InstalledVersion,
				"latest":        fw.LatestVersion,
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
		}
	}
}
