package poller

import (
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// newTestRecorder is a recorder over a fresh database that started at
// 12:00:00, with the devices given (all online).
func newTestRecorder(t *testing.T, ids ...string) (*LoadRecorder, *sql.DB) {
	t.Helper()
	db := pollerTestDB(t)
	for i, id := range ids {
		mustCreateDevice(t, db, &queries.Device{ID: id, Identity: id, Address: "192.0.2." + string(rune('1'+i)), Status: "online"})
	}
	r := NewLoadRecorder(db)
	r.started = at(12, 0, 0)
	return r, db
}

func cmdEvent(dev string, end time.Time, wall time.Duration, rows int) routeros.CommandEvent {
	return routeros.CommandEvent{DeviceID: dev, Command: "/interface/print", Start: end.Add(-wall), Wall: wall, Rows: rows}
}

func minuteRow(t *testing.T, db *sql.DB, dev string, minute time.Time) queries.DeviceMinute {
	t.Helper()
	rows, err := queries.ListDeviceMinutes(db, dev, minute, minute.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("device %s has %d rows at %s, want 1", dev, len(rows), minute.Format("15:04"))
	}
	return rows[0]
}

func minuteRowCount(t *testing.T, db *sql.DB, dev string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_minutes WHERE device_id = ?`, dev).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every command and every pooled dial the routeros layer reports is counted
// against its device and the minute it ended in: that is what the NMS cost
// the device.
func TestLoadRecorderSelfMetricsPerDeviceMinute(t *testing.T) {
	r, db := newTestRecorder(t, "d1", "d2")

	r.Session(routeros.SessionEvent{DeviceID: "d1", At: at(12, 0, 1)})                             // the first dial: not a reconnect
	r.Session(routeros.SessionEvent{DeviceID: "d2", At: at(12, 0, 1), Err: errors.New("refused")}) // failed: nothing opened
	r.Session(routeros.SessionEvent{DeviceID: "d2", At: at(12, 0, 2)})                             // d2's first session
	r.Command(cmdEvent("d1", at(12, 0, 10), 120*time.Millisecond, 59))
	r.Command(cmdEvent("d1", at(12, 0, 25), 80*time.Millisecond, 59))
	timeout := cmdEvent("d1", at(12, 0, 58), 30*time.Second, 0)
	timeout.Err, timeout.Timeout = errors.New("timed out"), true
	r.Command(timeout)
	r.Session(routeros.SessionEvent{DeviceID: "d1", At: at(12, 0, 59)}) // redial after the timeout
	// The verification login of an address move runs on its own pool key.
	r.Command(cmdEvent("d1:verify", at(12, 0, 59), 10*time.Millisecond, 1))
	// A command that started in 12:00 and ended in 12:01 counts in 12:01.
	r.Command(cmdEvent("d1", at(12, 1, 2), 5*time.Second, 7))
	// A one-shot session nobody tied to a device.
	r.Command(cmdEvent("", at(12, 0, 30), time.Second, 3))
	r.Command(cmdEvent("d2", at(12, 0, 30), 40*time.Millisecond, 4))

	if got := r.LastCommandEnd("d1"); !got.Equal(at(12, 1, 2)) {
		t.Errorf("LastCommandEnd(d1) = %v, want 12:01:02", got)
	}
	if got := r.LastCommandEnd("unknown"); !got.IsZero() {
		t.Errorf("LastCommandEnd of a device never seen = %v, want zero", got)
	}

	r.FlushMinutes(at(12, 2, 0), nil)

	m := minuteRow(t, db, "d1", at(12, 0, 0))
	if m.NMSCmds != 4 || m.NMSWallMs != 120+80+30000+10 || m.NMSRows != 59+59+1 || m.NMSTimeouts != 1 || m.NMSReconnects != 1 {
		t.Errorf("d1 12:00 = cmds %d wall %d rows %d timeouts %d reconnects %d", m.NMSCmds, m.NMSWallMs, m.NMSRows, m.NMSTimeouts, m.NMSReconnects)
	}
	if m = minuteRow(t, db, "d1", at(12, 1, 0)); m.NMSCmds != 1 || m.NMSWallMs != 5000 || m.NMSRows != 7 || m.NMSReconnects != 0 {
		t.Errorf("d1 12:01 = %+v", m)
	}
	if m = minuteRow(t, db, "d2", at(12, 0, 0)); m.NMSCmds != 1 || m.NMSWallMs != 40 || m.NMSRows != 4 || m.NMSReconnects != 0 {
		t.Errorf("d2 12:00 = %+v", m)
	}
	// A minute the NMS sent nothing in still has its row, with zeros.
	if m = minuteRow(t, db, "d2", at(12, 1, 0)); m.NMSCmds != 0 || m.NMSWallMs != 0 {
		t.Errorf("d2 12:01 = %+v, want zeros", m)
	}
}

// One row per device-minute: covered when a Tier 0 interval spans part of
// it, otherwise a gap that says why.
func TestLoadRecorderCoverageAndGapReasons(t *testing.T) {
	ids := []string{"ok", "partial", "paused", "down", "nosession", "failed", "nobaseline", "idle", "unknown"}
	r, db := newTestRecorder(t, ids...)

	for _, id := range ids {
		if id == "down" || id == "unknown" {
			continue
		}
		r.NoteLiveness(id, at(12, 0, 0), true)
		r.NoteLiveness(id, at(12, 0, 30), true)
		r.NoteLiveness(id, at(12, 1, 0), true)
	}
	r.NoteLiveness("down", at(12, 0, 0), false)
	r.NoteLiveness("down", at(12, 0, 30), false)
	r.NoteLiveness("down", at(12, 1, 0), false)
	r.NotePaused("paused", at(12, 0, 20))

	r.FlushMinutes(at(12, 1, 0), []DeviceMinuteCoverage{
		{DeviceID: "ok", Minute: at(12, 0, 0), PolledSeconds: 60, Reads: 4},
		{DeviceID: "partial", Minute: at(12, 0, 0), PolledSeconds: 14.6, Reads: 1, Failed: 2},
		{DeviceID: "down", Minute: at(12, 0, 0), NoSession: 3},
		{DeviceID: "nosession", Minute: at(12, 0, 0), NoSession: 2},
		{DeviceID: "failed", Minute: at(12, 0, 0), Failed: 1, NoSession: 1},
		{DeviceID: "nobaseline", Minute: at(12, 0, 0), Reads: 1},
		// A device deleted since it was read.
		{DeviceID: "gone", Minute: at(12, 0, 0), PolledSeconds: 60, Reads: 4},
	})

	want := map[string]queries.DeviceMinute{
		"ok":         {PolledS: 60, Tier0OK: 4, ReachableS: 60},
		"partial":    {PolledS: 15, Tier0OK: 1, ReachableS: 60},
		"paused":     {ReachableS: 60, Paused: true, GapReason: queries.GapPaused},
		"down":       {GapReason: queries.GapUnreachable},
		"nosession":  {ReachableS: 60, GapReason: queries.GapNoSession},
		"failed":     {ReachableS: 60, GapReason: queries.GapReadFailed},
		"nobaseline": {Tier0OK: 1, ReachableS: 60, GapReason: queries.GapNoBaseline},
		"idle":       {ReachableS: 60, GapReason: queries.GapNotPolled},
		// No liveness result yet: not known to be unreachable.
		"unknown": {GapReason: queries.GapNotPolled},
	}
	for id, w := range want {
		w.DeviceID, w.Bucket = id, at(12, 0, 0)
		if got := minuteRow(t, db, id, at(12, 0, 0)); got != w {
			t.Errorf("%s =\n %+v\nwant\n %+v", id, got, w)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_minutes`).Scan(&n); err != nil || n != len(ids) {
		t.Errorf("device_minutes rows = %d (err %v), want one per managed device", n, err)
	}
}

// Liveness is a timeline: a check stands until the next one, so the seconds
// a device was up are counted across minutes, and a result older than the
// longest plausible gap between checks is not stretched.
func TestLoadRecorderReachableSeconds(t *testing.T) {
	r, db := newTestRecorder(t, "d1", "d2")
	r.SetLivenessSpan(2 * time.Minute)

	r.NoteLiveness("d1", at(12, 0, 10), true)
	r.NoteLiveness("d1", at(12, 0, 40), true)
	r.NoteLiveness("d1", at(12, 1, 10), false)
	r.NoteLiveness("d1", at(12, 1, 40), false)
	r.NoteLiveness("d1", at(12, 2, 10), true)
	// d2 answered once; then the health loop stalled.
	r.NoteLiveness("d2", at(12, 0, 0), true)

	r.FlushMinutes(at(12, 5, 0), nil)

	for minute, want := range map[time.Time]int64{at(12, 0, 0): 50, at(12, 1, 0): 10, at(12, 2, 0): 50, at(12, 3, 0): 60, at(12, 4, 0): 10} {
		if got := minuteRow(t, db, "d1", minute).ReachableS; got != want {
			t.Errorf("d1 reachable_s at %s = %d, want %d", minute.Format("15:04"), got, want)
		}
	}
	// 12:04:10 is two minutes after d1's last check: nothing is known beyond.
	if m := minuteRow(t, db, "d1", at(12, 4, 0)); m.GapReason != queries.GapNotPolled {
		t.Errorf("d1 12:04 gap = %q, want not_polled (10 s reachable)", m.GapReason)
	}
	for minute, want := range map[time.Time]int64{at(12, 0, 0): 60, at(12, 1, 0): 60, at(12, 2, 0): 0, at(12, 4, 0): 0} {
		m := minuteRow(t, db, "d2", minute)
		if m.ReachableS != want {
			t.Errorf("d2 reachable_s at %s = %d, want %d", minute.Format("15:04"), m.ReachableS, want)
		}
		if want == 0 && m.GapReason != queries.GapNotPolled {
			t.Errorf("d2 at %s gap = %q: a stale liveness result is unknown, not unreachable", minute.Format("15:04"), m.GapReason)
		}
	}
}

func loadSample(cpu int, free, uptime int64) routeros.LoadSample {
	return routeros.LoadSample{CPULoad: cpu, FreeMemory: free, HasFreeMemory: true, UptimeSeconds: uptime, HasUptime: true}
}

// Quiet-window samples are summed per minute; the post-poll sample is kept
// apart; an uptime that went backwards marks the minute as rebooted.
func TestLoadRecorderCPUSamples(t *testing.T) {
	r, db := newTestRecorder(t, "d1")

	r.AddSample("d1", at(12, 0, 12), loadSample(10, 9000, 500))
	r.AddSample("d1", at(12, 0, 58), loadSample(30, 7000, 546))
	r.AddPostSample("d1", at(12, 0, 30), 64)
	r.AddSample("d1", at(12, 1, 12), loadSample(20, 8000, 606))
	r.AddSample("d1", at(12, 2, 12), loadSample(90, 20000, 14)) // rebooted
	r.AddSample("d1", at(12, 3, 12), loadSample(5, 21000, 74))
	// A sample without memory or uptime still counts its CPU.
	r.AddSample("d1", at(12, 4, 12), routeros.LoadSample{CPULoad: 7})

	if got, ok := r.LatestCPU()["d1"]; !ok || got.CPU != 7 || !got.At.Equal(at(12, 4, 12)) {
		t.Errorf("LatestCPU = %+v %v, want the 12:04:12 sample", got, ok)
	}

	r.FlushMinutes(at(12, 5, 0), nil)

	m := minuteRow(t, db, "d1", at(12, 0, 0))
	if m.CPUSum != 40 || m.CPUSamples != 2 || m.CPUMax != 30 || m.CPUPost != nullInt(64) || m.FreeMemoryMin != nullInt(7000) ||
		m.UptimeS != nullInt(546) || m.Rebooted {
		t.Errorf("12:00 = %+v", m)
	}
	if m = minuteRow(t, db, "d1", at(12, 1, 0)); m.CPUSum != 20 || m.CPUSamples != 1 || m.CPUPost.Valid || m.Rebooted {
		t.Errorf("12:01 = %+v", m)
	}
	if m = minuteRow(t, db, "d1", at(12, 2, 0)); !m.Rebooted || m.UptimeS != nullInt(14) || m.CPUMax != 90 {
		t.Errorf("12:02 = %+v, want the reboot marked", m)
	}
	if m = minuteRow(t, db, "d1", at(12, 3, 0)); m.Rebooted {
		t.Errorf("12:03 = %+v: only the minute the uptime went backwards in is marked", m)
	}
	if m = minuteRow(t, db, "d1", at(12, 4, 0)); m.CPUSamples != 1 || m.CPUSum != 7 || m.FreeMemoryMin.Valid || m.UptimeS.Valid {
		t.Errorf("12:04 = %+v, want CPU only", m)
	}

	// A restarted NMS still notices a reboot it did not watch: the last
	// stored uptime is the baseline.
	restarted := NewLoadRecorder(db)
	restarted.started = at(12, 5, 0)
	restarted.AddSample("d1", at(12, 5, 12), loadSample(50, 22000, 3))
	restarted.FlushMinutes(at(12, 6, 0), nil)
	if m = minuteRow(t, db, "d1", at(12, 5, 0)); !m.Rebooted {
		t.Errorf("12:05 after a restart = %+v, want the reboot marked", m)
	}
}

// The collector's cycles end to end: its Tier 0 intervals become the
// device's covered seconds, and crossing the hour rolls them into
// device_hours and onto the port's hourly row.
func TestCollectorCoverageBecomesDeviceMinutesAndHours(t *testing.T) {
	run := switchRunner("1000")
	c, clock := newTestPortStats(t, run)
	r := NewLoadRecorder(c.db)
	r.started = at(10, 58, 0)
	c.UseCoverageSink(r)
	const iv = 15 * time.Second

	clock.set(at(10, 58, 5))
	c.startupRollup()
	rx := int64(1000)
	for ts := at(10, 58, 5); !ts.After(at(11, 1, 5)); ts = ts.Add(iv) {
		clock.set(ts)
		run.byCmd["/interface/print"] = switchRunner(strconv.FormatInt(rx, 10)).byCmd["/interface/print"]
		c.cycle(t.Context(), iv)
		rx += 15_000
	}

	if m := minuteRow(t, c.db, "sw", at(10, 58, 0)); m.PolledS != 55 || m.Tier0OK != 4 || m.GapReason != "" {
		t.Errorf("10:58 = %+v, want 55 s polled by 4 reads", m)
	}
	if m := minuteRow(t, c.db, "sw", at(10, 59, 0)); m.PolledS != 60 || m.Tier0OK != 4 {
		t.Errorf("10:59 = %+v, want a fully covered minute", m)
	}
	hours, err := queries.ListDeviceHours(c.db, "sw", at(10, 0, 0), at(11, 0, 0))
	if err != nil || len(hours) != 1 || hours[0].CoveredS != 115 {
		t.Fatalf("device_hours = %+v (err %v), want hour 10 with 115 s covered", hours, err)
	}
	ports, err := queries.GetPortStatsSeries(c.db, queries.PortStats1h, "sw", "ether1", at(10, 0, 0), at(11, 0, 0))
	if err != nil || len(ports) != 1 || ports[0].CoveredS != nullInt(115) || ports[0].RxBytes != 7*15_000+10_000 {
		t.Fatalf("port_stats_1h = %+v (err %v), want the device's 115 covered seconds", ports, err)
	}
}

// Minutes are kept 14 days, hours 730, and a deleted device's rows go with
// the next sweep.
func TestSweepDeviceLoad(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "d1", Address: "192.0.2.1"})
	now := at(12, 0, 0)
	day := 24 * time.Hour
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, age := range []time.Duration{time.Hour, 13 * day, 15 * day} {
		exec(`INSERT INTO device_minutes (device_id, bucket, polled_s) VALUES ('d1', ?, 60)`, now.Add(-age).Unix())
	}
	for _, age := range []time.Duration{time.Hour, 729 * day, 731 * day} {
		exec(`INSERT INTO device_hours (device_id, bucket, covered_s) VALUES ('d1', ?, 3600)`, now.Add(-age).Unix())
	}
	exec(`INSERT INTO device_minutes (device_id, bucket, polled_s) VALUES ('deleted', ?, 60)`, now.Add(-time.Hour).Unix())
	exec(`INSERT INTO device_hours (device_id, bucket, covered_s) VALUES ('deleted', ?, 3600)`, now.Add(-time.Hour).Unix())

	sweepDeviceLoad(db, now)

	count := func(table string) (n int) {
		t.Helper()
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("device_minutes"); n != 2 {
		t.Errorf("device_minutes left = %d, want the two rows younger than 14 days", n)
	}
	if n := count("device_hours"); n != 2 {
		t.Errorf("device_hours left = %d, want the two rows younger than 730 days", n)
	}
}

// An instance that does not poll (a copy of a production database) must not
// grow the table: the manager wires the recorder to the collector, but rows
// are only written by the collector's flush, which never runs there — the
// database-only jobs Start launches write none.
func TestDeviceMinutesAreNotWrittenWithoutPolling(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "d1", Address: "192.0.2.1", Status: "online"})
	cfg := &config.Config{Pollers: config.PollersOff, RetentionInterval: time.Hour, HealthInterval: 30 * time.Second}
	m := NewManager(db, routeros.NewPool(), ws.NewHub(), cfg)
	if m.portStats.sink != CoverageSink(m.load) {
		t.Fatal("the manager must hand the collector's coverage to the load recorder")
	}
	// Whatever the recorder holds, and however long ago it started.
	m.load.started = time.Now().Add(-10 * time.Minute)
	m.load.NoteLiveness("d1", time.Now().Add(-5*time.Minute), true)
	m.load.Command(cmdEvent("d1", time.Now().Add(-5*time.Minute), time.Second, 1))

	m.Start()
	m.Stop()

	if n := minuteRowCount(t, db, "d1"); n != 0 {
		t.Fatalf("an instance with polling off wrote %d device-minutes", n)
	}
	// The same recorder does write once a flush reaches it.
	m.load.FlushMinutes(time.Now(), nil)
	if n := minuteRowCount(t, db, "d1"); n == 0 {
		t.Fatal("the recorder wrote nothing on a flush: the check above proves nothing")
	}
}

// Rows are written once per minute, from the recorder's start on, with a gap
// row for a minute nothing happened in; a stalled process does not backfill
// without bound.
func TestLoadRecorderFlushIsIncrementalAndBounded(t *testing.T) {
	r, db := newTestRecorder(t, "d1")
	r.started = at(12, 0, 40)

	r.FlushMinutes(at(12, 0, 0), nil) // the first cycle: nothing is final yet
	if n := minuteRowCount(t, db, "d1"); n != 0 {
		t.Fatalf("rows after a flush that finalised nothing = %d", n)
	}
	r.Command(cmdEvent("d1", at(12, 0, 50), time.Second, 1))
	r.FlushMinutes(at(12, 1, 0), []DeviceMinuteCoverage{{DeviceID: "d1", Minute: at(12, 0, 0), PolledSeconds: 15, Reads: 2}})
	if n := minuteRowCount(t, db, "d1"); n != 1 {
		t.Fatalf("rows = %d, want the start minute only (nothing before the NMS ran)", n)
	}
	// The same boundary again changes nothing — and must not wipe the row.
	r.FlushMinutes(at(12, 1, 0), nil)
	if m := minuteRow(t, db, "d1", at(12, 0, 0)); m.PolledS != 15 || m.NMSCmds != 1 {
		t.Errorf("12:00 after a repeated flush = %+v", m)
	}
	// Three minutes become final at once; two of them saw nothing.
	r.FlushMinutes(at(12, 4, 0), []DeviceMinuteCoverage{{DeviceID: "d1", Minute: at(12, 2, 0), PolledSeconds: 60, Reads: 4}})
	if n := minuteRowCount(t, db, "d1"); n != 4 {
		t.Fatalf("rows = %d, want 12:00–12:03", n)
	}
	if m := minuteRow(t, db, "d1", at(12, 3, 0)); m.Covered() || m.GapReason != queries.GapNotPolled {
		t.Errorf("12:03 = %+v, want an explicit gap", m)
	}
	// A late event for a minute already written lands in the first open one.
	r.Command(cmdEvent("d1", at(12, 3, 59), time.Second, 1))
	r.FlushMinutes(at(12, 5, 0), nil)
	if m := minuteRow(t, db, "d1", at(12, 4, 0)); m.NMSCmds != 1 {
		t.Errorf("12:04 = %+v, want the late command", m)
	}

	// The process stalled for half a day: only the last two hours are
	// written as gaps, the rest stays absent.
	r.FlushMinutes(at(23, 0, 0), nil)
	rows, err := queries.ListDeviceMinutes(db, "d1", at(12, 5, 0), at(23, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != int(maxLoadBackfill/time.Minute) || !rows[0].Bucket.Equal(at(23, 0, 0).Add(-maxLoadBackfill)) {
		t.Errorf("rows after a stall = %d starting %v, want %d from %v", len(rows), rows[0].Bucket, int(maxLoadBackfill/time.Minute), at(23, 0, 0).Add(-maxLoadBackfill))
	}
}
