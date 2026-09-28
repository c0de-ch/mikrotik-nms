package topology

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const mbps = int64(1_000_000)

// rates maps "dev/iface" → {rx, tx} bits/s.
type rates map[string][2]int64

func (r rates) fn() RateFunc {
	return func(dev, iface string) (int64, int64, bool) {
		v, ok := r[dev+"/"+iface]
		return v[0], v[1], ok
	}
}

func (r rates) without(keys ...string) rates {
	out := rates{}
	for k, v := range r {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

func up(a, ia, b, ib string) PathLink {
	return PathLink{DeviceA: a, IfaceA: ia, DeviceB: b, IfaceB: ib, Status: "up"}
}

// mac is a device interface's (managed) MAC.
func mac(dev, iface string) string { return strings.ToUpper("02:" + dev + ":" + iface) }

// ifs builds a device's interfaces from "name:type" specs, each with a MAC.
func ifs(dev string, specs ...string) []PathIface {
	var out []PathIface
	for _, s := range specs {
		name, typ, _ := strings.Cut(s, ":")
		out = append(out, PathIface{DeviceID: dev, Name: name, Type: typ, MAC: mac(dev, name), Running: true})
	}
	return out
}

// learned is one MAC in the FDB of each "dev/iface".
func learned(m string, at ...string) []PathHost {
	var out []PathHost
	for _, a := range at {
		dev, iface, _ := strings.Cut(a, "/")
		out = append(out, PathHost{DeviceID: dev, Iface: iface, MAC: m})
	}
	return out
}

func concat[T any](parts ...[]T) []T {
	var out []T
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---------- fixtures ----------

// meshFixture is contract §7.6: R (internet edge) → C (core) → S (access) →
// A1/A2 (APs), where MNDP on the flat L2 makes nearly every pair neighbours.
func meshFixture() (PathInput, rates) {
	in := PathInput{
		Devices: []PathDevice{
			{ID: "r", Name: "R", Type: "router", Status: "online", Address: "10.0.0.1"},
			{ID: "c", Name: "C", Type: "switch", Status: "online", Address: "10.0.0.2"},
			{ID: "s", Name: "S", Type: "switch", Status: "online", Address: "10.0.0.3"},
			{ID: "a1", Name: "A1", Type: "ap", Status: "online", Address: "10.0.0.4"},
			{ID: "a2", Name: "A2", Type: "ap", Status: "online", Address: "10.0.0.5"},
		},
		Links: []PathLink{
			up("r", "sfp1,bridge", "c", "sfp2,bridge"),
			up("r", "sfp1,bridge", "s", "sfp1,bridge"),
			up("r", "sfp1,bridge", "a1", "ether1,bridge"),
			up("r", "sfp1,bridge", "a2", "ether1,bridge"),
			up("c", "q1,bridge", "s", "sfp1,bridge"),
			up("c", "q1,bridge", "a1", "ether1,bridge"),
			up("c", "q1,bridge", "a2", "ether1,bridge"),
			up("s", "ether23,bridge", "a1", "ether1,bridge"),
			up("s", "ether5,bridge", "a2", "ether1,bridge"),
			up("a1", "ether1,bridge", "a2", "ether1,bridge"),
			up("c", "net28", "s", "net28"), // VLAN noise: never a tree edge
			{DeviceA: "r", IfaceA: "sfp1", DeviceB: "s", IfaceB: "ether7", Status: "down"},
		},
		Uplinks: []PathUplink{
			{DeviceID: "r", Kind: "default-route", Interface: "eth1-wan", IfaceType: "ether", GatewayIP: "203.0.113.1"},
			// Everyone else routes via the managed router: not an anchor.
			{DeviceID: "c", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "10.0.0.1"},
			{DeviceID: "s", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "10.0.0.1"},
			{DeviceID: "a1", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "10.0.0.1"},
			{DeviceID: "a2", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "10.0.0.1"},
		},
		Interfaces: concat(
			ifs("r", "eth1-wan:ether", "sfp1:ether", "bridge:bridge"),
			ifs("c", "sfp2:ether", "q1:ether", "net28:vlan", "bridge:bridge"),
			ifs("s", "sfp1:ether", "ether23:ether", "ether5:ether", "ether7:ether", "net28:vlan", "bridge:bridge"),
			ifs("a1", "ether1:ether", "wifi1:wifi", "bridge:bridge"),
			ifs("a2", "ether1:ether", "wifi1:wifi", "bridge:bridge"),
		),
		// FDB: a MAC is learned only on ports that face toward it.
		Hosts: concat(
			learned("aa:00:00:00:00:01", "a1/wifi1", "s/ether23", "c/q1", "r/sfp1"), // lower case on purpose
			learned("AA:00:00:00:00:02", "a1/wifi1", "s/ether23", "c/q1", "r/sfp1"),
			learned("BB:00:00:00:00:07", "s/ether7", "c/q1", "r/sfp1"),
			learned(mac("a2", "bridge"), "s/ether5", "c/q1", "r/sfp1"), // managed: never a client
		),
	}
	return in, rates{
		"r/eth1-wan": {100 * mbps, 10 * mbps},
		"r/sfp1":     {9 * mbps, 95 * mbps},
		"c/q1":       {8 * mbps, 90 * mbps},
		"s/ether23":  {5 * mbps, 60 * mbps},
		"s/ether5":   {2 * mbps, 20 * mbps},
		"s/ether7":   {1 * mbps, 5 * mbps},
		"a1/wifi1":   {4 * mbps, 55 * mbps},
	}
}

// meshDualWAN adds an interface-only LTE default route to R, plus a private
// gateway found on R's ether8 (its WAN role points at that gateway instead).
func meshDualWAN() (PathInput, rates) {
	in, r := meshFixture()
	in.Interfaces = append(in.Interfaces, ifs("r", "lte1:lte", "ether8:ether")...)
	in.Uplinks = append(in.Uplinks,
		PathUplink{DeviceID: "r", Kind: "default-route", Interface: "lte1", IfaceType: "lte"},
		PathUplink{DeviceID: "r", Kind: "gateway-host", Interface: "ether8", IfaceType: "ether", GatewayIP: "192.168.8.1"})
	r["r/lte1"] = [2]int64{30 * mbps, 3 * mbps}
	r["r/ether8"] = [2]int64{4 * mbps, 1 * mbps}
	return in, r
}

// meshNoRoutes is the mesh with no egress data at all (fallback tree).
func meshNoRoutes() (PathInput, rates) {
	in, r := meshFixture()
	in.Uplinks = nil
	return in, r
}

// meshRoutedGW: every device routes via an unmanaged private gateway whose
// switch port was never found (no gateway-host row) — still unanchored.
func meshRoutedGW() (PathInput, rates) {
	in, r := meshFixture()
	in.Uplinks = nil
	for _, d := range in.Devices {
		in.Uplinks = append(in.Uplinks, PathUplink{DeviceID: d.ID, Kind: "default-route", Interface: "bridge",
			IfaceType: "bridge", GatewayIP: "192.168.78.1"})
	}
	in.GatewayLabels = map[string]string{"192.168.78.1": "isp-box"}
	return in, r
}

// meshWithIslands adds two devices unreachable from the anchored tree: one
// with no egress data (orphan) and one routing via a private gateway.
func meshWithIslands() (PathInput, rates) {
	in, r := meshFixture()
	in.Devices = append(in.Devices,
		PathDevice{ID: "iso", Name: "island", Address: "172.16.9.1"},
		PathDevice{ID: "iso2", Name: "island-2", Address: "172.16.9.2"})
	in.Interfaces = append(in.Interfaces, ifs("iso", "ether1:ether", "ether2:ether")...)
	in.Interfaces = append(in.Interfaces, ifs("iso2", "ether1:ether", "bridge:bridge")...)
	in.Hosts = append(in.Hosts, learned("DD:00:00:00:00:09", "iso/ether2", "iso2/ether1")...)
	in.Uplinks = append(in.Uplinks, PathUplink{DeviceID: "iso2", Kind: "default-route", Interface: "bridge",
		IfaceType: "bridge", GatewayIP: "192.168.50.1"})
	r["iso/ether2"] = [2]int64{1 * mbps, 7 * mbps}
	r["iso2/ether1"] = [2]int64{2 * mbps, 3 * mbps}
	return in, r
}

// gwHostVPNFixture: an unmanaged ISP router found in core-sw's FDB on ether1
// (gateway-host), an AP behind the switch, and a remote site router whose
// default route is a WireGuard tunnel. core-sw also has a non-default EoIP.
func gwHostVPNFixture() (PathInput, rates) {
	in := PathInput{
		Devices: []PathDevice{
			{ID: "sw", Name: "core-sw", Type: "switch", Address: "192.168.1.2"},
			{ID: "ap", Name: "ap-1", Type: "ap", Address: "192.168.1.3"},
			{ID: "rt", Name: "remote-rt", Type: "router", Address: "10.8.0.2"},
		},
		Links: []PathLink{
			up("sw", "ether2,bridge", "ap", "ether1,bridge"),
			up("sw", "bridge", "ap", "bridge"),
		},
		Uplinks: []PathUplink{
			{DeviceID: "sw", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.168.1.1"},
			{DeviceID: "sw", Kind: "gateway-host", Interface: "ether1", IfaceType: "ether", GatewayIP: "192.168.1.1"},
			{DeviceID: "sw", Kind: "vpn", Interface: "eoip-tunnel1", IfaceType: "eoip"},
			{DeviceID: "ap", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.168.1.1"},
			{DeviceID: "rt", Kind: "default-route", Interface: "wg0", IfaceType: "wg"},
			{DeviceID: "rt", Kind: "vpn", Interface: "wg0", IfaceType: "wg"},
		},
		Interfaces: concat(
			ifs("sw", "ether1:ether", "ether2:ether", "bridge:bridge", "eoip-tunnel1:eoip"),
			ifs("ap", "ether1:ether", "wifi1:wifi", "bridge:bridge"),
			ifs("rt", "ether1:ether", "wg0:wg", "bridge:bridge"),
		),
		Hosts: concat(
			learned("CC:00:00:00:00:01", "sw/ether1"), // the ISP router itself
			learned(mac("ap", "bridge"), "sw/ether2"),
			learned(mac("sw", "bridge"), "ap/ether1"),
			learned("DD:00:00:00:00:01", "ap/wifi1", "sw/ether2"),
			learned("EE:00:00:00:00:01", "rt/ether1"),
			learned("EE:00:00:00:00:02", "rt/ether1"),
		),
		GatewayLabels: map[string]string{"192.168.1.1": "fritz"},
	}
	return in, rates{
		"sw/ether1": {50 * mbps, 5 * mbps},
		"sw/ether2": {3 * mbps, 40 * mbps},
		"ap/wifi1":  {2 * mbps, 35 * mbps},
		"rt/wg0":    {20 * mbps, 4 * mbps},
		"rt/ether1": {3 * mbps, 18 * mbps},
	}
}

// meshTwoAnchors is the lab's dual-router shape: R keeps its public default
// route, and the core switch C additionally finds the LAN firewall on its
// ether1 (gateway-host), so both are anchors and the R.sfp1–C.sfp2 cable
// joins two roots instead of being a tree edge. R also has a client on its own
// ether5, which C learns on sfp2.
func meshTwoAnchors() (PathInput, rates) {
	in, r := meshFixture()
	in.Interfaces = append(in.Interfaces, ifs("c", "ether1:ether")...)
	in.Interfaces = append(in.Interfaces, ifs("r", "ether5:ether")...)
	in.Uplinks = append(in.Uplinks,
		PathUplink{DeviceID: "c", Kind: "gateway-host", Interface: "ether1", IfaceType: "ether", GatewayIP: "192.168.1.254"})
	in.Hosts = append(in.Hosts, learned("FF:00:00:00:00:01", "c/ether1", "r/sfp1")...) // the firewall
	in.Hosts = append(in.Hosts, learned("CC:00:00:00:00:05", "r/ether5", "c/sfp2")...)
	r["c/ether1"] = [2]int64{60 * mbps, 6 * mbps}
	r["r/ether5"] = [2]int64{1 * mbps, 4 * mbps}
	return in, r
}

// chainFixture is a daisy chain d0 → d1 → … on one flat L2: every device
// hears every other (upstream ones on ether1, downstream ones on ether2), and
// the FDBs agree. Names run backwards so name order cannot fake tree order.
func chainFixture(n int) (PathInput, rates) {
	id := func(i int) string { return fmt.Sprintf("d%d", i) }
	var in PathInput
	for i := 0; i < n; i++ {
		in.Devices = append(in.Devices, PathDevice{ID: id(i), Name: fmt.Sprintf("sw-%c", 'z'-i),
			Address: fmt.Sprintf("10.1.0.%d", i+1)})
		in.Interfaces = append(in.Interfaces, ifs(id(i), "ether1:ether", "ether2:ether", "ether3:ether", "bridge:bridge")...)
	}
	in.Interfaces = append(in.Interfaces, ifs("d0", "ether9:ether")...)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			in.Links = append(in.Links, up(id(i), "ether2,bridge", id(j), "ether1,bridge"))
			in.Hosts = append(in.Hosts,
				PathHost{DeviceID: id(i), Iface: "ether2", MAC: mac(id(j), "bridge")},
				PathHost{DeviceID: id(j), Iface: "ether1", MAC: mac(id(i), "bridge")})
		}
	}
	last := id(n - 1)
	for i := 0; i < n-1; i++ {
		in.Hosts = append(in.Hosts, PathHost{DeviceID: id(i), Iface: "ether2", MAC: "AA:11:22:33:44:55"})
	}
	in.Hosts = append(in.Hosts, PathHost{DeviceID: last, Iface: "ether3", MAC: "AA:11:22:33:44:55"})
	in.Uplinks = []PathUplink{{DeviceID: "d0", Kind: "default-route", Interface: "ether9", IfaceType: "ether",
		GatewayIP: "198.51.100.1"}}

	r := rates{"d0/ether9": {100 * mbps, 10 * mbps}, last + "/ether3": {1 * mbps, 50 * mbps}}
	for i := 0; i < n-1; i++ {
		r[id(i)+"/ether2"] = [2]int64{int64(10-i) * mbps, int64(90-i) * mbps}
	}
	return in, r
}

// parallelFixture: x and y share several MNDP edges — a bond, its two member
// ports, the bridge, a VLAN, a duplicate row and IP-resolved "unknown" ends.
// With withFDB the bridge hosts table says the bond carries the traffic.
func parallelFixture(withFDB bool) (PathInput, rates) {
	in := PathInput{
		Devices: []PathDevice{
			{ID: "x", Name: "x-router", Address: "10.2.0.1"},
			{ID: "y", Name: "y-switch", Address: "10.2.0.2"},
		},
		Interfaces: concat(
			ifs("x", "ether9:ether", "ether1:ether", "ether2:ether", "bond1:bond", "bridge:bridge", "vlan10:vlan"),
			ifs("y", "ether1:ether", "ether2:ether", "ether5:ether", "bond1:bond", "bridge:bridge", "vlan10:vlan"),
		),
		Links: []PathLink{
			up("x", "ether1,bridge", "y", "ether1,bridge"),
			up("x", "ether1,bridge", "y", "ether1,bridge"), // duplicate row
			up("x", "ether2", "y", "ether2"),
			up("x", "bond1,bridge", "y", "bond1,bridge"),
			up("x", "bridge", "y", "bridge"),
			up("x", "vlan10", "y", "vlan10"),
			up("x", "unknown", "y", "ether1"),
			up("y", "", "x", "unknown"),
		},
		Uplinks: []PathUplink{{DeviceID: "x", Kind: "default-route", Interface: "ether9", IfaceType: "ether",
			GatewayIP: "198.51.100.7"}},
	}
	// A client behind y, learned on whichever x port carries the traffic.
	carrier := "ether1"
	if withFDB {
		carrier = "bond1"
	}
	in.Hosts = learned("AB:CD:EF:00:00:05", "y/ether5", "x/"+carrier)
	if withFDB {
		in.Hosts = append(in.Hosts, learned(mac("y", "bridge"), "x/bond1")...)
		in.Hosts = append(in.Hosts, learned(mac("x", "bridge"), "y/bond1")...)
	}
	return in, rates{
		"x/ether9": {10 * mbps, 1 * mbps},
		"x/ether1": {1 * mbps, 7 * mbps},
		"x/bond1":  {1 * mbps, 8 * mbps},
		"y/ether5": {1 * mbps, 6 * mbps},
	}
}

// ring3Fixture is a three-switch ring with no egress data. Nearest-ancestor
// inference alone yields a parent cycle a→b→c→a here; the tree must still
// come out acyclic.
func ring3Fixture() (PathInput, rates) {
	in := PathInput{
		Devices: []PathDevice{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "C"}},
		Links: []PathLink{
			up("a", "ether1", "b", "ether2"),
			up("a", "ether2", "c", "ether1"),
			up("b", "ether1", "c", "ether2"),
		},
		Interfaces: concat(
			ifs("a", "ether1:ether", "ether2:ether", "ether3:ether"),
			ifs("b", "ether1:ether", "ether2:ether"),
			ifs("c", "ether1:ether", "ether2:ether"),
		),
		Hosts: learned("AA:00:00:00:0A:03", "a/ether3"),
	}
	return in, rates{
		"a/ether1": {1 * mbps, 2 * mbps}, "a/ether3": {1 * mbps, 9 * mbps},
		"b/ether1": {5 * mbps, 6 * mbps}, "b/ether2": {7 * mbps, 8 * mbps},
		"c/ether1": {9 * mbps, 10 * mbps}, "c/ether2": {11 * mbps, 12 * mbps},
	}
}

// ring4Fixture is an STP ring R–A–B–C–R with R the internet edge and the B–C
// link blocked. MNDP still reports B↔C on the blocked ports and, over the
// forwarding tree, on the tree ports.
func ring4Fixture() (PathInput, rates) {
	in := PathInput{
		Devices: []PathDevice{{ID: "R", Name: "R"}, {ID: "A", Name: "A"}, {ID: "B", Name: "B"}, {ID: "C", Name: "C"}},
		Links: []PathLink{
			up("R", "ether1,bridge", "A", "ether1,bridge"),
			up("R", "ether1,bridge", "B", "ether1,bridge"),
			up("R", "ether2,bridge", "C", "ether1,bridge"),
			up("A", "ether2,bridge", "B", "ether1,bridge"),
			up("A", "ether1,bridge", "C", "ether1,bridge"),
			up("B", "ether1,bridge", "C", "ether1,bridge"),
			up("B", "ether2,bridge", "C", "ether2,bridge"), // the blocked ring link
		},
		Uplinks: []PathUplink{{DeviceID: "R", Kind: "default-route", Interface: "ether9", IfaceType: "ether",
			GatewayIP: "198.51.100.9"}},
		Interfaces: concat(
			ifs("R", "ether9:ether", "ether1:ether", "ether2:ether"),
			ifs("A", "ether1:ether", "ether2:ether"),
			ifs("B", "ether1:ether", "ether2:ether"),
			ifs("C", "ether1:ether", "ether2:ether"),
		),
	}
	return in, rates{
		"R/ether9": {30 * mbps, 3 * mbps}, "R/ether1": {1 * mbps, 20 * mbps}, "R/ether2": {1 * mbps, 9 * mbps},
		"A/ether2": {1 * mbps, 12 * mbps},
	}
}

// leafFixture: a single internet edge with twelve access ports.
func leafFixture() (PathInput, rates) {
	in := PathInput{
		Devices:    []PathDevice{{ID: "e", Name: "edge", Address: "10.3.0.1"}},
		Interfaces: ifs("e", "ether1:ether"),
		Uplinks: []PathUplink{{DeviceID: "e", Kind: "default-route", Interface: "ether1", IfaceType: "ether",
			GatewayIP: "198.51.100.3"}},
	}
	r := rates{"e/ether1": {100 * mbps, 1 * mbps}}
	for i := 2; i <= 13; i++ {
		p := fmt.Sprintf("ether%d", i)
		in.Interfaces = append(in.Interfaces, PathIface{DeviceID: "e", Name: p, Type: "ether"})
		in.Hosts = append(in.Hosts, PathHost{DeviceID: "e", Iface: p, MAC: fmt.Sprintf("AA:BB:CC:00:00:%02X", i)})
		r["e/"+p] = [2]int64{mbps / 10, int64(i) * mbps}
	}
	r["e/ether13"] = [2]int64{0, 500} // below MinBps: never a leaf
	return in, r
}

type fixture struct {
	name  string
	build func() (PathInput, rates)
}

func allFixtures() []fixture {
	return []fixture{
		{"mesh", meshFixture},
		{"mesh-dual-wan", meshDualWAN},
		{"mesh-no-routes", meshNoRoutes},
		{"mesh-routed-gw", meshRoutedGW},
		{"mesh-islands", meshWithIslands},
		{"gwhost-vpn", gwHostVPNFixture},
		{"mesh-two-anchors", meshTwoAnchors},
		{"chain6", func() (PathInput, rates) { return chainFixture(6) }},
		{"parallel", func() (PathInput, rates) { return parallelFixture(false) }},
		{"parallel-fdb", func() (PathInput, rates) { return parallelFixture(true) }},
		{"ring3", ring3Fixture},
		{"ring4", ring4Fixture},
		{"leaves", leafFixture},
		{"empty", func() (PathInput, rates) { return PathInput{}, rates{} }},
	}
}

// ---------- invariant checks ----------

var (
	validRoles     = map[string]bool{"wan": true, "uplink": true, "downlink": true, "peer": true, "access": true, "wireless": true, "vpn": true, "virtual": true, "idle": true}
	validHopKinds  = map[string]bool{"internet": true, "gateway": true, "vpn": true, "device": true, "clients": true}
	validNodeTypes = map[string]bool{"internet": true, "gateway": true, "vpn": true, "device": true, "port": true, "other": true}
)

// checkSankey asserts the Sankey is a well-formed DAG: indices in range, no
// self-links, every value >= minBps, no isolated nodes, at most one incoming
// tree link per node plus at most one from its "local:<dev>" source (a
// source-only node feeding exactly its own device), every device with a tree
// parent conserving flow (out − in < minBps), and no cycle (DFS).
func checkSankey(t *testing.T, label string, s Sankey, minBps int64) {
	t.Helper()
	if s.Nodes == nil || s.Links == nil {
		t.Fatalf("%s: nil nodes/links", label)
	}
	if !s.Estimated {
		t.Errorf("%s: Estimated = false", label)
	}
	if s.Direction != "download" && s.Direction != "upload" {
		t.Errorf("%s: direction %q", label, s.Direction)
	}
	ids := map[string]bool{}
	for _, n := range s.Nodes {
		if ids[n.ID] {
			t.Errorf("%s: duplicate node id %q", label, n.ID)
		}
		ids[n.ID] = true
		if !validNodeTypes[n.Type] {
			t.Errorf("%s: node %q has type %q", label, n.ID, n.Type)
		}
	}
	adj := make([][]int, len(s.Nodes))
	inDeg := make([]int, len(s.Nodes))
	localIn := make([]int, len(s.Nodes))
	in := make([]int64, len(s.Nodes))
	out := make([]int64, len(s.Nodes))
	linked := make([]bool, len(s.Nodes))
	isLocal := func(i int) bool { return strings.HasPrefix(s.Nodes[i].ID, "local:") }
	for _, l := range s.Links {
		if l.Source < 0 || l.Source >= len(s.Nodes) || l.Target < 0 || l.Target >= len(s.Nodes) {
			t.Fatalf("%s: link %+v out of range (%d nodes)", label, l, len(s.Nodes))
		}
		if l.Source == l.Target {
			t.Errorf("%s: self-link on %q", label, s.Nodes[l.Source].ID)
		}
		if l.Value < minBps {
			t.Errorf("%s: link %s→%s value %d < MinBps %d", label, s.Nodes[l.Source].ID, s.Nodes[l.Target].ID, l.Value, minBps)
		}
		adj[l.Source] = append(adj[l.Source], l.Target)
		if isLocal(l.Source) {
			localIn[l.Target]++
			src, dst := s.Nodes[l.Source], s.Nodes[l.Target]
			if src.Type != "other" || dst.Type != "device" || src.DeviceID != dst.DeviceID || src.ID != "local:"+dst.DeviceID {
				t.Errorf("%s: local source %+v feeds %+v", label, src, dst)
			}
		} else {
			inDeg[l.Target]++
		}
		in[l.Target] += l.Value
		out[l.Source] += l.Value
		linked[l.Source], linked[l.Target] = true, true
	}
	for i, n := range s.Nodes {
		if !linked[i] {
			t.Errorf("%s: node %q has no links", label, n.ID)
		}
		if inDeg[i] > 1 || localIn[i] > 1 {
			t.Errorf("%s: node %q has %d parents and %d local sources", label, n.ID, inDeg[i], localIn[i])
		}
		if isLocal(i) && (inDeg[i] > 0 || localIn[i] > 0 || len(adj[i]) != 1) {
			t.Errorf("%s: local node %q must be a source with one link", label, n.ID)
		}
		if n.Type == "device" && inDeg[i] == 1 && out[i]-in[i] >= minBps {
			t.Errorf("%s: device %q sends %d but receives %d: east-west surplus not balanced", label, n.ID, out[i], in[i])
		}
	}
	const (
		white = iota
		grey
		black
	)
	color := make([]int, len(s.Nodes))
	var visit func(int) bool
	visit = func(u int) bool {
		color[u] = grey
		for _, v := range adj[u] {
			if color[v] == grey || (color[v] == white && !visit(v)) {
				return false
			}
		}
		color[u] = black
		return true
	}
	for i := range s.Nodes {
		if color[i] == white && !visit(i) {
			t.Fatalf("%s: sankey has a cycle through %q", label, s.Nodes[i].ID)
		}
	}
}

// checkGraph asserts the tree/role/path invariants that hold for any input.
func checkGraph(t *testing.T, label string, in PathInput, r rates) {
	t.Helper()
	g := BuildRoleGraph(in)

	devs := g.Devices()
	if len(devs) != len(in.Devices) {
		t.Fatalf("%s: Devices() has %d entries, want %d", label, len(devs), len(in.Devices))
	}
	byID := map[string]DeviceTreeInfo{}
	anyAnchor := false
	for i, d := range devs {
		if _, dup := byID[d.DeviceID]; dup {
			t.Errorf("%s: device %s listed twice", label, d.DeviceID)
		}
		byID[d.DeviceID] = d
		anyAnchor = anyAnchor || d.Anchor
		if i > 0 {
			p := devs[i-1]
			if p.Depth > d.Depth || (p.Depth == d.Depth && p.Name > d.Name) {
				t.Errorf("%s: Devices() not sorted by depth, name at %d", label, i)
			}
		}
		if d.Anchor && d.ParentIface != "" {
			t.Errorf("%s: anchor %s has parent iface %q", label, d.DeviceID, d.ParentIface)
		}
	}
	if g.Anchored() != anyAnchor {
		t.Errorf("%s: Anchored() = %v, any anchor = %v", label, g.Anchored(), anyAnchor)
	}
	for _, d := range devs {
		// Parent chains terminate (forest) and depth counts device ancestors.
		depth, x := 0, d
		for steps := 0; ; steps++ {
			if steps > len(devs) {
				t.Fatalf("%s: parent chain of %s does not terminate", label, d.DeviceID)
			}
			p, isDev := byID[x.ParentID]
			if !isDev {
				switch {
				case x.ParentID == "", x.ParentID == "internet",
					strings.HasPrefix(x.ParentID, "gw:"), strings.HasPrefix(x.ParentID, "vpn:"):
				default:
					t.Errorf("%s: %s has unknown parent %q", label, x.DeviceID, x.ParentID)
				}
				break
			}
			depth++
			x = p
		}
		if depth != d.Depth {
			t.Errorf("%s: %s depth = %d, want %d", label, d.DeviceID, d.Depth, depth)
		}
	}

	roles := g.Roles()
	seen := map[portKey]bool{}
	for i, ri := range roles {
		k := portKey{ri.DeviceID, ri.Iface}
		if seen[k] {
			t.Errorf("%s: role for %v listed twice", label, k)
		}
		seen[k] = true
		if !validRoles[ri.Role] {
			t.Errorf("%s: %v has role %q", label, k, ri.Role)
		}
		if g.Role(ri.DeviceID, ri.Iface) != ri {
			t.Errorf("%s: Role(%v) disagrees with Roles()", label, k)
		}
		if n := len(g.AttachedMACs(ri.DeviceID, ri.Iface)); n != ri.ClientCount {
			t.Errorf("%s: %v AttachedMACs = %d, ClientCount = %d", label, k, n, ri.ClientCount)
		}
		if i > 0 {
			p := roles[i-1]
			pn, cn := byID[p.DeviceID].Name, byID[ri.DeviceID].Name
			if pn > cn || (pn == cn && p.DeviceID == ri.DeviceID && p.Iface > ri.Iface) {
				t.Errorf("%s: Roles() not sorted at %d", label, i)
			}
		}
	}
	for _, ifc := range in.Interfaces {
		if _, ok := byID[ifc.DeviceID]; ok && !seen[portKey{ifc.DeviceID, ifc.Name}] {
			t.Errorf("%s: interface %s/%s has no role", label, ifc.DeviceID, ifc.Name)
		}
	}

	// Every path is well-formed: synthetic hops, then device hops ending at
	// the target, then sinks; hop 0 unmeasured.
	for _, d := range devs {
		targets := []string{""}
		for _, ri := range roles {
			if ri.DeviceID == d.DeviceID {
				targets = append(targets, ri.Iface)
			}
		}
		for _, iface := range targets {
			hops, err := g.Path(d.DeviceID, iface, r.fn())
			if err != nil {
				t.Fatalf("%s: Path(%s, %q): %v", label, d.DeviceID, iface, err)
			}
			if len(hops) == 0 || hops[0].Measured || hops[0].DownBps != 0 || hops[0].UpBps != 0 {
				t.Errorf("%s: Path(%s, %q) hop 0 = %+v", label, d.DeviceID, iface, hops)
				continue
			}
			target := -1
			for i, h := range hops {
				if !validHopKinds[h.Kind] {
					t.Errorf("%s: hop kind %q", label, h.Kind)
				}
				if !h.Sink && h.Kind == "device" && h.DeviceID == d.DeviceID {
					target = i
				}
			}
			if target < 0 || hops[target].OutIface != iface {
				t.Errorf("%s: Path(%s, %q) target hop missing: %+v", label, d.DeviceID, iface, hops)
				continue
			}
			for i, h := range hops {
				if h.Sink != (i > target) {
					t.Errorf("%s: Path(%s, %q) hop %d sink = %v", label, d.DeviceID, iface, i, h.Sink)
				}
			}
		}
	}

	for _, dir := range []string{"download", "upload"} {
		checkSankey(t, label+"/"+dir, g.Sankey(r.fn(), SankeyOptions{Direction: dir}), 1000)
		for _, d := range devs {
			s := g.Sankey(r.fn(), SankeyOptions{Direction: dir, RootDeviceID: d.DeviceID})
			checkSankey(t, label+"/"+dir+"/"+d.DeviceID, s, 1000)
			if len(s.Nodes) > 0 && s.Nodes[0].ID != d.DeviceID {
				t.Errorf("%s: rooted sankey starts at %q, want %q", label, s.Nodes[0].ID, d.DeviceID)
			}
			for _, n := range s.Nodes {
				if n.Type == "internet" || n.Type == "gateway" || n.Type == "vpn" {
					t.Errorf("%s: rooted sankey has synthetic node %q", label, n.ID)
				}
			}
		}
		// A high threshold prunes but must never break the invariants.
		checkSankey(t, label+"/"+dir+"/min", g.Sankey(r.fn(), SankeyOptions{Direction: dir, MinBps: 8 * mbps}), 8*mbps)
		checkSankey(t, label+"/"+dir+"/nil", g.Sankey(nil, SankeyOptions{Direction: dir}), 1000)
	}
}

func TestRoleGraphInvariantsAllFixtures(t *testing.T) {
	for _, f := range allFixtures() {
		t.Run(f.name, func(t *testing.T) {
			in, r := f.build()
			checkGraph(t, f.name, in, r)
		})
	}
}

// ---------- mandatory mesh fixture (§7.6) ----------

func TestRoleGraphMeshTree(t *testing.T) {
	in, _ := meshFixture()
	g := BuildRoleGraph(in)
	if !g.Anchored() {
		t.Fatal("Anchored() = false, want true")
	}
	want := []DeviceTreeInfo{
		{DeviceID: "r", Name: "R", ParentID: "internet", UplinkIface: "eth1-wan", Depth: 0, Anchor: true},
		{DeviceID: "c", Name: "C", ParentID: "r", ParentIface: "sfp1", UplinkIface: "sfp2", Depth: 1},
		{DeviceID: "s", Name: "S", ParentID: "c", ParentIface: "q1", UplinkIface: "sfp1", Depth: 2},
		{DeviceID: "a1", Name: "A1", ParentID: "s", ParentIface: "ether23", UplinkIface: "ether1", Depth: 3},
		{DeviceID: "a2", Name: "A2", ParentID: "s", ParentIface: "ether5", UplinkIface: "ether1", Depth: 3},
	}
	if got := g.Devices(); !reflect.DeepEqual(got, want) {
		t.Errorf("Devices() =\n %+v\nwant\n %+v", got, want)
	}
}

func TestRoleGraphMeshRoles(t *testing.T) {
	in, _ := meshFixture()
	g := BuildRoleGraph(in)
	tests := []PortRoleInfo{
		{DeviceID: "r", Iface: "eth1-wan", Role: RoleWAN, NeighborName: "Internet", UpstreamNode: "internet"},
		{DeviceID: "r", Iface: "sfp1", Role: RoleDownlink, NeighborDeviceID: "c", NeighborName: "C", NeighborIface: "sfp2", NeighborCount: 1, MACCount: 4},
		{DeviceID: "r", Iface: "bridge", Role: RoleVirtual},
		{DeviceID: "c", Iface: "sfp2", Role: RoleUplink, NeighborDeviceID: "r", NeighborName: "R", NeighborIface: "sfp1"},
		{DeviceID: "c", Iface: "q1", Role: RoleDownlink, NeighborDeviceID: "s", NeighborName: "S", NeighborIface: "sfp1", NeighborCount: 1, MACCount: 4},
		{DeviceID: "c", Iface: "net28", Role: RoleVirtual},
		{DeviceID: "s", Iface: "sfp1", Role: RoleUplink, NeighborDeviceID: "c", NeighborName: "C", NeighborIface: "q1"},
		{DeviceID: "s", Iface: "ether23", Role: RoleDownlink, NeighborDeviceID: "a1", NeighborName: "A1", NeighborIface: "ether1", NeighborCount: 1, MACCount: 2},
		{DeviceID: "s", Iface: "ether5", Role: RoleDownlink, NeighborDeviceID: "a2", NeighborName: "A2", NeighborIface: "ether1", NeighborCount: 1, MACCount: 1},
		{DeviceID: "s", Iface: "ether7", Role: RoleAccess, ClientCount: 1, MACCount: 1},
		{DeviceID: "s", Iface: "net28", Role: RoleVirtual},
		{DeviceID: "a1", Iface: "ether1", Role: RoleUplink, NeighborDeviceID: "s", NeighborName: "S", NeighborIface: "ether23"},
		{DeviceID: "a1", Iface: "wifi1", Role: RoleWireless, ClientCount: 2, MACCount: 2},
		{DeviceID: "a2", Iface: "ether1", Role: RoleUplink, NeighborDeviceID: "s", NeighborName: "S", NeighborIface: "ether5"},
		{DeviceID: "a2", Iface: "wifi1", Role: RoleIdle},
	}
	for _, want := range tests {
		t.Run(want.DeviceID+"/"+want.Iface, func(t *testing.T) {
			if got := g.Role(want.DeviceID, want.Iface); got != want {
				t.Errorf("Role() =\n %+v\nwant\n %+v", got, want)
			}
		})
	}

	if got, want := len(g.Roles()), 19; got != want {
		t.Errorf("len(Roles()) = %d, want %d", got, want)
	}
	if got := g.AttachedMACs("a1", "wifi1"); !reflect.DeepEqual(got, []string{"AA:00:00:00:00:01", "AA:00:00:00:00:02"}) {
		t.Errorf("AttachedMACs(a1, wifi1) = %v", got)
	}
	if got := g.AttachedMACs("r", "sfp1"); len(got) != 0 {
		t.Errorf("AttachedMACs(r, sfp1) = %v, want empty (downlink)", got)
	}

	attach := []struct {
		mac       string
		dev, port string
		ok        bool
	}{
		{"AA:00:00:00:00:01", "a1", "wifi1", true},
		{"aa:00:00:00:00:02", "a1", "wifi1", true}, // lookup is case-insensitive
		{"BB:00:00:00:00:07", "s", "ether7", true},
		{mac("a2", "bridge"), "", "", false}, // managed MAC, never a client
		{"FF:FF:FF:FF:FF:FF", "", "", false},
	}
	for _, tt := range attach {
		dev, port, ok := g.AttachmentOf(tt.mac)
		if dev != tt.dev || port != tt.port || ok != tt.ok {
			t.Errorf("AttachmentOf(%s) = (%q, %q, %v), want (%q, %q, %v)", tt.mac, dev, port, ok, tt.dev, tt.port, tt.ok)
		}
	}
}

// Managed devices that do not speak MNDP still show up in switch FDBs. Their
// MACs are never clients: a port with only such a MAC is idle, and a port
// with both counts just the real client.
func TestRoleGraphManagedMACsAreNotClients(t *testing.T) {
	in, _ := meshFixture()
	in.Devices = append(in.Devices,
		PathDevice{ID: "cam", Name: "cam-switch", Address: "10.0.0.9"},
		PathDevice{ID: "ups", Name: "ups-card", Address: "10.0.0.10"})
	in.Interfaces = append(in.Interfaces, ifs("cam", "ether1:ether")...)
	in.Interfaces = append(in.Interfaces, ifs("ups", "ether1:ether")...)
	in.Hosts = append(in.Hosts, learned(mac("cam", "ether1"), "s/ether7", "c/q1", "r/sfp1")...)
	in.Hosts = append(in.Hosts, learned(mac("ups", "ether1"), "s/ether8", "c/q1", "r/sfp1")...)
	g := BuildRoleGraph(in)

	want := []PortRoleInfo{
		{DeviceID: "s", Iface: "ether7", Role: RoleAccess, ClientCount: 1, MACCount: 2},
		{DeviceID: "s", Iface: "ether8", Role: RoleIdle, MACCount: 1}, // FDB-only port, type guessed
	}
	for _, w := range want {
		if got := g.Role(w.DeviceID, w.Iface); got != w {
			t.Errorf("Role(%s, %s) =\n %+v\nwant\n %+v", w.DeviceID, w.Iface, got, w)
		}
	}
	if got := g.AttachedMACs("s", "ether7"); !reflect.DeepEqual(got, []string{"BB:00:00:00:00:07"}) {
		t.Errorf("AttachedMACs(s, ether7) = %v", got)
	}
	for _, m := range []string{mac("cam", "ether1"), mac("ups", "ether1")} {
		if dev, iface, ok := g.AttachmentOf(m); ok {
			t.Errorf("managed MAC %s attached at %s/%s", m, dev, iface)
		}
	}
}

// A MAC learned at edge ports on several devices (e.g. an unmanaged switch
// looping back, or stale FDB after a move) is attached at the deepest one.
func TestAttachmentPrefersDeepestEdgePort(t *testing.T) {
	in, _ := meshFixture()
	// c (depth 1) sorts before s (depth 2): depth must win over id order.
	in.Hosts = append(in.Hosts, learned("BB:00:00:00:00:07", "c/ether9")...)
	g := BuildRoleGraph(in)
	if got := g.Role("c", "ether9"); got.Role != RoleAccess || got.ClientCount != 1 {
		t.Fatalf("c/ether9 = %+v, want access with the client", got)
	}
	if dev, iface, ok := g.AttachmentOf("BB:00:00:00:00:07"); dev != "s" || iface != "ether7" || !ok {
		t.Errorf("AttachmentOf = (%q, %q, %v), want deepest edge s/ether7", dev, iface, ok)
	}
}

// A cable between two anchors is a peer link on both ends: it names the
// device across it, and the MACs learned there are that root's side of the
// network — transit, not clients attached at the port (live lab: the router's
// LAN port read "access · 70 clients" and claimed the firewall, then "idle"
// with no neighbour while carrying the router's whole LAN).
func TestRoleGraphPortFacingAnchorHasNoClients(t *testing.T) {
	in, _ := meshTwoAnchors()
	g := BuildRoleGraph(in)

	if d := treeOf(g, "c"); !d.Anchor || d.ParentID != "gw:192.168.1.254" {
		t.Fatalf("c = %+v, want anchor under gw:192.168.1.254", d)
	}
	if d := treeOf(g, "s"); d.ParentID != "c" || d.ParentIface != "q1" {
		t.Fatalf("s = %+v, want child of c via q1", d)
	}
	want := []PortRoleInfo{
		{DeviceID: "r", Iface: "sfp1", Role: RolePeer, NeighborDeviceID: "c", NeighborName: "C", NeighborIface: "sfp2", MACCount: 5},
		{DeviceID: "c", Iface: "sfp2", Role: RolePeer, NeighborDeviceID: "r", NeighborName: "R", NeighborIface: "sfp1", MACCount: 1},
		{DeviceID: "r", Iface: "ether5", Role: RoleAccess, ClientCount: 1, MACCount: 1},
		{DeviceID: "s", Iface: "ether7", Role: RoleAccess, ClientCount: 1, MACCount: 1},
	}
	for _, w := range want {
		if got := g.Role(w.DeviceID, w.Iface); got != w {
			t.Errorf("Role(%s, %s) =\n %+v\nwant\n %+v", w.DeviceID, w.Iface, got, w)
		}
	}
	if dev, iface, ok := g.AttachmentOf("FF:00:00:00:00:01"); ok {
		t.Errorf("firewall MAC attached at %s/%s, want no attachment (seen only on a wan port and an anchor link)", dev, iface)
	}
	// A peer link is not a tree edge: no path sink, never a Sankey leaf.
	hops, err := g.Path("r", "sfp1", nil)
	if err != nil || len(hops) != 2 || hops[1].OutIface != "sfp1" {
		t.Errorf("Path(r, sfp1) = %+v, %v; want internet → R without a sink", hops, err)
	}
	_, r := meshTwoAnchors()
	for _, dir := range []string{"download", "upload"} {
		sk := g.Sankey(r.fn(), SankeyOptions{Direction: dir})
		for _, id := range []string{"port:r:sfp1", "port:c:sfp2"} {
			if _, ok := sankeyNode(sk, id); ok {
				t.Errorf("%s: peer port %s became a Sankey leaf", dir, id)
			}
		}
	}
	for m, at := range map[string]string{"CC:00:00:00:00:05": "r/ether5", "BB:00:00:00:00:07": "s/ether7"} {
		if dev, iface, ok := g.AttachmentOf(m); !ok || dev+"/"+iface != at {
			t.Errorf("AttachmentOf(%s) = (%q, %q, %v), want %s", m, dev, iface, ok, at)
		}
	}
}

func treeOf(g *RoleGraph, id string) DeviceTreeInfo {
	for _, d := range g.Devices() {
		if d.DeviceID == id {
			return d
		}
	}
	return DeviceTreeInfo{}
}

func TestRoleUnknownInputs(t *testing.T) {
	in, _ := meshFixture()
	g := BuildRoleGraph(in)
	tests := []struct {
		dev, iface string
		role       string
		upstream   string
	}{
		{"r", "ether9", RoleIdle, ""},
		{"r", "sfp-sfpplus3", RoleIdle, ""},
		{"nope", "wlan3", RoleIdle, ""}, // wireless without clients
		{"nope", "wifi2", RoleIdle, ""},
		{"r", "wg-office", RoleVPN, "vpn:r:wg-office"},
		{"r", "l2tp-out1", RoleVPN, "vpn:r:l2tp-out1"},
		{"r", "vlan100", RoleVirtual, ""},
		{"r", "bridge2", RoleVirtual, ""},
		{"r", "green", RoleVirtual, ""}, // not a GRE tunnel
		{"r", "", RoleVirtual, ""},
	}
	for _, tt := range tests {
		t.Run(tt.dev+"/"+tt.iface, func(t *testing.T) {
			got := g.Role(tt.dev, tt.iface)
			if got.Role != tt.role || got.UpstreamNode != tt.upstream || got.DeviceID != tt.dev || got.Iface != tt.iface {
				t.Errorf("Role() = %+v, want role %q upstream %q", got, tt.role, tt.upstream)
			}
		})
	}
	var nilGraph *RoleGraph
	if got := nilGraph.Role("x", "ether1").Role; got != RoleIdle {
		t.Errorf("nil graph Role() = %q, want idle", got)
	}
}

// sankeyLinks maps "srcID>dstID" to the link.
func sankeyLinks(s Sankey) map[string]SankeyLink {
	out := map[string]SankeyLink{}
	for _, l := range s.Links {
		out[s.Nodes[l.Source].ID+">"+s.Nodes[l.Target].ID] = l
	}
	return out
}

func sankeyValues(s Sankey) map[string]int64 {
	out := map[string]int64{}
	for k, l := range sankeyLinks(s) {
		out[k] = l.Value
	}
	return out
}

func sankeyNode(s Sankey, id string) (SankeyNode, bool) {
	for _, n := range s.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return SankeyNode{}, false
}

func TestSankeyMesh(t *testing.T) {
	in, r := meshFixture()
	g := BuildRoleGraph(in)

	tests := []struct {
		dir  string
		want map[string]int64
	}{
		{"download", map[string]int64{
			"internet>r": 100 * mbps, "r>c": 95 * mbps, "c>s": 90 * mbps,
			"s>a1": 60 * mbps, "s>a2": 20 * mbps, "s>port:s:ether7": 5 * mbps,
			"a1>port:a1:wifi1": 55 * mbps,
		}},
		{"upload", map[string]int64{
			"internet>r": 10 * mbps, "r>c": 9 * mbps, "c>s": 8 * mbps,
			"s>a1": 5 * mbps, "s>a2": 2 * mbps, "s>port:s:ether7": 1 * mbps,
			"a1>port:a1:wifi1": 4 * mbps,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			s := g.Sankey(r.fn(), SankeyOptions{Direction: tt.dir})
			checkSankey(t, tt.dir, s, 1000)
			if s.Direction != tt.dir {
				t.Errorf("Direction = %q", s.Direction)
			}
			if got := sankeyValues(s); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("links =\n %v\nwant\n %v", got, tt.want)
			}
			if len(s.Nodes) != 8 {
				t.Errorf("len(Nodes) = %d, want 8 (A2 has no leaf)", len(s.Nodes))
			}
			links := sankeyLinks(s)
			for key, iface := range map[string]string{
				"internet>r": "", "r>c": "sfp1", "c>s": "q1", "s>a1": "ether23", "s>port:s:ether7": "ether7",
			} {
				if got := links[key].SourceIface; got != iface {
					t.Errorf("%s source_iface = %q, want %q", key, got, iface)
				}
			}
			if n, _ := sankeyNode(s, "internet"); n != (SankeyNode{ID: "internet", Name: "Internet", Type: "internet"}) {
				t.Errorf("internet node = %+v", n)
			}
			if n, _ := sankeyNode(s, "r"); n != (SankeyNode{ID: "r", Name: "R", Type: "device", DeviceID: "r"}) {
				t.Errorf("device node = %+v", n)
			}
			wantLeaf := SankeyNode{ID: "port:a1:wifi1", Name: "wifi1", Type: "port", DeviceID: "a1", Iface: "wifi1", ClientCount: 2}
			if n, _ := sankeyNode(s, "port:a1:wifi1"); n != wantLeaf {
				t.Errorf("leaf node = %+v, want %+v", n, wantLeaf)
			}
		})
	}
}

func TestSankeyEdgeValueFallbacks(t *testing.T) {
	in, base := meshFixture()
	g := BuildRoleGraph(in)
	tests := []struct {
		name  string
		rates rates
		key   string
		want  int64
	}{
		{"parent side", base, "r>c", 95 * mbps},
		{"child uplink rx", func() rates {
			r := base.without("r/sfp1")
			r["c/sfp2"] = [2]int64{93 * mbps, 9 * mbps}
			return r
		}(), "r>c", 93 * mbps},
		{"sum of child outflow", base.without("r/sfp1"), "r>c", 90 * mbps},
		{"wan unmeasured → outflow", base.without("r/eth1-wan"), "internet>r", 95 * mbps},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := g.Sankey(tt.rates.fn(), SankeyOptions{})
			checkSankey(t, tt.name, s, 1000)
			if got := sankeyValues(s)[tt.key]; got != tt.want {
				t.Errorf("%s = %d, want %d", tt.key, got, tt.want)
			}
		})
	}
}

// A measured zero on the parent side is a real zero: no fallback to the
// child's uplink, and the link drops with its subtree.
func TestSankeyMeasuredZeroIsNotAFallback(t *testing.T) {
	in, r := meshFixture()
	g := BuildRoleGraph(in)
	r = r.without()
	r["r/sfp1"] = [2]int64{0, 0}
	r["c/sfp2"] = [2]int64{93 * mbps, 9 * mbps}
	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "zero", s, 1000)
	for _, id := range []string{"c", "s", "a1", "a2"} {
		if _, ok := sankeyNode(s, id); ok {
			t.Errorf("node %q survived a measured-zero parent link", id)
		}
	}
	if got := sankeyValues(s)["internet>r"]; got != 100*mbps {
		t.Errorf("internet>r = %d, want wan rx", got)
	}
}

func TestSankeyPrunesBelowMinWithSubtree(t *testing.T) {
	in, r := meshFixture()
	g := BuildRoleGraph(in)
	r = r.without()
	r["s/ether5"] = [2]int64{0, 500}       // S→A2 below 1 kbps …
	r["a2/wifi1"] = [2]int64{0, 10 * mbps} // … so A2's busy leaf goes too
	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "prune", s, 1000)
	for _, id := range []string{"a2", "port:a2:wifi1"} {
		if _, ok := sankeyNode(s, id); ok {
			t.Errorf("node %q survived below-MinBps parent link", id)
		}
	}
	if got := sankeyValues(s)["s>a1"]; got != 60*mbps {
		t.Errorf("s>a1 = %d, sibling subtree must be untouched", got)
	}

	// MinBps above every value: nothing is left, and empty means [] not null.
	s = g.Sankey(r.fn(), SankeyOptions{MinBps: 1000 * mbps})
	b, _ := json.Marshal(s)
	if want := `{"estimated":true,"direction":"download","nodes":[],"links":[]}`; string(b) != want {
		t.Errorf("empty sankey JSON = %s, want %s", b, want)
	}
}

func TestSankeyRootDevice(t *testing.T) {
	in, r := meshFixture()
	g := BuildRoleGraph(in)

	s := g.Sankey(r.fn(), SankeyOptions{RootDeviceID: "s"})
	checkSankey(t, "root s", s, 1000)
	want := map[string]int64{
		"s>a1": 60 * mbps, "s>a2": 20 * mbps, "s>port:s:ether7": 5 * mbps, "a1>port:a1:wifi1": 55 * mbps,
	}
	if got := sankeyValues(s); !reflect.DeepEqual(got, want) {
		t.Errorf("links = %v, want %v", got, want)
	}
	if s.Nodes[0].ID != "s" {
		t.Errorf("root node = %q, want s", s.Nodes[0].ID)
	}

	for _, root := range []string{"unknown-device", "a2"} { // unknown; known but no flows
		s = g.Sankey(r.fn(), SankeyOptions{RootDeviceID: root})
		if len(s.Nodes) != 0 || len(s.Links) != 0 || s.Nodes == nil || s.Links == nil {
			t.Errorf("RootDeviceID %q: got %+v, want empty non-nil", root, s)
		}
	}
}

func TestSankeyLeafCap(t *testing.T) {
	in, r := leafFixture()
	g := BuildRoleGraph(in)
	tests := []struct {
		name       string
		opt        SankeyOptions
		wantLeaves int
		otherName  string
		otherValue int64
	}{
		{"default cap 8", SankeyOptions{}, 8, "+3 ports", (4 + 3 + 2) * mbps},
		{"cap 10", SankeyOptions{MaxLeavesPerDevice: 10}, 10, "+1 port", 2 * mbps},
		{"cap 20", SankeyOptions{MaxLeavesPerDevice: 20}, 11, "", 0},
		{"min 3M", SankeyOptions{MinBps: 3 * mbps}, 8, "+2 ports", (4 + 3) * mbps},
		{"upload ties", SankeyOptions{Direction: "upload"}, 8, "+3 ports", 3 * mbps / 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := g.Sankey(r.fn(), tt.opt)
			minBps := tt.opt.MinBps
			if minBps == 0 {
				minBps = 1000
			}
			checkSankey(t, tt.name, s, minBps)
			leaves := 0
			for _, n := range s.Nodes {
				if n.Type == "port" {
					leaves++
					if n.Iface == "ether13" {
						t.Errorf("sub-MinBps port ether13 became a leaf")
					}
				}
			}
			if leaves != tt.wantLeaves {
				t.Errorf("leaves = %d, want %d", leaves, tt.wantLeaves)
			}
			other, ok := sankeyNode(s, "other:e")
			if ok != (tt.otherName != "") || other.Name != tt.otherName {
				t.Errorf("other node = %+v (present %v), want name %q", other, ok, tt.otherName)
			}
			if got := sankeyValues(s)["e>other:e"]; got != tt.otherValue {
				t.Errorf("other value = %d, want %d", got, tt.otherValue)
			}
		})
	}
	// Download keeps the busiest ports.
	s := g.Sankey(r.fn(), SankeyOptions{})
	for i := 5; i <= 12; i++ {
		id := fmt.Sprintf("port:e:ether%d", i)
		if _, ok := sankeyNode(s, id); !ok {
			t.Errorf("busy port %s missing", id)
		}
	}
	if got := sankeyValues(s)["internet>e"]; got != 100*mbps {
		t.Errorf("internet>e = %d, want wan rx", got)
	}
}

func TestPathMesh(t *testing.T) {
	in, r := meshFixture()
	g := BuildRoleGraph(in)
	internet := Hop{Kind: "internet", ID: "internet", Label: "Internet"}
	hopR := Hop{Kind: "device", ID: "r", Label: "R", DeviceID: "r", InIface: "eth1-wan", OutIface: "sfp1", DownBps: 100 * mbps, UpBps: 10 * mbps, Measured: true}
	hopC := Hop{Kind: "device", ID: "c", Label: "C", DeviceID: "c", InIface: "sfp2", OutIface: "q1", DownBps: 95 * mbps, UpBps: 9 * mbps, Measured: true}
	hopS := func(out string) Hop {
		return Hop{Kind: "device", ID: "s", Label: "S", DeviceID: "s", InIface: "sfp1", OutIface: out, DownBps: 90 * mbps, UpBps: 8 * mbps, Measured: true}
	}

	tests := []struct {
		dev, iface string
		want       []Hop
	}{
		{"a1", "wifi1", []Hop{internet, hopR, hopC, hopS("ether23"),
			{Kind: "device", ID: "a1", Label: "A1", DeviceID: "a1", InIface: "ether1", OutIface: "wifi1", DownBps: 60 * mbps, UpBps: 5 * mbps, Measured: true},
			{Kind: "clients", ID: "clients:a1:wifi1", ClientCount: 2, Sink: true, DownBps: 55 * mbps, UpBps: 4 * mbps, Measured: true},
		}},
		{"s", "ether5", []Hop{internet, hopR, hopC, hopS("ether5"),
			{Kind: "device", ID: "a2", Label: "A2", DeviceID: "a2", InIface: "ether1", Sink: true, DownBps: 20 * mbps, UpBps: 2 * mbps, Measured: true},
		}},
		{"s", "ether7", []Hop{internet, hopR, hopC, hopS("ether7"),
			{Kind: "clients", ID: "clients:s:ether7", ClientCount: 1, Sink: true, DownBps: 5 * mbps, UpBps: 1 * mbps, Measured: true},
		}},
		{"r", "eth1-wan", []Hop{internet,
			{Kind: "device", ID: "r", Label: "R", DeviceID: "r", InIface: "eth1-wan", OutIface: "eth1-wan", DownBps: 100 * mbps, UpBps: 10 * mbps, Measured: true},
		}},
		{"c", "", []Hop{internet, hopR,
			{Kind: "device", ID: "c", Label: "C", DeviceID: "c", InIface: "sfp2", DownBps: 95 * mbps, UpBps: 9 * mbps, Measured: true},
		}},
		{"a2", "wifi1", []Hop{internet, hopR, hopC, hopS("ether5"), // idle: no sink
			{Kind: "device", ID: "a2", Label: "A2", DeviceID: "a2", InIface: "ether1", OutIface: "wifi1", DownBps: 20 * mbps, UpBps: 2 * mbps, Measured: true},
		}},
		{"s", "sfp1", []Hop{internet, hopR, hopC, hopS("sfp1")}}, // uplink: no sink
	}
	for _, tt := range tests {
		t.Run(tt.dev+"/"+tt.iface, func(t *testing.T) {
			got, err := g.Path(tt.dev, tt.iface, r.fn())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Path() =\n %+v\nwant\n %+v", got, tt.want)
			}
		})
	}

	if _, err := g.Path("nope", "ether1", r.fn()); !errors.Is(err, ErrUnknownDevice) {
		t.Errorf("unknown device err = %v, want ErrUnknownDevice", err)
	}
	var nilGraph *RoleGraph
	if _, err := nilGraph.Path("r", "", nil); !errors.Is(err, ErrUnknownDevice) {
		t.Errorf("nil graph err = %v", err)
	}
}

func TestPathSegmentFallsBackToInIface(t *testing.T) {
	in, r := meshFixture()
	g := BuildRoleGraph(in)
	r = r.without("c/q1")
	r["s/sfp1"] = [2]int64{88 * mbps, 7 * mbps}
	hops, err := g.Path("s", "ether7", r.fn())
	if err != nil {
		t.Fatal(err)
	}
	if h := hops[3]; h.ID != "s" || h.DownBps != 88*mbps || h.UpBps != 7*mbps || !h.Measured {
		t.Errorf("S hop = %+v, want rx/tx of its uplink sfp1", h)
	}

	hops, _ = g.Path("s", "ether7", nil)
	for _, h := range hops {
		if h.Measured || h.DownBps != 0 || h.UpBps != 0 {
			t.Errorf("nil rates: hop %+v measured", h)
		}
	}
}

// ---------- gateway-host + VPN ----------

func TestRoleGraphGatewayHostAndVPN(t *testing.T) {
	in, r := gwHostVPNFixture()
	g := BuildRoleGraph(in)
	if !g.Anchored() {
		t.Fatal("Anchored() = false")
	}
	wantDevs := []DeviceTreeInfo{
		{DeviceID: "sw", Name: "core-sw", ParentID: "gw:192.168.1.1", UplinkIface: "ether1", Depth: 0, Anchor: true},
		{DeviceID: "rt", Name: "remote-rt", ParentID: "vpn:rt:wg0", UplinkIface: "wg0", Depth: 0, Anchor: true},
		{DeviceID: "ap", Name: "ap-1", ParentID: "sw", ParentIface: "ether2", UplinkIface: "ether1", Depth: 1},
	}
	if got := g.Devices(); !reflect.DeepEqual(got, wantDevs) {
		t.Errorf("Devices() =\n %+v\nwant\n %+v", got, wantDevs)
	}

	roles := []PortRoleInfo{
		{DeviceID: "sw", Iface: "ether1", Role: RoleWAN, NeighborName: "fritz", MACCount: 1, GatewayIP: "192.168.1.1", UpstreamNode: "gw:192.168.1.1"},
		{DeviceID: "sw", Iface: "ether2", Role: RoleDownlink, NeighborDeviceID: "ap", NeighborName: "ap-1", NeighborIface: "ether1", NeighborCount: 1, MACCount: 2},
		{DeviceID: "sw", Iface: "eoip-tunnel1", Role: RoleVPN, UpstreamNode: "vpn:sw:eoip-tunnel1"},
		{DeviceID: "sw", Iface: "bridge", Role: RoleVirtual},
		{DeviceID: "ap", Iface: "ether1", Role: RoleUplink, NeighborDeviceID: "sw", NeighborName: "core-sw", NeighborIface: "ether2", MACCount: 1},
		{DeviceID: "ap", Iface: "wifi1", Role: RoleWireless, ClientCount: 1, MACCount: 1},
		{DeviceID: "rt", Iface: "wg0", Role: RoleVPN, UpstreamNode: "vpn:rt:wg0"},
		{DeviceID: "rt", Iface: "ether1", Role: RoleAccess, ClientCount: 2, MACCount: 2},
	}
	for _, want := range roles {
		if got := g.Role(want.DeviceID, want.Iface); got != want {
			t.Errorf("Role(%s, %s) =\n %+v\nwant\n %+v", want.DeviceID, want.Iface, got, want)
		}
	}
	if _, _, ok := g.AttachmentOf("CC:00:00:00:00:01"); ok {
		t.Error("gateway MAC on the wan port must not be a client")
	}

	paths := []struct {
		dev, iface string
		want       []Hop
	}{
		{"ap", "wifi1", []Hop{
			{Kind: "internet", ID: "internet", Label: "Internet"},
			{Kind: "gateway", ID: "gw:192.168.1.1", Label: "fritz"},
			{Kind: "device", ID: "sw", Label: "core-sw", DeviceID: "sw", InIface: "ether1", OutIface: "ether2", DownBps: 50 * mbps, UpBps: 5 * mbps, Measured: true},
			{Kind: "device", ID: "ap", Label: "ap-1", DeviceID: "ap", InIface: "ether1", OutIface: "wifi1", DownBps: 40 * mbps, UpBps: 3 * mbps, Measured: true},
			{Kind: "clients", ID: "clients:ap:wifi1", ClientCount: 1, Sink: true, DownBps: 35 * mbps, UpBps: 2 * mbps, Measured: true},
		}},
		{"rt", "ether1", []Hop{
			{Kind: "internet", ID: "internet", Label: "Internet"},
			{Kind: "vpn", ID: "vpn:rt:wg0", Label: "wg0"},
			{Kind: "device", ID: "rt", Label: "remote-rt", DeviceID: "rt", InIface: "wg0", OutIface: "ether1", DownBps: 20 * mbps, UpBps: 4 * mbps, Measured: true},
			{Kind: "clients", ID: "clients:rt:ether1", ClientCount: 2, Sink: true, DownBps: 18 * mbps, UpBps: 3 * mbps, Measured: true},
		}},
	}
	for _, tt := range paths {
		got, err := g.Path(tt.dev, tt.iface, r.fn())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("Path(%s, %s) =\n %+v\nwant\n %+v", tt.dev, tt.iface, got, tt.want)
		}
	}

	sankeys := []struct {
		dir  string
		want map[string]int64
	}{
		{"download", map[string]int64{
			"internet>gw:192.168.1.1": 50 * mbps, "gw:192.168.1.1>sw": 50 * mbps, "sw>ap": 40 * mbps,
			"ap>port:ap:wifi1": 35 * mbps, "internet>vpn:rt:wg0": 20 * mbps, "vpn:rt:wg0>rt": 20 * mbps,
			"rt>port:rt:ether1": 18 * mbps,
		}},
		{"upload", map[string]int64{
			"internet>gw:192.168.1.1": 5 * mbps, "gw:192.168.1.1>sw": 5 * mbps, "sw>ap": 3 * mbps,
			"ap>port:ap:wifi1": 2 * mbps, "internet>vpn:rt:wg0": 4 * mbps, "vpn:rt:wg0>rt": 4 * mbps,
			"rt>port:rt:ether1": 3 * mbps,
		}},
	}
	for _, tt := range sankeys {
		s := g.Sankey(r.fn(), SankeyOptions{Direction: tt.dir})
		checkSankey(t, tt.dir, s, 1000)
		if got := sankeyValues(s); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s links =\n %v\nwant\n %v", tt.dir, got, tt.want)
		}
		if n, _ := sankeyNode(s, "gw:192.168.1.1"); n.Type != "gateway" || n.Name != "fritz" {
			t.Errorf("gateway node = %+v", n)
		}
		if n, _ := sankeyNode(s, "vpn:rt:wg0"); n.Type != "vpn" || n.Name != "wg0" {
			t.Errorf("vpn node = %+v", n)
		}
		if _, ok := sankeyNode(s, "vpn:sw:eoip-tunnel1"); ok {
			t.Error("a tunnel that carries no default route is not on the tree")
		}
	}
}

func TestRoleGraphDualWAN(t *testing.T) {
	in, r := meshDualWAN()
	g := BuildRoleGraph(in)
	for iface, want := range map[string]PortRoleInfo{
		"eth1-wan": {DeviceID: "r", Iface: "eth1-wan", Role: RoleWAN, NeighborName: "Internet", UpstreamNode: "internet"},
		"lte1":     {DeviceID: "r", Iface: "lte1", Role: RoleWAN, NeighborName: "Internet", UpstreamNode: "internet"},
		"ether8":   {DeviceID: "r", Iface: "ether8", Role: RoleWAN, NeighborName: "192.168.8.1", GatewayIP: "192.168.8.1", UpstreamNode: "gw:192.168.8.1"},
	} {
		if got := g.Role("r", iface); got != want {
			t.Errorf("Role(r, %s) =\n %+v\nwant\n %+v", iface, got, want)
		}
	}
	devs := g.Devices()
	if devs[0].DeviceID != "r" || devs[0].ParentID != "internet" || devs[0].UplinkIface != "eth1-wan" {
		t.Errorf("r = %+v, want anchored on its first WAN port", devs[0])
	}

	// internet→R sums both internet WAN ports; the gateway-host port leads
	// to a different upstream and is not part of that edge.
	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "dual-wan", s, 1000)
	if got := sankeyValues(s)["internet>r"]; got != 130*mbps {
		t.Errorf("internet>r = %d, want eth1-wan rx + lte1 rx", got)
	}
	if _, ok := sankeyNode(s, "gw:192.168.8.1"); ok {
		t.Error("gateway not on R's upstream path is not in the tree")
	}
	s = g.Sankey(r.fn(), SankeyOptions{Direction: "upload"})
	if got := sankeyValues(s)["internet>r"]; got != 13*mbps {
		t.Errorf("upload internet>r = %d, want eth1-wan tx + lte1 tx", got)
	}

	for iface, want := range map[string]Hop{
		"lte1":     {Kind: "device", ID: "r", Label: "R", DeviceID: "r", InIface: "lte1", OutIface: "lte1", DownBps: 30 * mbps, UpBps: 3 * mbps, Measured: true},
		"eth1-wan": {Kind: "device", ID: "r", Label: "R", DeviceID: "r", InIface: "eth1-wan", OutIface: "eth1-wan", DownBps: 100 * mbps, UpBps: 10 * mbps, Measured: true},
		"ether8":   {Kind: "device", ID: "r", Label: "R", DeviceID: "r", InIface: "eth1-wan", OutIface: "ether8", DownBps: 100 * mbps, UpBps: 10 * mbps, Measured: true},
	} {
		hops, err := g.Path("r", iface, r.fn())
		if err != nil {
			t.Fatal(err)
		}
		if len(hops) != 2 || hops[1] != want {
			t.Errorf("Path(r, %s) = %+v, want [internet %+v]", iface, hops, want)
		}
	}
}

// ---------- no anchor / unreachable ----------

func TestRoleGraphNoRoutes(t *testing.T) {
	in, r := meshNoRoutes()
	g := BuildRoleGraph(in)
	if g.Anchored() {
		t.Fatal("Anchored() = true without any egress data")
	}
	byID := map[string]DeviceTreeInfo{}
	for _, d := range g.Devices() {
		byID[d.DeviceID] = d
		if d.Anchor || d.ParentID == "internet" || strings.HasPrefix(d.ParentID, "gw:") || strings.HasPrefix(d.ParentID, "vpn:") {
			t.Errorf("%s: synthetic parent %q without egress data", d.DeviceID, d.ParentID)
		}
	}
	// The fallback guesses uplinks from L2 adjacency alone (contract §11: the
	// top of the tree may come out wrong). The edge switch still resolves.
	for dev, port := range map[string]string{"a1": "ether23", "a2": "ether5"} {
		if d := byID[dev]; d.ParentID != "s" || d.ParentIface != port || d.UplinkIface != "ether1" {
			t.Errorf("%s = %+v, want child of s on %s", dev, d, port)
		}
	}
	for _, dir := range []string{"download", "upload"} {
		s := g.Sankey(r.fn(), SankeyOptions{Direction: dir})
		checkSankey(t, dir, s, 1000)
		for _, n := range s.Nodes {
			if n.Type == "internet" || n.Type == "gateway" || n.Type == "vpn" {
				t.Errorf("%s: synthetic node %q without egress data", dir, n.ID)
			}
		}
		if len(s.Links) == 0 {
			t.Errorf("%s: orphan roots must still carry their subtrees", dir)
		}
	}
	for _, d := range g.Devices() {
		hops, err := g.Path(d.DeviceID, "", r.fn())
		if err != nil {
			t.Fatal(err)
		}
		if hops[0].Kind != "device" || hops[0].InIface != "" {
			t.Errorf("Path(%s) starts with %+v, want the orphan root device", d.DeviceID, hops[0])
		}
	}
}

func TestRoleGraphRoutedGatewayRoots(t *testing.T) {
	in, r := meshRoutedGW()
	g := BuildRoleGraph(in)
	if g.Anchored() {
		t.Fatal("a routed-only private gateway is not an anchor")
	}
	roots := 0
	for _, d := range g.Devices() {
		if d.Depth != 0 {
			continue
		}
		roots++
		if d.ParentID != "gw:192.168.78.1" || d.ParentIface != "" {
			t.Errorf("root %s parent = %q/%q, want the routed gateway", d.DeviceID, d.ParentID, d.ParentIface)
		}
		hops, err := g.Path(d.DeviceID, "", r.fn())
		if err != nil {
			t.Fatal(err)
		}
		if len(hops) != 3 || hops[0].ID != "internet" || hops[1].ID != "gw:192.168.78.1" || hops[1].Label != "isp-box" {
			t.Fatalf("Path(%s) = %+v", d.DeviceID, hops)
		}
		// The routed edge has no port: unmeasured.
		if h := hops[2]; h.InIface != "" || h.Measured {
			t.Errorf("root hop behind routed gateway = %+v, want unmeasured without in_iface", h)
		}
		if ri := g.Role(d.DeviceID, d.UplinkIface); d.UplinkIface != "" && (ri.Role != RoleUplink || ri.UpstreamNode != "gw:192.168.78.1" || ri.NeighborName != "isp-box") {
			t.Errorf("root uplink role = %+v", ri)
		}
	}
	if roots == 0 {
		t.Fatal("no roots")
	}

	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "routed", s, 1000)
	gwIn := sankeyValues(s)["internet>gw:192.168.78.1"]
	var gwOut int64
	for k, v := range sankeyValues(s) {
		if strings.HasPrefix(k, "gw:192.168.78.1>") {
			gwOut += v
		}
	}
	if gwIn == 0 || gwIn != gwOut {
		t.Errorf("internet>gw = %d, gw outflow = %d: routed edges carry their subtree sums", gwIn, gwOut)
	}
}

func TestRoleGraphUnreachableIslands(t *testing.T) {
	in, r := meshWithIslands()
	g := BuildRoleGraph(in)
	if !g.Anchored() {
		t.Fatal("Anchored() = false")
	}
	byID := map[string]DeviceTreeInfo{}
	for _, d := range g.Devices() {
		byID[d.DeviceID] = d
	}
	if d := byID["iso"]; d.ParentID != "" || d.Depth != 0 || d.UplinkIface != "" || d.Anchor {
		t.Errorf("iso = %+v, want orphan", d)
	}
	if d := byID["iso2"]; d.ParentID != "gw:192.168.50.1" || d.Anchor {
		t.Errorf("iso2 = %+v, want under its routed gateway", d)
	}
	if d := byID["a1"]; d.ParentID != "s" {
		t.Errorf("islands must not disturb the anchored tree: a1 = %+v", d)
	}
	// Learned at an edge port on both islands (equal depth): smallest device id.
	if dev, iface, ok := g.AttachmentOf("DD:00:00:00:00:09"); dev != "iso" || iface != "ether2" || !ok {
		t.Errorf("AttachmentOf = (%q, %q, %v), want iso/ether2", dev, iface, ok)
	}

	hops, err := g.Path("iso", "ether2", r.fn())
	if err != nil {
		t.Fatal(err)
	}
	want := []Hop{
		{Kind: "device", ID: "iso", Label: "island", DeviceID: "iso", OutIface: "ether2"},
		{Kind: "clients", ID: "clients:iso:ether2", ClientCount: 1, Sink: true, DownBps: 7 * mbps, UpBps: 1 * mbps, Measured: true},
	}
	if !reflect.DeepEqual(hops, want) {
		t.Errorf("Path(iso) =\n %+v\nwant\n %+v", hops, want)
	}

	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "islands", s, 1000)
	vals := sankeyValues(s)
	for key, v := range map[string]int64{
		"internet>r": 100 * mbps, "iso>port:iso:ether2": 7 * mbps,
		"internet>gw:192.168.50.1": 3 * mbps, "gw:192.168.50.1>iso2": 3 * mbps, "iso2>port:iso2:ether1": 3 * mbps,
	} {
		if vals[key] != v {
			t.Errorf("%s = %d, want %d", key, vals[key], v)
		}
	}
}

// ---------- chains, parallel edges, rings ----------

func TestRoleGraphMultiHopChain(t *testing.T) {
	const n = 6
	in, r := chainFixture(n)
	g := BuildRoleGraph(in)
	for i, d := range g.Devices() {
		wantParent, wantPort, wantUp := fmt.Sprintf("d%d", i-1), "ether2", "ether1"
		if i == 0 {
			wantParent, wantPort, wantUp = "internet", "", "ether9"
		}
		if d.DeviceID != fmt.Sprintf("d%d", i) || d.ParentID != wantParent || d.ParentIface != wantPort ||
			d.UplinkIface != wantUp || d.Depth != i {
			t.Errorf("Devices()[%d] = %+v, want d%d under %s", i, d, i, wantParent)
		}
	}

	hops, err := g.Path("d5", "ether3", r.fn())
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, h := range hops {
		order = append(order, h.ID+"["+h.InIface+">"+h.OutIface+"]")
	}
	wantOrder := "internet[>] d0[ether9>ether2] d1[ether1>ether2] d2[ether1>ether2] d3[ether1>ether2] " +
		"d4[ether1>ether2] d5[ether1>ether3] clients:d5:ether3[>]"
	if got := strings.Join(order, " "); got != wantOrder {
		t.Errorf("path order =\n %s\nwant\n %s", got, wantOrder)
	}
	wantDown := []int64{0, 100, 90, 89, 88, 87, 86, 50}
	for i, h := range hops {
		if h.DownBps != wantDown[i]*mbps {
			t.Errorf("hop %d (%s) down = %d, want %d", i, h.ID, h.DownBps, wantDown[i]*mbps)
		}
	}
	if !hops[len(hops)-1].Sink || hops[len(hops)-1].ClientCount != 1 {
		t.Errorf("clients sink = %+v", hops[len(hops)-1])
	}

	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "chain", s, 1000)
	want := map[string]int64{"internet>d0": 100 * mbps, "d5>port:d5:ether3": 50 * mbps}
	for i := 0; i < n-1; i++ {
		want[fmt.Sprintf("d%d>d%d", i, i+1)] = int64(90-i) * mbps
	}
	if got := sankeyValues(s); !reflect.DeepEqual(got, want) {
		t.Errorf("chain links =\n %v\nwant\n %v", got, want)
	}
}

func TestRoleGraphParallelEdgesCollapse(t *testing.T) {
	tests := []struct {
		name     string
		withFDB  bool
		port     string // port carrying x→y
		peer     []string
		wantFlow int64
	}{
		{"no FDB: smallest wired port", false, "ether1", []string{"ether2", "bond1"}, 7 * mbps},
		{"FDB picks the bond", true, "bond1", []string{"ether1", "ether2"}, 8 * mbps},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, r := parallelFixture(tt.withFDB)
			g := BuildRoleGraph(in)
			var y DeviceTreeInfo
			for _, d := range g.Devices() {
				if d.DeviceID == "y" {
					y = d
				}
			}
			if y.ParentID != "x" || y.ParentIface != tt.port || y.UplinkIface != tt.port {
				t.Errorf("y = %+v, want child of x via %s", y, tt.port)
			}
			if got := g.Role("x", tt.port); got.Role != RoleDownlink || got.NeighborDeviceID != "y" || got.NeighborIface != tt.port {
				t.Errorf("x/%s = %+v, want downlink to y", tt.port, got)
			}
			// The parallel links the tree did not choose are peer links to y.
			for _, p := range tt.peer {
				if got := g.Role("x", p); got.Role != RolePeer || got.NeighborDeviceID != "y" || got.NeighborIface != p {
					t.Errorf("x/%s = %+v, want peer to y/%s", p, got, p)
				}
			}
			for _, p := range []string{"bridge", "vlan10"} {
				if got := g.Role("x", p).Role; got != RoleVirtual {
					t.Errorf("x/%s role = %q, want virtual", p, got)
				}
			}
			s := g.Sankey(r.fn(), SankeyOptions{})
			checkSankey(t, tt.name, s, 1000)
			between := 0
			for _, l := range s.Links {
				if s.Nodes[l.Source].ID == "x" && s.Nodes[l.Target].ID == "y" {
					between++
					if l.Value != tt.wantFlow || l.SourceIface != tt.port {
						t.Errorf("x>y = %+v, want %d via %s", l, tt.wantFlow, tt.port)
					}
				}
			}
			if between != 1 {
				t.Errorf("x>y links = %d, want exactly 1", between)
			}
		})
	}
}

func TestChoosePort(t *testing.T) {
	set := func(ps ...string) map[string]bool {
		m := map[string]bool{}
		for _, p := range ps {
			m[p] = true
		}
		return m
	}
	wired := func(p string) bool { return IsPhysicalPort(p, "") }
	tests := []struct {
		name string
		l, f map[string]bool
		want string
	}{
		{"single MNDP port", set("ether1"), nil, "ether1"},
		{"single MNDP port beats FDB", set("ether1"), set("ether2"), "ether1"},
		{"parallel: FDB agrees", set("ether1", "ether2"), set("ether2"), "ether2"},
		{"parallel: FDB elsewhere → smallest wired", set("ether2", "ether1"), set("ether3"), "ether1"},
		{"parallel: wired beats bond", set("bond1", "ether3", "ether2"), nil, "ether2"},
		{"parallel: radios only → smallest", set("wlan2", "wlan1"), nil, "wlan1"},
		{"FDB only: wired first", nil, set("wlan1", "sfp2"), "sfp2"},
		{"FDB only: radios", nil, set("wlan2", "wlan1"), "wlan1"},
		{"nothing", nil, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := choosePort(tt.l, tt.f, wired); got != tt.want {
				t.Errorf("choosePort() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRoleGraphRingWithoutAnchorIsATree(t *testing.T) {
	in, r := ring3Fixture()
	g := BuildRoleGraph(in)
	// Without breaking, the inference yields a→b→c→a. Walking from "a" the
	// revisit is detected at c, whose parent is cut: c becomes the root.
	want := []DeviceTreeInfo{
		{DeviceID: "c", Name: "C", ParentID: "", UplinkIface: "ether1", Depth: 0},
		{DeviceID: "b", Name: "B", ParentID: "c", ParentIface: "ether2", UplinkIface: "ether1", Depth: 1},
		{DeviceID: "a", Name: "A", ParentID: "b", ParentIface: "ether2", UplinkIface: "ether1", Depth: 2},
	}
	if got := g.Devices(); !reflect.DeepEqual(got, want) {
		t.Errorf("Devices() =\n %+v\nwant\n %+v", got, want)
	}
	if got := g.Role("a", "ether2"); got.Role != RolePeer || got.NeighborDeviceID != "c" || got.NeighborIface != "ether1" {
		t.Errorf("a/ether2 (the ring's cut edge) = %+v, want peer to c/ether1", got)
	}
	hops, err := g.Path("a", "ether3", r.fn())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range hops {
		ids = append(ids, h.ID)
	}
	if got := strings.Join(ids, " "); got != "c b a clients:a:ether3" {
		t.Errorf("path = %s", got)
	}
	// Download: a sends 9M to its client but receives 8M from b — the
	// surplus comes from a's local source.
	for dir, want := range map[string]string{
		"download": "a>port:a:ether3 b>a c>b local:a>a",
		"upload":   "a>port:a:ether3 b>a c>b",
	} {
		s := g.Sankey(r.fn(), SankeyOptions{Direction: dir})
		checkSankey(t, dir, s, 1000)
		var keys []string
		for k := range sankeyValues(s) {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if got := strings.Join(keys, " "); got != want {
			t.Errorf("%s: links %s, want %s", dir, got, want)
		}
	}
}

func TestRoleGraphRingWithAnchor(t *testing.T) {
	in, r := ring4Fixture()
	g := BuildRoleGraph(in)
	want := map[string][2]string{"A": {"R", "ether1"}, "B": {"A", "ether2"}, "C": {"R", "ether2"}}
	for _, d := range g.Devices() {
		if d.DeviceID == "R" {
			if d.ParentID != "internet" || !d.Anchor {
				t.Errorf("R = %+v", d)
			}
			continue
		}
		if w := want[d.DeviceID]; d.ParentID != w[0] || d.ParentIface != w[1] || d.UplinkIface != "ether1" {
			t.Errorf("%s = %+v, want parent %s via %s", d.DeviceID, d, w[0], w[1])
		}
	}
	for p, across := range map[string]string{"B/ether2": "C", "C/ether2": "B"} { // the blocked link
		dev, iface, _ := strings.Cut(p, "/")
		if got := g.Role(dev, iface); got.Role != RolePeer || got.NeighborDeviceID != across || got.NeighborIface != "ether2" {
			t.Errorf("%s = %+v, want peer to %s/ether2", p, got, across)
		}
	}
	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "ring4", s, 1000)
	wantLinks := map[string]int64{"internet>R": 30 * mbps, "R>A": 20 * mbps, "A>B": 12 * mbps, "R>C": 9 * mbps}
	if got := sankeyValues(s); !reflect.DeepEqual(got, wantLinks) {
		t.Errorf("links = %v, want %v", got, wantLinks)
	}
}

func TestBreakCycles(t *testing.T) {
	tests := []struct {
		name   string
		parent map[string]string
		want   map[string]string
	}{
		{"three-cycle", map[string]string{"a": "b", "b": "c", "c": "a"}, map[string]string{"a": "b", "b": "c"}},
		{"cycle not through the start", map[string]string{"a": "b", "b": "c", "c": "b"}, map[string]string{"a": "b", "b": "c"}},
		{"two-cycle", map[string]string{"a": "b", "b": "a"}, map[string]string{"a": "b"}},
		{"two separate cycles", map[string]string{"a": "b", "b": "a", "c": "d", "d": "c"}, map[string]string{"a": "b", "c": "d"}},
		{"self loop", map[string]string{"a": "a"}, map[string]string{}},
		{"tree untouched", map[string]string{"b": "a", "c": "a", "d": "c"}, map[string]string{"b": "a", "c": "a", "d": "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids := map[string]bool{}
			for k, v := range tt.parent {
				ids[k], ids[v] = true, true
			}
			var byID []string
			for id := range ids {
				byID = append(byID, id)
			}
			sort.Strings(byID)
			breakCycles(byID, tt.parent)
			if !reflect.DeepEqual(tt.parent, tt.want) {
				t.Errorf("breakCycles() = %v, want %v", tt.parent, tt.want)
			}
		})
	}
}

// ---------- helpers ----------

func TestIsPhysicalPort(t *testing.T) {
	tests := []struct {
		name, typ string
		want      bool
	}{
		{"ether1", "ether", true},
		{"uplink", "ether", true},
		{"sfp-sfpplus1", "", true},
		{"SFP28-1", "", true},
		{"qsfpplus1-1", "", true},
		{"combo1", "", true},
		{"bridge", "bridge", false},
		{"wifi1", "wifi", false},
		{"vlan10", "vlan", false},
		{"eth1-wan", "", false}, // same rule as the port grid: needs type ether
	}
	for _, tt := range tests {
		if got := IsPhysicalPort(tt.name, tt.typ); got != tt.want {
			t.Errorf("IsPhysicalPort(%q, %q) = %v, want %v", tt.name, tt.typ, got, tt.want)
		}
	}
}

func TestIsWirelessIfaceType(t *testing.T) {
	for typ, want := range map[string]bool{
		"wifi": true, "wlan": true, "wireless": true, "cap": true, "CAP": true, "60g": true, "wifiwave2": true,
		"ether": false, "bridge": false, "": false,
	} {
		if got := IsWirelessIfaceType(typ); got != want {
			t.Errorf("IsWirelessIfaceType(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestIsAdjacencyPort(t *testing.T) {
	tests := []struct {
		name, typ string
		want      bool
	}{
		{"ether1", "ether", true},
		{"sfp1", "", true},
		{"wifi1", "wifi", true},
		{"cap3", "cap", true},
		{"bond1", "bond", true},
		{"net28", "vlan", false},
		{"ether1-vlan10", "vlan", false}, // a VLAN named after its parent port
		{"bridge", "bridge", false},
		{"lo", "loopback", false},
		{"veth1", "veth", false},
		{"wg0", "wg", false},
		{"eoip-ether1", "eoip", false},
		{"net28", "", false},
	}
	for _, tt := range tests {
		if got := isAdjacencyPort(tt.name, tt.typ); got != tt.want {
			t.Errorf("isAdjacencyPort(%q, %q) = %v, want %v", tt.name, tt.typ, got, tt.want)
		}
	}
}

func TestNormPort(t *testing.T) {
	for in, want := range map[string]string{
		"ether1,bridge": "ether1", "net28": "net28", "unknown": "", "": "", " sfp1 ,bridge": "sfp1", "UNKNOWN": "",
	} {
		if got := normPort(in); got != want {
			t.Errorf("normPort(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGuessIfaceType(t *testing.T) {
	for name, want := range map[string]string{
		"ether5": "ether", "sfp-sfpplus1": "ether", "wlan1": "wifi", "wifi2": "wifi", "bond1": "bond",
		"bridge1": "bridge", "vlan78": "vlan", "lo": "loopback", "wg0": "wg", "wireguard1": "wg",
		"gre-tunnel1": "gre", "green": "", "eoip1": "eoip", "ovpn-out1": "ovpn-out", "net28": "",
	} {
		if got := guessIfaceType(name); got != want {
			t.Errorf("guessIfaceType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestRoleGraphEmptyAndNil(t *testing.T) {
	var nilGraph *RoleGraph
	for _, g := range []*RoleGraph{BuildRoleGraph(PathInput{}), nilGraph} {
		if g.Anchored() || len(g.Roles()) != 0 || len(g.Devices()) != 0 {
			t.Errorf("empty graph not empty")
		}
		if g.Roles() == nil || g.Devices() == nil || g.AttachedMACs("x", "y") == nil {
			t.Errorf("empty listings must be [] not nil")
		}
		s := g.Sankey(nil, SankeyOptions{Direction: "upload"})
		if s.Direction != "upload" || s.Nodes == nil || s.Links == nil || len(s.Nodes) != 0 {
			t.Errorf("empty sankey = %+v", s)
		}
		if _, _, ok := g.AttachmentOf("AA:BB:CC:DD:EE:FF"); ok {
			t.Error("AttachmentOf on empty graph")
		}
	}
}

// TestBuildRoleGraphIgnoresUnknownDevices: rows that reference devices not in
// Devices (deleted between queries) must not create phantom nodes.
func TestBuildRoleGraphIgnoresUnknownDevices(t *testing.T) {
	in, r := meshFixture()
	in.Links = append(in.Links, up("r", "sfp1,bridge", "ghost", "ether1"), up("ghost", "ether1", "s", "ether7"))
	in.Uplinks = append(in.Uplinks, PathUplink{DeviceID: "ghost", Kind: "default-route", Interface: "ether1", GatewayIP: "198.51.100.66"})
	in.Hosts = append(in.Hosts, PathHost{DeviceID: "ghost", Iface: "ether2", MAC: "AA:AA:AA:AA:AA:AA"})
	in.Interfaces = append(in.Interfaces, PathIface{DeviceID: "ghost", Name: "ether9", MAC: "12:34:56:78:9A:BC"})
	g := BuildRoleGraph(in)
	for _, d := range g.Devices() {
		if d.DeviceID == "ghost" {
			t.Fatal("ghost device in tree")
		}
	}
	for _, ri := range g.Roles() {
		if ri.DeviceID == "ghost" {
			t.Fatalf("ghost port %+v", ri)
		}
	}
	base, _ := meshFixture()
	if got, want := g.Devices(), BuildRoleGraph(base).Devices(); !reflect.DeepEqual(got, want) {
		t.Errorf("unknown-device rows changed the tree:\n %+v\nwant\n %+v", got, want)
	}
	checkGraph(t, "ghost", in, r)
}

// The exported surface is a cross-role contract (backend-api builds on it):
// pin the signatures at compile time.
var (
	_ func(PathInput) *RoleGraph                                = BuildRoleGraph
	_ func(*RoleGraph) bool                                     = (*RoleGraph).Anchored
	_ func(*RoleGraph, string, string) PortRoleInfo             = (*RoleGraph).Role
	_ func(*RoleGraph) []PortRoleInfo                           = (*RoleGraph).Roles
	_ func(*RoleGraph) []DeviceTreeInfo                         = (*RoleGraph).Devices
	_ func(*RoleGraph, string) (string, string, bool)           = (*RoleGraph).AttachmentOf
	_ func(*RoleGraph, string, string) []string                 = (*RoleGraph).AttachedMACs
	_ func(*RoleGraph, string, string, RateFunc) ([]Hop, error) = (*RoleGraph).Path
	_ func(*RoleGraph, RateFunc, SankeyOptions) Sankey          = (*RoleGraph).Sankey
	_ func(string, string) bool                                 = IsPhysicalPort
	_ func(string) bool                                         = IsWirelessIfaceType
	_ RateFunc                                                  = func(string, string) (int64, int64, bool) { return 0, 0, false }
	_                                                           = [...]string{RoleWAN, RoleUplink, RoleDownlink, RolePeer, RoleAccess, RoleWireless, RoleVPN, RoleVirtual, RoleIdle}
)

// The API layer caches one graph and serves it from concurrent handlers.
func TestRoleGraphConcurrentReads(t *testing.T) {
	in, r := meshWithIslands()
	g := BuildRoleGraph(in)
	want := g.Sankey(r.fn(), SankeyOptions{})
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				_ = g.Roles()
				_ = g.Devices()
				_, _, _ = g.AttachmentOf("AA:00:00:00:00:01")
				_ = g.AttachedMACs("a1", "wifi1")
				if _, err := g.Path("a1", "wifi1", r.fn()); err != nil {
					t.Error(err)
				}
				dir := "download"
				if i%2 == 1 {
					dir = "upload"
				}
				s := g.Sankey(r.fn(), SankeyOptions{Direction: dir})
				if dir == "download" && !reflect.DeepEqual(s, want) {
					t.Error("concurrent Sankey differs")
				}
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// ---------- egress-based uplinks, peer links, bonds, carriers ----------

// meshSecondEdge adds a second internet edge in another branch: router
// "Alab" (interface-only LTE default route, so an internet anchor whose name
// sorts before R) cabled to S.ether10. Everyone else still routes via R.
func meshSecondEdge() (PathInput, rates) {
	in, r := meshFixture()
	in.Devices = append(in.Devices, PathDevice{ID: "alab", Name: "Alab", Type: "router", Address: "10.0.0.9"})
	in.Interfaces = append(in.Interfaces, ifs("alab", "ether1:ether", "lte1:lte", "bridge:bridge")...)
	in.Interfaces = append(in.Interfaces, ifs("s", "ether10:ether")...)
	in.Links = append(in.Links,
		up("alab", "ether1,bridge", "s", "ether10,bridge"),
		up("alab", "ether1,bridge", "c", "q1,bridge"),
		up("alab", "ether1,bridge", "r", "sfp1,bridge"),
		up("alab", "ether1,bridge", "a1", "ether1,bridge"),
		up("alab", "ether1,bridge", "a2", "ether1,bridge"))
	in.Uplinks = append(in.Uplinks, PathUplink{DeviceID: "alab", Kind: "default-route", Interface: "lte1", IfaceType: "lte"})
	in.Hosts = append(in.Hosts, learned(mac("alab", "bridge"), "s/ether10", "c/q1", "r/sfp1")...)
	r["alab/lte1"] = [2]int64{7 * mbps, 1 * mbps}
	r["s/ether10"] = [2]int64{1 * mbps, 2 * mbps}
	return in, r
}

// ringPeerFixture is a forwarding ring R–A–B–C–A behind an internet edge R:
// the A.ether3–C.ether2 closure carries traffic (e.g. another MSTP instance)
// and MNDP both ways, but the tree runs R → A → B → C. MNDP over the flat L2
// also hears every device along the tree paths.
func ringPeerFixture() (PathInput, rates) {
	in := PathInput{
		Devices: []PathDevice{
			{ID: "R", Name: "R", Address: "10.4.0.1"}, {ID: "A", Name: "A", Address: "10.4.0.2"},
			{ID: "B", Name: "B", Address: "10.4.0.3"}, {ID: "C", Name: "C", Address: "10.4.0.4"},
		},
		Links: []PathLink{
			up("R", "ether1,bridge", "A", "ether1,bridge"),
			up("R", "ether1,bridge", "B", "ether1,bridge"),
			up("R", "ether1,bridge", "C", "ether1,bridge"),
			up("A", "ether2,bridge", "B", "ether1,bridge"),
			up("A", "ether2,bridge", "C", "ether1,bridge"),
			up("B", "ether2,bridge", "C", "ether1,bridge"),
			up("A", "ether3,bridge", "C", "ether2,bridge"), // the ring closure
		},
		Uplinks: []PathUplink{{DeviceID: "R", Kind: "default-route", Interface: "ether9", IfaceType: "ether", GatewayIP: "198.51.100.4"}},
		Interfaces: concat(
			ifs("R", "ether9:ether", "ether1:ether", "bridge:bridge"),
			ifs("A", "ether1:ether", "ether2:ether", "ether3:ether", "bridge:bridge"),
			ifs("B", "ether1:ether", "ether2:ether", "bridge:bridge"),
			ifs("C", "ether1:ether", "ether2:ether", "ether5:ether", "bridge:bridge"),
		),
		Hosts: concat(
			learned(mac("R", "bridge"), "A/ether1", "B/ether1", "C/ether1"),
			learned(mac("A", "bridge"), "R/ether1", "B/ether1", "C/ether1"),
			learned(mac("B", "bridge"), "R/ether1", "A/ether2", "C/ether1"),
			learned(mac("C", "bridge"), "R/ether1", "A/ether2", "B/ether2"),
			learned("AA:00:00:00:0C:05", "C/ether5", "B/ether2", "A/ether2", "R/ether1"),
		),
	}
	for _, d := range []string{"A", "B", "C"} {
		in.Uplinks = append(in.Uplinks, PathUplink{DeviceID: d, Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "10.4.0.1"})
	}
	return in, rates{
		"R/ether9": {50 * mbps, 5 * mbps}, "R/ether1": {5 * mbps, 45 * mbps},
		"A/ether2": {4 * mbps, 30 * mbps}, "A/ether3": {3 * mbps, 10 * mbps},
		"B/ether2": {3 * mbps, 20 * mbps}, "C/ether2": {10 * mbps, 3 * mbps},
		"C/ether5": {2 * mbps, 18 * mbps},
	}
}

// bondFixture: a switch X (internet edge) whose LACP bond1 (ether1+ether2)
// trunks to switch Y, where MNDP reports the member ports; Y serves a NAS on
// bond2 (ether7+ether8). Bridge FDBs learn on the bonds, never the members.
func bondFixture() (PathInput, rates) {
	in := PathInput{
		Devices: []PathDevice{{ID: "x", Name: "X", Address: "10.5.0.1"}, {ID: "y", Name: "Y", Address: "10.5.0.2"}},
		Links: []PathLink{
			up("x", "ether1,bridge", "y", "ether1,bridge"),
			up("x", "ether2,bridge", "y", "ether2,bridge"),
		},
		Uplinks: []PathUplink{
			{DeviceID: "x", Kind: "default-route", Interface: "ether9", IfaceType: "ether", GatewayIP: "198.51.100.5"},
			{DeviceID: "y", Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "10.5.0.1"},
		},
		Interfaces: concat(
			ifs("x", "ether9:ether", "ether1:ether", "ether2:ether", "bond1:bond", "bridge:bridge"),
			ifs("y", "ether1:ether", "ether2:ether", "bond1:bond", "ether7:ether", "ether8:ether", "bond2:bond", "bridge:bridge"),
		),
		Hosts: concat(
			learned(mac("y", "bridge"), "x/bond1"),
			learned(mac("x", "bridge"), "y/bond1"),
			learned("AA:00:00:00:0D:01", "y/bond2", "x/bond1"), // the NAS
		),
		Relations: []PathRelation{
			{DeviceID: "x", Iface: "ether1", Parent: "bond1", Kind: RelationBond},
			{DeviceID: "x", Iface: "ether2", Parent: "bond1", Kind: RelationBond},
			{DeviceID: "y", Iface: "ether1", Parent: "bond1", Kind: RelationBond},
			{DeviceID: "y", Iface: "ether2", Parent: "bond1", Kind: RelationBond},
			{DeviceID: "y", Iface: "ether7", Parent: "bond2", Kind: RelationBond},
			{DeviceID: "y", Iface: "ether8", Parent: "bond2", Kind: RelationBond},
		},
	}
	return in, rates{
		"x/ether9": {100 * mbps, 10 * mbps},
		"x/bond1":  {8 * mbps, 90 * mbps}, "x/ether1": {4 * mbps, 45 * mbps}, "x/ether2": {4 * mbps, 45 * mbps},
		"y/bond2": {6 * mbps, 80 * mbps}, "y/ether7": {3 * mbps, 40 * mbps}, "y/ether8": {3 * mbps, 40 * mbps},
	}
}

// pppoeFixture: the stock home edge — a PPPoE client over a WAN VLAN over
// ether1 (pppoe-out1 → vlan-wan → ether1), clients on ether2.
func pppoeFixture() (PathInput, rates) {
	in := PathInput{
		Devices:    []PathDevice{{ID: "r", Name: "R", Address: "192.168.88.1"}},
		Interfaces: ifs("r", "ether1:ether", "ether2:ether", "vlan-wan:vlan", "pppoe-out1:pppoe-out", "bridge:bridge"),
		Uplinks:    []PathUplink{{DeviceID: "r", Kind: "default-route", Interface: "pppoe-out1", IfaceType: "pppoe-out"}},
		Hosts:      learned("AA:00:00:00:0E:02", "r/ether2"),
		Relations: []PathRelation{
			{DeviceID: "r", Iface: "pppoe-out1", Parent: "vlan-wan", Kind: RelationPPPoE},
			{DeviceID: "r", Iface: "vlan-wan", Parent: "ether1", Kind: RelationVLAN},
		},
	}
	return in, rates{
		"r/pppoe-out1": {300 * mbps, 10 * mbps}, "r/vlan-wan": {303 * mbps, 11 * mbps},
		"r/ether1": {305 * mbps, 11 * mbps}, "r/ether2": {9 * mbps, 290 * mbps},
	}
}

// meshSharedPort: A1 and A2 both hang off S.ether5 through an unmanaged
// switch, so S.ether5's total is shared by two children.
func meshSharedPort() (PathInput, rates) {
	in, r := meshFixture()
	var links []PathLink
	for _, l := range in.Links {
		if l.DeviceA == "s" && l.IfaceA == "ether23,bridge" {
			l.IfaceA = "ether5,bridge"
		}
		links = append(links, l)
	}
	in.Links = links
	var hosts []PathHost
	for _, h := range in.Hosts {
		if h.DeviceID == "s" && h.Iface == "ether23" {
			h.Iface = "ether5"
		}
		hosts = append(hosts, h)
	}
	in.Hosts = hosts
	r = r.without("s/ether23")
	r["s/ether5"] = [2]int64{7 * mbps, 100 * mbps}
	r["a1/ether1"] = [2]int64{70 * mbps, 5 * mbps}
	r["a2/ether1"] = [2]int64{30 * mbps, 2 * mbps}
	return in, r
}

func TestRoleGraphNewFixturesInvariants(t *testing.T) {
	for _, f := range []fixture{
		{"mesh-second-edge", meshSecondEdge}, {"ring-peer", ringPeerFixture}, {"bond", bondFixture},
		{"pppoe", pppoeFixture}, {"mesh-shared-port", meshSharedPort},
	} {
		t.Run(f.name, func(t *testing.T) {
			in, r := f.build()
			checkGraph(t, f.name, in, r)
		})
	}
}

// A second internet edge in another branch must not turn the tree upside
// down: every device takes the uplink toward its own default gateway, not
// toward whichever anchor sorts first by name.
func TestRoleGraphSecondEdgeElsewhere(t *testing.T) {
	in, r := meshSecondEdge()
	g := BuildRoleGraph(in)
	for dev, want := range map[string][2]string{"c": {"r", "sfp1"}, "s": {"c", "q1"}, "a1": {"s", "ether23"}, "a2": {"s", "ether5"}} {
		if d := treeOf(g, dev); d.ParentID != want[0] || d.ParentIface != want[1] {
			t.Errorf("%s = %+v, want child of %s via %s", dev, d, want[0], want[1])
		}
	}
	if d := treeOf(g, "alab"); !d.Anchor || d.ParentID != "internet" || d.UplinkIface != "lte1" {
		t.Errorf("alab = %+v, want an internet anchor on lte1", d)
	}
	for _, k := range []string{"r/sfp1", "c/q1"} {
		dev, iface, _ := strings.Cut(k, "/")
		if got := g.Role(dev, iface).Role; got != RoleDownlink {
			t.Errorf("%s = %q, want downlink (the real trunk)", k, got)
		}
	}
	if got := g.Role("s", "ether10"); got.Role != RolePeer || got.NeighborDeviceID != "alab" || got.NeighborIface != "ether1" {
		t.Errorf("s/ether10 = %+v, want peer to alab/ether1", got)
	}
	s := g.Sankey(r.fn(), SankeyOptions{})
	vals := sankeyValues(s)
	for key, v := range map[string]int64{"internet>r": 100 * mbps, "r>c": 95 * mbps, "c>s": 90 * mbps, "internet>alab": 7 * mbps} {
		if vals[key] != v {
			t.Errorf("%s = %d, want %d", key, vals[key], v)
		}
	}
	if _, ok := sankeyNode(s, "port:s:ether10"); ok {
		t.Error("peer port s/ether10 became a Sankey leaf")
	}
}

// A forwarding ring closure is a peer link: named on both ends, visible (it
// carries traffic) but neither a tree edge, a path sink nor a Sankey leaf.
func TestRoleGraphRingPeerLink(t *testing.T) {
	in, r := ringPeerFixture()
	g := BuildRoleGraph(in)
	for dev, want := range map[string][2]string{"A": {"R", "ether1"}, "B": {"A", "ether2"}, "C": {"B", "ether2"}} {
		if d := treeOf(g, dev); d.ParentID != want[0] || d.ParentIface != want[1] || d.UplinkIface != "ether1" {
			t.Errorf("%s = %+v, want child of %s via %s", dev, d, want[0], want[1])
		}
	}
	want := map[string]PortRoleInfo{
		"A/ether3": {DeviceID: "A", Iface: "ether3", Role: RolePeer, NeighborDeviceID: "C", NeighborName: "C", NeighborIface: "ether2"},
		"C/ether2": {DeviceID: "C", Iface: "ether2", Role: RolePeer, NeighborDeviceID: "A", NeighborName: "A", NeighborIface: "ether3"},
		"C/ether5": {DeviceID: "C", Iface: "ether5", Role: RoleAccess, ClientCount: 1, MACCount: 1},
	}
	for key, w := range want {
		dev, iface, _ := strings.Cut(key, "/")
		if got := g.Role(dev, iface); got != w {
			t.Errorf("%s =\n %+v\nwant\n %+v", key, got, w)
		}
	}
	hops, err := g.Path("A", "ether3", r.fn())
	if err != nil {
		t.Fatal(err)
	}
	if last := hops[len(hops)-1]; last.Sink || last.DeviceID != "A" || last.OutIface != "ether3" {
		t.Errorf("Path(A, ether3) ends with %+v, want A without a sink", last)
	}
	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "ring-peer", s, 1000)
	wantLinks := map[string]int64{"internet>R": 50 * mbps, "R>A": 45 * mbps, "A>B": 30 * mbps, "B>C": 20 * mbps, "C>port:C:ether5": 18 * mbps}
	if got := sankeyValues(s); !reflect.DeepEqual(got, wantLinks) {
		t.Errorf("links = %v, want %v (no peer leaves)", got, wantLinks)
	}
}

// Bond members fold into their bond: the tree edge and the Sankey use the
// bond, members take its role with Master set, and never become leaves (the
// bond already counts their traffic).
func TestRoleGraphBondMembers(t *testing.T) {
	in, r := bondFixture()
	g := BuildRoleGraph(in)
	if d := treeOf(g, "y"); d.ParentID != "x" || d.ParentIface != "bond1" || d.UplinkIface != "bond1" {
		t.Fatalf("y = %+v, want child of x via bond1 (MNDP saw only the members)", d)
	}
	want := map[string]PortRoleInfo{
		"x/bond1":  {DeviceID: "x", Iface: "bond1", Role: RoleDownlink, NeighborDeviceID: "y", NeighborName: "Y", NeighborIface: "bond1", NeighborCount: 1, MACCount: 2},
		"x/ether1": {DeviceID: "x", Iface: "ether1", Role: RoleDownlink, NeighborDeviceID: "y", NeighborName: "Y", NeighborIface: "bond1", NeighborCount: 1, Master: "bond1"},
		"y/bond1":  {DeviceID: "y", Iface: "bond1", Role: RoleUplink, NeighborDeviceID: "x", NeighborName: "X", NeighborIface: "bond1", MACCount: 1},
		"y/ether2": {DeviceID: "y", Iface: "ether2", Role: RoleUplink, NeighborDeviceID: "x", NeighborName: "X", NeighborIface: "bond1", Master: "bond1"},
		"y/bond2":  {DeviceID: "y", Iface: "bond2", Role: RoleAccess, ClientCount: 1, MACCount: 1},
		"y/ether7": {DeviceID: "y", Iface: "ether7", Role: RoleAccess, Master: "bond2"},
	}
	for key, w := range want {
		dev, iface, _ := strings.Cut(key, "/")
		if got := g.Role(dev, iface); got != w {
			t.Errorf("%s =\n %+v\nwant\n %+v", key, got, w)
		}
	}
	if dev, iface, ok := g.AttachmentOf("AA:00:00:00:0D:01"); !ok || dev != "y" || iface != "bond2" {
		t.Errorf("NAS attached at %s/%s (%v), want y/bond2", dev, iface, ok)
	}
	// A path through a member port ends where its bond does.
	hops, err := g.Path("x", "ether1", r.fn())
	if err != nil {
		t.Fatal(err)
	}
	if last := hops[len(hops)-1]; !last.Sink || last.DeviceID != "y" || last.InIface != "bond1" || last.DownBps != 45*mbps {
		t.Errorf("Path(x, ether1) sink = %+v, want Y over bond1 at the member's 45M", last)
	}
	hops, _ = g.Path("y", "ether7", r.fn())
	if last := hops[len(hops)-1]; last.ID != "clients:y:bond2" || last.ClientCount != 1 {
		t.Errorf("Path(y, ether7) sink = %+v, want the bond's clients", last)
	}

	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "bond", s, 1000)
	wantLinks := map[string]int64{"internet>x": 100 * mbps, "x>y": 90 * mbps, "y>port:y:bond2": 80 * mbps}
	if got := sankeyValues(s); !reflect.DeepEqual(got, wantLinks) {
		t.Errorf("links = %v, want %v (members never leaves)", got, wantLinks)
	}
}

// The physical port under a PPPoE-over-VLAN WAN faces the same upstream: an
// uplink to the Internet, not an "idle" downstream leaf (which drew the whole
// WAN download as upload).
func TestRoleGraphWANCarrier(t *testing.T) {
	in, r := pppoeFixture()
	g := BuildRoleGraph(in)
	want := map[string]PortRoleInfo{
		"pppoe-out1": {DeviceID: "r", Iface: "pppoe-out1", Role: RoleWAN, NeighborName: "Internet", UpstreamNode: "internet"},
		"vlan-wan":   {DeviceID: "r", Iface: "vlan-wan", Role: RoleVirtual},
		"ether1":     {DeviceID: "r", Iface: "ether1", Role: RoleUplink, NeighborName: "Internet", UpstreamNode: "internet"},
		"ether2":     {DeviceID: "r", Iface: "ether2", Role: RoleAccess, ClientCount: 1, MACCount: 1},
	}
	for iface, w := range want {
		if got := g.Role("r", iface); got != w {
			t.Errorf("%s =\n %+v\nwant\n %+v", iface, got, w)
		}
	}
	for dir, wantLinks := range map[string]map[string]int64{
		"download": {"internet>r": 300 * mbps, "r>port:r:ether2": 290 * mbps},
		"upload":   {"internet>r": 10 * mbps, "r>port:r:ether2": 9 * mbps},
	} {
		s := g.Sankey(r.fn(), SankeyOptions{Direction: dir})
		checkSankey(t, dir, s, 1000)
		if got := sankeyValues(s); !reflect.DeepEqual(got, wantLinks) {
			t.Errorf("%s links = %v, want %v", dir, got, wantLinks)
		}
	}
	// A plain VLAN WAN (no PPPoE) marks its carrier the same way.
	in.Uplinks = []PathUplink{{DeviceID: "r", Kind: "default-route", Interface: "vlan-wan", IfaceType: "vlan", GatewayIP: "198.51.100.77"}}
	g = BuildRoleGraph(in)
	if got := g.Role("r", "ether1"); got.Role != RoleUplink || got.UpstreamNode != "internet" {
		t.Errorf("ether1 under vlan-wan = %+v, want uplink to the internet", got)
	}
	if got := g.Role("r", "vlan-wan").Role; got != RoleWAN {
		t.Errorf("vlan-wan = %q, want wan", got)
	}
}

// Two children behind one parent port: each edge is the child's own uplink,
// not the port's whole total twice.
func TestRoleGraphSharedParentPort(t *testing.T) {
	in, r := meshSharedPort()
	g := BuildRoleGraph(in)
	if ri := g.Role("s", "ether5"); ri.Role != RoleDownlink || ri.NeighborCount != 2 {
		t.Fatalf("s/ether5 = %+v, want downlink to both APs", ri)
	}
	// Download into each AP is its own uplink rx (70M + 30M), not S.ether5's
	// 100M twice.
	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "shared", s, 1000)
	vals := sankeyValues(s)
	if vals["s>a1"] != 70*mbps || vals["s>a2"] != 30*mbps {
		t.Errorf("s>a1 / s>a2 = %d / %d, want 70M / 30M (each AP's uplink)", vals["s>a1"], vals["s>a2"])
	}
	up := sankeyValues(g.Sankey(r.fn(), SankeyOptions{Direction: "upload"}))
	if up["s>a1"] != 5*mbps || up["s>a2"] != 2*mbps {
		t.Errorf("upload s>a1 / s>a2 = %d / %d, want 5M / 2M", up["s>a1"], up["s>a2"])
	}
	// One AP unmeasured: it gets what the shared port carried beyond the other.
	r2 := r.without("a2/ether1")
	vals = sankeyValues(g.Sankey(r2.fn(), SankeyOptions{}))
	if vals["s>a2"] != 30*mbps {
		t.Errorf("s>a2 without its uplink = %d, want 100M − 70M", vals["s>a2"])
	}
	hops, err := g.Path("a2", "wifi1", r.fn())
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hops {
		if h.DeviceID == "a2" && !h.Sink && (h.DownBps != 30*mbps || h.UpBps != 2*mbps) {
			t.Errorf("a2 hop = %+v, want its own uplink 30M/2M", h)
		}
	}
}

// East-west traffic: a device sending more downstream than it receives gets
// the surplus from its "local:<dev>" source.
func TestSankeyLocalSurplus(t *testing.T) {
	in, r := meshFixture()
	g := BuildRoleGraph(in)
	r["s/ether7"] = [2]int64{1 * mbps, 30 * mbps} // a NAS on S serving 30M
	s := g.Sankey(r.fn(), SankeyOptions{})
	checkSankey(t, "local", s, 1000)
	vals := sankeyValues(s)
	if vals["local:s>s"] != 20*mbps { // out 60+20+30 = 110 vs in 90
		t.Errorf("local:s>s = %d, want 20M (links %v)", vals["local:s>s"], vals)
	}
	if n, ok := sankeyNode(s, "local:s"); !ok || n != (SankeyNode{ID: "local:s", Name: "Local / east-west", Type: "other", DeviceID: "s"}) {
		t.Errorf("local node = %+v (present %v)", n, ok)
	}
	if _, ok := sankeyNode(s, "local:c"); ok {
		t.Error("C conserves flow: no local source")
	}
	// Rooted at S its inflow is unknown: no local source.
	s = g.Sankey(r.fn(), SankeyOptions{RootDeviceID: "s"})
	if _, ok := sankeyNode(s, "local:s"); ok {
		t.Error("the Sankey root gets no local source")
	}
	// Below MinBps: none.
	r["s/ether7"] = [2]int64{1 * mbps, 10*mbps + 500}
	if _, ok := sankeyNode(g.Sankey(r.fn(), SankeyOptions{}), "local:s"); ok {
		t.Error("a surplus below MinBps must not add a local source")
	}
}
