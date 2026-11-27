package queries

import "testing"

func TestPreferIP(t *testing.T) {
	cases := []struct {
		existing, candidate, want string
	}{
		{"", "", ""},
		{"", "192.168.1.5", "192.168.1.5"},
		{"192.168.1.5", "", "192.168.1.5"},            // empty candidate never clears
		{"2001:db8::1", "192.168.1.5", "192.168.1.5"}, // upgrade v6 -> v4
		{"192.168.1.5", "2001:db8::1", "192.168.1.5"}, // keep v4 over v6
		{"192.168.1.5", "192.168.1.9", "192.168.1.5"}, // same family: keep first (stable)
		{"2001:db8::1", "2001:db8::2", "2001:db8::1"}, // same family: keep first
	}
	for _, c := range cases {
		if got := PreferIP(c.existing, c.candidate); got != c.want {
			t.Errorf("PreferIP(%q, %q) = %q, want %q", c.existing, c.candidate, got, c.want)
		}
	}
}

func TestUpsertMACLookup_PrefersIPv4(t *testing.T) {
	db := testDB(t)
	const mac = "1C:F6:4C:00:00:50"

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

	set("2001:db8:100:10::888") // first sighting: IPv6 only
	if got := get(); got != "2001:db8:100:10::888" {
		t.Fatalf("after v6 insert = %q", got)
	}
	set("192.168.60.50") // IPv4 arrives -> should win
	if got := get(); got != "192.168.60.50" {
		t.Fatalf("IPv4 should win, got %q", got)
	}
	set("2001:db8:100:10::999") // later IPv6 must NOT clobber the IPv4
	if got := get(); got != "192.168.60.50" {
		t.Fatalf("IPv6 must not overwrite IPv4, got %q", got)
	}
	set("") // empty must not clear
	if got := get(); got != "192.168.60.50" {
		t.Fatalf("empty must not clear, got %q", got)
	}
	set("192.168.60.51") // newer IPv4: the persisted cache tracks the client's current v4
	if got := get(); got != "192.168.60.51" {
		t.Fatalf("v4->v4 should refresh to newest, got %q", got)
	}
}

// The device that reported a client is kept when a later writer does not
// name one: a write without a device must not blank "Reported By".
func TestUpsertMACLookupKeepsTheReportingDevice(t *testing.T) {
	db := testDB(t)
	const mac = "02:00:00:00:D0:01"
	get := func() *MACLookup {
		t.Helper()
		m, err := GetMACLookup(db, mac)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if err := UpsertMACLookup(db, &MACLookup{MACAddress: mac, IPAddress: "192.0.2.50", Source: "arp", DeviceName: "rtr", DeviceID: "d-rtr"}); err != nil {
		t.Fatal(err)
	}
	if err := UpsertMACLookup(db, &MACLookup{MACAddress: mac, IPAddress: "192.0.2.50", Source: "wifi", SSID: "lab"}); err != nil {
		t.Fatal(err)
	}
	if m := get(); m.DeviceName != "rtr" || m.DeviceID != "d-rtr" || m.Source != "wifi" || m.SSID != "lab" {
		t.Fatalf("after a write without a device: device %q (%q), source %q, ssid %q; want rtr (d-rtr), wifi, lab",
			m.DeviceName, m.DeviceID, m.Source, m.SSID)
	}
	if err := UpsertMACLookup(db, &MACLookup{MACAddress: mac, Source: "wifi", DeviceName: "ap", DeviceID: "d-ap"}); err != nil {
		t.Fatal(err)
	}
	if m := get(); m.DeviceName != "ap" || m.DeviceID != "d-ap" {
		t.Fatalf("a write that names a device: got %q (%q), want ap (d-ap)", m.DeviceName, m.DeviceID)
	}
}
