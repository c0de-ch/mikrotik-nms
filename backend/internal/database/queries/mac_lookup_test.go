package queries

import "testing"

func TestPreferIP(t *testing.T) {
	cases := []struct {
		existing, candidate, want string
	}{
		{"", "", ""},
		{"", "192.168.1.5", "192.168.1.5"},
		{"192.168.1.5", "", "192.168.1.5"},            // empty candidate never clears
		{"2a01:4f8::1", "192.168.1.5", "192.168.1.5"}, // upgrade v6 -> v4
		{"192.168.1.5", "2a01:4f8::1", "192.168.1.5"}, // keep v4 over v6
		{"192.168.1.5", "192.168.1.9", "192.168.1.5"}, // same family: keep first (stable)
		{"2a01:4f8::1", "2a01:4f8::2", "2a01:4f8::1"}, // same family: keep first
	}
	for _, c := range cases {
		if got := PreferIP(c.existing, c.candidate); got != c.want {
			t.Errorf("PreferIP(%q, %q) = %q, want %q", c.existing, c.candidate, got, c.want)
		}
	}
}

func TestUpsertMACLookup_PrefersIPv4(t *testing.T) {
	db := testDB(t)
	const mac = "1C:F6:4C:9E:40:4A"

	set := func(ip string) {
		if err := UpsertMACLookup(db, &MACLookup{MACAddress: mac, IPAddress: ip, Source: "test"}); err != nil {
			t.Fatalf("upsert %q: %v", ip, err)
		}
	}
	get := func() string {
		m, err := GetMACLookup(db, mac)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		return m.IPAddress
	}

	set("2a01:4f8:202:13d1::888") // first sighting: IPv6 only
	if got := get(); got != "2a01:4f8:202:13d1::888" {
		t.Fatalf("after v6 insert = %q", got)
	}
	set("192.168.78.50") // IPv4 arrives -> should win
	if got := get(); got != "192.168.78.50" {
		t.Fatalf("IPv4 should win, got %q", got)
	}
	set("2a01:4f8:202:13d1::999") // later IPv6 must NOT clobber the IPv4
	if got := get(); got != "192.168.78.50" {
		t.Fatalf("IPv6 must not overwrite IPv4, got %q", got)
	}
	set("") // empty must not clear
	if got := get(); got != "192.168.78.50" {
		t.Fatalf("empty must not clear, got %q", got)
	}
	set("192.168.78.51") // newer IPv4: the persisted cache tracks the client's current v4
	if got := get(); got != "192.168.78.51" {
		t.Fatalf("v4->v4 should refresh to newest, got %q", got)
	}
}
