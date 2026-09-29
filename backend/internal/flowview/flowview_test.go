package flowview

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func TestDirectionsTruthTable(t *testing.T) {
	cases := []struct {
		facing        string
		in, out       uint32
		wantDown, wUp bool
	}{
		// S = {10, 13}
		{"up", 10, 1, true, false},   // in ∈ S
		{"up", 1, 13, false, true},   // out ∈ S
		{"up", 10, 13, true, true},   // both (hairpin)
		{"up", 1, 2, false, false},   // neither
		{"down", 10, 1, false, true}, // in ∈ S → upload
		{"down", 1, 13, true, false}, // out ∈ S → download
		{"down", 13, 10, true, true}, // both
		{"down", 0, 0, false, false}, // 0 is never in S
	}
	for _, c := range cases {
		p := Point{FlowPoint: queries.FlowPoint{IfIndexes: []uint32{10, 13}, Facing: c.facing}}
		down, up := p.Directions(c.in, c.out)
		if down != c.wantDown || up != c.wUp {
			t.Errorf("%s in=%d out=%d: down=%v up=%v, want %v %v", c.facing, c.in, c.out, down, up, c.wantDown, c.wUp)
		}
	}
	// ifIndex 0 in the stored set is never matched either.
	p := Point{FlowPoint: queries.FlowPoint{IfIndexes: []uint32{0, 1}, Facing: "up"}}
	if down, up := p.Directions(0, 0); down || up {
		t.Errorf("ifIndex 0 matched: %v %v", down, up)
	}
}

func labPlan(self ...string) *Plan {
	rows := []queries.FlowPrefix{
		{Prefix: pfx("100.64.0.0/10"), Class: "transit", DeviceID: "rb", Iface: "eth1-wan"},
		{Prefix: pfx("192.168.78.0/23"), Class: "internal", DeviceID: "rb", Iface: "bridge"},
		{Prefix: pfx("192.168.28.0/24"), Class: "internal", DeviceID: "rb", Iface: "net28"},
		{Prefix: pfx("172.16.28.0/28"), Class: "vpn", DeviceID: "rb", Iface: "wg-labnet"},
		{Prefix: pfx("2a01:4f8:202:13d1:28::/80"), Class: "internal", DeviceID: "rb", Iface: "net28"},
	}
	known := map[netip.Addr]bool{ip("100.64.0.1"): true, ip("192.168.28.33"): true, ip("10.99.0.5"): true}
	s := map[netip.Addr]bool{}
	for _, a := range self {
		s[ip(a)] = true
	}
	return NewPlan(rows, nil, nil, known, s)
}

func TestPlanClassLab(t *testing.T) {
	pl := labPlan("192.168.78.202")
	for addr, want := range map[string]string{
		"100.64.0.1":                       ClassExternal, // plan (transit) beats known host
		"100.114.241.213":                  ClassExternal,
		"192.168.28.33":                    ClassInternal,
		"1.1.1.1":                          ClassExternal,
		"192.168.78.202":                   ClassSelf,
		"172.16.28.2":                      ClassVPN,
		"10.99.0.5":                        ClassInternal, // known host outside the plan
		"10.99.0.6":                        ClassExternal, // RFC 1918 is not a blanket rule
		"224.0.0.251":                      ClassMulticast,
		"ff02::fb":                         ClassMulticast,
		"255.255.255.255":                  ClassMulticast,
		"192.168.79.255":                   ClassMulticast, // broadcast of the internal /23
		"192.168.28.255":                   ClassMulticast,
		"100.127.255.255":                  ClassExternal, // broadcast of a transit prefix is not special
		"2a01:4f8:202:13d1:28::33":         ClassInternal,
		"2606:4700::1111":                  ClassExternal,
		"::ffff:192.168.28.33":             ClassInternal, // v4-mapped is unmapped
		"::ffff:192.168.78.202":            ClassSelf,
		"2a01:4f8:202:13d1:28:ffff:ffff:1": ClassInternal,
	} {
		if got := pl.Class(ip(addr)); got != want {
			t.Errorf("Class(%s) = %s, want %s", addr, got, want)
		}
	}
	if got := pl.Class(netip.Addr{}); got != ClassExternal {
		t.Errorf("invalid addr = %s", got)
	}
	if got := (*Plan)(nil).Class(ip("192.168.28.33")); got != ClassExternal {
		t.Errorf("nil plan = %s", got)
	}
	if got := (*Plan)(nil).Class(ip("224.0.0.1")); got != ClassMulticast {
		t.Errorf("nil plan multicast = %s", got)
	}
}

func TestPlanClassOrder(t *testing.T) {
	rows := []queries.FlowPrefix{
		{Prefix: pfx("192.168.0.0/16"), Class: "internal"},
		{Prefix: pfx("192.168.23.0/24"), Class: "transit"}, // more specific wins
	}
	internal := ParsePrefixList("192.168.23.7, 100.64.9.0/24")
	external := ParsePrefixList("192.168.50.0/24,192.168.23.7/32,junk")
	known := map[netip.Addr]bool{ip("192.168.23.9"): true}
	self := map[netip.Addr]bool{ip("192.168.50.1"): true}
	pl := NewPlan(rows, internal, external, known, self)
	for addr, want := range map[string]string{
		"192.168.50.1": ClassSelf,     // self beats the external setting
		"192.168.50.2": ClassExternal, // external setting beats the internal plan row
		"192.168.23.7": ClassExternal, // external setting beats the internal setting
		"100.64.9.1":   ClassInternal, // internal setting beats nothing-in-plan
		"192.168.23.9": ClassExternal, // most specific plan row (transit) beats known host
		"192.168.1.1":  ClassInternal,
	} {
		if got := pl.Class(ip(addr)); got != want {
			t.Errorf("Class(%s) = %s, want %s", addr, got, want)
		}
	}
	// WithSelf swaps only the self set.
	other := pl.WithSelf(map[netip.Addr]bool{ip("192.168.1.1"): true})
	if other.Class(ip("192.168.1.1")) != ClassSelf || other.Class(ip("192.168.50.1")) != ClassExternal {
		t.Errorf("WithSelf: %s %s", other.Class(ip("192.168.1.1")), other.Class(ip("192.168.50.1")))
	}
	if pl.Class(ip("192.168.1.1")) != ClassInternal {
		t.Error("WithSelf modified the base plan")
	}
}

func TestParsePrefixList(t *testing.T) {
	got := ParsePrefixList(" 10.0.0.0/8,\n192.168.1.7 ; 2001:db8::1, junk, 300.1.1.1/24, ::ffff:10.1.2.0/120\t172.16.5.9/16")
	want := []netip.Prefix{pfx("10.0.0.0/8"), pfx("192.168.1.7/32"), pfx("2001:db8::1/128"), pfx("10.1.2.0/24"), pfx("172.16.0.0/16")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParsePrefixList = %v, want %v", got, want)
	}
	if got := ParsePrefixList(""); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
}

func agg(in, out uint32, src, dst string, proto uint8, port uint16, bytes int64) queries.FlowAgg {
	a := queries.FlowAgg{InIf: in, OutIf: out, Proto: proto, Port: port, Bytes: bytes, Packets: bytes / 100, Flows: 1}
	if src != "" {
		a.Src = ip(src)
	}
	if dst != "" {
		a.Dst = ip(dst)
	}
	return a
}

func TestSelectFiltersAndOther(t *testing.T) {
	rows := []queries.FlowAgg{
		agg(7, 1, "9.9.9.9", "192.168.78.60", 6, 853, 1000),      // download to an internal host
		agg(1, 7, "192.168.78.60", "9.9.9.9", 6, 853, 200),       // upload
		agg(7, 1, "9.9.9.9", "192.168.78.81", 6, 853, 300),       // local = the exporter itself (self)
		agg(7, 1, "9.9.9.9", "192.168.28.25", 6, 443, 400),       // local excluded by ExcludeIPs
		agg(7, 1, "", "", 0, 0, 50),                              // folded other: never filtered
		agg(7, 3, "9.9.9.9", "192.168.78.60", 6, 853, 99999),     // neither direction
		agg(7, 1, "8.8.8.8", "::ffff:192.168.78.61", 17, 53, 10), // v4-mapped local
	}
	pl := labPlan("192.168.78.81")
	p := Point{FlowPoint: queries.FlowPoint{IfIndexes: []uint32{1}, Facing: "down", LocalInternal: true},
		ExcludeIPs: map[netip.Addr]bool{ip("192.168.28.25"): true}}

	down := Select(rows, p, Download, pl)
	var got []int64
	for _, r := range down {
		got = append(got, r.Bytes)
		if !r.Other && (r.Local != r.Dst || r.Remote != r.Src) {
			t.Errorf("download local/remote = %v/%v for %v→%v", r.Local, r.Remote, r.Src, r.Dst)
		}
	}
	if want := []int64{1000, 50, 10}; !reflect.DeepEqual(got, want) {
		t.Fatalf("download bytes = %v, want %v", got, want)
	}
	if !down[1].Other || down[1].Src.IsValid() || down[1].Local.IsValid() {
		t.Errorf("other row = %+v", down[1])
	}

	up := Select(rows, p, Upload, pl)
	if len(up) != 1 || up[0].Bytes != 200 || up[0].Local != ip("192.168.78.60") || up[0].Remote != ip("9.9.9.9") {
		t.Fatalf("upload = %+v", up)
	}

	// Without local_internal / exclude everything in direction stays.
	p.LocalInternal, p.ExcludeIPs = false, nil
	if n := len(Select(rows, p, Download, nil)); n != 5 {
		t.Fatalf("unfiltered download rows = %d, want 5", n)
	}
	// A vpn local endpoint passes local_internal.
	p.LocalInternal = true
	vpn := Select([]queries.FlowAgg{agg(7, 1, "1.1.1.1", "172.16.28.2", 6, 443, 5)}, p, Download, pl)
	if len(vpn) != 1 {
		t.Fatalf("vpn local dropped")
	}
}

func dirRows() []DirRow {
	mk := func(src, dst string, proto uint8, port uint16, bytes int64) DirRow {
		return DirRow{Src: ip(src), Dst: ip(dst), Local: ip(dst), Remote: ip(src), Proto: proto, Port: port,
			Bytes: bytes, Packets: 1, Flows: 1}
	}
	return []DirRow{
		mk("1.1.1.1", "192.168.28.33", 6, 443, 500),
		mk("1.1.1.1", "192.168.28.33", 17, 443, 300),
		mk("1.1.1.1", "192.168.28.25", 6, 443, 200),
		mk("8.8.8.8", "192.168.28.33", 17, 53, 100),
		mk("9.9.9.9", "192.168.28.25", 6, 853, 100),
		{Other: true, Bytes: 40, Packets: 2, Flows: 3},
	}
}

func TestGroupAllGroupings(t *testing.T) {
	cases := []struct {
		by       string
		keys     []string
		bytes    []int64
		otherB   int64
		first    GroupRow
		firstSet func(GroupRow) bool
	}{
		{by: "src", keys: []string{"1.1.1.1", "8.8.8.8"}, bytes: []int64{1000, 100}, otherB: 140,
			firstSet: func(g GroupRow) bool { return g.Src == ip("1.1.1.1") && !g.Dst.IsValid() && g.Port == 0 }},
		{by: "dst", keys: []string{"192.168.28.33", "192.168.28.25"}, bytes: []int64{900, 300}, otherB: 40,
			firstSet: func(g GroupRow) bool { return g.Dst == ip("192.168.28.33") && !g.Src.IsValid() }},
		{by: "pair", keys: []string{"1.1.1.1|192.168.28.33", "1.1.1.1|192.168.28.25"}, bytes: []int64{800, 200}, otherB: 240,
			firstSet: func(g GroupRow) bool { return g.Src.IsValid() && g.Dst.IsValid() && g.Proto == 0 }},
		{by: "conv", keys: []string{"1.1.1.1|192.168.28.33|6/443", "1.1.1.1|192.168.28.33|17/443"}, bytes: []int64{500, 300}, otherB: 440,
			firstSet: func(g GroupRow) bool { return g.Src.IsValid() && g.Dst.IsValid() && g.Proto == 6 && g.Port == 443 }},
		{by: "app", keys: []string{"6/443", "17/443"}, bytes: []int64{700, 300}, otherB: 240,
			firstSet: func(g GroupRow) bool { return !g.Src.IsValid() && !g.Dst.IsValid() && g.Proto == 6 && g.Port == 443 }},
	}
	for _, c := range cases {
		top, other, total := Group(dirRows(), c.by, 2)
		if total != 1240 {
			t.Errorf("%s: total %d", c.by, total)
		}
		var keys []string
		var bytes []int64
		for _, g := range top {
			keys = append(keys, g.Key)
			bytes = append(bytes, g.Bytes)
		}
		if !reflect.DeepEqual(keys, c.keys) || !reflect.DeepEqual(bytes, c.bytes) {
			t.Errorf("%s: top %v %v, want %v %v", c.by, keys, bytes, c.keys, c.bytes)
		}
		if other.Bytes != c.otherB || other.Key != "" {
			t.Errorf("%s: other %+v, want %d", c.by, other, c.otherB)
		}
		if !c.firstSet(top[0]) {
			t.Errorf("%s: fields of %+v", c.by, top[0])
		}
		var sum int64
		for _, g := range top {
			sum += g.Bytes
		}
		if sum+other.Bytes != total {
			t.Errorf("%s: top+other %d != total %d", c.by, sum+other.Bytes, total)
		}
	}
	// Ties sort by key ascending; limit <= 0 keeps everything; the folded
	// other row carries its packets/flows.
	top, other, _ := Group(dirRows(), "src", 0)
	if len(top) != 3 || top[1].Key != "8.8.8.8" || top[2].Key != "9.9.9.9" || other.Bytes != 40 || other.Flows != 3 {
		t.Fatalf("unlimited src: %+v other %+v", top, other)
	}
	if top, _, total := Group(nil, "pair", 5); top == nil || len(top) != 0 || total != 0 {
		t.Fatalf("empty: %v %d", top, total)
	}
}

func TestCounterBytesLab(t *testing.T) {
	for _, c := range []struct {
		name, facing, side string
		downIsTx           bool
	}{
		{"RB5009 eth1-wan", "up", "same", false},
		{"switch001 ether1", "down", "peer", false},
		{"RB5009 net28", "down", "same", true},
		{"switch001 sfp-sfpplus2", "down", "peer", false},
		{"far end of an up point", "up", "peer", true},
	} {
		if got := CounterDownIsTx(c.facing, c.side); got != c.downIsTx {
			t.Errorf("%s: downIsTx = %v", c.name, got)
		}
		down, up := CounterBytes(c.facing, c.side, 10, 20)
		if (c.downIsTx && (down != 20 || up != 10)) || (!c.downIsTx && (down != 10 || up != 20)) {
			t.Errorf("%s: CounterBytes = %d/%d", c.name, down, up)
		}
	}
}

func TestExpandVLANIDs(t *testing.T) {
	if got := ExpandVLANIDs("6,28, 10-12,2-4094,x,0,4095"); !reflect.DeepEqual(got, []int{6, 28, 10, 11, 12}) {
		t.Fatalf("ExpandVLANIDs = %v", got)
	}
}

// ---------- suggestions ----------

const (
	rbID = "c7a3774f-9d5e-489a-b7dd-6a8ebb9fc119"
	swID = "0bf4fc44-0dd5-4a7f-9823-41a314e61e4d"
)

func labSuggestInput() SuggestInput {
	now := time.Now().UTC()
	return SuggestInput{
		Exporters: []queries.FlowExporter{
			{ID: 1, Name: "switch002-rb5009", Address: "192.168.78.202", Kind: "routeros", DeviceID: rbID, Enabled: true},
			{ID: 2, Name: "firewall003", Address: "192.168.78.81", Kind: "opnsense", Enabled: true},
		},
		Ifaces: []queries.FlowIface{
			{ExporterID: 1, IfIndex: 1, Name: "eth1-wan", Type: "ether", Source: "device"},
			{ExporterID: 1, IfIndex: 3, Name: "eth3", Type: "ether", Source: "device"},
			{ExporterID: 1, IfIndex: 9, Name: "sfp-sfpplus1", Type: "ether", Source: "device"},
			{ExporterID: 1, IfIndex: 10, Name: "bridge", Type: "bridge", Source: "device"},
			{ExporterID: 1, IfIndex: 12, Name: "management", Type: "vlan", VLANID: 6, Parent: "bridge", Source: "device"},
			{ExporterID: 1, IfIndex: 13, Name: "net28", Type: "vlan", VLANID: 28, Parent: "bridge", Source: "device"},
			{ExporterID: 1, IfIndex: 15, Name: "wg-labnet", Type: "wg", Source: "device"},
			{ExporterID: 1, IfIndex: 787, Name: "net88", Type: "vlan", VLANID: 88, Parent: "bridge", Source: "device"},
			{ExporterID: 2, IfIndex: 1, Source: "learned", Hint: "192.168.78.0/24,192.168.79.0/24", LastSeen: now},
			{ExporterID: 2, IfIndex: 7, Source: "learned", Hint: "", LastSeen: now},
		},
		Uplinks: []queries.DeviceUplink{
			{DeviceID: rbID, Kind: "default-route", Interface: "eth1-wan", IfaceType: "ether", GatewayIP: "100.64.0.1"},
			{DeviceID: swID, Kind: "gateway-host", Interface: "ether1", IfaceType: "ether", GatewayIP: "192.168.78.81"},
		},
		VLANs: []queries.BridgeVLAN{
			{DeviceID: rbID, BridgeName: "bridge", VLANIDs: "1", CurrentUntagged: "bridge,sfp-sfpplus1"},
			{DeviceID: rbID, BridgeName: "bridge", VLANIDs: "28", CurrentTagged: "sfp-sfpplus1", CurrentUntagged: "eth3"},
			{DeviceID: rbID, BridgeName: "bridge", VLANIDs: "6,28", CurrentTagged: "bridge"},
			{DeviceID: rbID, BridgeName: "bridge", VLANIDs: "88"},
			// switch001's own table must not leak into the RB5009 trunk.
			{DeviceID: swID, BridgeName: "bridge", VLANIDs: "28", CurrentTagged: "sfp-sfpplus2", CurrentUntagged: "ether12"},
		},
		Hosts: []queries.PortHost{
			{DeviceID: swID, InterfaceName: "ether1", MACAddress: "00:03:2d:21:fb:4c"},
			{DeviceID: rbID, InterfaceName: "eth3", MACAddress: "78:9A:18:00:00:25"},
		},
		MACByIP: map[netip.Addr]string{ip("192.168.78.81"): "00:03:2D:21:FB:4C"},
		Devices: []queries.Device{
			{ID: rbID, Identity: "switch002-rb5009", Address: "192.168.78.202"},
			{ID: swID, Identity: "switch001", Address: "192.168.78.201"},
		},
		Adjacent: []Adjacency{
			{DevA: rbID, IfA: "sfp-sfpplus1", DevB: swID, IfB: "sfp-sfpplus2"},
			{DevA: swID, IfA: "sfp-sfpplus2", DevB: rbID, IfB: "sfp-sfpplus1"},
			// A non-physical "adjacency" never yields a trunk view.
			{DevA: rbID, IfA: "bridge", DevB: swID, IfB: "bridge"},
		},
		IfaceTypes: map[string]string{rbID + "\x00eth3": "ether", rbID + "\x00bridge": "bridge"},
	}
}

func TestSuggestLab(t *testing.T) {
	in := labSuggestInput()
	got := Suggest(in)
	if len(got) != 3 {
		t.Fatalf("suggestions = %d: %+v", len(got), got)
	}

	g := got[0]
	if g.Rule != RuleGatewayHost || g.Exists || g.Point == nil {
		t.Fatalf("rule G = %+v", g)
	}
	wantG := queries.FlowPoint{ExporterID: 2, Name: "switch001 · ether1", Kind: "derived", IfIndexes: []uint32{1},
		Facing: "down", PortSide: "peer", DeviceID: swID, Iface: "ether1", LocalInternal: true,
		ExcludeAttach: []queries.PortRef{}, Enabled: true,
		Note: "Derived from firewall003 NetFlow on its LAN interface: switch001 cannot export flows, and firewall003 is the only device on this port, so this is the port's IP traffic. Non-IP frames (ARP, LLDP, STP) are not included."}
	if !reflect.DeepEqual(*g.Point, wantG) {
		t.Fatalf("rule G point =\n%+v\nwant\n%+v", *g.Point, wantG)
	}

	noteT := "Derived from switch002-rb5009 IPFIX on bridge, net28: routed and router-terminated traffic crossing this trunk. L2-switched VLAN traffic that switch002-rb5009 does not route is invisible; hosts on switch002-rb5009 eth3 are excluded."
	excl := []queries.PortRef{{DeviceID: rbID, Iface: "eth3"}}
	for i, want := range []queries.FlowPoint{
		{ExporterID: 1, Name: "switch001 · sfp-sfpplus2", Kind: "derived", IfIndexes: []uint32{10, 13}, Facing: "down",
			PortSide: "peer", DeviceID: swID, Iface: "sfp-sfpplus2", ExcludeAttach: excl, Note: noteT, Enabled: true},
		{ExporterID: 1, Name: "switch002-rb5009 · sfp-sfpplus1", Kind: "derived", IfIndexes: []uint32{10, 13}, Facing: "down",
			PortSide: "same", DeviceID: rbID, Iface: "sfp-sfpplus1", ExcludeAttach: excl, Note: noteT, Enabled: true},
	} {
		s := got[1+i]
		if s.Rule != RuleTrunk || s.Point == nil || !reflect.DeepEqual(*s.Point, want) {
			t.Fatalf("rule T #%d =\n%+v\nwant\n%+v", i, s.Point, want)
		}
	}

	// A role=lan iface wins over hints; another MAC on the port extends the
	// note; existing derived points are flagged.
	in.Ifaces = append(in.Ifaces, queries.FlowIface{ExporterID: 2, IfIndex: 4, Name: "LAN", Role: "lan"})
	in.Hosts = append(in.Hosts, queries.PortHost{DeviceID: swID, InterfaceName: "ether1", MACAddress: "AA:BB:CC:00:00:01"})
	in.Points = []queries.FlowPoint{{ExporterID: 1, Kind: "derived", DeviceID: swID, Iface: "sfp-sfpplus2"}}
	got = Suggest(in)
	if got[0].Point.IfIndexes[0] != 4 {
		t.Errorf("role=lan not preferred: %v", got[0].Point.IfIndexes)
	}
	if want := wantG.Note + " Note: the port also carries 1 other MAC(s); the view shows only traffic to or from firewall003."; got[0].Point.Note != want {
		t.Errorf("note with other MAC = %q", got[0].Point.Note)
	}
	if !got[1].Exists || got[2].Exists || got[0].Exists {
		t.Errorf("exists flags = %v %v %v", got[0].Exists, got[1].Exists, got[2].Exists)
	}

	// No LAN iface yet: only the waiting reason.
	in = labSuggestInput()
	in.Ifaces = in.Ifaces[:8]
	got = Suggest(in)
	if got[0].Rule != RuleGatewayHost || got[0].Point != nil || got[0].Reason != "waiting for first flows from firewall003" {
		t.Fatalf("waiting = %+v", got[0])
	}

	// No routed interface on the trunk's VLANs: no trunk view.
	in = labSuggestInput()
	in.VLANs = []queries.BridgeVLAN{{DeviceID: rbID, BridgeName: "bridge", VLANIDs: "88", CurrentTagged: "sfp-sfpplus1"}}
	in.Ifaces[7].Type = "ether" // net88 not a vlan iface
	for _, s := range Suggest(in) {
		if s.Rule == RuleTrunk {
			t.Fatalf("unexpected trunk view %+v", s.Point)
		}
	}
}

func TestNoteTrunkWithoutExclude(t *testing.T) {
	want := "Derived from R IPFIX on bridge: routed and router-terminated traffic crossing this trunk. L2-switched VLAN traffic that R does not route is invisible."
	if got := NoteTrunk("R", []string{"bridge"}, nil); got != want {
		t.Fatalf("NoteTrunk = %q", got)
	}
}

// ---------- sankey ----------

// checkDAG asserts valid indices, no self-links, positive values and no
// cycle.
func checkDAG(t *testing.T, nodes []SankeyNode, links []SankeyLink) {
	t.Helper()
	adj := make([][]int, len(nodes))
	for _, l := range links {
		if l.Source < 0 || l.Source >= len(nodes) || l.Target < 0 || l.Target >= len(nodes) || l.Source == l.Target {
			t.Fatalf("bad link %+v (%d nodes)", l, len(nodes))
		}
		if l.Value <= 0 {
			t.Fatalf("non-positive link %+v", l)
		}
		adj[l.Source] = append(adj[l.Source], l.Target)
	}
	state := make([]int, len(nodes))
	var visit func(int)
	visit = func(n int) {
		if state[n] == 1 {
			t.Fatalf("cycle through %s", nodes[n].ID)
		}
		if state[n] == 2 {
			return
		}
		state[n] = 1
		for _, m := range adj[n] {
			visit(m)
		}
		state[n] = 2
	}
	for i := range nodes {
		visit(i)
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		if seen[n.ID] {
			t.Fatalf("duplicate node %s", n.ID)
		}
		seen[n.ID] = true
	}
}

func edgeMap(nodes []SankeyNode, links []SankeyLink, reverse bool) map[string]int64 {
	out := map[string]int64{}
	for _, l := range links {
		a, b := nodes[l.Source].ID, nodes[l.Target].ID
		if reverse {
			a, b = b, a
		}
		out[a+">"+b] = l.Value
	}
	return out
}

func TestBuildSankey(t *testing.T) {
	rows := dirRows()
	attach := func(a netip.Addr) (string, string, string, bool) {
		switch a {
		case ip("192.168.28.33"):
			return swID, "ether12", "switch001", true
		case ip("192.168.28.25"):
			return rbID, "eth3", "switch002-rb5009", true
		}
		return "", "", "", false
	}
	names := func(a netip.Addr) (string, string) {
		if a == ip("1.1.1.1") {
			return "one.one.one.one", ClassExternal
		}
		if a.Is4() && a.As4()[0] == 192 {
			return "", ClassInternal
		}
		return "", ClassExternal
	}
	opt := SankeyOptions{Dir: Download, PointID: 7, PointName: "switch001 · ether1", Seconds: 8, MaxRemote: 2, MaxLocal: 1}
	nodes, links := BuildSankey(rows, opt, names, attach)
	checkDAG(t, nodes, links)
	want := map[string]int64{
		// bytes*8/8 = bytes
		"remote:1.1.1.1>point:7":                       1000,
		"remote:8.8.8.8>point:7":                       100,
		"other:remote>point:7":                         140, // 9.9.9.9 + folded 40
		"point:7>host:192.168.28.33":                   900,
		"point:7>other:local":                          340, // .25 + folded 40
		"host:192.168.28.33>port:" + swID + ":ether12": 900,
	}
	if got := edgeMap(nodes, links, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("download edges = %v\nwant %v", got, want)
	}
	byID := map[string]SankeyNode{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if n := byID["remote:1.1.1.1"]; n.Name != "one.one.one.one" || n.Type != NodeRemote || n.IP != "1.1.1.1" || n.Class != ClassExternal {
		t.Errorf("remote node = %+v", n)
	}
	if n := byID["host:192.168.28.33"]; n.Name != "192.168.28.33" || n.Type != NodeHost || n.Class != ClassInternal {
		t.Errorf("host node = %+v", n)
	}
	if n := byID["port:"+swID+":ether12"]; n.Name != "switch001 · ether12" || n.DeviceID != swID || n.Iface != "ether12" {
		t.Errorf("port node = %+v", n)
	}
	if byID["other:remote"].Name != "Other remote (1)" || byID["other:local"].Name != "Other hosts (1)" {
		t.Errorf("other names = %q %q", byID["other:remote"].Name, byID["other:local"].Name)
	}
	if byID["point:7"].Name != "switch001 · ether1" || byID["point:7"].Type != NodePoint {
		t.Errorf("point node = %+v", byID["point:7"])
	}

	// Upload: the same nodes, every link reversed.
	opt.Dir = Upload
	upNodes, upLinks := BuildSankey(rows, opt, names, attach)
	checkDAG(t, upNodes, upLinks)
	if got := edgeMap(upNodes, upLinks, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("upload edges reversed = %v", got)
	}
	if len(upNodes) != len(nodes) {
		t.Fatalf("upload nodes %d != %d", len(upNodes), len(nodes))
	}

	// Apps as the remote layer; tiny values round up to 1.
	opt = SankeyOptions{Dir: Download, Remote: RemoteApp, PointID: 7, PointName: "p", Seconds: 1e9}
	nodes, links = BuildSankey(rows, opt, nil, nil)
	checkDAG(t, nodes, links)
	got := edgeMap(nodes, links, false)
	if got["app:6/443>point:7"] != 1 || got["other:remote>point:7"] != 1 || len(got) != 4+1+2+1 {
		t.Fatalf("app edges = %v", got)
	}
	for _, n := range nodes {
		if n.ID == "app:6/443" && n.Name != "HTTPS (tcp/443)" {
			t.Errorf("app name %q", n.Name)
		}
		if n.ID == "app:17/443" && n.Name != "QUIC (udp/443)" {
			t.Errorf("quic name %q", n.Name)
		}
	}

	// Empty input or no covered seconds: empty, non-nil.
	for _, c := range []struct {
		rows []DirRow
		secs float64
	}{{nil, 60}, {rows, 0}, {[]DirRow{{Bytes: 0}}, 60}} {
		n, l := BuildSankey(c.rows, SankeyOptions{Seconds: c.secs}, nil, nil)
		if n == nil || l == nil || len(n) != 0 || len(l) != 0 {
			t.Fatalf("empty case %+v: %v %v", c, n, l)
		}
	}
}

func TestAppName(t *testing.T) {
	for _, c := range []struct {
		proto uint8
		port  uint16
		want  string
	}{{6, 443, "HTTPS (tcp/443)"}, {17, 853, "DNS over QUIC (udp/853)"}, {6, 50514, "tcp/50514"}, {1, 0, "icmp"}, {47, 0, "gre"}, {99, 0, "ip-99"}} {
		if got := AppName(c.proto, c.port); got != c.want {
			t.Errorf("AppName(%d,%d) = %q, want %q", c.proto, c.port, got, c.want)
		}
	}
}

// The collector widens LAN /128s to their /64 and adds host rows: a LAN GUA
// is internal, so a local_internal point keeps its IPv6 upload, and a host
// row keeps its enclosing class while being matched as a single address.
func TestLANIPv6AndHostRows(t *testing.T) {
	a, p := netip.MustParseAddr, netip.MustParsePrefix
	pl := NewPlan([]queries.FlowPrefix{
		{Prefix: p("192.168.78.0/23"), Class: "internal"},
		{Prefix: p("2a01:4f8:202:13d1::/64"), Class: "internal"},
		{Prefix: p("2a01:4f8:202:13d1:78::2019/128"), Class: "internal"},
		{Prefix: p("100.64.0.0/10"), Class: "transit"},
		{Prefix: p("100.114.241.213/32"), Class: "transit"},
	}, nil, nil, nil, nil)
	for ip, want := range map[string]string{
		"2a01:4f8:202:13d1:78::60":   ClassInternal,
		"2a01:4f8:202:13d1:78::2019": ClassInternal,
		"100.114.241.213":            ClassExternal,
		"100.64.0.1":                 ClassExternal,
		"2620:fe::fe":                ClassExternal,
	} {
		if got := pl.Class(a(ip)); got != want {
			t.Errorf("Class(%s) = %s, want %s", ip, got, want)
		}
	}
	pt := Point{FlowPoint: queries.FlowPoint{IfIndexes: []uint32{1}, Facing: FacingDown, LocalInternal: true}}
	rows := []queries.FlowAgg{
		{InIf: 1, OutIf: 7, Src: a("192.168.78.60"), Dst: a("9.9.9.9"), Bytes: 1000},
		{InIf: 1, OutIf: 7, Src: a("2a01:4f8:202:13d1:78::60"), Dst: a("2620:fe::fe"), Bytes: 2000},
	}
	if got := Totals(Select(rows, pt, Upload, pl)); got != 3000 {
		t.Fatalf("upload kept %d bytes, want 3000", got)
	}
	down, up := SumByBucket(rows, pt, pl)
	if up[time.Time{}.Unix()] != 3000 || len(down) != 0 {
		t.Fatalf("SumByBucket down %v up %v", down, up)
	}
}
