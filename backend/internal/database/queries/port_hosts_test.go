package queries

import (
	"testing"
	"time"
)

func TestUpsertAndListPortHosts(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	seedPortStatsDevice(t, db, "2")

	t1 := minute(10, 0)
	t2 := minute(10, 5)

	err := UpsertPortHosts(db, "1", []PortHost{
		{DeviceID: "ignored", InterfaceName: "ether7", MACAddress: "aa:bb:cc:00:00:01", VID: 1, Bridge: "bridge"},
		{InterfaceName: "wifi2", MACAddress: "AA:BB:CC:00:00:02", VID: 28, Bridge: "bridge"},
		{InterfaceName: "wifi2", MACAddress: "AA:BB:CC:00:00:02", VID: 30, Bridge: "bridge"}, // duplicate: last wins
		{InterfaceName: "", MACAddress: "AA:BB:CC:00:00:03"},                                 // no interface: skipped
		{InterfaceName: "ether8", MACAddress: " "},                                           // no MAC: skipped
	}, t1)
	if err != nil {
		t.Fatalf("UpsertPortHosts #1: %v", err)
	}
	if err := UpsertPortHosts(db, "2", []PortHost{{InterfaceName: "sfp1", MACAddress: "AA:BB:CC:00:00:01"}}, t1); err != nil {
		t.Fatalf("UpsertPortHosts device 2: %v", err)
	}

	got, err := ListPortHosts(db, t1, "1")
	if err != nil {
		t.Fatalf("ListPortHosts: %v", err)
	}
	want := []PortHost{
		{DeviceID: "1", InterfaceName: "ether7", MACAddress: "AA:BB:CC:00:00:01", VID: 1, Bridge: "bridge", FirstSeen: t1, LastSeen: t1},
		{DeviceID: "1", InterfaceName: "wifi2", MACAddress: "AA:BB:CC:00:00:02", VID: 30, Bridge: "bridge", FirstSeen: t1, LastSeen: t1},
	}
	if len(got) != len(want) {
		t.Fatalf("hosts = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("host[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Second dump (seenAt in a non-UTC zone): first_seen kept, last_seen and
	// vid/bridge updated; the MAC missing from this dump is left alone.
	cest := time.FixedZone("CEST", 2*3600)
	if err := UpsertPortHosts(db, "1", []PortHost{
		{InterfaceName: "wifi2", MACAddress: "AA:BB:CC:00:00:02", VID: 31, Bridge: "bridge2"},
	}, t2.In(cest)); err != nil {
		t.Fatalf("UpsertPortHosts #2: %v", err)
	}

	fresh, err := ListPortHosts(db, t2, "")
	if err != nil {
		t.Fatalf("ListPortHosts fresh: %v", err)
	}
	if len(fresh) != 1 {
		t.Fatalf("fresh hosts = %+v, want only the re-seen wifi2 row", fresh)
	}
	h := fresh[0]
	if h.DeviceID != "1" || h.VID != 31 || h.Bridge != "bridge2" || !h.FirstSeen.Equal(t1) || !h.LastSeen.Equal(t2) {
		t.Errorf("updated host = %+v", h)
	}
	if h.LastSeen.Location() != time.UTC {
		t.Errorf("last_seen location = %v, want UTC", h.LastSeen.Location())
	}

	all, _ := ListPortHosts(db, t1, "")
	if len(all) != 3 || all[0].DeviceID != "1" || all[2].DeviceID != "2" {
		t.Errorf("all hosts (ordered by device) = %+v", all)
	}

	if err := UpsertPortHosts(db, "1", nil, t2); err != nil {
		t.Errorf("empty dump: %v", err)
	}
}

func TestDeleteStalePortHosts(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")

	if err := UpsertPortHosts(db, "1", []PortHost{
		{InterfaceName: "ether1", MACAddress: "AA:AA:AA:AA:AA:01"},
		{InterfaceName: "ether1", MACAddress: "AA:AA:AA:AA:AA:02"},
	}, minute(8, 0)); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if err := UpsertPortHosts(db, "1", []PortHost{{InterfaceName: "ether1", MACAddress: "AA:AA:AA:AA:AA:02"}}, minute(12, 0)); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	// A paused device's rows are kept however old: nothing ages while its
	// polling is paused.
	seedPortStatsDevice(t, db, "2")
	if err := UpsertPortHosts(db, "2", []PortHost{{InterfaceName: "sfp1", MACAddress: "AA:AA:AA:AA:AA:03"}}, minute(8, 0)); err != nil {
		t.Fatal(err)
	}
	if err := PauseDevicePolling(db, "2", minute(13, 0)); err != nil {
		t.Fatal(err)
	}

	n, err := DeleteStalePortHosts(db, minute(10, 0), minute(12, 30))
	if err != nil || n != 1 {
		t.Fatalf("DeleteStalePortHosts: n=%d err=%v, want 1", n, err)
	}
	left, _ := ListPortHosts(db, time.Time{}, "")
	if len(left) != 2 || left[0].MACAddress != "AA:AA:AA:AA:AA:02" || left[1].DeviceID != "2" {
		t.Errorf("left = %+v, want device 1's fresh row and the paused device's row", left)
	}
	// Once the pause has ended the paused device's old row goes too.
	if n, err := DeleteStalePortHosts(db, minute(10, 0), minute(13, 0)); err != nil || n != 1 {
		t.Errorf("after the pause: n=%d err=%v, want 1", n, err)
	}

	newest, err := LatestPortHostsDumps(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(newest) != 1 || !newest["1"].Equal(minute(12, 0)) {
		t.Errorf("LatestPortHostsDumps = %v, want device 1 at 12:00", newest)
	}
}

func TestListLatestPortHostsForMACs(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	seedPortStatsDevice(t, db, "2")
	const gw, other = "AA:BB:CC:00:00:01", "AA:BB:CC:00:00:09"

	// Device 1, dump at 10:00: the gateway on ether7. Dump at 10:05: it moved
	// to ether8 (the ether7 row keeps its 10:00 last_seen).
	if err := UpsertPortHosts(db, "1", []PortHost{{InterfaceName: "ether7", MACAddress: gw}, {InterfaceName: "ether2", MACAddress: other}}, minute(10, 0)); err != nil {
		t.Fatal(err)
	}
	if err := UpsertPortHosts(db, "1", []PortHost{{InterfaceName: "ether8", MACAddress: gw}, {InterfaceName: "ether2", MACAddress: other}}, minute(10, 5)); err != nil {
		t.Fatal(err)
	}
	// Device 2 learned it on its uplink at 10:03 (its only dump).
	if err := UpsertPortHosts(db, "2", []PortHost{{InterfaceName: "sfp1", MACAddress: gw}}, minute(10, 3)); err != nil {
		t.Fatal(err)
	}

	got, err := ListLatestPortHostsForMACs(db, minute(9, 0), []string{"aa:bb:cc:00:00:01"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].DeviceID != "1" || got[0].InterfaceName != "ether8" || !got[0].LastSeen.Equal(minute(10, 5)) ||
		got[1].DeviceID != "2" || got[1].InterfaceName != "sfp1" {
		t.Fatalf("rows = %+v, want 1/ether8 (latest dump only) and 2/sfp1", got)
	}

	// The freshness floor still applies.
	if got, _ := ListLatestPortHostsForMACs(db, minute(10, 4), []string{gw}); len(got) != 1 || got[0].DeviceID != "1" {
		t.Errorf("fresh since 10:04 = %+v, want only device 1", got)
	}
	if got, err := ListLatestPortHostsForMACs(db, minute(9, 0), nil); err != nil || got != nil {
		t.Errorf("no MACs = %+v err %v", got, err)
	}
}
