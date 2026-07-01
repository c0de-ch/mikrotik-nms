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
			cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			links := c.Collect(cctx)
			cancel()
			c.hub.Publish(LiveTrafficTopic, map[string]interface{}{"links": links})
		}
	}
}

// maxIfacesPerDevice caps how many distinct interfaces we sample on a single
// device per cycle. On a flat L2 the neighbor graph is a near-mesh (every device
// sees every other via MNDP), so many links share one uplink interface; we
// therefore sample each (device, interface) ONCE and fan the result out to all
// links on it. This cap is a backstop so a pathological device can never stall
// the whole cycle.
const maxIfacesPerDevice = 48

// Collect samples each distinct (device, interface) once — from whichever
// endpoint of a link is online — and maps the reading onto every link that
// shares that interface. Devices run concurrently; interfaces on one device are
// serialized by the pooled client's mutex.
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

	// One link keyed to the interface we'll read it from.
	type linkRef struct {
		linkID, source, target string
		swap                   bool // read from the B side → swap rx/tx into A's frame
	}
	// device -> interface -> links measured on it (dedup of redundant polls).
	byDevIface := make(map[string]map[string][]linkRef)
	for _, l := range links {
		if l.Status != "up" {
			continue
		}
		var devID, iface string
		var swap bool
		switch {
		case online[l.DeviceAID] && l.InterfaceA != "" && l.InterfaceA != "unknown":
			devID, iface, swap = l.DeviceAID, l.InterfaceA, false
		case online[l.DeviceBID] && l.InterfaceB != "" && l.InterfaceB != "unknown":
			devID, iface, swap = l.DeviceBID, l.InterfaceB, true
		default:
			continue
		}
		ifaces := byDevIface[devID]
		if ifaces == nil {
			ifaces = make(map[string][]linkRef)
			byDevIface[devID] = ifaces
		}
		ifaces[iface] = append(ifaces[iface], linkRef{l.ID, l.DeviceAID, l.DeviceBID, swap})
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = make([]LinkTraffic, 0, len(links))
	)
	for devID, ifaces := range byDevIface {
		wg.Add(1)
		go func(devID string, ifaces map[string][]linkRef) {
			defer wg.Done()
			client := c.pool.Get(devID)
			if client == nil {
				return
			}
			polled := 0
			for iface, refs := range ifaces {
				if ctx.Err() != nil || polled >= maxIfacesPerDevice {
					return
				}
				polled++
				data, err := routeros.GetTrafficSnapshot(client, iface)
				if err != nil {
					continue
				}
				mu.Lock()
				for _, r := range refs {
					rx, tx := data.RxBitsPerSec, data.TxBitsPerSec
					if r.swap {
						rx, tx = tx, rx
					}
					out = append(out, LinkTraffic{ID: r.linkID, Source: r.source, Target: r.target, RxBps: rx, TxBps: tx})
				}
				mu.Unlock()
			}
		}(devID, ifaces)
	}
	wg.Wait()
	return out
}
