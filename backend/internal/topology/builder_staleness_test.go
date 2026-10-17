package topology

import (
	"database/sql"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// stalenessDB seeds four devices whose interfaces resolve each other's MACs:
// a weak switch read every 5 min ("weak", 15 min window), a router read every
// 2 min ("fast", 6 min window), a paused switch ("held", 6 min window) and a
// target ("tgt").
func stalenessDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/stale.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for i, id := range []string{"weak", "fast", "held", "tgt"} {
		d := queries.Device{ID: id, Identity: id, Address: "192.0.2." + string(rune('1'+i)), Status: "online", Tags: "[]"}
		if err := queries.CreateDevice(db, &d); err != nil {
			t.Fatal(err)
		}
		if err := queries.UpsertInterface(db, &queries.Interface{
			ID: id + ":ether1", DeviceID: id, Name: "ether1", Type: "ether", MACAddress: "02:00:00:00:00:0" + string(rune('1'+i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// seen upserts reporter's neighbor row for the device owning mac, last seen
// at ts.
func seen(t *testing.T, db *sql.DB, reporter, mac string, ts time.Time) {
	t.Helper()
	if err := queries.UpsertNeighbor(db, &queries.Neighbor{
		ID: reporter + ":ether1:" + mac, DeviceID: reporter, LocalInterface: "ether1", NeighborMAC: mac,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE neighbors SET last_seen = ? WHERE device_id = ? AND neighbor_mac = ?`,
		ts.UTC().Format("2006-01-02 15:04:05"), reporter, mac); err != nil {
		t.Fatal(err)
	}
}

func linkStatus(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	links, err := queries.ListLinks(db)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, l := range links {
		out[l.DeviceAID+"-"+l.DeviceBID] = l.Status
	}
	return out
}

// Link staleness is three of the reporting device's own neighbor reads: a
// weak switch's 10-minute-old sighting still holds its link up, a router's
// does not, and a paused device's rows do not age at all — only rows that
// were stale before its last read stay stale.
func TestBuildLinkStalenessPerReporter(t *testing.T) {
	db := stalenessDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	age := func(reporter queries.Device, _ time.Time) (time.Duration, bool) {
		switch reporter.ID {
		case "weak":
			return 15 * time.Minute, false
		case "held":
			return 6 * time.Minute, true
		}
		return 6 * time.Minute, false
	}
	const tgtMAC, fastMAC, weakMAC = "02:00:00:00:00:04", "02:00:00:00:00:02", "02:00:00:00:00:01"

	// First build: every sighting fresh, every link up.
	seen(t, db, "weak", tgtMAC, now)
	seen(t, db, "fast", tgtMAC, now)
	seen(t, db, "held", weakMAC, now.Add(-2*time.Hour))
	seen(t, db, "held", fastMAC, now.Add(-2*time.Hour))
	if _, err := NewBuilder(db).UseNeighborAge(age).Build(); err != nil {
		t.Fatal(err)
	}
	if got := linkStatus(t, db); got["tgt-weak"] != "up" || got["fast-tgt"] != "up" || got["held-weak"] != "up" || got["fast-held"] != "up" {
		t.Fatalf("first build links = %v", got)
	}

	// Ten minutes without new sightings: inside the weak switch's window,
	// past the router's. The paused device's two-hour-old rows were current
	// at its last read and stay so.
	seen(t, db, "weak", tgtMAC, now.Add(-10*time.Minute))
	seen(t, db, "fast", tgtMAC, now.Add(-10*time.Minute))
	// One paused-device row was already stale at its last read: 2 h older
	// than its newest.
	seen(t, db, "held", fastMAC, now.Add(-4*time.Hour))
	time.Sleep(1100 * time.Millisecond) // the mark-down compares to the second
	if _, err := NewBuilder(db).UseNeighborAge(age).Build(); err != nil {
		t.Fatal(err)
	}
	got := linkStatus(t, db)
	if got["tgt-weak"] != "up" {
		t.Errorf("weak switch link = %q, want up (10 min < its 15 min window)", got["tgt-weak"])
	}
	if got["fast-tgt"] != "down" {
		t.Errorf("router link = %q, want down (10 min > its 6 min window)", got["fast-tgt"])
	}
	if got["held-weak"] != "up" {
		t.Errorf("paused device's link = %q, want up (nothing ages while paused)", got["held-weak"])
	}
	if got["fast-held"] != "down" {
		t.Errorf("paused device's stale-before-pause link = %q, want down", got["fast-held"])
	}

	// Without a NeighborAge every stored row counts, as before.
	if _, err := NewBuilder(db).Build(); err != nil {
		t.Fatal(err)
	}
	if got := linkStatus(t, db); got["fast-tgt"] != "up" {
		t.Errorf("without an age rule the router link = %q, want up", got["fast-tgt"])
	}
}
