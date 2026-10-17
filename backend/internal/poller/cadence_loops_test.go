package poller

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// sqlStamp is a time in CURRENT_TIMESTAMP's text layout, for seeding rows.
func sqlStamp(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05") }

// setClass stores a probed class for a test device.
func setClass(t *testing.T, db *sql.DB, id string, c devclass.Class) {
	t.Helper()
	if err := queries.SetDeviceClass(db, id, c, devclass.ResourceFacts{}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// A paused device runs neither Tier 0 nor Tier 4a; its snapshot and rates do
// not expire while the pause lasts; and the counter delta across the pause is
// a gap — the paused minutes get no rows (no zeros, no spread of the bytes
// the device counted meanwhile).
func TestPortStatsPausedDeviceNothingAgesAndPauseIsAGap(t *testing.T) {
	run := switchRunner("1000")
	c, clock := newTestPortStats(t, run)
	cs := &stubCadence{
		iv:     map[devclass.Tier]time.Duration{devclass.TierCounters: 15 * time.Second, devclass.TierEthernet: 5 * time.Minute},
		paused: map[string]bool{},
	}
	c.UseCadence(cs)
	const iv = 15 * time.Second
	ctx := context.Background()
	counters := func(rx string) { run.byCmd["/interface/print"] = switchRunner(rx).byCmd["/interface/print"] }

	c.cycle(ctx, iv) // 12:00:05: baseline
	clock.set(at(12, 0, 20))
	counters("16000") // +15000 B in 15 s
	c.cycle(ctx, iv)
	if r := snapshotRate(t, c, "sw", "ether1"); r.RxBps != 8000 {
		t.Fatalf("rate before the pause = %d, want 8000", r.RxBps)
	}

	cs.paused["sw"] = true
	before := len(run.calls)
	for ts := at(12, 0, 35); ts.Before(at(13, 0, 0)); ts = ts.Add(iv) {
		clock.set(ts)
		c.cycle(ctx, iv)
	}
	if len(run.calls) != before {
		t.Fatalf("a paused device was read: %q", run.calls[before:])
	}
	clock.set(at(12, 59, 50))
	if _, readAt, ok := c.DeviceSnapshot("sw"); !ok || !readAt.Equal(at(12, 0, 20)) {
		t.Errorf("paused snapshot: ok=%v read at %v, want the last read still served", ok, readAt)
	}
	if r := snapshotRate(t, c, "sw", "ether1"); r.RxBps != 8000 {
		t.Errorf("paused rate = %d, want the last one kept", r.RxBps)
	}
	if runs := c.reg.NextRuns("sw"); len(runs) != 0 {
		t.Errorf("paused device next runs = %v, want none", runs)
	}

	// Resumed: the snapshot ages on the wall clock again, and the first
	// read after the pause is a gap.
	cs.paused["sw"] = false
	clock.set(at(13, 0, 5))
	counters("50000000")
	c.cycle(ctx, iv)
	if r := snapshotRate(t, c, "sw", "ether1"); r.RxBps != 0 {
		t.Errorf("first rate after the pause = %d, want 0 (a gap)", r.RxBps)
	}
	clock.set(at(13, 0, 20))
	counters("50015000")
	c.cycle(ctx, iv)
	if r := snapshotRate(t, c, "sw", "ether1"); r.RxBps != 8000 {
		t.Errorf("second rate after the pause = %d, want 8000", r.RxBps)
	}
	clock.set(at(13, 2, 5))
	c.finishCycle(at(13, 2, 5), iv)

	rows, err := queries.GetPortStatsSeries(c.db, queries.PortStats1m, "sw", "ether1", at(11, 0, 0), at(14, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	var buckets []string
	for _, r := range rows {
		buckets = append(buckets, r.Bucket.Format("15:04"))
	}
	if strings.Join(buckets, ",") != "12:00,13:00" {
		t.Errorf("minute rows at %v, want 12:00 and 13:00 only — the paused hour is a gap", buckets)
	}
}

// The snapshot of a device that is not paused still expires on the wall
// clock — the pause exemption is not a general one.
func TestPortStatsSnapshotStillExpiresWithoutPause(t *testing.T) {
	c, clock := newTestPortStats(t, switchRunner("1000"))
	c.UseCadence(&stubCadence{iv: map[devclass.Tier]time.Duration{devclass.TierCounters: 15 * time.Second}})
	c.cycle(context.Background(), 15*time.Second)
	clock.set(at(12, 0, 51))
	if _, _, ok := c.DeviceSnapshot("sw"); ok {
		t.Error("an unpaused device's snapshot older than 3 intervals must not be served")
	}
}

// topoTestManager is a bare manager for the topology tick: real class
// cadence, a fake clock, scripted devices and no stagger.
func topoTestManager(t *testing.T, db *sql.DB, devices map[string]*fakeRunner) (*Manager, *testClock, *ScheduleRegistry) {
	t.Helper()
	cfg := &config.Config{TopologyInterval: time.Minute, NetworkHealthInterval: time.Minute}
	reg := NewScheduleRegistry()
	clock := &testClock{t: at(12, 0, 0)}
	m := &Manager{
		db: db, hub: ws.NewHub(), cfg: cfg,
		cadence:      NewCadence(db, cfg),
		topoClock:    newTierClock(reg),
		portStats:    NewPortStatsCollector(db, nil, nil),
		ipRejectSeen: map[string]time.Time{},
		now:          clock.now,
		runner: func(id string) routeros.Runner {
			if r, ok := devices[id]; ok {
				return r
			}
			return nil
		},
	}
	m.topoClock.phase = noJitter
	return m, clock, reg
}

func topoRunner() *fakeRunner {
	return &fakeRunner{byCmd: map[string]fakeReply{
		"/ip/neighbor/print": rep(),
		"/ip/route/print":    rep(),
	}}
}

// The topology loop reads each device's neighbors (Tier 3d) and default
// routes (Tier 3e) on its own class cadence over the topology env floor —
// C3: neighbors 2 min, routes 60 s; C1: both 5 min — and never a paused
// device; the link graph is rebuilt once per floor interval.
func TestTopologyTickPerClassCadenceAndPause(t *testing.T) {
	db := pollerTestDB(t)
	for _, id := range []string{"r3", "s1", "held"} {
		mustCreateDevice(t, db, &queries.Device{ID: id, Identity: id, Address: "192.0.2." + map[string]string{"r3": "1", "s1": "2", "held": "3"}[id]})
	}
	setClass(t, db, "r3", devclass.C3)
	setClass(t, db, "s1", devclass.C1)
	setClass(t, db, "held", devclass.C3)
	if err := queries.PauseDevicePolling(db, "held", time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	runners := map[string]*fakeRunner{"r3": topoRunner(), "s1": topoRunner(), "held": topoRunner()}
	m, clock, reg := topoTestManager(t, db, runners)
	reads := func(id, cmd string) int { return runners[id].countCalls(cmd) }

	steps := []struct {
		at                 time.Duration
		r3n, r3r, s1n, s1r int
	}{
		{0, 1, 1, 1, 1},
		{30 * time.Second, 1, 1, 1, 1},
		{time.Minute, 1, 2, 1, 1},
		{2 * time.Minute, 2, 3, 1, 1},
		{5 * time.Minute, 3, 4, 2, 2},
	}
	for _, st := range steps {
		clock.set(at(12, 0, 0).Add(st.at))
		m.pollTopology(context.Background())
		if got := [4]int{reads("r3", "/ip/neighbor/print"), reads("r3", "/ip/route/print"), reads("s1", "/ip/neighbor/print"), reads("s1", "/ip/route/print")}; got != [4]int{st.r3n, st.r3r, st.s1n, st.s1r} {
			t.Errorf("+%v: neighbor/route reads r3 %d/%d, s1 %d/%d; want %d/%d, %d/%d",
				st.at, got[0], got[1], got[2], got[3], st.r3n, st.r3r, st.s1n, st.s1r)
		}
	}
	if len(runners["held"].calls) != 0 {
		t.Errorf("a paused device was read: %q", runners["held"].calls)
	}
	if runs := reg.NextRuns("held"); len(runs) != 0 {
		t.Errorf("paused device next runs = %v", runs)
	}
	if next := reg.NextRuns("s1")["neighbors"]; !next.Equal(at(12, 10, 0)) {
		t.Errorf("C1 next neighbor read = %v, want 5 min after the last", next)
	}
	if !m.lastBuild.Equal(at(12, 5, 0)) {
		t.Errorf("last graph rebuild at %v", m.lastBuild)
	}
}

// A device without a pooled connection is retried after noClientRetry, not
// a whole interval later.
func TestTopologyTickRetriesWithoutClient(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "late", Identity: "late", Address: "192.0.2.9"})
	runners := map[string]*fakeRunner{}
	m, clock, _ := topoTestManager(t, db, runners)
	m.pollTopology(context.Background())
	runners["late"] = topoRunner()
	clock.set(at(12, 0, 0).Add(noClientRetry - time.Second))
	m.pollTopology(context.Background())
	if len(runners["late"].calls) != 0 {
		t.Fatal("retried before noClientRetry")
	}
	clock.set(at(12, 0, 0).Add(noClientRetry))
	m.pollTopology(context.Background())
	if got := len(runners["late"].calls); got != 2 {
		t.Errorf("calls after the retry delay = %d, want the neighbor and route reads", got)
	}
}

// The neighbor sweep keeps a device's rows for a day (or three of its reads,
// when a floor is longer) and never deletes a paused device's.
func TestSweepNeighborsPerDeviceAndPause(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "sw", Identity: "sw", Address: "192.0.2.10"})
	mustCreateDevice(t, db, &queries.Device{ID: "held", Identity: "held", Address: "192.0.2.11"})
	now := time.Now().UTC()
	if err := queries.PauseDevicePolling(db, "held", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []struct {
		dev, mac string
		age      time.Duration
	}{
		{"sw", "02:00:00:00:10:01", 25 * time.Hour},
		{"sw", "02:00:00:00:10:02", time.Hour},
		{"held", "02:00:00:00:11:01", 30 * time.Hour},
	} {
		if err := queries.UpsertNeighbor(db, &queries.Neighbor{ID: n.dev + ":ether1:" + n.mac, DeviceID: n.dev, LocalInterface: "ether1", NeighborMAC: n.mac}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE neighbors SET last_seen = ? WHERE device_id = ? AND neighbor_mac = ?`, sqlStamp(now.Add(-n.age)), n.dev, n.mac); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{db: db, cadence: NewCadence(db, nil)}
	m.sweepNeighbors(now)
	left := map[string]int{}
	rows, _ := queries.ListAllNeighbors(db)
	for _, r := range rows {
		left[r.DeviceID]++
	}
	if left["sw"] != 1 || left["held"] != 1 {
		t.Errorf("neighbors left = %v, want sw's recent row and the paused device's old one", left)
	}
}
