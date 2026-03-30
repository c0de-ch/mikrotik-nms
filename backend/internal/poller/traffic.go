package poller

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// TrafficManager handles on-demand traffic monitoring streams.
// It starts monitoring when WebSocket clients subscribe to traffic topics
// and stops when no subscribers remain.
type TrafficManager struct {
	db   *sql.DB
	pool *routeros.Pool
	hub  *ws.Hub

	mu      sync.Mutex
	streams map[string]context.CancelFunc // key: "deviceID:interface"
}

func NewTrafficManager(db *sql.DB, pool *routeros.Pool, hub *ws.Hub) *TrafficManager {
	return &TrafficManager{
		db:      db,
		pool:    pool,
		hub:     hub,
		streams: make(map[string]context.CancelFunc),
	}
}

// CheckSubscriptions polls the hub for traffic topic subscribers and starts/stops streams accordingly.
func (tm *TrafficManager) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			tm.stopAll()
			return
		case <-ticker.C:
			tm.reconcileStreams(ctx)
		}
	}
}

func (tm *TrafficManager) reconcileStreams(ctx context.Context) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Get all devices to check for traffic subscriptions
	devices, err := queries.ListDevices(tm.db)
	if err != nil {
		return
	}

	activeTopics := make(map[string]bool)

	for _, dev := range devices {
		if dev.Status != "online" {
			continue
		}

		ifaces, err := queries.ListInterfacesByDevice(tm.db, dev.ID)
		if err != nil {
			continue
		}

		for _, iface := range ifaces {
			topic := fmt.Sprintf("traffic.%s.%s", dev.ID, iface.Name)
			streamKey := fmt.Sprintf("%s:%s", dev.ID, iface.Name)
			count := tm.hub.TopicSubscriberCount(topic)

			if count > 0 {
				activeTopics[streamKey] = true
				// Start stream if not running
				if _, exists := tm.streams[streamKey]; !exists {
					streamCtx, cancel := context.WithCancel(ctx)
					tm.streams[streamKey] = cancel
					go tm.runStream(streamCtx, dev, iface.Name, topic)
				}
			}
		}
	}

	// Stop streams with no subscribers
	for key, cancel := range tm.streams {
		if !activeTopics[key] {
			cancel()
			delete(tm.streams, key)
		}
	}
}

func (tm *TrafficManager) runStream(ctx context.Context, dev queries.Device, ifaceName, topic string) {
	log.Printf("traffic: starting stream for %s/%s", dev.Identity, ifaceName)
	defer log.Printf("traffic: stopped stream for %s/%s", dev.Identity, ifaceName)

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			client := tm.pool.Get(dev.ID)
			if client == nil {
				continue
			}

			data, err := routeros.GetTrafficSnapshot(client, ifaceName)
			if err != nil {
				log.Printf("traffic: snapshot %s/%s: %v", dev.Identity, ifaceName, err)
				continue
			}

			// Store sample
			_ = queries.InsertTrafficSample(tm.db, &queries.TrafficSample{
				DeviceID:        dev.ID,
				InterfaceName:   ifaceName,
				RxBitsPerSec:    data.RxBitsPerSec,
				TxBitsPerSec:    data.TxBitsPerSec,
				RxPacketsPerSec: data.RxPacketsPerSec,
				TxPacketsPerSec: data.TxPacketsPerSec,
			})

			// Broadcast to WebSocket subscribers
			tm.hub.Publish(topic, map[string]interface{}{
				"device_id":      dev.ID,
				"interface_name": ifaceName,
				"rx_bps":         data.RxBitsPerSec,
				"tx_bps":         data.TxBitsPerSec,
				"rx_pps":         data.RxPacketsPerSec,
				"tx_pps":         data.TxPacketsPerSec,
				"timestamp":      time.Now().UTC().Format(time.RFC3339),
			})
		}
	}
}

func (tm *TrafficManager) stopAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for key, cancel := range tm.streams {
		cancel()
		delete(tm.streams, key)
	}
}
