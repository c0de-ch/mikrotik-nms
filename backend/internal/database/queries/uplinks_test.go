package queries

import (
	"testing"
	"time"
)

// A paused device's egress rows stay current however old: its routes are not
// read during the pause, and the map and the flow plan must keep its last
// known egress meanwhile. Other devices' rows still age out of the window.
func TestListDeviceUplinksKeepsPausedDevices(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	seedPortStatsDevice(t, db, "2")
	for _, id := range []string{"1", "2"} {
		if err := ReplaceDeviceUplinks(db, id, []DeviceUplink{{DeviceID: id, Kind: "default-route", Interface: "ether1", GatewayIP: "198.51.100.1"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE device_uplinks SET last_seen = ?`, sqlTime(time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	devices := func() map[string]bool {
		t.Helper()
		ups, err := ListDeviceUplinks(db, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, u := range ups {
			out[u.DeviceID] = true
		}
		return out
	}
	if got := devices(); len(got) != 0 {
		t.Fatalf("hour-old rows = %v, want none", got)
	}
	if err := PauseDevicePolling(db, "2", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := devices(); len(got) != 1 || !got["2"] {
		t.Fatalf("with device 2 paused = %v, want only its rows", got)
	}
	if err := PauseDevicePolling(db, "2", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := devices(); len(got) != 0 {
		t.Fatalf("after the pause ended = %v, want none", got)
	}
}
