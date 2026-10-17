package poller

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// Egress rows are current for three of their device's own route reads, at
// least the old fixed 10 min: a weak device read every 5 min keeps 12-minute-
// old rows that a router read every minute does not, a raised topology floor
// stretches every device's window with its reads, and a paused device's rows
// never age.
func TestFreshUplinksPerDeviceWindow(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"weak": devclass.C1, "fast": devclass.C3, "held": devclass.C3}, "held")
	now := time.Now().UTC().Truncate(time.Second)
	for id, age := range map[string]time.Duration{"weak": 12 * time.Minute, "fast": 12 * time.Minute, "held": 3 * time.Hour} {
		if err := queries.ReplaceDeviceUplinks(db, id, []queries.DeviceUplink{{DeviceID: id, Kind: "default-route", Interface: "ether1", GatewayIP: "203.0.113.1"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE device_uplinks SET last_seen = ? WHERE device_id = ?`,
			now.Add(-age).Format("2006-01-02 15:04:05"), id); err != nil {
			t.Fatal(err)
		}
	}
	fresh := func(cs CadenceSource) map[string]bool {
		t.Helper()
		ups, err := FreshUplinks(db, cs, now)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, u := range ups {
			out[u.DeviceID] = true
		}
		return out
	}
	if got := fresh(NewCadence(db, nil)); len(got) != 2 || !got["weak"] || !got["held"] {
		t.Errorf("fresh uplinks = %v, want the weak device's and the paused device's", got)
	}
	// MIKROTIK_NMS_TOPOLOGY_INTERVAL=10m: the fast router is read every
	// 10 min too, and keeps its rows for 30.
	if got := fresh(NewCadence(db, &config.Config{TopologyInterval: 10 * time.Minute})); len(got) != 3 {
		t.Errorf("fresh uplinks with a 10 min floor = %v, want all three", got)
	}
	// Without a schedule: the fleet-wide one (the old fixed window).
	if got := fresh(nil); len(got) != 1 || !got["held"] {
		t.Errorf("fresh uplinks on the fleet schedule = %v, want only the paused device's", got)
	}
}
