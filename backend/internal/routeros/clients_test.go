package routeros

import "testing"

func TestActiveLeases(t *testing.T) {
	leases := []DHCPLease{
		{Address: "192.168.78.10", Server: "main"},
		{Address: "192.168.23.100", Server: "openwater-dhcp"},
		{Address: "192.168.23.101", Server: "openwater-dhcp"},
		{Address: "192.168.78.11", Server: "main"},
		{Address: "10.0.0.5", Server: ""}, // unknown/empty server — always kept
	}

	// No disabled servers → everything is returned unchanged.
	if got := activeLeases(leases, nil); len(got) != len(leases) {
		t.Fatalf("nil disabled set: want %d leases, got %d", len(leases), len(got))
	}
	if got := activeLeases(leases, map[string]bool{}); len(got) != len(leases) {
		t.Fatalf("empty disabled set: want %d leases, got %d", len(leases), len(got))
	}

	// The disabled openwater-dhcp server's two reservations are dropped; the
	// enabled "main" server's leases and the empty-server lease stay.
	got := activeLeases(leases, map[string]bool{"openwater-dhcp": true})
	if len(got) != 3 {
		t.Fatalf("want 3 leases after dropping disabled server, got %d: %+v", len(got), got)
	}
	for _, l := range got {
		if l.Server == "openwater-dhcp" {
			t.Fatalf("a disabled-server lease leaked through: %+v", l)
		}
	}
}

// The DHCP servers are printed only to tell a disabled server's leftover
// reservations from live leases: a device without any lease is not asked for
// them.
func TestGetDHCPLeasesAsksForServersOnlyWhenThereAreLeases(t *testing.T) {
	none := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/dhcp-server/lease/print": rep(),
		"/ip/dhcp-server/print":       rep(),
	}}
	leases, err := GetDHCPLeases(none)
	if err != nil || len(leases) != 0 {
		t.Fatalf("no leases: got %v, %v", leases, err)
	}
	if len(none.calls) != 1 {
		t.Fatalf("a device without leases was sent %v, want the lease print alone", none.calls)
	}

	some := &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/dhcp-server/lease/print": rep(
			map[string]string{"address": "192.0.2.10", "mac-address": "02:00:00:00:00:01", "server": "lan"},
			map[string]string{"address": "192.0.2.11", "mac-address": "02:00:00:00:00:02", "server": "old"}),
		"/ip/dhcp-server/print": rep(
			map[string]string{"name": "lan", "disabled": "false"},
			map[string]string{"name": "old", "disabled": "true"}),
	}}
	leases, err = GetDHCPLeases(some)
	if err != nil || len(leases) != 1 || leases[0].Address != "192.0.2.10" {
		t.Fatalf("with a disabled server: got %+v, %v; want the live lease alone", leases, err)
	}
	if len(some.calls) != 2 {
		t.Fatalf("a device with leases was sent %v, want the lease print and the server print", some.calls)
	}
}
