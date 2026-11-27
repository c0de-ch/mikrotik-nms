package poller

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

func watchClient(t *testing.T, db *sql.DB, mac string) {
	t.Helper()
	if err := queries.CreatePingTarget(db, &queries.PingTarget{ID: "w-" + mac, Kind: "client", MACAddress: mac, Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

type signalRow struct {
	ap, ssid string
	dbm      int
	at       time.Time
}

func signalSamples(t *testing.T, db *sql.DB, mac string) []signalRow {
	t.Helper()
	got, err := queries.GetClientSignalSamples(db, mac, time.Unix(0, 0), time.Now().Add(24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]signalRow, 0, len(got))
	for i := len(got) - 1; i >= 0; i-- { // oldest first
		row := signalRow{ap: got[i].APName, ssid: got[i].SSID, at: got[i].RecordedAt}
		if got[i].SignalDBm != nil {
			row.dbm = *got[i].SignalDBm
		}
		out = append(out, row)
	}
	return out
}

// A client watch asks no device for its registration tables: with one watch
// configured the connectivity cycle used to send every online device up to
// three registration prints — switches without a wireless package included,
// every 30 s, whatever their class.
func TestClientWatchSignalCostsTheDevicesNoCommand(t *testing.T) {
	db := pollerTestDB(t)
	pool := routeros.NewPool()
	fakes := map[string]*routerostest.Device{"ap": routerostest.NewDevice(t), "sw": routerostest.NewDevice(t)}
	for id, fake := range fakes {
		// The pool is dialled to the fake under the device's id; the address
		// in the database is not used on this path.
		mustCreateDevice(t, db, &queries.Device{ID: id, Identity: id, Address: "192.0.2." + map[string]string{"ap": "1", "sw": "2"}[id]})
		fake.Dial(t, pool, id)
	}
	watchClient(t, db, "02:00:00:00:B0:01")

	cp := NewConnectivityPoller(db, pool, ws.NewHub(), time.Minute)
	cp.poll(context.Background())
	cp.poll(context.Background())

	for id, fake := range fakes {
		if cmds := fake.Commands(); len(cmds) != 0 {
			t.Errorf("%s was sent %d commands for a client watch: %s", id, len(cmds), strings.Join(cmds, ", "))
		}
	}
}

// The signal of a watched client is what the wifi tracker last read on the
// client's controller: one sample per registration read, stamped with the
// time of that read, and none while the controller is not read again.
func TestClientWatchSignalComesFromTheWifiTrackersReads(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"ap3": devclass.C3})
	const x, absent = "02:00:00:00:B0:01", "02:00:00:00:B0:02"
	ap := wifiController(x)
	wt := NewWifiTracker(db, nil, ws.NewHub(), 30*time.Second)
	clock := &testClock{t: at(12, 0, 0)}
	wt.now = clock.now
	wt.clock = newTierClock(NewScheduleRegistry())
	wt.clock.phase = noJitter
	wt.UseCadence(NewCadence(db, nil))
	wt.runner = scriptedRunners(map[string]*fakeRunner{"ap3": ap})
	watchClient(t, db, x)
	watchClient(t, db, absent)

	cp := NewConnectivityPoller(db, routeros.NewPool(), ws.NewHub(), time.Minute)
	cp.UseWifiReadings(wt)

	cp.poll(context.Background())
	if got := signalSamples(t, db, x); len(got) != 0 {
		t.Fatalf("samples before any registration read = %v, want none", got)
	}

	wt.poll(context.Background())
	cp.poll(context.Background())
	got := signalSamples(t, db, x)
	if len(got) != 1 || got[0].ap != "wifi1" || got[0].ssid != "lab" || got[0].dbm != -60 || !got[0].at.Equal(at(12, 0, 0)) {
		t.Fatalf("samples after the first read = %+v, want one: wifi1 / lab / -60 dBm at 12:00:00", got)
	}

	// The same read again: no second sample.
	clock.set(at(12, 0, 10))
	wt.poll(context.Background())
	cp.poll(context.Background())
	cp.poll(context.Background())
	if got := signalSamples(t, db, x); len(got) != 1 {
		t.Fatalf("samples without a new registration read = %d, want still 1", len(got))
	}

	// The controller's next read (C3: 30 s) brings the next sample.
	ap.byCmd["/interface/wifi/registration-table/print"] = rep(
		map[string]string{"mac-address": x, "interface": "wifi2", "ssid": "lab", "signal": "-71"})
	clock.set(at(12, 0, 31))
	wt.poll(context.Background())
	cp.poll(context.Background())
	got = signalSamples(t, db, x)
	if len(got) != 2 || got[1].ap != "wifi2" || got[1].dbm != -71 || !got[1].at.Equal(at(12, 0, 31)) {
		t.Fatalf("samples after the second read = %+v, want a second one: wifi2 / -71 dBm at 12:00:31", got)
	}
	if got := signalSamples(t, db, absent); len(got) != 0 {
		t.Errorf("a watched client no controller lists got %d samples", len(got))
	}
	if n := ap.countCalls("/interface/wifi/registration-table/print"); n != 2 {
		t.Errorf("registration reads = %d, want 2 (the tracker's own; the watch adds none)", n)
	}
}

// Reading answers from the freshest current registration read, in either
// letter case of the MAC, and no longer once that read has aged out.
func TestWifiTrackerReading(t *testing.T) {
	db := pollerTestDB(t)
	classFleet(t, db, map[string]devclass.Class{"old": devclass.C1, "new": devclass.C3})
	const x = "02:00:00:00:B0:03"
	runners := map[string]*fakeRunner{"old": wifiController(x), "new": wifiController()}
	wt := NewWifiTracker(db, nil, ws.NewHub(), 30*time.Second)
	clock := &testClock{t: at(12, 0, 0)}
	wt.now = clock.now
	wt.clock = newTierClock(NewScheduleRegistry())
	wt.clock.phase = noJitter
	wt.UseCadence(NewCadence(db, nil))
	wt.runner = scriptedRunners(runners)

	if _, ok := wt.Reading(x); ok {
		t.Fatal("a reading before any read")
	}
	wt.poll(context.Background())
	r, ok := wt.Reading(strings.ToLower(x))
	if !ok || r.DeviceID != "old" || !strings.HasPrefix(r.Signal, "-60") || !r.At.Equal(at(12, 0, 0)) {
		t.Fatalf("reading = %+v, %v; want the C1 controller's read of 12:00:00", r, ok)
	}

	// The client roams to the C3 controller, which is read again first: its
	// fresher read wins over the C1 controller's older one.
	setRegistrations(runners["new"], x)
	clock.set(at(12, 0, 31))
	wt.poll(context.Background())
	if r, ok := wt.Reading(x); !ok || r.DeviceID != "new" || !r.At.Equal(at(12, 0, 31)) {
		t.Fatalf("reading after the roam = %+v, %v; want the C3 controller's read of 12:00:31", r, ok)
	}

	// Both controllers go offline: their reads age out (3 of their
	// intervals) and the reading goes with them.
	for _, id := range []string{"old", "new"} {
		if _, err := db.Exec(`UPDATE devices SET status = 'offline' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
	clock.set(at(12, 30, 0))
	wt.poll(context.Background())
	if r, ok := wt.Reading(x); ok {
		t.Fatalf("a reading from reads that aged out: %+v", r)
	}
}
