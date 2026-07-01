package poller

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// LiveTrafficTopic is the WS topic carrying per-link throughput for the map.
const LiveTrafficTopic = "topology.traffic"

// LiveTrafficCollector samples throughput on every topology link and publishes a
// single fleet-wide snapshot on the "topology.traffic" WS topic. It is
// subscriber-gated: it only touches the devices while at least one client is
// watching the map, so it costs nothing otherwise (mirrors TrafficManager).
type LiveTrafficCollector struct {
	db   *sql.DB
	pool *routeros.Pool
	hub  *ws.Hub
}

func NewLiveTrafficCollector(db *sql.DB, pool *routeros.Pool, hub *ws.Hub) *LiveTrafficCollector {
	return &LiveTrafficCollector{db: db, pool: pool, hub: hub}
}

// LinkTraffic is one link's live throughput, keyed by the topology edge id
// (== links.id, the same id the /topology graph emits) so the frontend can
// merge it straight onto the corresponding edge.
type LinkTraffic struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	RxBps  int64  `json:"rx_bps"`
	TxBps  int64  `json:"tx_bps"`
}

func (c *LiveTrafficCollector) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.hub.TopicSubscriberCount(LiveTrafficTopic) == 0 {
				continue
			}
			c.hub.Publish(LiveTrafficTopic, map[string]interface{}{
				"links": c.Collect(ctx),
			})
		}
	}
}

// Collect samples every up-link once, from whichever endpoint is online. It
// polls each device's interfaces on that device's pooled client (serialized per
// device by the client mutex) and runs the devices concurrently.
func (c *LiveTrafficCollector) Collect(ctx context.Context) []LinkTraffic {
	links, err := queries.ListLinks(c.db)
	if err != nil {
		return []LinkTraffic{}
	}
	devices, err := queries.ListDevices(c.db)
	if err != nil {
		return []LinkTraffic{}
	}
	online := make(map[string]bool, len(devices))
	for _, d := range devices {
		online[d.ID] = d.Status == "online"
	}

	type probe struct {
		linkID, source, target, iface string
		swap                          bool // sampled from the B side → swap rx/tx to A's frame
	}
	byDevice := make(map[string][]probe)
	for _, l := range links {
		if l.Status != "up" {
			continue
		}
		switch {
		case online[l.DeviceAID] && l.InterfaceA != "" && l.InterfaceA != "unknown":
			byDevice[l.DeviceAID] = append(byDevice[l.DeviceAID], probe{l.ID, l.DeviceAID, l.DeviceBID, l.InterfaceA, false})
		case online[l.DeviceBID] && l.InterfaceB != "" && l.InterfaceB != "unknown":
			byDevice[l.DeviceBID] = append(byDevice[l.DeviceBID], probe{l.ID, l.DeviceAID, l.DeviceBID, l.InterfaceB, true})
		}
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make([]LinkTraffic, 0, len(links))
	)
	for devID, probes := range byDevice {
		wg.Add(1)
		go func(devID string, probes []probe) {
			defer wg.Done()
			client := c.pool.Get(devID)
			if client == nil {
				return
			}
			for _, p := range probes {
				if ctx.Err() != nil {
					return
				}
				data, err := routeros.GetTrafficSnapshot(client, p.iface)
				if err != nil {
					continue
				}
				rx, tx := data.RxBitsPerSec, data.TxBitsPerSec
				if p.swap {
					rx, tx = tx, rx
				}
				mu.Lock()
				out = append(out, LinkTraffic{ID: p.linkID, Source: p.source, Target: p.target, RxBps: rx, TxBps: tx})
				mu.Unlock()
			}
		}(devID, probes)
	}
	wg.Wait()
	return out
}
