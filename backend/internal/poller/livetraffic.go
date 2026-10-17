package poller

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/topology"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// LiveTrafficTopic is the WS topic carrying per-link throughput for the map.
const LiveTrafficTopic = "topology.traffic"

// SourceSnapshot is the source every snapshot-backed live view reports: the
// rates are the port-stats collector's counter deltas, read without a device
// call.
const SourceSnapshot = "snapshot"

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
	// SampledAt is when each device's counters were last read
	// (PortStatsCollector.SnapshotWithReadTimes); a device missing from it
	// counts as read at Snapshot.TS.
	SampledAt map[string]time.Time
}

// LoadLinkTraffic derives every measurable link's rate from snap and the
// links, devices and uplinks tables (egress rows current by cs, see
// FreshUplinks), reading each link on its fresher end by sampledAt (nil:
// every device at snap.TS). It never calls a device.
func LoadLinkTraffic(db *sql.DB, cs CadenceSource, snap PortSnapshot, sampledAt map[string]time.Time, now time.Time) (LinkTrafficPayload, error) {
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
	ups, err := FreshUplinks(db, cs, now)
	if err != nil {
		return out, err
	}
	out.Links = BuildLinkTraffic(LinkTrafficInput{Links: links, Devices: devices, Uplinks: ups, Snapshot: snap, SampledAt: sampledAt})
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
// reading fresher by more than slack first (readings of one counter cycle
// arrive seconds apart and count as equally fresh); on a tie the online
// device, then the device on the shorter counter cadence (with per-class
// cadences a C3 switch's counters are up to four times fresher than a C1's),
// else false so the caller keeps its first choice.
func (a linkEnd) preferredTo(b linkEnd, slack time.Duration) bool {
	if d := a.at.Sub(b.at); d > slack || d < -slack {
		return d > 0
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
	// Half a counter interval: a device that missed a read is a whole
	// interval behind, one cycle's replies are seconds apart.
	slack := time.Duration(in.Snapshot.IntervalSeconds) * time.Second / 2

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
		case okA && (!okB || !b.preferredTo(a, slack)):
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

// linkReadings is the part of *PortStatsCollector the map-link feed reads:
// the snapshot with each device's read time, taken together.
type linkReadings interface {
	SnapshotWithReadTimes() (PortSnapshot, map[string]time.Time)
}

// LiveTrafficCollector publishes per-link throughput for the network map on
// topology.traffic. Every rate comes from the port-stats snapshot: the
// collector listens in-process to the port-stats collector's traffic.ports
// publication — the end of a counter cycle — and then reads that collector's
// snapshot and per-device read times — so an open map costs the devices
// nothing. (It used to run one monitor-traffic per link interface every 5 s:
// 100–270 commands a minute on a 1-core switch.) It is subscriber-gated: the
// listener is attached only while someone watches the map, and a
// topology.traffic message goes out once per counter cycle.
type LiveTrafficCollector struct {
	db      *sql.DB
	ports   linkReadings
	hub     *ws.Hub
	cadence CadenceSource // judges the egress rows (FreshUplinks)
	now     func() time.Time
	cycles  chan struct{} // a counter cycle ended and Run has not caught up yet (capacity 1)

	// watchEvery is how often Run checks for map viewers.
	watchEvery time.Duration
}

// NewLiveTrafficCollector wires the feed to the port-stats collector whose
// snapshot it publishes; the fleet-wide schedule judges egress rows until
// UseCadence.
func NewLiveTrafficCollector(db *sql.DB, ports *PortStatsCollector, hub *ws.Hub) *LiveTrafficCollector {
	return &LiveTrafficCollector{
		db:         db,
		ports:      ports,
		hub:        hub,
		cadence:    newFleetCadence(db),
		now:        time.Now,
		cycles:     make(chan struct{}, 1),
		watchEvery: 2 * time.Second,
	}
}

// UseCadence judges the egress rows by cs (the manager's schedule). Call
// before Run.
func (c *LiveTrafficCollector) UseCadence(cs CadenceSource) {
	if cs != nil {
		c.cadence = cs
	}
}

// Run attaches the cycle listener while the map has viewers and publishes the
// link rates once per counter cycle; it returns when ctx is done.
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
		case <-c.cycles:
			if stop == nil || c.hub.TopicSubscriberCount(LiveTrafficTopic) == 0 {
				continue
			}
			// The newest state, not the published copy: the rates and the
			// read times the fresher-end rule needs come from one lock.
			snap, readAt := c.ports.SnapshotWithReadTimes()
			if !snap.Ready || snap.TS == nil {
				continue
			}
			payload, err := LoadLinkTraffic(c.db, c.cadence, snap, readAt, c.now())
			if err != nil {
				log.Printf("poller livetraffic: %v", err)
				continue
			}
			c.hub.Publish(LiveTrafficTopic, payload)
		}
	}
}

// offer tells Run a counter cycle ended (a traffic.ports publication), at
// most once ahead: cycles Run has not caught up with collapse into one, as
// only the newest state matters. It runs on the publisher's goroutine, so it
// never blocks and never touches the database.
func (c *LiveTrafficCollector) offer(v any) {
	if snap, ok := v.(PortSnapshot); !ok || !snap.Ready {
		return
	}
	select {
	case c.cycles <- struct{}{}:
	default: // one is pending already
	}
}
