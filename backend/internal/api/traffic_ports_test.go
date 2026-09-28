package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/topology"
)

const mbps = int64(1_000_000)

// fakePortStats stands in for the collector.
type fakePortStats struct {
	snap            poller.PortSnapshot
	flushed, rolled time.Time
}

func (f *fakePortStats) Snapshot() poller.PortSnapshot { return f.snap }
func (f *fakePortStats) FlushedThrough() time.Time     { return f.flushed }
func (f *fakePortStats) RolledThrough() time.Time      { return f.rolled }

// trafficFixture is the contract §7.6 mesh (R internet edge → C core → S
// access → A1/A2 APs, MNDP hearing nearly every pair) stored in a real DB,
// plus a live snapshot with the §7.6 download rates.
type trafficFixture struct {
	db   *sql.DB
	s    *Server
	fake *fakePortStats
	end  time.Time // FlushedThrough: minute-aligned end of the 1m windows
}

func ifaceMAC(dev, iface string) string { return strings.ToUpper("02:" + dev + ":" + iface) }

func newTrafficFixture(t *testing.T) *trafficFixture {
	t.Helper()
	db := newTestDB(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	for i, d := range []struct{ id, name string }{{"r", "R"}, {"c", "C"}, {"s", "S"}, {"a1", "A1"}, {"a2", "A2"}} {
		must(queries.CreateDevice(db, &queries.Device{ID: d.id, Identity: d.name,
			Address: fmt.Sprintf("10.0.0.%d", i+1), Username: "admin", APIPort: 8728, Status: "online"}))
	}
	ifaces := map[string][]string{
		"r":  {"eth1-wan:ether", "sfp1:ether", "bridge:bridge"},
		"c":  {"sfp2:ether", "q1:ether", "net28:vlan", "bridge:bridge"},
		"s":  {"sfp1:ether", "ether23:ether", "ether5:ether", "ether7:ether", "net28:vlan", "bridge:bridge"},
		"a1": {"ether1:ether", "wifi1:wifi", "bridge:bridge"},
		"a2": {"ether1:ether", "wifi1:wifi", "bridge:bridge"},
	}
	for dev, specs := range ifaces {
		for _, spec := range specs {
			name, typ, _ := strings.Cut(spec, ":")
			must(queries.UpsertInterface(db, &queries.Interface{ID: uuid.NewString(), DeviceID: dev, Name: name,
				Type: typ, MACAddress: ifaceMAC(dev, name), Running: true}))
		}
	}
	for _, l := range [][4]string{
		{"r", "sfp1,bridge", "c", "sfp2,bridge"}, {"r", "sfp1,bridge", "s", "sfp1,bridge"},
		{"r", "sfp1,bridge", "a1", "ether1,bridge"}, {"r", "sfp1,bridge", "a2", "ether1,bridge"},
		{"c", "q1,bridge", "s", "sfp1,bridge"}, {"c", "q1,bridge", "a1", "ether1,bridge"},
		{"c", "q1,bridge", "a2", "ether1,bridge"}, {"s", "ether23,bridge", "a1", "ether1,bridge"},
		{"s", "ether5,bridge", "a2", "ether1,bridge"}, {"a1", "ether1,bridge", "a2", "ether1,bridge"},
		{"c", "net28", "s", "net28"},
	} {
		must(queries.UpsertLink(db, &queries.Link{ID: uuid.NewString(), DeviceAID: l[0], InterfaceA: l[1],
			DeviceBID: l[2], InterfaceB: l[3], LinkType: "ethernet", DiscoveredBy: "mndp", Status: "up"}))
	}
	must(queries.ReplaceDeviceUplinks(db, "r", []queries.DeviceUplink{
		{DeviceID: "r", Kind: "default-route", Interface: "eth1-wan", IfaceType: "ether", GatewayIP: "203.0.113.1"}}))
	for _, dev := range []string{"c", "s", "a1", "a2"} {
		must(queries.ReplaceDeviceUplinks(db, dev, []queries.DeviceUplink{
			{DeviceID: dev, Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "10.0.0.1"}}))
	}

	// FDB: a MAC is learned only on ports that face toward it.
	fdb := map[string][]string{
		"AA:00:00:00:00:01":      {"a1/wifi1", "s/ether23", "c/q1", "r/sfp1"},
		"AA:00:00:00:00:02":      {"a1/wifi1", "s/ether23", "c/q1", "r/sfp1"},
		"BB:00:00:00:00:07":      {"s/ether7", "c/q1", "r/sfp1"},
		ifaceMAC("a2", "bridge"): {"s/ether5", "c/q1", "r/sfp1"}, // managed: never a client
	}
	perDev := map[string][]queries.PortHost{}
	for mac, at := range fdb {
		for _, a := range at {
			dev, iface, _ := strings.Cut(a, "/")
			perDev[dev] = append(perDev[dev], queries.PortHost{InterfaceName: iface, MACAddress: mac, VID: 78})
		}
	}
	for dev, hosts := range perDev {
		must(queries.UpsertPortHosts(db, dev, hosts, time.Now()))
	}

	must(queries.UpsertMACLookup(db, &queries.MACLookup{MACAddress: "BB:00:00:00:00:07", IPAddress: "192.168.78.50",
		HostName: "printer", Source: "dhcp"}))
	must(queries.UpsertMACLookup(db, &queries.MACLookup{MACAddress: "AA:00:00:00:00:01", IPAddress: "192.168.78.20",
		DNSName: "laptop.lan", Source: "capsman", AP: "A1", SSID: "home", Signal: "-55"}))

	must(queries.UpsertBridgeVLAN(db, &queries.BridgeVLAN{ID: uuid.NewString(), DeviceID: "s", BridgeName: "bridge",
		VLANIDs: "78", CurrentTagged: "bridge,sfp1", CurrentUntagged: "ether7,ether23"}))
	must(queries.UpsertBridgeVLAN(db, &queries.BridgeVLAN{ID: uuid.NewString(), DeviceID: "s", BridgeName: "bridge",
		VLANIDs: "10,20-21,100-4094", CurrentTagged: "bridge,sfp1,ether7"}))
	must(queries.UpsertVLANLabel(db, &queries.VLANLabel{VLANID: 78, Name: "lan78"}))

	ts := time.Now().UTC()
	rate := func(dev, iface, typ string, rx, tx int64) poller.PortRate {
		return poller.PortRate{DeviceID: dev, Iface: iface, Type: typ, Running: true, RxBps: rx, TxBps: tx}
	}
	fake := &fakePortStats{
		snap: poller.PortSnapshot{TS: &ts, IntervalSeconds: 15, Ready: true, Ports: []poller.PortRate{
			rate("a1", "ether1", "ether", 60*mbps, 4*mbps),
			rate("a1", "wifi1", "wifi", 4*mbps, 55*mbps),
			rate("c", "q1", "ether", 8*mbps, 90*mbps),
			rate("c", "sfp2", "ether", 95*mbps, 9*mbps),
			rate("r", "bridge", "bridge", 3*mbps, 1*mbps),
			rate("r", "eth1-wan", "ether", 100*mbps, 10*mbps),
			rate("r", "sfp1", "ether", 9*mbps, 95*mbps),
			rate("s", "ether23", "ether", 5*mbps, 60*mbps),
			rate("s", "ether5", "ether", 2*mbps, 20*mbps),
			rate("s", "ether7", "ether", 1*mbps, 5*mbps),
			rate("s", "sfp1", "ether", 90*mbps, 8*mbps),
		}},
		flushed: time.Now().UTC().Truncate(time.Minute),
	}
	return &trafficFixture{db: db, s: &Server{db: db, portStats: fake}, fake: fake, end: fake.flushed}
}

// seedMinutes writes n one-minute rows ending at f.end for a port: each minute
// carries rxBytes/txBytes, with the given peak rates.
func (f *trafficFixture) seedMinutes(t *testing.T, dev, iface string, n int, rxBytes, txBytes, rxMax, txMax int64) {
	t.Helper()
	var rows []queries.PortStatRow
	for i := n; i >= 1; i-- {
		rows = append(rows, queries.PortStatRow{DeviceID: dev, InterfaceName: iface,
			Bucket:   f.end.Add(-time.Duration(i) * time.Minute),
			RxBpsAvg: rxBytes * 8 / 60, TxBpsAvg: txBytes * 8 / 60, RxBpsMax: rxMax, TxBpsMax: txMax,
			RxBytes: rxBytes, TxBytes: txBytes, Samples: 4})
	}
	if err := queries.InsertPortStats1m(f.db, rows); err != nil {
		t.Fatal(err)
	}
}

func (f *trafficFixture) router() http.Handler {
	r := chi.NewRouter()
	f.s.mountTrafficRoutes(r)
	return r
}

// get issues a GET against the traffic routes and decodes the JSON body.
func (f *trafficFixture) get(t *testing.T, path string, params map[string]string, out any) int {
	t.Helper()
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	rec := httptest.NewRecorder()
	f.router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: decode %q: %v", path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

func TestTrafficRoutesStaticBeforeParam(t *testing.T) {
	f := newTrafficFixture(t)
	for path, marker := range map[string]string{
		"/traffic/ports/latest":                       `"interval_seconds"`,
		"/traffic/ports/top":                          `"rows"`,
		"/traffic/ports/roles":                        `"anchored"`,
		"/traffic/ports/history?device=r&iface=sfp1":  `"step_seconds"`,
		"/traffic/ports/behind?device=r&iface=sfp1":   `"mac_count"`,
		"/traffic/path?device=r":                      `"hops"`,
		"/traffic/sankey":                             `"estimated"`,
		"/traffic/summary":                            `"device_id"`,
		"/traffic/r/" + url.PathEscape("eth1-wan"):    `[]`,
		"/traffic/ports/history?device=r&iface=a%2Fb": `"iface":"a/b"`,
	} {
		rec := httptest.NewRecorder()
		f.router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), marker) {
			t.Errorf("GET %s = %d %q, want 200 containing %s", path, rec.Code, rec.Body.String(), marker)
		}
	}
}

func TestTrafficPortsLatestWithoutCollector(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleTrafficPortsLatest(rec, httptest.NewRequest(http.MethodGet, "/traffic/ports/latest", nil))
	want := `{"ts":null,"interval_seconds":15,"ready":false,"ports":[]}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("got %d %s, want %s", rec.Code, rec.Body.String(), want)
	}

	rec = httptest.NewRecorder()
	s.handleGetTrafficSummary(rec, httptest.NewRequest(http.MethodGet, "/traffic/summary", nil))
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("summary without collector = %s, want []", rec.Body.String())
	}
}

func TestTrafficPortsLatestEnriched(t *testing.T) {
	f := newTrafficFixture(t)
	var resp latestPortsResponse
	if code := f.get(t, "/traffic/ports/latest", nil, &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !resp.Ready || resp.TS == nil || resp.IntervalSeconds != 15 || len(resp.Ports) != len(f.fake.snap.Ports) {
		t.Fatalf("unexpected snapshot: %+v", resp)
	}
	got := map[string]latestPortRow{}
	for _, p := range resp.Ports {
		got[p.DeviceID+"/"+p.Iface] = p
	}
	for key, want := range map[string][2]string{
		"r/eth1-wan": {"R", "wan"}, "r/sfp1": {"R", "downlink"}, "r/bridge": {"R", "virtual"},
		"c/sfp2": {"C", "uplink"}, "s/ether7": {"S", "access"}, "a1/wifi1": {"A1", "wireless"},
	} {
		if p := got[key]; p.DeviceName != want[0] || p.Role != want[1] {
			t.Errorf("%s: name/role = %q/%q, want %q/%q", key, p.DeviceName, p.Role, want[0], want[1])
		}
	}
}

func TestTrafficPortRoles(t *testing.T) {
	f := newTrafficFixture(t)
	var resp portRolesResponse
	if code := f.get(t, "/traffic/ports/roles", nil, &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !resp.Anchored {
		t.Fatal("want anchored (R has a public default route)")
	}
	parents := map[string]string{}
	for _, d := range resp.Devices {
		parents[d.DeviceID] = d.ParentID + "/" + d.ParentIface
	}
	wantParents := map[string]string{"r": "internet/", "c": "r/sfp1", "s": "c/q1", "a1": "s/ether23", "a2": "s/ether5"}
	if !reflect.DeepEqual(parents, wantParents) {
		t.Fatalf("parents = %v, want %v", parents, wantParents)
	}

	got := map[string]portRoleRow{}
	for _, r := range resp.Roles {
		got[r.DeviceID+"/"+r.Iface] = r
	}
	for key, want := range map[string][2]string{
		"r/eth1-wan": {"wan", "Internet"},
		"r/sfp1":     {"downlink", "C"},
		"c/sfp2":     {"uplink", "↑ R"},
		"s/ether7":   {"access", "printer"},
		"a1/wifi1":   {"wireless", "2 Wi-Fi clients"},
		"a2/wifi1":   {"idle", ""},
		"c/net28":    {"virtual", ""},
	} {
		r := got[key]
		if r.Role != want[0] || r.Behind != want[1] {
			t.Errorf("%s: role/behind = %q/%q, want %q/%q", key, r.Role, r.Behind, want[0], want[1])
		}
	}
	if r := got["s/ether7"]; r.DeviceName != "S" || r.ClientCount != 1 || r.MACCount != 1 {
		t.Errorf("s/ether7 = %+v", r)
	}
}

func TestTrafficBehindSummary(t *testing.T) {
	tp := &trafficTopo{}
	cases := map[string]struct {
		role, upstream, neighbor, gw string
		count                        int
		want                         string
	}{
		"wan internet":      {role: "wan", upstream: "internet", want: "Internet"},
		"wan gateway":       {role: "wan", upstream: "gw:192.168.8.1", neighbor: "opnsense", gw: "192.168.8.1", want: "via opnsense"},
		"wan gateway no nm": {role: "wan", upstream: "gw:192.168.8.1", gw: "192.168.8.1", want: "via 192.168.8.1"},
		"uplink orphan":     {role: "uplink", want: ""},
		"downlink many":     {role: "downlink", neighbor: "sw2", count: 3, want: "sw2 +2"},
		"access many":       {role: "access", count: 4, want: "4 clients"},
		"vpn":               {role: "vpn", want: "VPN tunnel"},
		"idle":              {role: "idle", want: ""},
	}
	for name, c := range cases {
		var ri topology.PortRoleInfo
		ri.Role, ri.UpstreamNode, ri.NeighborName, ri.GatewayIP = c.role, c.upstream, c.neighbor, c.gw
		ri.NeighborCount, ri.ClientCount = c.count, c.count
		if got := tp.behind(ri); got != c.want {
			t.Errorf("%s: behind = %q, want %q", name, got, c.want)
		}
	}
}

func TestTrafficPath(t *testing.T) {
	f := newTrafficFixture(t)
	var resp trafficPathResponse
	if code := f.get(t, "/traffic/path", map[string]string{"device": "s", "iface": "ether7"}, &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.Role != "access" || !resp.Anchored {
		t.Fatalf("role/anchored = %q/%v", resp.Role, resp.Anchored)
	}
	var ids []string
	for _, h := range resp.Hops {
		ids = append(ids, h.ID)
	}
	if want := []string{"internet", "r", "c", "s", "clients:s:ether7"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("hops = %v, want %v", ids, want)
	}
	// Segment rates come from the live snapshot (§6).
	for i, want := range [][2]int64{{0, 0}, {100 * mbps, 10 * mbps}, {95 * mbps, 9 * mbps}, {90 * mbps, 8 * mbps}, {5 * mbps, 1 * mbps}} {
		if h := resp.Hops[i]; h.DownBps != want[0] || h.UpBps != want[1] || h.Measured != (i > 0) {
			t.Errorf("hop %d (%s): down/up/measured = %d/%d/%v, want %d/%d", i, h.ID, h.DownBps, h.UpBps, h.Measured, want[0], want[1])
		}
	}

	if code := f.get(t, "/traffic/path", map[string]string{"device": "nope"}, nil); code != http.StatusNotFound {
		t.Errorf("unknown device: status %d, want 404", code)
	}
	if code := f.get(t, "/traffic/path", nil, nil); code != http.StatusBadRequest {
		t.Errorf("missing device: status %d, want 400", code)
	}
	resp = trafficPathResponse{}
	if f.get(t, "/traffic/path", map[string]string{"device": "c"}, &resp); resp.Role != "" || len(resp.Hops) != 3 {
		t.Errorf("no iface: role %q, %d hops; want \"\" and internet/R/C", resp.Role, len(resp.Hops))
	}
}

// checkSankeyShape asserts valid indices, no self-links, one parent per node
// and values >= 1 kbps.
func checkSankeyShape(t *testing.T, sk trafficSankeyResponse) {
	t.Helper()
	parent := map[int]bool{}
	for _, l := range sk.Links {
		if l.Source < 0 || l.Source >= len(sk.Nodes) || l.Target < 0 || l.Target >= len(sk.Nodes) || l.Source == l.Target {
			t.Fatalf("bad link %+v (%d nodes)", l, len(sk.Nodes))
		}
		if parent[l.Target] {
			t.Fatalf("node %d has two parents", l.Target)
		}
		parent[l.Target] = true
		if l.Value < 1000 {
			t.Fatalf("link below MinBps: %+v", l)
		}
	}
}

// sankeyEdges maps "sourceID>targetID" → value.
func sankeyEdges(sk trafficSankeyResponse) map[string]int64 {
	out := map[string]int64{}
	for _, l := range sk.Links {
		out[sk.Nodes[l.Source].ID+">"+sk.Nodes[l.Target].ID] = l.Value
	}
	return out
}

func TestTrafficSankeyLive(t *testing.T) {
	f := newTrafficFixture(t)
	var sk trafficSankeyResponse
	if code := f.get(t, "/traffic/sankey", map[string]string{"range": "live"}, &sk); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	checkSankeyShape(t, sk)
	if !sk.Estimated || sk.Direction != "download" || sk.Range != "live" || sk.From != nil || sk.To != nil {
		t.Fatalf("header = %+v", sk)
	}
	want := map[string]int64{
		"internet>r": 100 * mbps, "r>c": 95 * mbps, "c>s": 90 * mbps, "s>a1": 60 * mbps, "s>a2": 20 * mbps,
		"s>port:s:ether7": 5 * mbps, "a1>port:a1:wifi1": 55 * mbps,
	}
	if got := sankeyEdges(sk); !reflect.DeepEqual(got, want) {
		t.Fatalf("download edges = %v, want %v", got, want)
	}

	sk = trafficSankeyResponse{}
	f.get(t, "/traffic/sankey", map[string]string{"range": "live", "dir": "upload", "device": "s"}, &sk)
	checkSankeyShape(t, sk)
	if got := sankeyEdges(sk); got["s>a1"] != 5*mbps || got["s>port:s:ether7"] != 1*mbps || got["internet>r"] != 0 {
		t.Fatalf("upload edges rooted at S = %v", got)
	}

	for params, code := range map[string]int{"dir=sideways": 400, "range=2h": 400, "device=nope&range=live": 200} {
		q, _ := url.ParseQuery(params)
		p := map[string]string{}
		for k := range q {
			p[k] = q.Get(k)
		}
		var body trafficSankeyResponse
		if got := f.get(t, "/traffic/sankey", p, &body); got != code {
			t.Errorf("%s: status %d, want %d", params, got, code)
		}
		if code == 200 && (body.Nodes == nil || len(body.Nodes) != 0 || body.Links == nil) {
			t.Errorf("%s: want empty non-nil nodes/links, got %+v", params, body)
		}
	}
}

func TestTrafficSankeyRange(t *testing.T) {
	f := newTrafficFixture(t)
	// 10 minutes at 1 Mbit/s (7.5 MB per minute) on the WAN and R→C only;
	// every other port is known but has no rows, i.e. a measured zero.
	f.seedMinutes(t, "r", "eth1-wan", 10, 7_500_000, 750_000, 2*mbps, mbps)
	f.seedMinutes(t, "r", "sfp1", 10, 750_000, 7_500_000, mbps, 2*mbps)

	var sk trafficSankeyResponse
	if code := f.get(t, "/traffic/sankey", map[string]string{"range": "1h"}, &sk); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	checkSankeyShape(t, sk)
	if sk.From == nil || sk.To == nil || !sk.To.Equal(f.end) || !sk.From.Equal(f.end.Add(-time.Hour)) {
		t.Fatalf("from/to = %v/%v, want %v..%v", sk.From, sk.To, f.end.Add(-time.Hour), f.end)
	}
	// Averaged over the covered 10 minutes, not the whole hour.
	if got, want := sankeyEdges(sk), map[string]int64{"internet>r": mbps, "r>c": mbps}; !reflect.DeepEqual(got, want) {
		t.Fatalf("edges = %v, want %v", got, want)
	}

	// C has no rows at all (offline): its q1 is unmeasured, so C→S falls
	// back to S's uplink rx. S itself was polled, so its row-less ether23 is
	// a measured zero and S→A1 is dropped.
	f.seedMinutes(t, "s", "sfp1", 10, 3_750_000, 0, mbps, 0)
	f.seedMinutes(t, "s", "ether7", 10, 0, 1_875_000, 0, mbps)
	sk = trafficSankeyResponse{}
	f.get(t, "/traffic/sankey", map[string]string{"range": "1h"}, &sk)
	checkSankeyShape(t, sk)
	want := map[string]int64{"internet>r": mbps, "r>c": mbps, "c>s": mbps / 2, "s>port:s:ether7": mbps / 4}
	if got := sankeyEdges(sk); !reflect.DeepEqual(got, want) {
		t.Fatalf("edges with offline C = %v, want %v", got, want)
	}

	// No rows in the window at all: empty.
	sk = trafficSankeyResponse{}
	f.get(t, "/traffic/sankey", map[string]string{"range": "7d"}, &sk)
	if len(sk.Nodes) != 0 || len(sk.Links) != 0 {
		t.Fatalf("7d without hourly rows: %+v", sk)
	}
}

func TestTrafficPortsTopLive(t *testing.T) {
	f := newTrafficFixture(t)
	var resp topPortsResponse
	if code := f.get(t, "/traffic/ports/top", map[string]string{"range": "live", "metric": "bytes"}, &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.Metric != "avg" || resp.Resolution != "live" || resp.From != nil || resp.To != nil {
		t.Fatalf("header = %+v", resp)
	}
	if len(resp.Rows) != 10 { // 11 snapshot ports minus the virtual r/bridge
		t.Fatalf("rows = %d, want 10", len(resp.Rows))
	}
	top := resp.Rows[0]
	if top.Rank != 1 || top.DeviceID != "r" || top.Iface != "eth1-wan" || top.Role != "wan" || top.Behind != "Internet" ||
		top.Value != 110*mbps || top.RxAvg != 100*mbps || top.RxMax != 100*mbps || !top.Running || top.Type != "ether" {
		t.Fatalf("top row = %+v", top)
	}
	for i := 1; i < len(resp.Rows); i++ {
		if resp.Rows[i].Value > resp.Rows[i-1].Value || resp.Rows[i].Rank != i+1 {
			t.Fatalf("rows not ranked: %+v", resp.Rows)
		}
	}
	if len(top.Sparkline) != 0 {
		t.Fatalf("sparkline without 1m rows = %v, want []", top.Sparkline)
	}

	// dir=tx ranks by tx; limit caps; physical=0 keeps the bridge.
	resp = topPortsResponse{}
	f.get(t, "/traffic/ports/top", map[string]string{"range": "live", "dir": "tx", "limit": "2", "physical": "0"}, &resp)
	if len(resp.Rows) != 2 || resp.Rows[0].Iface != "sfp1" || resp.Rows[0].Value != 95*mbps {
		t.Fatalf("dir=tx limit=2: %+v", resp.Rows)
	}
	resp = topPortsResponse{}
	f.get(t, "/traffic/ports/top", map[string]string{"range": "live", "physical": "0", "limit": "200"}, &resp)
	if len(resp.Rows) != 11 {
		t.Fatalf("physical=0: %d rows, want 11", len(resp.Rows))
	}

	// device=a2: every known interface, idle ones as zeros (bridge is virtual).
	resp = topPortsResponse{}
	f.get(t, "/traffic/ports/top", map[string]string{"range": "live", "device": "a2"}, &resp)
	var names []string
	for _, r := range resp.Rows {
		names = append(names, r.Iface+":"+r.Role)
		if r.Value != 0 || r.Running {
			t.Errorf("a2 row not idle: %+v", r)
		}
	}
	if want := []string{"ether1:uplink", "wifi1:idle"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("device=a2 rows = %v, want %v", names, want)
	}

	// The live sparkline shows the last 30 minutes of 1m buckets.
	f.seedMinutes(t, "r", "eth1-wan", 5, 7_500_000, 0, mbps, 0)
	resp = topPortsResponse{}
	f.get(t, "/traffic/ports/top", map[string]string{"range": "live", "limit": "1"}, &resp)
	if sp := resp.Rows[0].Sparkline; len(sp) != 5 || sp[0].RxBps != mbps || !sp[4].TS.Equal(f.end.Add(-time.Minute)) {
		t.Fatalf("live sparkline = %+v", sp)
	}
}

func TestTrafficPortsTopRange(t *testing.T) {
	f := newTrafficFixture(t)
	f.seedMinutes(t, "r", "eth1-wan", 10, 7_500_000, 750_000, 3*mbps, 2*mbps)
	f.seedMinutes(t, "c", "net28", 10, 75_000_000, 0, 20*mbps, 0) // virtual: filtered by default

	var resp topPortsResponse
	if code := f.get(t, "/traffic/ports/top", nil, &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.Range != "1h" || resp.Metric != "avg" || resp.Dir != "total" || resp.Resolution != "1m" {
		t.Fatalf("defaults = %+v", resp)
	}
	if resp.From == nil || !resp.From.Equal(f.end.Add(-time.Hour)) || resp.To == nil || !resp.To.Equal(f.end) ||
		resp.CoverageFrom == nil || !resp.CoverageFrom.Equal(f.end.Add(-10*time.Minute)) {
		t.Fatalf("window = %v..%v coverage %v", resp.From, resp.To, resp.CoverageFrom)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("rows = %+v, want only r/eth1-wan", resp.Rows)
	}
	row := resp.Rows[0]
	if row.RxAvg != mbps || row.TxAvg != mbps/10 || row.RxMax != 3*mbps || row.TxMax != 2*mbps ||
		row.RxBytes != 75_000_000 || row.TxBytes != 7_500_000 || row.Value != mbps+mbps/10 || row.Running != true {
		t.Fatalf("row = %+v", row)
	}
	// 1h sparkline step is 2 minutes; points before coverage are omitted.
	if len(row.Sparkline) != 5 || !row.Sparkline[0].TS.Equal(f.end.Add(-10*time.Minute)) || row.Sparkline[0].RxBps != mbps {
		t.Fatalf("sparkline = %+v", row.Sparkline)
	}

	resp = topPortsResponse{}
	f.get(t, "/traffic/ports/top", map[string]string{"metric": "max", "physical": "false", "sparkline": "0"}, &resp)
	if len(resp.Rows) != 2 || resp.Rows[0].Iface != "net28" || resp.Rows[0].Value != 20*mbps || len(resp.Rows[0].Sparkline) != 0 {
		t.Fatalf("metric=max physical=false: %+v", resp.Rows)
	}
	resp = topPortsResponse{}
	f.get(t, "/traffic/ports/top", map[string]string{"metric": "bytes", "dir": "rx"}, &resp)
	if resp.Rows[0].Value != 75_000_000 {
		t.Fatalf("metric=bytes dir=rx value = %d", resp.Rows[0].Value)
	}

	// A device's list is complete even without rows for most ports.
	resp = topPortsResponse{}
	f.get(t, "/traffic/ports/top", map[string]string{"device": "r", "range": "6h"}, &resp)
	var names []string
	for _, r := range resp.Rows {
		names = append(names, r.Iface)
	}
	if want := []string{"eth1-wan", "sfp1"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("device=r rows = %v, want %v", names, want)
	}

	// No coverage: null coverage, device rows still listed with zeros.
	resp = topPortsResponse{}
	f.get(t, "/traffic/ports/top", map[string]string{"device": "r", "range": "30d"}, &resp)
	if resp.CoverageFrom != nil || len(resp.Rows) != 2 || resp.Rows[0].Value != 0 || len(resp.Rows[0].Sparkline) != 0 {
		t.Fatalf("30d without hourly rows: %+v", resp)
	}

	for params, want := range map[string]int{
		"metric=p99": 400, "dir=up": 400, "range=1y": 400, "device=nope": 200, "limit=abc": 200,
	} {
		q, _ := url.ParseQuery(params)
		p := map[string]string{}
		for k := range q {
			p[k] = q.Get(k)
		}
		var body topPortsResponse
		if code := f.get(t, "/traffic/ports/top", p, &body); code != want {
			t.Errorf("%s: status %d, want %d", params, code, want)
		}
		if params == "device=nope" && (body.Rows == nil || len(body.Rows) != 0) {
			t.Errorf("unknown device: rows = %v, want []", body.Rows)
		}
	}
}

func TestTrafficPortHistory(t *testing.T) {
	f := newTrafficFixture(t)
	f.seedMinutes(t, "r", "eth1-wan", 10, 7_500_000, 750_000, 3*mbps, 2*mbps)

	var h portHistoryResponse
	params := map[string]string{"device": "r", "iface": "eth1-wan", "range": "1h"}
	if code := f.get(t, "/traffic/ports/history", params, &h); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if h.Resolution != "1m" || h.StepSeconds != 60 || !h.From.Equal(f.end.Add(-time.Hour)) || !h.To.Equal(f.end) ||
		h.CoverageFrom == nil || !h.CoverageFrom.Equal(f.end.Add(-10*time.Minute)) {
		t.Fatalf("header = %+v", h)
	}
	if len(h.Points) != 10 || h.Points[0].RxBps != mbps || h.Points[0].TxBps != mbps/10 {
		t.Fatalf("points = %+v", h.Points)
	}
	want := queries.SeriesStats{RxAvg: mbps, TxAvg: mbps / 10, RxMax: 3 * mbps, TxMax: 2 * mbps,
		RxP95: mbps, TxP95: mbps / 10, RxBytes: 75_000_000, TxBytes: 7_500_000}
	if h.Stats != want {
		t.Fatalf("stats = %+v, want %+v", h.Stats, want)
	}

	// 24h: 5-minute points from the covered bucket onward.
	h = portHistoryResponse{}
	f.get(t, "/traffic/ports/history", map[string]string{"device": "r", "iface": "eth1-wan", "range": "24h"}, &h)
	if h.StepSeconds != 300 || len(h.Points) < 2 || len(h.Points) > 3 {
		t.Fatalf("24h: step %d, %d points", h.StepSeconds, len(h.Points))
	}

	// Unknown iface on a known device: zero-filled over the covered span.
	h = portHistoryResponse{}
	f.get(t, "/traffic/ports/history", map[string]string{"device": "r", "iface": "ether99"}, &h)
	if len(h.Points) != 10 || h.Points[3].RxBps != 0 || h.Stats != (queries.SeriesStats{}) {
		t.Fatalf("unknown iface: %+v", h)
	}

	// No hourly rows: null coverage, no points.
	h = portHistoryResponse{}
	f.get(t, "/traffic/ports/history", map[string]string{"device": "r", "iface": "eth1-wan", "range": "7d"}, &h)
	if h.CoverageFrom != nil || h.Points == nil || len(h.Points) != 0 || h.Resolution != "1h" {
		t.Fatalf("7d: %+v", h)
	}

	for p, code := range map[string]int{"device=r": 400, "iface=x": 400, "device=nope&iface=x": 404, "device=r&iface=x&range=2d": 400} {
		q, _ := url.ParseQuery(p)
		m := map[string]string{}
		for k := range q {
			m[k] = q.Get(k)
		}
		if got := f.get(t, "/traffic/ports/history", m, nil); got != code {
			t.Errorf("%s: status %d, want %d", p, got, code)
		}
	}
}

func TestTrafficPortHistoryLive(t *testing.T) {
	f := newTrafficFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i, rx := range []int64{1 * mbps, 2 * mbps, 3 * mbps} {
		at := now.Add(time.Duration(i-3) * time.Second).Format("2006-01-02 15:04:05")
		if _, err := f.db.Exec(`INSERT INTO traffic_samples (device_id, interface_name, rx_bits_per_sec, tx_bits_per_sec, collected_at)
			VALUES ('r', 'eth1-wan', ?, ?, ?)`, rx, rx/2, at); err != nil {
			t.Fatal(err)
		}
	}
	var h portHistoryResponse
	if code := f.get(t, "/traffic/ports/history", map[string]string{"device": "r", "iface": "eth1-wan", "range": "live"}, &h); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if h.Resolution != "1s" || h.StepSeconds != 1 || h.CoverageFrom != nil || h.To.Sub(h.From) != 5*time.Minute {
		t.Fatalf("header = %+v", h)
	}
	if len(h.Points) != 3 || h.Points[0].RxBps != mbps || h.Points[2].RxBps != 3*mbps || !h.Points[0].TS.Before(h.Points[2].TS) {
		t.Fatalf("points not ascending: %+v", h.Points)
	}
	if h.Stats.RxAvg != 2*mbps || h.Stats.RxMax != 3*mbps || h.Stats.RxP95 != 3*mbps || h.Stats.TxAvg != mbps {
		t.Fatalf("stats = %+v", h.Stats)
	}
}

func TestTrafficPortBehind(t *testing.T) {
	f := newTrafficFixture(t)

	var b portBehindResponse
	if code := f.get(t, "/traffic/ports/behind", map[string]string{"device": "s", "iface": "ether7"}, &b); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if b.Role != "access" || b.Neighbor != nil || b.Uplink != nil || b.ClientCount != 1 || b.MACCount != 1 || len(b.Clients) != 1 {
		t.Fatalf("s/ether7 = %+v", b)
	}
	c := b.Clients[0]
	if c.MAC != "BB:00:00:00:00:07" || c.IP != "192.168.78.50" || c.HostName != "printer" || c.VID != 78 || c.Wireless ||
		c.AttachedDeviceID != "s" || c.AttachedIface != "ether7" || c.AttachedDeviceName != "S" || c.LastSeen.IsZero() {
		t.Fatalf("client = %+v", c)
	}
	wantVLANs := []behindVLAN{{VID: 10, Tagged: true}, {VID: 20, Tagged: true}, {VID: 21, Tagged: true}, {VID: 78, Name: "lan78"}}
	if !reflect.DeepEqual(b.VLANs, wantVLANs) {
		t.Fatalf("vlans = %+v, want %+v", b.VLANs, wantVLANs)
	}

	// A downlink lists everything behind it; the managed A2 MAC only counts.
	b = portBehindResponse{}
	f.get(t, "/traffic/ports/behind", map[string]string{"device": "r", "iface": "sfp1"}, &b)
	if b.Role != "downlink" || b.Neighbor == nil || b.Neighbor.DeviceID != "c" || b.Neighbor.Iface != "sfp2" ||
		b.ClientCount != 3 || b.MACCount != 4 || len(b.VLANs) != 0 || b.VLANs == nil {
		t.Fatalf("r/sfp1 = %+v", b)
	}
	var macs []string
	for _, c := range b.Clients {
		macs = append(macs, c.MAC)
	}
	// Named first (printer, laptop.lan sorted by name), then the unnamed one.
	if want := []string{"AA:00:00:00:00:01", "BB:00:00:00:00:07", "AA:00:00:00:00:02"}; !reflect.DeepEqual(macs, want) {
		t.Fatalf("client order = %v, want %v", macs, want)
	}
	if first := b.Clients[0]; first.HostName != "laptop.lan" || !first.Wireless || first.SSID != "home" ||
		first.AttachedDeviceID != "a1" || first.AttachedIface != "wifi1" {
		t.Fatalf("wireless client = %+v", first)
	}

	b = portBehindResponse{}
	f.get(t, "/traffic/ports/behind", map[string]string{"device": "a1", "iface": "wifi1"}, &b)
	if b.Role != "wireless" || len(b.Clients) != 2 || !b.Clients[1].Wireless {
		t.Fatalf("a1/wifi1 = %+v", b)
	}

	b = portBehindResponse{}
	f.get(t, "/traffic/ports/behind", map[string]string{"device": "r", "iface": "eth1-wan"}, &b)
	if b.Role != "wan" || b.Uplink == nil || b.Uplink.NodeID != "internet" || b.Uplink.Label != "Internet" || b.Neighbor != nil {
		t.Fatalf("r/eth1-wan = %+v", b)
	}
	b = portBehindResponse{}
	f.get(t, "/traffic/ports/behind", map[string]string{"device": "c", "iface": "sfp2"}, &b)
	if b.Role != "uplink" || b.Neighbor == nil || b.Neighbor.DeviceID != "r" || b.Neighbor.Iface != "sfp1" || b.Uplink != nil {
		t.Fatalf("c/sfp2 = %+v", b)
	}

	if code := f.get(t, "/traffic/ports/behind", map[string]string{"device": "nope", "iface": "x"}, nil); code != http.StatusNotFound {
		t.Errorf("unknown device: %d", code)
	}
	if code := f.get(t, "/traffic/ports/behind", map[string]string{"device": "r"}, nil); code != http.StatusBadRequest {
		t.Errorf("missing iface: %d", code)
	}
}

func TestSummarizeSnapshot(t *testing.T) {
	p := func(dev, iface, typ string, rx int64) poller.PortRate {
		return poller.PortRate{DeviceID: dev, Iface: iface, Type: typ, RxBps: rx, TxBps: rx / 2}
	}
	snap := poller.PortSnapshot{Ports: []poller.PortRate{
		p("a", "ether1", "ether", 1), p("a", "lan-br", "bridge", 2), p("a", "sfp1", "ether", 3),
		p("b", "ether1", "ether", 4), p("b", "ether2", "ether", 5),
		p("c", "sfp1", "ether", 6), p("c", "wlan1", "wlan", 7),
		p("d", "bridge", "", 8),
	}}
	want := []deviceTrafficSummary{{"a", 2, 1}, {"b", 4, 2}, {"c", 6, 3}, {"d", 8, 4}}
	if got := summarizeSnapshot(snap); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}

func TestExpandVLANIDs(t *testing.T) {
	for in, want := range map[string][]int{
		"10":              {10},
		"10,20-22, 30":    {10, 20, 21, 22, 30},
		"1-4094":          nil,
		"5-3,x,0,4095,7-": nil,
		"":                nil,
	} {
		if got := expandVLANIDs(in); !reflect.DeepEqual(got, want) {
			t.Errorf("expandVLANIDs(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestTrafficHandlersConcurrent exercises the shared role-graph cache under
// -race.
func TestTrafficHandlersConcurrent(t *testing.T) {
	f := newTrafficFixture(t)
	h := f.router()
	paths := []string{"/traffic/ports/roles", "/traffic/ports/latest", "/traffic/ports/top?range=live",
		"/traffic/path?device=a1&iface=wifi1", "/traffic/sankey?range=live", "/traffic/ports/behind?device=r&iface=sfp1"}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d", path, rec.Code)
			}
		}(paths[i%len(paths)])
	}
	wg.Wait()
}

// seedMinuteRows writes one-minute rows for a port at the given offsets (in
// minutes before f.end), each carrying rxBytes.
func (f *trafficFixture) seedMinuteRows(t *testing.T, dev, iface string, rxBytes int64, minutesAgo ...int) {
	t.Helper()
	var rows []queries.PortStatRow
	for _, m := range minutesAgo {
		rows = append(rows, queries.PortStatRow{DeviceID: dev, InterfaceName: iface,
			Bucket: f.end.Add(-time.Duration(m) * time.Minute), RxBpsAvg: rxBytes * 8 / 60, RxBpsMax: rxBytes * 8 / 60,
			RxBytes: rxBytes, Samples: 4})
	}
	if err := queries.InsertPortStats1m(f.db, rows); err != nil {
		t.Fatal(err)
	}
}

// Minutes without a single row fleet-wide are collector downtime: drawn as
// gaps (null rates) and left out of averages and the percentile, while a port
// idle in a covered minute is a real zero.
func TestTrafficHistoryCollectorGaps(t *testing.T) {
	f := newTrafficFixture(t)
	f.seedMinuteRows(t, "r", "eth1-wan", 7_500_000, 10, 9, 8, 7, 6, 3, 2, 1) // down 5 and 4 minutes ago
	f.seedMinuteRows(t, "c", "q1", 7_500_000, 10)

	rec := httptest.NewRecorder()
	f.router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/traffic/ports/history?device=r&iface=eth1-wan&range=1h", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rx_bps":null,"tx_bps":null`) {
		t.Fatalf("history = %d %s, want null rates for the gap", rec.Code, rec.Body.String())
	}
	var h portHistoryResponse
	f.get(t, "/traffic/ports/history", map[string]string{"device": "r", "iface": "eth1-wan", "range": "1h"}, &h)
	if len(h.Points) != 10 {
		t.Fatalf("points = %+v", h.Points)
	}
	for i, p := range h.Points {
		gap := i == 5 || i == 6
		if p.Gap != gap || (!gap && p.RxBps != mbps) {
			t.Errorf("point %d = %+v, want gap=%v", i, p, gap)
		}
	}
	// 8 covered minutes at 1 Mbit/s: the average is not diluted by the gap.
	if h.Stats.RxAvg != mbps || h.Stats.RxP95 != mbps || h.Stats.RxBytes != 60_000_000 {
		t.Errorf("stats = %+v, want avg/p95 1 Mbit/s over the covered minutes", h.Stats)
	}

	// Another port idle in a covered minute: zero, not a gap.
	h = portHistoryResponse{}
	f.get(t, "/traffic/ports/history", map[string]string{"device": "c", "iface": "q1", "range": "1h"}, &h)
	if p := h.Points[1]; p.Gap || p.RxBps != 0 {
		t.Errorf("idle covered minute = %+v, want a zero point", p)
	}
	if h.Points[5].Gap != true {
		t.Errorf("collector-down minute = %+v, want a gap on every port", h.Points[5])
	}

	// Top talkers average over covered seconds too.
	var top topPortsResponse
	f.get(t, "/traffic/ports/top", map[string]string{"range": "1h", "limit": "1"}, &top)
	if top.Rows[0].RxAvg != mbps {
		t.Errorf("top avg = %d, want 1 Mbit/s", top.Rows[0].RxAvg)
	}
}

// A sparkline step that coverage starts inside is divided by its covered
// seconds, not the whole step (no false dip at the start of data).
func TestTrafficSparklinePartialFirstStep(t *testing.T) {
	f := newTrafficFixture(t)
	f.seedMinuteRows(t, "r", "eth1-wan", 7_500_000, 9, 8, 7, 6, 5, 4, 3, 2, 1) // starts mid 2-minute step
	var top topPortsResponse
	f.get(t, "/traffic/ports/top", map[string]string{"range": "1h", "limit": "1"}, &top)
	sp := top.Rows[0].Sparkline
	if len(sp) != 5 || !sp[0].TS.Equal(f.end.Add(-10*time.Minute)) {
		t.Fatalf("sparkline = %+v", sp)
	}
	for i, p := range sp {
		if p.Gap || p.RxBps != mbps {
			t.Errorf("sparkline[%d] = %+v, want 1 Mbit/s", i, p)
		}
	}
}

// On the hourly table, a first hour the collector only covered in part
// starts at its first covered minute: day-one averages are not diluted.
func TestTrafficHourlyFirstHourRefined(t *testing.T) {
	f := newTrafficFixture(t)
	hour := f.end.Truncate(time.Hour).Add(-2 * time.Hour)
	var minutes []int
	for m := 41; m < 60; m++ {
		minutes = append(minutes, int(f.end.Sub(hour.Add(time.Duration(m)*time.Minute))/time.Minute))
	}
	f.seedMinuteRows(t, "r", "eth1-wan", 7_500_000, minutes...)
	if err := queries.RollupPortStats1h(f.db, hour, hour.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.fake.rolled = hour.Add(time.Hour)

	var top topPortsResponse
	f.get(t, "/traffic/ports/top", map[string]string{"range": "7d", "limit": "1"}, &top)
	if top.CoverageFrom == nil || !top.CoverageFrom.Equal(hour.Add(41*time.Minute)) {
		t.Fatalf("coverage_from = %v, want %v", top.CoverageFrom, hour.Add(41*time.Minute))
	}
	if len(top.Rows) != 1 || top.Rows[0].RxAvg != mbps {
		t.Fatalf("7d top = %+v, want 1 Mbit/s over the 19 covered minutes", top.Rows)
	}
	var h portHistoryResponse
	f.get(t, "/traffic/ports/history", map[string]string{"device": "r", "iface": "eth1-wan", "range": "7d"}, &h)
	if len(h.Points) != 1 || h.Points[0].RxBps != mbps || h.Stats.RxAvg != mbps {
		t.Fatalf("7d history = %+v", h)
	}

	// Once port_stats_1m no longer holds the whole hour (retention), the
	// hour counts in full again.
	if _, err := queries.DeleteOldPortStats(f.db, queries.PortStats1m, hour.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	top = topPortsResponse{}
	f.get(t, "/traffic/ports/top", map[string]string{"range": "7d", "limit": "1"}, &top)
	if !top.CoverageFrom.Equal(hour) || top.Rows[0].RxAvg != 316_667 { // 19 minutes' bytes over the full hour
		t.Errorf("after the retention cut: coverage %v avg %d", top.CoverageFrom, top.Rows[0].RxAvg)
	}
}

// A cable between two anchors is a peer link: named on both ends through
// every endpoint, and — carrying traffic — never dropped from top talkers.
func TestTrafficPeerLink(t *testing.T) {
	f := newTrafficFixture(t)
	if err := queries.UpsertInterface(f.db, &queries.Interface{ID: uuid.NewString(), DeviceID: "c", Name: "ether1",
		Type: "ether", MACAddress: ifaceMAC("c", "ether1"), Running: true}); err != nil {
		t.Fatal(err)
	}
	if err := queries.ReplaceGatewayHosts(f.db, []queries.DeviceUplink{{DeviceID: "c", Kind: "gateway-host",
		Interface: "ether1", IfaceType: "ether", GatewayIP: "192.168.1.254"}}); err != nil {
		t.Fatal(err)
	}

	var roles portRolesResponse
	f.get(t, "/traffic/ports/roles", nil, &roles)
	got := map[string]portRoleRow{}
	for _, r := range roles.Roles {
		got[r.DeviceID+"/"+r.Iface] = r
	}
	for key, want := range map[string][2]string{"r/sfp1": {"peer", "↔ C sfp2"}, "c/sfp2": {"peer", "↔ R sfp1"}} {
		if r := got[key]; r.Role != want[0] || r.Behind != want[1] || r.ClientCount != 0 {
			t.Errorf("%s = %+v, want %s %q", key, r, want[0], want[1])
		}
	}

	var b portBehindResponse
	f.get(t, "/traffic/ports/behind", map[string]string{"device": "r", "iface": "sfp1"}, &b)
	if b.Role != "peer" || b.Neighbor == nil || b.Neighbor.DeviceID != "c" || b.Neighbor.Iface != "sfp2" || b.Uplink != nil {
		t.Errorf("behind r/sfp1 = %+v", b)
	}

	var top topPortsResponse
	f.get(t, "/traffic/ports/top", map[string]string{"range": "live", "limit": "200"}, &top)
	found := false
	for _, r := range top.Rows {
		if r.DeviceID == "r" && r.Iface == "sfp1" {
			found = r.Role == "peer" && r.NeighborDeviceID == "c" && r.Value == 104*mbps
		}
	}
	if !found {
		t.Errorf("peer link r/sfp1 missing from physical top talkers: %+v", top.Rows)
	}

	var p trafficPathResponse
	f.get(t, "/traffic/path", map[string]string{"device": "r", "iface": "sfp1"}, &p)
	if p.Role != "peer" || len(p.Hops) != 2 || p.Hops[len(p.Hops)-1].Sink {
		t.Errorf("path r/sfp1 = %+v, want internet → R without a sink", p)
	}
}

// Bond membership read from the device folds members into their bond.
func TestTrafficBondMembers(t *testing.T) {
	f := newTrafficFixture(t)
	for _, spec := range []string{"bond1:bond", "ether8:ether", "ether9:ether"} {
		name, typ, _ := strings.Cut(spec, ":")
		if err := queries.UpsertInterface(f.db, &queries.Interface{ID: uuid.NewString(), DeviceID: "s", Name: name,
			Type: typ, MACAddress: ifaceMAC("s", name), Running: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := queries.UpsertPortHosts(f.db, "s", []queries.PortHost{{InterfaceName: "bond1", MACAddress: "DD:00:00:00:00:01"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := queries.ReplaceInterfaceRelations(f.db, "s", []queries.InterfaceRelation{
		{InterfaceName: "ether8", Kind: "bond", Parent: "bond1"}, {InterfaceName: "ether9", Kind: "bond", Parent: "bond1"},
	}); err != nil {
		t.Fatal(err)
	}
	f.fake.snap.Ports = append(f.fake.snap.Ports,
		poller.PortRate{DeviceID: "s", Iface: "bond1", Type: "bond", Running: true, RxBps: 2 * mbps, TxBps: 30 * mbps},
		poller.PortRate{DeviceID: "s", Iface: "ether8", Type: "ether", Running: true, RxBps: mbps, TxBps: 15 * mbps},
		poller.PortRate{DeviceID: "s", Iface: "ether9", Type: "ether", Running: true, RxBps: mbps, TxBps: 15 * mbps})

	var roles portRolesResponse
	f.get(t, "/traffic/ports/roles", nil, &roles)
	got := map[string]portRoleRow{}
	for _, r := range roles.Roles {
		got[r.DeviceID+"/"+r.Iface] = r
	}
	if r := got["s/bond1"]; r.Role != "access" || r.ClientCount != 1 || r.Behind != "DD:00:00:00:00:01" {
		t.Errorf("s/bond1 = %+v", r)
	}
	if r := got["s/ether8"]; r.Role != "access" || r.Master != "bond1" || r.ClientCount != 0 || r.Behind != "member of bond1 · DD:00:00:00:00:01" {
		t.Errorf("s/ether8 = %+v", r)
	}
	var sk trafficSankeyResponse
	f.get(t, "/traffic/sankey", map[string]string{"range": "live"}, &sk)
	edges := sankeyEdges(sk)
	if edges["s>port:s:bond1"] != 30*mbps || edges["s>port:s:ether8"] != 0 || edges["s>port:s:ether9"] != 0 {
		t.Errorf("sankey = %v, want the bond as the only leaf", edges)
	}
}

// For a device the live snapshot holds, it is the port list: interface rows
// it lacks (removed/renamed on the device, blank names) are ghosts.
func TestTrafficGhostInterfaces(t *testing.T) {
	f := newTrafficFixture(t)
	for _, name := range []string{"", "wifi18"} {
		if err := queries.UpsertInterface(f.db, &queries.Interface{ID: uuid.NewString(), DeviceID: "a1", Name: name,
			Type: "wifi", Running: false}); err != nil {
			t.Fatal(err)
		}
	}
	var top topPortsResponse
	f.get(t, "/traffic/ports/top", map[string]string{"range": "live", "device": "a1", "physical": "0"}, &top)
	var names []string
	for _, r := range top.Rows {
		names = append(names, r.Iface)
	}
	// bridge is still known from a1's fresh default-route row.
	if want := []string{"ether1", "wifi1", "bridge"}; !reflect.DeepEqual(names, want) {
		t.Errorf("a1 ports = %q, want %q (no ghosts)", names, want)
	}
	var roles portRolesResponse
	f.get(t, "/traffic/ports/roles", nil, &roles)
	for _, r := range roles.Roles {
		if r.DeviceID == "a1" && (r.Iface == "" || r.Iface == "wifi18") {
			t.Errorf("ghost role %+v", r)
		}
	}
}

// A device created after the role graph was cached is found on the next
// /traffic/path, not 404 for the rest of the cache TTL.
func TestTrafficTopoCacheRebuildsForNewDevice(t *testing.T) {
	f := newTrafficFixture(t)
	if code := f.get(t, "/traffic/ports/roles", nil, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	if err := queries.CreateDevice(f.db, &queries.Device{ID: "n1", Identity: "new-sw", Address: "10.0.0.77",
		Username: "admin", APIPort: 8728, Status: "online"}); err != nil {
		t.Fatal(err)
	}
	var p trafficPathResponse
	if code := f.get(t, "/traffic/path", map[string]string{"device": "n1"}, &p); code != http.StatusOK ||
		len(p.Hops) != 1 || p.Hops[0].DeviceID != "n1" {
		t.Fatalf("path of a new device = %d %+v", code, p)
	}
	f.s.invalidateTrafficTopo()
	f.s.topoMu.Lock()
	cached := f.s.topo
	f.s.topoMu.Unlock()
	if cached != nil {
		t.Error("invalidateTrafficTopo left the cache in place")
	}
}

// The admin traffic purge also clears the port-stats history.
func TestPurgeHistoryTrafficClearsPortStats(t *testing.T) {
	f := newTrafficFixture(t)
	f.seedMinuteRows(t, "r", "eth1-wan", 1000, 1, 2)
	old := f.end.AddDate(0, 0, -3)
	if err := queries.InsertPortStats1m(f.db, []queries.PortStatRow{{DeviceID: "r", InterfaceName: "eth1-wan", Bucket: old, RxBytes: 1, Samples: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := queries.RollupPortStats1h(f.db, old.Add(-time.Hour), f.end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		var n int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	purge := func(body string) purgeResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		f.s.handlePurgeHistory(rec, httptest.NewRequest(http.MethodPost, "/admin/purge-history", strings.NewReader(body)))
		var resp purgeResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
			t.Fatalf("purge %s = %d %s", body, rec.Code, rec.Body.String())
		}
		return resp
	}
	h1 := count("port_stats_1h")
	resp := purge(`{"traffic": true, "older_than_days": 1}`)
	if resp.Deleted["port_stats_1m"] != 1 || count("port_stats_1m") != 2 || count("port_stats_1h") != h1-1 {
		t.Errorf("older_than_days=1: deleted %v, 1m left %d, 1h left %d", resp.Deleted, count("port_stats_1m"), count("port_stats_1h"))
	}
	resp = purge(`{"traffic": true}`)
	if _, ok := resp.Deleted["port_stats_1h"]; !ok || count("port_stats_1m") != 0 || count("port_stats_1h") != 0 {
		t.Errorf("full purge: deleted %v, 1m %d, 1h %d", resp.Deleted, count("port_stats_1m"), count("port_stats_1h"))
	}
}
