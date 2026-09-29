package flow

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowview"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

func TestBuildPlanLab(t *testing.T) {
	now := tLab
	rb := planAddrsOf("rb", []routeros.DeviceAddress{
		{Address: "100.114.241.213/10", Interface: "eth1-wan"},
		{Address: "192.168.78.202/23", Interface: "bridge"},
		{Address: "192.168.28.202/24", Interface: "net28"},
		{Address: "172.16.28.1/28", Interface: "wg-labnet"},
		{Address: "2a01:4f8:202:13d1:28::202/64", Interface: "net28"},
		{Address: "169.254.1.1/16", Interface: "ether9"},
		{Address: "garbage", Interface: "x"},
	}, map[string]string{"wg-labnet": "wg", "eth1-wan": "ether"})
	sw := planAddrsOf("sw", []routeros.DeviceAddress{{Address: "192.168.78.201/23", Interface: "bridge"}}, nil)
	lte := planAddrsOf("lte", []routeros.DeviceAddress{
		{Address: "192.168.23.2/24", Interface: "lte1"},  // WAN of the LTE router
		{Address: "10.99.0.1/24", Interface: "ether2"},   // a shared WAN below
		{Address: "192.168.111.2/24", Interface: "wg0"},  // vpn here ...
		{Address: "192.168.200.1/24", Interface: "wg1"}}, // vpn, inside an external entry
		map[string]string{"wg0": "wg", "wg1": "wg"})
	other := planAddrsOf("other", []routeros.DeviceAddress{
		{Address: "10.99.0.7/24", Interface: "ether1"},
		{Address: "192.168.111.81/24", Interface: "ether5"}, // ... internal on another device
		{Address: "192.168.200.9/24", Interface: "ether6"}}, nil)
	addrs := append(append(append(rb, sw...), lte...), other...)
	uplinks := []queries.DeviceUplink{
		{DeviceID: "rb", Kind: "default-route", Interface: "eth1-wan", IfaceType: "ether"},
		{DeviceID: "lte", Kind: "default-route", Interface: "lte1", IfaceType: "lte"},
		{DeviceID: "lte", Kind: "default-route", Interface: "ether2", IfaceType: "ether"},
	}
	devAddrs := map[string]netip.Addr{"rb": netip.MustParseAddr("192.168.78.202"), "sw": netip.MustParseAddr("192.168.78.201")}
	external := parsePrefixCSV("192.168.200.0/24")
	plan := buildPlan(addrs, devAddrs, uplinks, external, now)

	got, hosts := map[string]string{}, map[string]string{}
	for _, p := range plan {
		if p.Prefix.IsSingleIP() {
			hosts[p.Prefix.String()] = p.Class + "/" + p.DeviceID
		} else {
			got[p.Prefix.String()] = p.Class
		}
		if !p.UpdatedAt.Equal(now) {
			t.Errorf("%s updated_at %v", p.Prefix, p.UpdatedAt)
		}
	}
	// Every address also gets a host row with its prefix's class (the
	// views' "self" set).
	for h, want := range map[string]string{"100.114.241.213/32": "transit/rb", "192.168.78.202/32": "internal/rb",
		"172.16.28.1/32": "vpn/rb", "2a01:4f8:202:13d1:28::202/128": "internal/rb", "192.168.78.201/32": "internal/sw",
		"192.168.200.9/32": "vpn/other"} {
		if hosts[h] != want {
			t.Errorf("host row %s = %q, want %q", h, hosts[h], want)
		}
	}
	if len(hosts) != len(addrs) { // one per address (link-local and garbage were never addresses)
		t.Errorf("host rows = %d, want %d: %v", len(hosts), len(addrs), hosts)
	}
	want := map[string]string{
		"100.64.0.0/10":          "transit",  // only the RB5009, its WAN
		"192.168.78.0/23":        "internal", // many devices
		"192.168.28.0/24":        "internal",
		"172.16.28.0/28":         "vpn", // wg
		"2a01:4f8:202:13d1::/64": "internal",
		"192.168.23.0/24":        "transit",  // LTE WAN, single device
		"10.99.0.0/24":           "internal", // WAN of lte, but another device has an address inside
		"192.168.111.0/24":       "internal", // vpn on one device, internal on another: internal wins
		"192.168.200.0/24":       "vpn",      // ... unless inside an external entry
	}
	if len(got) != len(want) {
		t.Fatalf("plan = %v", got)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s = %q, want %q", p, got[p], c)
		}
	}
	// A device address of another device inside the WAN prefix also
	// prevents transit.
	plan = buildPlan(rb, map[string]netip.Addr{"x": netip.MustParseAddr("100.100.0.1")}, uplinks, nil, now)
	for _, p := range plan {
		if p.Prefix.String() == "100.64.0.0/10" && p.Class != "internal" {
			t.Fatalf("WAN prefix holding another device's address = %s", p.Class)
		}
	}
}

// LAN clients holding only DHCPv6 /128s (the lab's switches and IoT
// hosts): the LAN /64 must be learned as internal, a WAN DHCPv6 /128 stays a
// transit /64, and a LAN client's default route via the LAN router is no
// uplink (its addresses never become transit, even a lone v6 /64).
func TestBuildPlanIPv6HostLengthLAN(t *testing.T) {
	rb := planAddrsOf("rb", []routeros.DeviceAddress{
		{Address: "192.168.78.202/23", Interface: "bridge"},
		{Address: "100.114.241.213/10", Interface: "eth1-wan"},
		{Address: "2a0d:3341:ee00:9c4d::1234/128", Interface: "eth1-wan"},
		{Address: "2a01:4f8:202:13d1:80::1/80", Interface: "pool80"},
	}, nil)
	sw := planAddrsOf("sw012", []routeros.DeviceAddress{
		{Address: "192.168.78.19/23", Interface: "bridge"},
		{Address: "2a01:4f8:202:13d1:78::2019/128", Interface: "bridge"},
	}, nil)
	iot := planAddrsOf("iot", []routeros.DeviceAddress{{Address: "2a01:4f8:202:13d1:78::202a/128", Interface: "ether1"}}, nil)
	lone := planAddrsOf("lone", []routeros.DeviceAddress{
		{Address: "192.168.78.30/23", Interface: "bridge"},
		{Address: "2a01:4f8:202:99::30/128", Interface: "bridge"}, // a /64 no other device holds
	}, nil)
	uplinks := []queries.DeviceUplink{
		{DeviceID: "rb", Kind: "default-route", Interface: "eth1-wan", IfaceType: "ether", GatewayIP: "100.64.0.1"},
		{DeviceID: "sw012", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.168.78.81"},
		{DeviceID: "lone", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.168.78.81%bridge"},
	}
	addrs := slices.Concat(rb, sw, iot, lone)
	got := map[string]string{}
	for _, p := range buildPlan(addrs, nil, uplinks, nil, tLab) {
		got[p.Prefix.String()] = p.Class
	}
	for p, want := range map[string]string{
		"2a01:4f8:202:13d1::/64":         "internal",
		"2a01:4f8:202:13d1:78::2019/128": "internal", // host row, not transit
		"2a0d:3341:ee00:9c4d::/64":       "transit",
		"2a01:4f8:202:13d1:80::/80":      "internal", // explicit lengths kept
		"2a01:4f8:202:99::/64":           "internal",
		"192.168.78.0/23":                "internal",
		"100.64.0.0/10":                  "transit",
	} {
		if got[p] != want {
			t.Errorf("%s = %q, want %q", p, got[p], want)
		}
	}
	// The views: a LAN GUA is internal, so a local_internal point keeps
	// its IPv6 conversations.
	var rows []queries.FlowPrefix
	for _, p := range buildPlan(addrs, nil, uplinks, nil, tLab) {
		rows = append(rows, p)
	}
	pl := flowview.NewPlan(rows, nil, nil, nil, nil)
	for a, want := range map[string]string{"2a01:4f8:202:13d1:78::60": "internal", "2620:fe::fe": "external",
		"2a0d:3341:ee00:9c4d::1": "external"} {
		if c := pl.Class(netip.MustParseAddr(a)); c != want {
			t.Errorf("class(%s) = %s, want %s", a, c, want)
		}
	}
	pt := flowview.Point{FlowPoint: queries.FlowPoint{IfIndexes: []uint32{1}, Facing: "down", LocalInternal: true}}
	in := []queries.FlowAgg{
		{InIf: 1, OutIf: 7, Src: netip.MustParseAddr("192.168.78.60"), Dst: netip.MustParseAddr("9.9.9.9"), Bytes: 1000},
		{InIf: 1, OutIf: 7, Src: netip.MustParseAddr("2a01:4f8:202:13d1:78::60"), Dst: netip.MustParseAddr("2620:fe::fe"), Bytes: 2000},
	}
	if n := flowview.Totals(flowview.Select(in, pt, flowview.Upload, pl)); n != 3000 {
		t.Errorf("upload bytes kept = %d, want 3000 (IPv6 LAN row dropped)", n)
	}
}

func TestNativeFacingRules(t *testing.T) {
	rb := queries.FlowExporter{ID: 1, Name: "rb", Address: "192.168.78.202", Kind: "routeros", DeviceID: "rb"}
	fw := queries.FlowExporter{ID: 2, Name: "firewall003", Address: "192.168.78.81", Kind: "opnsense"}
	ctx := &pointContext{
		exporters: map[int64]queries.FlowExporter{1: rb, 2: fw},
		ifaces: map[int64]map[uint32]queries.FlowIface{
			1: {1: {Name: "eth1-wan", Type: "ether"}, 13: {Name: "net28", Type: "vlan"}, 10: {Name: "bridge", Type: "bridge"},
				15: {Name: "wg-labnet", Type: "wg"}, 20: {Name: "lte1"}, 21: {Name: "ovpn1"}, 22: {Name: "ether5"}},
			2: {1: {Hint: "192.168.78.0/24,192.168.79.0/24"}, 5: {Hint: "9.9.9.0/24,142.250.203.0/24"},
				6: {Hint: "10.20.30.0/24"}, 7: {}, 8: {Hint: "8.8.8.0/24", Role: "lan"}, 9: {Hint: "192.168.78.0/24", Role: "wan"},
				10: {Hint: "8.8.4.0/24", Role: "other"}, 11: {Role: "vpn"}},
		},
		uplinks: []queries.DeviceUplink{
			{DeviceID: "rb", Kind: "default-route", Interface: "eth1-wan", IfaceType: "ether"},
			{DeviceID: "rb", Kind: "default-route", Interface: "ovpn1", IfaceType: "ovpn-client"}, // VPN default route: via (c)
			{DeviceID: "rb", Kind: "vpn", Interface: "ovpn1"},
			{DeviceID: "rb", Kind: "gateway-host", Interface: "ether5", GatewayIP: "192.168.78.81"},
			{DeviceID: "other", Kind: "default-route", Interface: "net28"},
		},
		ifTypes:  map[string]map[string]string{"rb": {"lte1": "lte"}},
		internal: parsePrefixCSV("10.20.0.0/16"),
	}
	cases := []struct {
		exp    queries.FlowExporter
		idx    uint32
		name   string
		facing string
	}{
		{rb, 1, "rb · eth1-wan", "up"}, // (a) default route
		{rb, 13, "rb · net28", "down"}, // another device's default route
		{rb, 10, "rb · bridge", "down"},
		{rb, 15, "rb · wg-labnet", "up"}, // (c) VPN type
		{rb, 20, "rb · lte1", "down"},    // lte is not a VPN type and no uplink
		{rb, 21, "rb · ovpn1", "up"},     // (b) vpn uplink
		{rb, 22, "rb · ether5", "up"},    // (d) gateway-host
		{rb, 99, "rb · if#99", "down"},   // unknown name
		{fw, 1, "firewall003 · if#1", "down"},
		{fw, 5, "firewall003 · if#5", "up"},
		{fw, 6, "firewall003 · if#6", "down"}, // overlaps the internal plan
		{fw, 7, "firewall003 · if#7", "down"}, // no hint yet
		{fw, 8, "firewall003 · if#8", "down"}, // role lan
		{fw, 9, "firewall003 · if#9", "up"},   // role wan wins over the hint
		{fw, 10, "firewall003 · if#10", "down"},
		{fw, 11, "firewall003 · if#11", "up"},
	}
	for _, c := range cases {
		p := ctx.nativeSpec(c.exp, c.idx)
		if p.Name != c.name || p.Facing != c.facing {
			t.Errorf("%s/%d = %q %s, want %q %s", c.exp.Name, c.idx, p.Name, p.Facing, c.name, c.facing)
		}
		if c.exp.DeviceID != "" && (p.PortSide != "same" || p.DeviceID != "rb" || p.LocalInternal) {
			t.Errorf("device point %+v", p)
		}
		if c.exp.DeviceID == "" && (p.PortSide != "" || p.DeviceID != "" || !p.LocalInternal) {
			t.Errorf("OPNsense point %+v", p)
		}
		if p.Kind != "native" || !p.Auto || !p.Enabled || p.ExcludeAttach == nil || len(p.IfIndexes) != 1 {
			t.Errorf("point shape %+v", p)
		}
	}
}

func TestSweep(t *testing.T) {
	db := testDB(t)
	now := tLab
	keep := seedExporter(t, db, queries.FlowExporter{Name: "a", Address: "10.0.0.1", Kind: "other"})
	gone := seedExporter(t, db, queries.FlowExporter{Name: "b", Address: "10.0.0.2", Kind: "other"})
	row := func(e int64, b time.Time) queries.FlowRow {
		return queries.FlowRow{ExporterID: e, Bucket: b, InIf: 1, OutIf: 2, Src: rosAddr, Dst: opnAddr, Bytes: 1}
	}
	old1m, new1m := now.AddDate(0, 0, -4), now.Add(-time.Hour)
	_ = queries.InsertFlows1m(db, []queries.FlowRow{row(keep, old1m), row(keep, new1m), row(gone, new1m)},
		[]queries.FlowMeta{{ExporterID: keep, Bucket: old1m}, {ExporterID: keep, Bucket: new1m}, {ExporterID: gone, Bucket: new1m}})
	_ = queries.RollupFlows1h(db, keep, old1m, 10)
	_ = queries.RollupFlows1h(db, keep, new1m, 10)
	_, _ = db.Exec(`UPDATE flow_1h SET bucket = ? WHERE bucket = ?`, now.AddDate(0, 0, -100).Unix(), old1m.Truncate(time.Hour).Unix())
	_, _ = db.Exec(`UPDATE flow_1h_meta SET bucket = ? WHERE bucket = ?`, now.AddDate(0, 0, -100).Unix(), old1m.Truncate(time.Hour).Unix())
	_ = queries.DeleteFlowExporter(db, gone)
	_ = queries.UpsertFlowTemplate(db, queries.FlowTemplate{ExporterID: keep, Version: 9, TemplateID: 256, Fields: []byte{0, 0}, UpdatedAt: now.AddDate(0, 0, -8)})
	_ = queries.UpsertFlowTemplate(db, queries.FlowTemplate{ExporterID: keep, Version: 9, TemplateID: 259, Fields: []byte{0, 0}, UpdatedAt: now.AddDate(0, 0, -1)})
	_ = queries.TouchFlowIfaces(db, keep, []queries.FlowIfaceSeen{{IfIndex: 3}}, now.AddDate(0, 0, -31))
	_ = queries.TouchFlowIfaces(db, keep, []queries.FlowIfaceSeen{{IfIndex: 4}}, now.AddDate(0, 0, -8))
	_ = queries.TouchFlowIfaces(db, keep, []queries.FlowIfaceSeen{{IfIndex: 1}}, now)
	for _, idx := range []uint32{1, 4} {
		_, _ = queries.EnsureNativeFlowPoint(db, &queries.FlowPoint{ExporterID: keep, Name: "p", IfIndexes: []uint32{idx}, Facing: "down"})
	}
	_, _ = db.Exec(`UPDATE flow_points SET created_at = ?`, now.AddDate(0, 0, -20).Unix())

	Sweep(db, now)
	for table, want := range map[string]int{"flow_1m": 1, "flow_1m_meta": 1, "flow_1h": 1, "flow_1h_meta": 1,
		"flow_templates": 1, "flow_exporter_ifaces": 2, "flow_points": 1} {
		if n := countTable(t, db, table); n != want {
			t.Errorf("%s = %d rows after sweep, want %d", table, n, want)
		}
	}
}

// A plan built while an answering device's uplink rows are stale (restart
// after a longer downtime) must be retried soon: those rows decide transit.
func TestStaleUplinks(t *testing.T) {
	wan := queries.DeviceUplink{DeviceID: "rb", Kind: "default-route", Interface: "eth1-wan"}
	vpn := queries.DeviceUplink{DeviceID: "rb", Kind: "vpn", Interface: "wg-labnet"}
	gw := queries.DeviceUplink{DeviceID: "sw", Kind: "gateway-host", Interface: "ether1"}
	for _, tc := range []struct {
		name       string
		answered   []string
		fresh, all []queries.DeviceUplink
		want       bool
	}{
		{"fresh", []string{"rb"}, []queries.DeviceUplink{wan}, []queries.DeviceUplink{wan, vpn}, false},
		{"stale", []string{"rb"}, nil, []queries.DeviceUplink{wan, vpn}, true},
		{"stale device did not answer", []string{"sw"}, nil, []queries.DeviceUplink{wan}, false},
		{"no uplinks at all", []string{"rb", "sw"}, nil, nil, false},
		{"gateway-host rows do not count", []string{"sw"}, nil, []queries.DeviceUplink{gw}, false},
	} {
		answered := map[string]bool{}
		for _, id := range tc.answered {
			answered[id] = true
		}
		if got := staleUplinks(answered, tc.fresh, tc.all); got != tc.want {
			t.Errorf("%s: staleUplinks = %v, want %v", tc.name, got, tc.want)
		}
	}
}
