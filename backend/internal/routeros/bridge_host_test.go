package routeros

import (
	"errors"
	"slices"
	"testing"

	ros "github.com/go-routeros/routeros/v3"
	"github.com/go-routeros/routeros/v3/proto"
)

// The stacking menus worth printing are the ones the device has an interface
// of: every bond, VLAN and PPPoE client is one.
func TestRelationMenusOf(t *testing.T) {
	for _, tt := range []struct {
		types []string
		want  RelationMenus
	}{
		{nil, RelationMenus{}},
		{[]string{"ether", "bridge", "wifi", "wg", "pppoe-in"}, RelationMenus{}},
		{[]string{"ether", "bond", "vlan"}, RelationMenus{Bonding: true, VLAN: true}},
		{[]string{" VLAN "}, RelationMenus{VLAN: true}},
		{[]string{"pppoe-out", "ether"}, RelationMenus{PPPoE: true}},
	} {
		if got := RelationMenusOf(tt.types...); got != tt.want {
			t.Errorf("RelationMenusOf(%q) = %+v, want %+v", tt.types, got, tt.want)
		}
	}
	if AllRelationMenus != (RelationMenus{Bonding: true, VLAN: true, PPPoE: true}) {
		t.Errorf("AllRelationMenus = %+v", AllRelationMenus)
	}
}

// A stacking read prints only the menus asked for. A menu the device refuses
// (a !trap) is named in Refused and the others are still read; a failure that
// is not the device's answer fails the whole read.
func TestGetInterfaceRelations(t *testing.T) {
	menus := func() map[string]fakeReply {
		return map[string]fakeReply{
			"/interface/bonding/print":      rep(map[string]string{"name": "bond1", "slaves": "ether1,ether2"}),
			"/interface/vlan/print":         rep(map[string]string{"name": "vlan-mgmt", "interface": "bridge", "vlan-id": "99"}),
			"/interface/pppoe-client/print": rep(map[string]string{"name": "pppoe-out1", "interface": "ether9"}),
		}
	}
	bond := []InterfaceRelation{{Iface: "ether1", Parent: "bond1", Kind: RelationBond}, {Iface: "ether2", Parent: "bond1", Kind: RelationBond}}
	vlan := InterfaceRelation{Iface: "vlan-mgmt", Parent: "bridge", Kind: RelationVLAN, VLANID: 99}
	pppoe := InterfaceRelation{Iface: "pppoe-out1", Parent: "ether9", Kind: RelationPPPoE}

	f := &fakeRunner{byCmd: menus()}
	read, err := GetInterfaceRelations(f, AllRelationMenus)
	if err != nil || len(read.Refused) != 0 {
		t.Fatalf("all menus: refused %v, err %v", read.Refused, err)
	}
	if want := append(append([]InterfaceRelation{}, bond...), vlan, pppoe); !slices.Equal(read.Relations, want) {
		t.Errorf("all menus: relations = %+v, want %+v", read.Relations, want)
	}
	if want := []string{
		"/interface/bonding/print =.proplist=name,slaves",
		"/interface/vlan/print =.proplist=name,interface,vlan-id",
		"/interface/pppoe-client/print =.proplist=name,interface",
	}; !slices.Equal(f.calls, want) {
		t.Errorf("all menus: calls = %q, want %q", f.calls, want)
	}

	// Only the VLAN menu: one print, and the other kinds settled as none.
	f = &fakeRunner{byCmd: menus()}
	read, err = GetInterfaceRelations(f, RelationMenus{VLAN: true})
	if err != nil || !slices.Equal(read.Relations, []InterfaceRelation{vlan}) || len(f.calls) != 1 {
		t.Errorf("VLAN menu only: relations %+v, calls %q, err %v", read.Relations, f.calls, err)
	}
	if !read.Settled(RelationBond) || !read.Settled(RelationVLAN) || !read.Settled(RelationPPPoE) {
		t.Errorf("VLAN menu only: refused %v, want every kind settled", read.Refused)
	}

	// No menu: nothing sent, nothing there.
	f = &fakeRunner{byCmd: menus()}
	if read, err = GetInterfaceRelations(f, RelationMenus{}); err != nil || len(read.Relations) != 0 || len(f.calls) != 0 {
		t.Errorf("no menu: relations %+v, calls %q, err %v", read.Relations, f.calls, err)
	}

	// A refused menu does not cost the others.
	replies := menus()
	replies["/interface/vlan/print"] = fakeReply{err: trapErr("not enough permissions (9)")}
	f = &fakeRunner{byCmd: replies}
	read, err = GetInterfaceRelations(f, AllRelationMenus)
	if err != nil {
		t.Fatalf("a refused menu fails the read: %v", err)
	}
	if want := append(append([]InterfaceRelation{}, bond...), pppoe); !slices.Equal(read.Relations, want) {
		t.Errorf("VLAN menu refused: relations = %+v, want %+v", read.Relations, want)
	}
	if !slices.Equal(read.Refused, []string{RelationVLAN}) || read.Settled(RelationVLAN) || !read.Settled(RelationBond) {
		t.Errorf("VLAN menu refused: refused = %v", read.Refused)
	}

	// No answer, or a session the device ended: nothing is known.
	for name, failure := range map[string]error{
		"a timeout": errors.New("i/o timeout"),
		"a !fatal": &ros.SessionFatalError{Err: &ros.DeviceError{Sentence: &proto.Sentence{Word: "!fatal",
			Map: map[string]string{"message": "session terminated"}}}},
	} {
		replies := menus()
		replies["/interface/vlan/print"] = fakeReply{err: failure}
		f = &fakeRunner{byCmd: replies}
		if read, err := GetInterfaceRelations(f, AllRelationMenus); err == nil || len(read.Relations) != 0 {
			t.Errorf("%s on the VLAN menu: relations %+v, err %v; want the read failed", name, read.Relations, err)
		}
		if len(f.calls) != 2 {
			t.Errorf("%s on the VLAN menu: calls %q, want the read to stop there", name, f.calls)
		}
	}
}

func TestParseBridgeHost(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		ok   bool
		want BridgeHost
	}{
		{
			name: "dynamic entry on an access port",
			in: map[string]string{
				"mac-address": "78:9A:18:5E:11:02", "on-interface": "ether7", "interface": "ether7",
				"bridge": "bridge", "vid": "28", "local": "false", "external": "false",
				"dynamic": "true", "invalid": "false", "disabled": "false",
			},
			ok:   true,
			want: BridgeHost{MACAddress: "78:9A:18:5E:11:02", Interface: "ether7", Bridge: "bridge", VID: 28, Dynamic: true},
		},
		{
			name: "switch-chip learned (external) entry is kept",
			in: map[string]string{
				"mac-address": "48:A9:8A:01:02:03", "on-interface": "sfp-sfpplus1", "interface": "sfp-sfpplus1",
				"bridge": "bridge", "vid": "1", "local": "false", "external": "true",
				"dynamic": "true", "invalid": "false", "disabled": "false",
			},
			ok:   true,
			want: BridgeHost{MACAddress: "48:A9:8A:01:02:03", Interface: "sfp-sfpplus1", Bridge: "bridge", VID: 1, External: true, Dynamic: true},
		},
		{
			name: "no vid, lowercase mac, interface fallback",
			in: map[string]string{
				"mac-address": "aa:bb:cc:dd:ee:ff", "interface": "wifi2", "bridge": "bridgeLocal",
				"local": "false", "dynamic": "true",
			},
			ok:   true,
			want: BridgeHost{MACAddress: "AA:BB:CC:DD:EE:FF", Interface: "wifi2", Bridge: "bridgeLocal", Dynamic: true},
		},
		{
			name: "on-interface wins over interface",
			in:   map[string]string{"mac-address": "00:11:22:33:44:55", "on-interface": "ether2", "interface": "bond1"},
			ok:   true,
			want: BridgeHost{MACAddress: "00:11:22:33:44:55", Interface: "ether2"},
		},
		{
			name: "local entry is skipped",
			in: map[string]string{
				"mac-address": "48:A9:8A:AA:BB:CC", "on-interface": "bridge", "interface": "bridge",
				"bridge": "bridge", "local": "true", "external": "false", "dynamic": "false",
			},
			ok: false,
		},
		{
			name: "invalid entry is skipped",
			in:   map[string]string{"mac-address": "00:11:22:33:44:66", "on-interface": "ether3", "invalid": "true"},
			ok:   false,
		},
		{
			name: "disabled static entry is skipped",
			in: map[string]string{
				"mac-address": "00:11:22:33:44:88", "on-interface": "ether4", "interface": "ether4",
				"bridge": "bridge", "local": "false", "dynamic": "false", "invalid": "false", "disabled": "true",
			},
			ok: false,
		},
		{
			name: "empty mac is skipped",
			in:   map[string]string{"on-interface": "ether3"},
			ok:   false,
		},
		{
			name: "empty interface is skipped",
			in:   map[string]string{"mac-address": "00:11:22:33:44:77"},
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseBridgeHost(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("parseBridgeHost() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// Attribute shapes verified read-only on RouterOS 7.23 (switch001 bonds,
// RB5009 VLANs).
func TestParseInterfaceRelations(t *testing.T) {
	got := parseBondSlaves(map[string]string{"name": "bonding3-nas024", "slaves": "ether30,ether31, ether32,ether33", "running": "true"})
	want := []InterfaceRelation{
		{Iface: "ether30", Parent: "bonding3-nas024", Kind: RelationBond},
		{Iface: "ether31", Parent: "bonding3-nas024", Kind: RelationBond},
		{Iface: "ether32", Parent: "bonding3-nas024", Kind: RelationBond},
		{Iface: "ether33", Parent: "bonding3-nas024", Kind: RelationBond},
	}
	if len(got) != len(want) {
		t.Fatalf("bond slaves = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bond slave %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got := parseBondSlaves(map[string]string{"name": "bond9", "slaves": ""}); len(got) != 0 {
		t.Errorf("bond without slaves = %+v", got)
	}
	if got := parseBondSlaves(map[string]string{"slaves": "ether1"}); len(got) != 0 {
		t.Errorf("nameless bond = %+v", got)
	}

	if got := parseVLANCarrier(map[string]string{"name": "net28", "interface": "bridge", "vlan-id": "28"}); len(got) != 1 ||
		got[0] != (InterfaceRelation{Iface: "net28", Parent: "bridge", Kind: RelationVLAN, VLANID: 28}) {
		t.Errorf("vlan = %+v", got)
	}
	if got := VLANs([]InterfaceRelation{{Iface: "ether7", Parent: "bond1", Kind: RelationBond},
		{Iface: "net28", Parent: "bridge", Kind: RelationVLAN, VLANID: 28}}); len(got) != 1 ||
		got[0] != (VLANInfo{Name: "net28", VLANID: 28, Parent: "bridge"}) {
		t.Errorf("VLANs = %+v", got)
	}
	if got := VLANs(nil); got == nil || len(got) != 0 {
		t.Errorf("VLANs of no relations = %#v, want an empty, non-nil list (read, none)", got)
	}
	if got := parseCarrier(map[string]string{"name": "pppoe-out1", "interface": "ether1"}, RelationPPPoE); len(got) != 1 ||
		got[0] != (InterfaceRelation{Iface: "pppoe-out1", Parent: "ether1", Kind: RelationPPPoE}) {
		t.Errorf("pppoe = %+v", got)
	}
	if got := parseCarrier(map[string]string{"name": "pppoe-out2"}, RelationPPPoE); len(got) != 0 {
		t.Errorf("carrier without interface = %+v", got)
	}
}
