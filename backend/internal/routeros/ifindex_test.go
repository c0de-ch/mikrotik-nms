package routeros

import "testing"

func TestIfIndexFromID(t *testing.T) {
	ok := map[string]uint32{"*1": 1, "*D": 13, "*d": 13, "*3ED": 1005, "*313": 787, "*A": 10, "*FFFFFFFF": 0xFFFFFFFF}
	for in, want := range ok {
		if got, valid := IfIndexFromID(in); !valid || got != want {
			t.Errorf("IfIndexFromID(%q) = %d, %v; want %d", in, got, valid, want)
		}
	}
	for _, in := range []string{"", "*", "*ZZ", "D", "*0", "*100000000", "**1", "* 1", "*-1", "*+1", "*0x1"} {
		if got, valid := IfIndexFromID(in); valid {
			t.Errorf("IfIndexFromID(%q) = %d, true; want false", in, got)
		}
	}
}

func TestParseVLANInfo(t *testing.T) {
	v, ok := parseVLANInfo(map[string]string{".id": "*D", "name": "net28", "vlan-id": "28", "interface": "bridge"})
	if !ok || v != (VLANInfo{Name: "net28", VLANID: 28, Parent: "bridge"}) {
		t.Fatalf("net28 = %+v, %v", v, ok)
	}
	v, ok = parseVLANInfo(map[string]string{"name": "odd", "vlan-id": "x"})
	if !ok || v.VLANID != 0 || v.Parent != "" {
		t.Fatalf("junk vlan-id = %+v, %v", v, ok)
	}
	if _, ok := parseVLANInfo(map[string]string{"vlan-id": "6"}); ok {
		t.Fatal("row without a name must be skipped")
	}
}
