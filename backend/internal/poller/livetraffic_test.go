package poller

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// liveDB opens an empty database for the live-view tests.
func liveDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// liveDevice stores a device with a probed class ("" leaves it unprobed).
func liveDevice(t *testing.T, db *sql.DB, id string, class devclass.Class, ifaces ...string) {
	t.Helper()
	d := &queries.Device{ID: id, Identity: id, Address: "192.0.2." + id, Username: "nms", APIPort: 8728, Status: "online"}
	if err := queries.CreateDevice(db, d); err != nil {
		t.Fatalf("create device %s: %v", id, err)
	}
	if class != "" {
		if err := queries.SetDeviceClass(db, id, class, devclass.ResourceFacts{}, time.Now()); err != nil {
			t.Fatalf("set class: %v", err)
		}
	}
	for _, name := range ifaces {
		if err := queries.UpsertInterface(db, &queries.Interface{ID: id + ":" + name, DeviceID: id, Name: name, Type: "ether", Running: true}); err != nil {
			t.Fatalf("upsert interface: %v", err)
		}
	}
}

func rateOf(dev, iface, typ string, rx, tx int64) PortRate {
	return PortRate{DeviceID: dev, Iface: iface, Type: typ, Running: true, RxBps: rx, TxBps: tx}
}

func readySnapshot(ts time.Time, ports ...PortRate) PortSnapshot {
	return PortSnapshot{TS: &ts, IntervalSeconds: 15, Ready: true, Ports: ports}
}

func byID(links []LinkTraffic) map[string]LinkTraffic {
	m := make(map[string]LinkTraffic, len(links))
	for _, l := range links {
		m[l.ID] = l
	}
	return m
}

func TestBuildLinkTrafficPrefersTheFresherEnd(t *testing.T) {
	ts := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	// a's sfp1 faces b's ether1; a reports 10 in / 90 out, b the mirror.
	snap := readySnapshot(ts, rateOf("a", "sfp1", "ether", 10, 90), rateOf("b", "ether1", "ether", 91, 11))
	link := queries.Link{ID: "L", DeviceAID: "a", InterfaceA: "sfp1,bridge", DeviceBID: "b", InterfaceB: "ether1", Status: "up"}
	online := func(id string) queries.Device { return queries.Device{ID: id, Status: "online"} }
	classed := func(id string, c devclass.Class) queries.Device {
		d := online(id)
		d.DeviceClass = string(c)
		return d
	}

	tests := []struct {
		name      string
		devices   []queries.Device
		sampledAt map[string]time.Time
		wantDev   string
		wantRx    int64
		wantTx    int64
	}{
		{"a fresher", []queries.Device{online("a"), online("b")},
			map[string]time.Time{"a": ts, "b": ts.Add(-30 * time.Second)}, "a", 10, 90},
		// Read from b, the rate is turned into a's frame: b's tx enters a.
		{"b fresher", []queries.Device{online("a"), online("b")},
			map[string]time.Time{"a": ts.Add(-45 * time.Second), "b": ts}, "b", 11, 91},
		{"tie: the online end", []queries.Device{{ID: "a", Status: "unknown"}, online("b")}, nil, "b", 11, 91},
		{"tie: the shorter counter cadence", []queries.Device{classed("a", devclass.C1), classed("b", devclass.C3)}, nil, "b", 11, 91},
		{"tie everywhere: the A end", []queries.Device{classed("a", devclass.C3), classed("b", devclass.C3)}, nil, "a", 10, 90},
	}
	for _, tt := range tests {
		got := BuildLinkTraffic(LinkTrafficInput{Links: []queries.Link{link}, Devices: tt.devices, Snapshot: snap, SampledAt: tt.sampledAt})
		if len(got) != 1 {
			t.Fatalf("%s: got %d links, want 1", tt.name, len(got))
		}
		l := got[0]
		if l.DeviceID != tt.wantDev || l.RxBps != tt.wantRx || l.TxBps != tt.wantTx || l.Source != "a" || l.Target != "b" {
			t.Errorf("%s: got %+v, want read on %s rx=%d tx=%d", tt.name, l, tt.wantDev, tt.wantRx, tt.wantTx)
		}
		wantIface := map[string]string{"a": "sfp1", "b": "ether1"}[tt.wantDev]
		if l.Iface != wantIface {
			t.Errorf("%s: measured iface %q, want %q", tt.name, l.Iface, wantIface)
		}
	}
}

func TestBuildLinkTrafficMeasurableEnds(t *testing.T) {
	ts := time.Now().UTC()
	snap := readySnapshot(ts,
		rateOf("a", "bridge", "bridge", 500, 500),
		rateOf("a", "ether2", "ether", 3, 4),
		rateOf("b", "bridge", "bridge", 600, 600),
		rateOf("b", "net28", "vlan", 7, 8),
	)
	links := []queries.Link{
		{ID: "bridges", DeviceAID: "a", InterfaceA: "bridge", DeviceBID: "b", InterfaceB: "bridge", Status: "up"},
		{ID: "one-side", DeviceAID: "a", InterfaceA: "unknown", DeviceBID: "b", InterfaceB: "net28", Status: "up"},
		{ID: "down", DeviceAID: "a", InterfaceA: "ether2", DeviceBID: "b", InterfaceB: "net28", Status: "down"},
		{ID: "missing", DeviceAID: "a", InterfaceA: "ether9", DeviceBID: "c", InterfaceB: "ether1", Status: "up"},
	}
	got := byID(BuildLinkTraffic(LinkTrafficInput{Links: links, Snapshot: snap}))
	if _, ok := got["bridges"]; ok {
		t.Error("a bridge-to-bridge link was measured on a bridge (its whole bridged traffic)")
	}
	if l, ok := got["one-side"]; !ok || l.DeviceID != "b" || l.RxBps != 8 || l.TxBps != 7 {
		t.Errorf("one-side = %+v, want read on b/net28 in a's frame", l)
	}
	for _, id := range []string{"down", "missing"} {
		if _, ok := got[id]; ok {
			t.Errorf("%s link got a rate", id)
		}
	}
}

func TestBuildLinkTrafficEgressEdges(t *testing.T) {
	ts := time.Now().UTC()
	snap := readySnapshot(ts,
		rateOf("r", "eth1-wan", "ether", 100, 10),
		rateOf("r", "wg0", "wg", 5, 6),
		rateOf("r", "wg1", "wg", 1, 2),
		rateOf("s", "bridge", "bridge", 9, 9),
		rateOf("s", "ether7", "ether", 40, 30),
		rateOf("t", "ether1", "ether", 2, 2),
	)
	devices := []queries.Device{{ID: "r", Address: "10.0.0.1"}, {ID: "s", Address: "10.0.0.2"}, {ID: "t", Address: "10.0.0.3"}}
	ups := []queries.DeviceUplink{
		{DeviceID: "r", Kind: "default-route", Interface: "eth1-wan", IfaceType: "ether", GatewayIP: "203.0.113.1"},
		{DeviceID: "r", Kind: "vpn", Interface: "wg0", IfaceType: "wg"},
		// A tunnel that carries the default route is drawn once, as the route.
		{DeviceID: "r", Kind: "default-route", Interface: "wg1", IfaceType: "wg"},
		{DeviceID: "r", Kind: "vpn", Interface: "wg1", IfaceType: "wg"},
		// A switch's route via its bridge to an unmanaged gateway: the edge
		// exists but a bridge is not measurable; the gateway's FDB port is.
		{DeviceID: "s", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "10.0.0.254"},
		{DeviceID: "s", Kind: "gateway-host", Interface: "ether7", GatewayIP: "10.0.0.254"},
		// A managed next-hop is an L2 link already: no egress edge.
		{DeviceID: "t", Kind: "default-route", Interface: "ether1", IfaceType: "ether", GatewayIP: "10.0.0.1"},
		// An FDB attachment for a gateway nobody routes through: no node, no edge.
		{DeviceID: "t", Kind: "gateway-host", Interface: "ether1", GatewayIP: "10.9.9.9"},
	}
	got := byID(BuildLinkTraffic(LinkTrafficInput{Devices: devices, Uplinks: ups, Snapshot: snap}))

	want := map[string]LinkTraffic{
		"up:r:eth1-wan:203.0.113.1": {Source: "r", Target: "internet", DeviceID: "r", Iface: "eth1-wan", RxBps: 100, TxBps: 10},
		"up:r:wg0:":                 {Source: "r", Target: "vpn:r:wg0", DeviceID: "r", Iface: "wg0", RxBps: 5, TxBps: 6},
		"up:r:wg1:":                 {Source: "r", Target: "vpn:r:wg1", DeviceID: "r", Iface: "wg1", RxBps: 1, TxBps: 2},
		"gwhost:10.0.0.254":         {Source: "s", Target: "gw:10.0.0.254", DeviceID: "s", Iface: "ether7", RxBps: 40, TxBps: 30},
	}
	if len(got) != len(want) {
		t.Errorf("got %d edges %v, want %d", len(got), got, len(want))
	}
	for id, w := range want {
		w.ID = id
		if g, ok := got[id]; !ok || g != w {
			t.Errorf("%s = %+v, want %+v", id, g, w)
		}
	}
}

func TestNewSnapshotMeta(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 40, 0, time.UTC)
	m := NewSnapshotMeta(PortSnapshot{IntervalSeconds: 15}, now)
	if m.Source != SourceSnapshot || m.SampledAt != nil || m.AgeSeconds != nil || m.Ready {
		t.Errorf("before the first cycle: %+v", m)
	}
	m = NewSnapshotMeta(readySnapshot(now.Add(-12500*time.Millisecond)), now)
	if m.SampledAt == nil || m.AgeSeconds == nil || *m.AgeSeconds != 12 || !m.Ready || m.IntervalSeconds != 15 {
		t.Errorf("after a cycle: %+v (age %v)", m, m.AgeSeconds)
	}
	// A snapshot stamped after now (clock step) never reports a negative age.
	if m = NewSnapshotMeta(readySnapshot(now.Add(time.Second)), now); *m.AgeSeconds != 0 {
		t.Errorf("future snapshot age = %d", *m.AgeSeconds)
	}
}

// The port-stats collector publishes only while traffic.ports has a
// subscriber; the map feed's in-process listener must count as one.
func TestPortStatsPublishesToAnInProcessListener(t *testing.T) {
	hub := ws.NewHub()
	go hub.Run()
	c := NewPortStatsCollector(nil, nil, hub)
	got := make(chan any, 1)
	defer hub.Listen(PortRatesTopic, func(v any) { got <- v })()
	c.publish()
	select {
	case v := <-got:
		if _, ok := v.(PortSnapshot); !ok {
			t.Fatalf("listener got %T, want PortSnapshot", v)
		}
	default:
		t.Fatal("the collector did not publish to the in-process listener")
	}
}

// End to end: while the map is watched, each traffic.ports publication turns
// into one topology.traffic message — and no device sees a single command.
func TestLiveTrafficCollectorPublishesFromTheSnapshot(t *testing.T) {
	db := liveDB(t)
	liveDevice(t, db, "a", devclass.C1, "sfp1")
	liveDevice(t, db, "b", devclass.C3, "ether1")
	if err := queries.UpsertLink(db, &queries.Link{ID: "L", DeviceAID: "a", InterfaceA: "sfp1", DeviceBID: "b",
		InterfaceB: "ether1", LinkType: "ethernet", DiscoveredBy: "mndp", Status: "up"}); err != nil {
		t.Fatal(err)
	}
	links, err := queries.ListLinks(db)
	if err != nil || len(links) != 1 {
		t.Fatalf("links: %v %v", links, err)
	}
	linkID := links[0].ID

	// Both devices have a live pooled client to a tripwire endpoint.
	pool := routeros.NewPool(false)
	fake := routerostest.NewDevice(t)
	fake.Dial(t, pool, "a")
	fake.Dial(t, pool, "b")

	hub := ws.NewHub()
	go hub.Run()
	c := NewLiveTrafficCollector(db, pool, hub)
	c.watchEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); c.Run(ctx) }()
	defer func() { cancel(); wg.Wait() }()

	// Nobody watches the map: the collector must not listen to the snapshot.
	time.Sleep(30 * time.Millisecond)
	if n := hub.TopicSubscriberCount(PortRatesTopic); n != 0 {
		t.Fatalf("listening to traffic.ports with no map viewer (%d)", n)
	}

	payloads := make(chan LinkTrafficPayload, 4)
	stopWatching := hub.Listen(LiveTrafficTopic, func(v any) { payloads <- v.(LinkTrafficPayload) })
	waitUntil(t, func() bool { return hub.TopicSubscriberCount(PortRatesTopic) == 1 })

	ts := time.Now().UTC()
	hub.Publish(PortRatesTopic, readySnapshot(ts, rateOf("a", "sfp1", "ether", 10, 90), rateOf("b", "ether1", "ether", 91, 11)))
	select {
	case p := <-payloads:
		if p.Source != SourceSnapshot || p.SampledAt == nil || !p.SampledAt.Equal(ts) || len(p.Links) != 1 {
			t.Fatalf("payload = %+v", p)
		}
		// Equal freshness, so the C3 end (b) is read, turned into a's frame.
		if l := p.Links[0]; l.ID != linkID || l.DeviceID != "b" || l.RxBps != 11 || l.TxBps != 91 {
			t.Errorf("link = %+v", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no topology.traffic message after a snapshot")
	}

	// A snapshot that is not ready yet publishes nothing.
	hub.Publish(PortRatesTopic, PortSnapshot{IntervalSeconds: 15, Ports: []PortRate{}})
	select {
	case p := <-payloads:
		t.Fatalf("published from a not-ready snapshot: %+v", p)
	case <-time.After(50 * time.Millisecond):
	}

	// The last viewer leaves: the listener comes off again.
	stopWatching()
	waitUntil(t, func() bool { return hub.TopicSubscriberCount(PortRatesTopic) == 0 })

	if cmds := fake.Commands(); len(cmds) != 0 {
		t.Fatalf("the map feed sent device commands: %v", cmds)
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}
