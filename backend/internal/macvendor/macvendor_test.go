package macvendor

import "testing"

func TestLookup(t *testing.T) {
	cases := []struct {
		mac  string
		want string
	}{
		// Stable, long-standing IEEE MA-L assignments.
		{"00:00:00:11:22:33", "XEROX CORPORATION"},
		{"00-C0-D9-aa-bb-cc", "QUINTE NETWORK CONFIDENTIALITY"}, // hyphen + lowercase tail
		{"00c0d9aabbcc", "QUINTE NETWORK CONFIDENTIALITY"},      // no separators
		{"", ""},
		{"zz:zz", ""},             // malformed
		{"1c:f6", ""},             // too short for an OUI
		{"FF:FF:FF:00:00:00", ""}, // unregistered prefix
	}
	for _, c := range cases {
		if got := Lookup(c.mac); got != c.want {
			t.Errorf("Lookup(%q) = %q, want %q", c.mac, got, c.want)
		}
	}
}

func TestIsRandomized(t *testing.T) {
	cases := []struct {
		mac  string
		want bool
	}{
		{"1C:F6:4C:9E:40:4A", false}, // real burned-in MAC (0x1C, U/L bit clear)
		{"08:F9:E0:FF:D9:E4", false}, // real
		{"00:00:00:00:00:00", false},
		{"4E:9B:CC:36:96:E8", true}, // locally administered (0x4E)
		{"7A:3D:63:29:4F:97", true}, // randomized phone MAC (0x7A)
		{"DA:9C:D6:A4:63:32", true}, // 0xDA
		{"", false},
	}
	for _, c := range cases {
		if got := IsRandomized(c.mac); got != c.want {
			t.Errorf("IsRandomized(%q) = %v, want %v", c.mac, got, c.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	// A randomized MAC has no registered vendor.
	v, r := Describe("4E:9B:CC:36:96:E8")
	if v != "" || !r {
		t.Errorf("Describe(randomized) = (%q, %v), want (\"\", true)", v, r)
	}
	v, r = Describe("00:00:00:de:ad:be")
	if v != "XEROX CORPORATION" || r {
		t.Errorf("Describe(xerox) = (%q, %v), want (\"XEROX CORPORATION\", false)", v, r)
	}
}
