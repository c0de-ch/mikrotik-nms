package queries

import (
	"testing"
	"time"
)

func TestDeviceFindingsUpsertListAndRetention(t *testing.T) {
	db := testDB(t)
	if err := CreateDevice(db, &Device{ID: "d1", Address: "192.0.2.1", Identity: "r1", Username: "u", APIPort: 8728, Status: "online"}); err != nil {
		t.Fatalf("create device: %v", err)
	}
	f := &DeviceFinding{DeviceID: "d1", Kind: "parallel_api_poller", Severity: "info", Detail: "user x from 192.0.2.50 via api"}
	for i := 0; i < 3; i++ {
		if err := UpsertDeviceFinding(db, f); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	f.Severity = "warn"
	if err := UpsertDeviceFinding(db, f); err != nil {
		t.Fatal(err)
	}
	if err := UpsertDeviceFinding(db, &DeviceFinding{DeviceID: "d1", Kind: "login_failure", Severity: "info", Detail: "user y from 192.0.2.60 via ssh"}); err != nil {
		t.Fatal(err)
	}

	rows, err := ListDeviceFindings(db, "d1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 deduplicated findings, got %d: %+v", len(rows), rows)
	}
	var par DeviceFinding
	for _, r := range rows {
		if r.Kind == "parallel_api_poller" {
			par = r
		}
	}
	if par.Count != 4 || par.Severity != "warn" || par.FirstSeen.IsZero() || par.LastSeen.Before(par.FirstSeen) {
		t.Fatalf("deduplicated finding = %+v", par)
	}
	if other, _ := ListDeviceFindings(db, "nope", 0); len(other) != 0 {
		t.Fatalf("unknown device returned %d rows", len(other))
	}

	// Retention: age one row and sweep.
	if _, err := db.Exec(`UPDATE device_findings SET last_seen = ? WHERE kind = 'login_failure'`, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := DeleteOldDeviceFindings(db, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("delete old: n=%d err=%v", n, err)
	}
	rows, _ = ListDeviceFindings(db, "d1", 0)
	if len(rows) != 1 || rows[0].Kind != "parallel_api_poller" {
		t.Fatalf("after sweep: %+v", rows)
	}

	// Deleting the device cascades.
	if _, err := db.Exec(`DELETE FROM devices WHERE id = 'd1'`); err != nil {
		t.Fatal(err)
	}
	if rows, _ = ListDeviceFindings(db, "d1", 0); len(rows) != 0 {
		t.Fatalf("cascade left %d rows", len(rows))
	}
}
