package poller

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/topology"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// LiveTrafficTopic is the WS topic carrying per-link throughput for the map.
const LiveTrafficTopic = "topology.traffic"

// SourceSnapshot is the source every snapshot-backed live view reports: the
// rates are the port-stats collector's counter deltas, read without a device
// call.
const SourceSnapshot = "snapshot"

// uplinkFreshness is how old a device_uplinks row may be and still count;
// the same window the map's topology builder draws egress edges from.
const uplinkFreshness = 10 * time.Minute

// SnapshotMeta tells a live view how fresh snapshot-derived rates are, so it
// can say "updated 12 s ago" instead of implying a live reading.
type SnapshotMeta struct {
	Source string `json:"source"` // always SourceSnapshot
	// SampledAt is the end of the counter cycle the rates come from; nil
	// before the first cycle (or, for one device, while the snapshot has none
	// of its interfaces).
	SampledAt *time.Time `json:"sampled_at"`
	// AgeSeconds is how old SampledAt was when the response was built. A
	// client adds the time since it received the response rather than
	// comparing SampledAt with its own, possibly skewed, clock.
	AgeSeconds *int `json:"age_seconds"`
	// IntervalSeconds is how often the counters are read.
	IntervalSeconds int `json:"interval_seconds"`
	// Ready is false until the collector has two readings to diff.
	Ready bool `json:"ready"`
}

// NewSnapshotMeta describes snap as seen at now.
func NewSnapshotMeta(snap PortSnapshot, now time.Time) SnapshotMeta {
	m := SnapshotMeta{Source: SourceSnapshot, IntervalSeconds: snap.IntervalSeconds, Ready: snap.Ready}
	if snap.TS != nil {
		ts := *snap.TS
		age := max(int(now.Sub(ts)/time.Second), 0)
		m.SampledAt, m.AgeSeconds = &ts, &age
	}
	return m
}

// LinkTraffic is one link's throughput, keyed by the topology edge id
// (links.id, or the synthetic egress edge ids the /topology graph emits) so
// the frontend can merge it straight onto the corresponding edge. Rates are in
// Source's frame: rx enters Source over the link, tx leaves it.
type LinkTraffic struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	RxBps  int64  `json:"rx_bps"`
	TxBps  int64  `json:"tx_bps"`
	// DeviceID and Iface name the end the rate was read from.
	DeviceID string `json:"device_id"`
	Iface    string `json:"iface"`
}

// LinkTrafficPayload is what GET /traffic/links serves and topology.traffic
// publishes.
type LinkTrafficPayload struct {
	SnapshotMeta
	Links []LinkTraffic `json:"links"`
}

// LinkTrafficInput is everything BuildLinkTraffic reads.
type LinkTrafficInput struct {
	Links    []queries.Link
	Devices  []queries.Device
	Uplinks  []queries.DeviceUplink
	Snapshot PortSnapshot
	// SampledAt is when each device's counters were last read. A device
	// missing from it counts as read at Snapshot.TS — which is every device
	// today, since the snapshot carries one cycle time; per-device read
	// times (per-class counter cadences) slot in here.
	SampledAt map[string]time.Time
}

// LoadLinkTraffic derives every measurable link's rate from snap and the
// links, devices and uplinks tables. It never calls a device.
func LoadLinkTraffic(db *sql.DB, snap PortSnapshot, now time.Time) (LinkTrafficPayload, error) {
	out := LinkTrafficPayload{SnapshotMeta: NewSnapshotMeta(snap, now), Links: []LinkTraffic{}}
	if len(snap.Ports) == 0 {
		return out, nil
	}
	links, err := queries.ListLinks(db)
	if err != nil {
		return out, err
	}
	devices, err := queries.ListDevices(db)
	if err != nil {
		return out, err
	}
	ups, err := queries.ListDeviceUplinks(db, uplinkFreshness)
	if err != nil {
		return out, err
	}
	out.Links = BuildLinkTraffic(LinkTrafficInput{Links: links, Devices: devices, Uplinks: ups, Snapshot: snap})
	return out, nil
}

// linkEnd is one end of a link that the snapshot can measure.
type linkEnd struct {
	device  string
	iface   string
	rate    PortRate
	at      time.Time     // when the device's counters were read
	online  bool          // liveness status, a tie-break
	cadence time.Duration // the device's counter cadence, a tie-break
}

// preferredTo reports whether a is the better end to read a link from: the
// fresher reading first; on a tie the online device, then the device on the
// shorter counter cadence (with per-class cadences a C3 switch's counters
// are up to four times fresher than a C1's, and reading them spares nothing
// on the weak one), else false so the caller keeps its first choice.
func (a linkEnd) preferredTo(b linkEnd) bool {
	if !a.at.Equal(b.at) {
		return a.at.After(b.at)
	}
	if a.online != b.online {
		return a.online
	}
	return a.cadence < b.cadence
}

// linkPort reduces a links/uplinks interface value to the local port:
// MNDP reports "ether1,bridge" for a bridge port; "" and "unknown" mean the
// end has no port.
func linkPort(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "unknown") {
		return ""
	}
	return s
}

// BuildLinkTraffic maps snapshot rates onto the map's edges: every up link
// (read from whichever end is preferred, see linkEnd.preferredTo), every
// egress edge (internet, gateway, VPN) and every FDB-anchored gateway edge,
// with the edge ids and skip rules of topology.Builder.appendUplinks. An end
// is measurable when the snapshot has its port and the port is not a bridge:
// a bridge counts the device's whole bridged traffic, not that link's.
func BuildLinkTraffic(in LinkTrafficInput) []LinkTraffic {
	rates := make(map[portStatsKey]PortRate, len(in.Snapshot.Ports))
	for _, p := range in.Snapshot.Ports {
		rates[portStatsKey{p.DeviceID, p.Iface}] = p
	}
	devices := make(map[string]queries.Device, len(in.Devices))
	managedIP := make(map[string]bool, len(in.Devices))
	for _, d := range in.Devices {
		devices[d.ID] = d
		if d.Address != "" {
			managedIP[d.Address] = true
		}
	}
	var ts time.Time
	if in.Snapshot.TS != nil {
		ts = *in.Snapshot.TS
	}

	end := func(deviceID, iface string) (linkEnd, bool) {
		port := linkPort(iface)
		if port == "" {
			return linkEnd{}, false
		}
		r, ok := rates[portStatsKey{deviceID, port}]
		if !ok || strings.EqualFold(r.Type, "bridge") {
			return linkEnd{}, false
		}
		e := linkEnd{device: deviceID, iface: port, rate: r, at: ts}
		if at, ok := in.SampledAt[deviceID]; ok {
			e.at = at
		}
		if d, ok := devices[deviceID]; ok {
			e.online = d.Status == "online"
			e.cadence = devclass.EffectiveInterval(d.EffectiveClass(), d.Profile(), devclass.TierCounters)
		}
		return e, true
	}
	// measured is the link's rate read from e, in source's frame.
	measured := func(id, source, target string, e linkEnd) LinkTraffic {
		lt := LinkTraffic{ID: id, Source: source, Target: target, DeviceID: e.device, Iface: e.iface,
			RxBps: e.rate.RxBps, TxBps: e.rate.TxBps}
		if e.device != source {
			// Read from the far end: what it sends is what enters source.
			lt.RxBps, lt.TxBps = e.rate.TxBps, e.rate.RxBps
		}
		return lt
	}

	out := make([]LinkTraffic, 0, len(in.Links))
	for _, l := range in.Links {
		if l.Status != "up" {
			continue
		}
		a, okA := end(l.DeviceAID, l.InterfaceA)
		b, okB := end(l.DeviceBID, l.InterfaceB)
		switch {
		case okA && (!okB || !b.preferredTo(a)):
			out = append(out, measured(l.ID, l.DeviceAID, l.DeviceBID, a))
		case okB:
			out = append(out, measured(l.ID, l.DeviceAID, l.DeviceBID, b))
		}
	}

	// Egress edges, mirroring appendUplinks: a default route through a
	// managed next-hop is already an L2 link; a tunnel carrying the default
	// route is drawn (and measured) once, as that route's edge; a private,
	// unmanaged next-hop becomes a gateway node whose FDB-discovered
	// attachment port is a measurable edge of its own.
	routeEgress := make(map[string]bool)
	gatewayNodes := make(map[string]bool)
	for _, u := range in.Uplinks {
		if u.Kind != "default-route" {
			continue
		}
		if u.Interface != "" {
			routeEgress[u.DeviceID+":"+u.Interface] = true
		}
		if gw := u.GatewayIP; gw != "" && !managedIP[gw] && topology.IsPrivateIP(gw) {
			gatewayNodes[gw] = true
		}
	}
	for _, u := range in.Uplinks {
		var id, target string
		gw := u.GatewayIP
		switch u.Kind {
		case "default-route":
			if u.Interface == "" || (gw != "" && managedIP[gw]) {
				continue
			}
			id = topology.UplinkEdgeID(u.DeviceID, u.Interface, gw)
			switch {
			case gw == "" && topology.IsVPNIfaceType(u.IfaceType):
				target = "vpn:" + u.DeviceID + ":" + u.Interface
			case gatewayNodes[gw]:
				target = "gw:" + gw
			default:
				target = "internet" // public next-hop, or an interface-only route
			}
		case "vpn":
			if u.Interface == "" || routeEgress[u.DeviceID+":"+u.Interface] {
				continue
			}
			id = topology.UplinkEdgeID(u.DeviceID, u.Interface, "")
			target = "vpn:" + u.DeviceID + ":" + u.Interface
		case "gateway-host":
			if !gatewayNodes[gw] {
				continue
			}
			id, target = "gwhost:"+gw, "gw:"+gw
		default:
			continue
		}
		if e, ok := end(u.DeviceID, u.Interface); ok {
			out = append(out, measured(id, u.DeviceID, target, e))
		}
	}
	return out
}

// LiveTrafficCollector publishes per-link throughput for the network map on
// topology.traffic. Every rate comes from the port-stats snapshot, which it
// receives by listening to the collector's traffic.ports publication
// in-process — so an open map costs the devices nothing. (It used to run one
// monitor-traffic per link interface every 5 s: 100–270 commands a minute on
// a 1-core switch.) It is subscriber-gated: the listener is attached only
// while someone watches the map, and a topology.traffic message goes out once
// per counter cycle.
type LiveTrafficCollector struct {
	db    *sql.DB
	hub   *ws.Hub
	now   func() time.Time
	snaps chan PortSnapshot // newest snapshot not yet published (capacity 1)

	// watchEvery is how often Run checks for map viewers.
	watchEvery time.Duration
}

// NewLiveTrafficCollector wires the collector. The pool is not used — link
// rates come from the snapshot — and the parameter only keeps the
// constructor's signature.
func NewLiveTrafficCollector(db *sql.DB, _ *routeros.Pool, hub *ws.Hub) *LiveTrafficCollector {
	return &LiveTrafficCollector{
		db:         db,
		hub:        hub,
		now:        time.Now,
		snaps:      make(chan PortSnapshot, 1),
		watchEvery: 2 * time.Second,
	}
}

// Run attaches the snapshot listener while the map has viewers and publishes
// each snapshot's link rates; it returns when ctx is done.
func (c *LiveTrafficCollector) Run(ctx context.Context) {
	ticker := time.NewTicker(c.watchEvery)
	defer ticker.Stop()
	var stop func()
	defer func() {
		if stop != nil {
			stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			watching := c.hub.TopicSubscriberCount(LiveTrafficTopic) > 0
			switch {
			case watching && stop == nil:
				stop = c.hub.Listen(PortRatesTopic, c.offer)
			case !watching && stop != nil:
				stop()
				stop = nil
			}
		case snap := <-c.snaps:
			if c.hub.TopicSubscriberCount(LiveTrafficTopic) == 0 {
				continue
			}
			payload, err := LoadLinkTraffic(c.db, snap, c.now())
			if err != nil {
				log.Printf("poller livetraffic: %v", err)
				continue
			}
			c.hub.Publish(LiveTrafficTopic, payload)
		}
	}
}

// offer hands a published snapshot to Run, replacing one Run has not picked
// up yet: only the newest matters. It runs on the port-stats collector's
// goroutine, so it never blocks and never touches the database.
func (c *LiveTrafficCollector) offer(v any) {
	snap, ok := v.(PortSnapshot)
	if !ok || !snap.Ready || snap.TS == nil {
		return
	}
	for {
		select {
		case c.snaps <- snap:
			return
		default:
			select {
			case <-c.snaps: // drop the stale one Run has not taken
			default:
			}
		}
	}
}
