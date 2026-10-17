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

// Fresh uplinks are judged per device: each device's rows against its own
// window, a paused device's rows at any age; a nil rule is the fixed default
// window with the pause still honoured.
func TestListFreshDeviceUplinksPerDevice(t *testing.T) {
	db := testDB(t)
	for _, id := range []string{"1", "2", "3"} {
		seedPortStatsDevice(t, db, id)
		if err := ReplaceDeviceUplinks(db, id, []DeviceUplink{{DeviceID: id, Kind: "default-route", Interface: "ether1", GatewayIP: "198.51.100.1"}}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	age := func(id string, d time.Duration) {
		t.Helper()
		if _, err := db.Exec(`UPDATE device_uplinks SET last_seen = ? WHERE device_id = ?`, sqlTime(now.Add(-d)), id); err != nil {
			t.Fatal(err)
		}
	}
	age("1", 12*time.Minute) // a weak device, read every 5 min: within its 15 min
	age("2", 12*time.Minute) // a device on the default window: stale
	age("3", 3*time.Hour)    // paused
	if err := PauseDevicePolling(db, "3", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	devices := func(rule UplinkAge) map[string]bool {
		t.Helper()
		ups, err := ListFreshDeviceUplinks(db, rule, now)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, u := range ups {
			out[u.DeviceID] = true
		}
		return out
	}
	rule := func(dev Device, at time.Time) (time.Duration, bool) {
		if dev.ID == "1" {
			return 15 * time.Minute, dev.PollingPaused(at)
		}
		return DefaultUplinkWindow, dev.PollingPaused(at)
	}
	if got := devices(rule); len(got) != 2 || !got["1"] || !got["3"] {
		t.Errorf("per-device rule = %v, want 1 (its own window) and 3 (paused)", got)
	}
	if got := devices(nil); len(got) != 1 || !got["3"] {
		t.Errorf("default rule = %v, want only the paused 3", got)
	}
}
