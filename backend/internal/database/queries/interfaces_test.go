package queries

import "testing"

func TestDeleteInterfacesNotIn(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	seedPortStatsDevice(t, db, "2")
	for _, i := range []Interface{
		{ID: "1:ether1", DeviceID: "1", Name: "ether1"}, {ID: "1:wifi18", DeviceID: "1", Name: "wifi18"},
		{ID: "1:", DeviceID: "1", Name: ""}, {ID: "2:ether1", DeviceID: "2", Name: "ether1"},
	} {
		if err := UpsertInterface(db, &i); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := DeleteInterfacesNotIn(db, "1", nil); err != nil || n != 0 {
		t.Fatalf("empty names: n=%d err=%v, want a no-op", n, err)
	}
	n, err := DeleteInterfacesNotIn(db, "1", []string{"ether1", "ether2"})
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v, want the stale and the blank row gone", n, err)
	}
	left, _ := ListInterfacesByDevice(db, "1")
	if len(left) != 1 || left[0].Name != "ether1" {
		t.Errorf("device 1 = %+v", left)
	}
	if other, _ := ListInterfacesByDevice(db, "2"); len(other) != 1 {
		t.Errorf("other device touched: %+v", other)
	}
}

func TestSyncInterfacesUpsertsAndPrunesInOneCall(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	seedPortStatsDevice(t, db, "2")
	mtu := 1500
	for _, i := range []Interface{
		{ID: "1:ether1", DeviceID: "1", Name: "ether1", Type: "ether"},
		{ID: "1:wifi18", DeviceID: "1", Name: "wifi18", Type: "wifi"},
		{ID: "2:ether1", DeviceID: "2", Name: "ether1"},
	} {
		if err := UpsertInterface(db, &i); err != nil {
			t.Fatal(err)
		}
	}

	if n, err := SyncInterfaces(db, "1", nil, nil); err != nil || n != 0 {
		t.Fatalf("nothing to do: n=%d err=%v", n, err)
	}

	// ether1 changes, ether2 is new, wifi18 went away.
	n, err := SyncInterfaces(db, "1", []Interface{
		{ID: "1:ether1", DeviceID: "ignored", Name: "ether1", Type: "ether", MACAddress: "02:00:00:00:00:01", MTU: &mtu, Running: true, Comment: "uplink"},
		{ID: "1:ether2", Name: "ether2", Type: "ether", Disabled: true},
	}, []string{"ether1", "ether2"})
	if err != nil || n != 1 {
		t.Fatalf("sync: pruned=%d err=%v, want wifi18 pruned", n, err)
	}
	got, _ := ListInterfacesByDevice(db, "1")
	if len(got) != 2 || got[0].Name != "ether1" || got[1].Name != "ether2" {
		t.Fatalf("device 1 = %+v", got)
	}
	e1 := got[0]
	if e1.DeviceID != "1" || e1.MACAddress != "02:00:00:00:00:01" || e1.MTU == nil || *e1.MTU != 1500 ||
		!e1.Running || e1.Comment != "uplink" {
		t.Errorf("ether1 = %+v", e1)
	}
	if !got[1].Disabled {
		t.Errorf("ether2 = %+v", got[1])
	}
	if other, _ := ListInterfacesByDevice(db, "2"); len(other) != 1 {
		t.Errorf("other device touched: %+v", other)
	}

	// Upserts without a keep list never prune (the empty-reply guard).
	if n, err := SyncInterfaces(db, "1", []Interface{{ID: "1:ether1", Name: "ether1"}}, nil); err != nil || n != 0 {
		t.Fatalf("upsert-only: n=%d err=%v", n, err)
	}
	if left, _ := ListInterfacesByDevice(db, "1"); len(left) != 2 {
		t.Errorf("upsert-only sync pruned: %+v", left)
	}
}
