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
