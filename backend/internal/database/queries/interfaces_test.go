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
