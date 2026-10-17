package poller

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// FDB rows are current for three of their device's own dump intervals: a weak
// switch dumped every 15 min keeps 20-minute-old rows that a router dumped
// every 5 min would not, and a paused device's rows are judged against its
// newest dump — they do not age during the pause, while a row that was
// already stale at that dump stays stale.
func TestFreshPortHostsPerDeviceWindow(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"weak": devclass.C1, "fast": devclass.C3, "held": devclass.C3}, "held")
	now := time.Now().UTC().Truncate(time.Second)
	dump := func(dev, mac string, age time.Duration) {
		t.Helper()
		if err := queries.UpsertPortHosts(db, dev, []queries.PortHost{{InterfaceName: "ether1", MACAddress: mac}}, now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	dump("weak", "02:00:00:00:C0:01", 20*time.Minute)
	dump("fast", "02:00:00:00:C0:02", 20*time.Minute)
	dump("held", "02:00:00:00:C0:03", 2*time.Hour)
	dump("held", "02:00:00:00:C0:04", 2*time.Hour+30*time.Minute) // 30 min before its newest dump

	rows, err := FreshPortHosts(db, NewCadence(db, nil), now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, h := range rows {
		got[h.MACAddress] = true
	}
	if !got["02:00:00:00:C0:01"] || got["02:00:00:00:C0:02"] || !got["02:00:00:00:C0:03"] || got["02:00:00:00:C0:04"] || len(got) != 2 {
		t.Errorf("fresh rows = %v, want the weak switch's and the paused device's newest dump", got)
	}
}
