package routeros

import "testing"

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

	if got := parseCarrier(map[string]string{"name": "net28", "interface": "bridge", "vlan-id": "28"}, RelationVLAN); len(got) != 1 ||
		got[0] != (InterfaceRelation{Iface: "net28", Parent: "bridge", Kind: RelationVLAN}) {
		t.Errorf("vlan = %+v", got)
	}
	if got := parseCarrier(map[string]string{"name": "pppoe-out1", "interface": "ether1"}, RelationPPPoE); len(got) != 1 ||
		got[0] != (InterfaceRelation{Iface: "pppoe-out1", Parent: "ether1", Kind: RelationPPPoE}) {
		t.Errorf("pppoe = %+v", got)
	}
	if got := parseCarrier(map[string]string{"name": "pppoe-out2"}, RelationPPPoE); len(got) != 0 {
		t.Errorf("carrier without interface = %+v", got)
	}
}
