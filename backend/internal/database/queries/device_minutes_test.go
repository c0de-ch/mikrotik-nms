package queries

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

func n64(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func TestDeviceMinutesRoundTrip(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")

	rows := []DeviceMinute{
		{DeviceID: "1", Bucket: minute(12, 0), PolledS: 60, Tier0OK: 4, ReachableS: 60,
			CPUSum: 31, CPUSamples: 2, CPUMax: 19, CPUPost: n64(44), FreeMemoryMin: n64(9_000_000), UptimeS: n64(3600),
			NMSCmds: 7, NMSWallMs: 420, NMSRows: 310, NMSTimeouts: 1, NMSReconnects: 1},
		{DeviceID: "1", Bucket: minute(12, 1), ReachableS: 60, Paused: true, GapReason: GapPaused},
		{DeviceID: "1", Bucket: minute(12, 2), PolledS: 20, Tier0OK: 1, ReachableS: 60, CPUSum: 5, CPUSamples: 1, CPUMax: 5,
			UptimeS: n64(12), Rebooted: true},
		// A device deleted since the minute was recorded: skipped.
		{DeviceID: "gone", Bucket: minute(12, 0), PolledS: 60},
	}
	if err := UpsertDeviceMinutes(db, rows); err != nil {
		t.Fatalf("UpsertDeviceMinutes: %v", err)
	}
	if n := countRows(t, db, "device_minutes"); n != 3 {
		t.Fatalf("device_minutes rows = %d, want 3", n)
	}

	// Buckets are unix seconds; NULL where nothing was measured.
	var bucket int64
	var cpuSum, cpuMax, gap any
	if err := db.QueryRow(`SELECT bucket, cpu_sum, cpu_max, gap_reason FROM device_minutes WHERE paused = 1`).
		Scan(&bucket, &cpuSum, &cpuMax, &gap); err != nil {
		t.Fatal(err)
	}
	if bucket != minute(12, 1).Unix() || cpuSum != nil || cpuMax != nil || gap != "paused" {
		t.Errorf("paused row = bucket %d cpu_sum %v cpu_max %v gap %v", bucket, cpuSum, cpuMax, gap)
	}
	if err := db.QueryRow(`SELECT gap_reason FROM device_minutes WHERE bucket = ?`, minute(12, 0).Unix()).Scan(&gap); err != nil || gap != nil {
		t.Errorf("covered row gap_reason = %v (err %v), want NULL", gap, err)
	}

	got, err := ListDeviceMinutes(db, "1", minute(12, 0), minute(12, 3))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, rows[:3]) {
		t.Fatalf("round trip =\n %+v\nwant\n %+v", got, rows[:3])
	}
	if !got[0].Covered() || got[1].Covered() || !got[2].Covered() {
		t.Error("Covered: a minute is covered when polled_s > 0")
	}
	if avg, ok := got[0].CPUAvg(); !ok || avg != 15.5 {
		t.Errorf("CPUAvg = %v %v, want 15.5", avg, ok)
	}
	if _, ok := got[1].CPUAvg(); ok {
		t.Error("CPUAvg of a minute without samples must not be known")
	}

	// [from, to) and replace-on-key.
	upd := rows[0]
	upd.NMSCmds = 9
	if err := UpsertDeviceMinutes(db, []DeviceMinute{upd}); err != nil {
		t.Fatal(err)
	}
	got, _ = ListDeviceMinutes(db, "1", minute(12, 0), minute(12, 1))
	if len(got) != 1 || got[0].NMSCmds != 9 {
		t.Errorf("replace = %+v", got)
	}

	since, err := ListDeviceMinutesSince(db, minute(12, 1))
	if err != nil || len(since) != 2 || since[0].Bucket != minute(12, 1) {
		t.Errorf("ListDeviceMinutesSince = %+v err %v", since, err)
	}
}

func TestDeviceCoverageQueries(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	seedPortStatsDevice(t, db, "2")
	seedPortStatsDevice(t, db, "3") // never recorded

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(UpsertDeviceMinutes(db, []DeviceMinute{
		{DeviceID: "1", Bucket: minute(11, 58), PolledS: 60},
		{DeviceID: "1", Bucket: minute(12, 0), PolledS: 60},
		{DeviceID: "1", Bucket: minute(12, 1), GapReason: GapUnreachable},
		{DeviceID: "1", Bucket: minute(12, 2), PolledS: 15},
		{DeviceID: "2", Bucket: minute(12, 1), PolledS: 60},
	}))

	cov, err := GetDeviceCoverage(db, DeviceMinutes, "1", minute(12, 0), minute(12, 5))
	must(err)
	if !cov.HasFirst || !cov.First.Equal(minute(11, 58)) {
		t.Errorf("first = %v %v, want 11:58 (the first row ever, not the first in the window)", cov.First, cov.HasFirst)
	}
	want := map[int64]int64{minute(12, 0).Unix(): 60, minute(12, 2).Unix(): 15}
	if len(cov.Secs) != len(want) || cov.Secs[minute(12, 0).Unix()] != 60 || cov.Secs[minute(12, 2).Unix()] != 15 {
		t.Errorf("covered = %v, want %v (gap rows carry no seconds)", cov.Secs, want)
	}
	if cov, err = GetDeviceCoverage(db, DeviceMinutes, "3", minute(12, 0), minute(12, 5)); err != nil || cov.HasFirst || len(cov.Secs) != 0 {
		t.Errorf("unrecorded device = %+v err %v", cov, err)
	}

	fleet, err := GetFleetCoverage(db, DeviceMinutes, minute(12, 0), minute(12, 5))
	must(err)
	if c := fleet["1"]; !c.HasFirst || !c.First.Equal(minute(11, 58)) || c.Secs != 75 {
		t.Errorf("fleet[1] = %+v, want first 11:58 and 75 s in the window", c)
	}
	if c := fleet["2"]; !c.HasFirst || !c.First.Equal(minute(12, 1)) || c.Secs != 60 {
		t.Errorf("fleet[2] = %+v", c)
	}
	if c, ok := fleet["3"]; !ok || c.HasFirst || c.Secs != 0 {
		t.Errorf("fleet[3] = %+v ok %v, want a known device without rows", c, ok)
	}
	if _, err := GetDeviceCoverage(db, DeviceLoadTable("users"), "1", minute(12, 0), minute(12, 5)); err == nil {
		t.Error("unknown table must be rejected")
	}
}

func TestRollupDeviceHours(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	if err := UpdateDeviceInfo(db, "1", "MikroTik", "board", "7.20", "", "arm"); err != nil {
		t.Fatal(err)
	}

	var rows []DeviceMinute
	// Hour 10: 20 covered minutes with CPU 1..20, one gap minute, one
	// covered minute without a sample.
	for i := 0; i < 20; i++ {
		rows = append(rows, DeviceMinute{DeviceID: "1", Bucket: minute(10, i), PolledS: 60, Tier0OK: 1,
			CPUSum: int64(i + 1), CPUSamples: 1, CPUMax: int64(i + 1), FreeMemoryMin: n64(int64(1000 - i)), NMSCmds: 5})
	}
	rows = append(rows,
		DeviceMinute{DeviceID: "1", Bucket: minute(10, 30), GapReason: GapUnreachable, NMSCmds: 2},
		DeviceMinute{DeviceID: "1", Bucket: minute(10, 31), PolledS: 30, NMSCmds: 1},
		// Two samples in one minute: the p95 runs over minute averages.
		DeviceMinute{DeviceID: "1", Bucket: minute(11, 0), PolledS: 60, CPUSum: 90, CPUSamples: 2, CPUMax: 80, NMSCmds: 6},
		// Hour 12: only gaps.
		DeviceMinute{DeviceID: "1", Bucket: minute(12, 0), Paused: true, GapReason: GapPaused},
		// Outside the rolled window.
		DeviceMinute{DeviceID: "1", Bucket: minute(13, 0), PolledS: 60},
	)
	if err := UpsertDeviceMinutes(db, rows); err != nil {
		t.Fatal(err)
	}

	roll := func() {
		t.Helper()
		if err := RollupDeviceHours(db, minute(10, 20), minute(13, 40)); err != nil { // truncated: [10:00, 13:00)
			t.Fatalf("RollupDeviceHours: %v", err)
		}
	}
	roll()
	got, err := ListDeviceHours(db, "1", minute(0, 0), minute(23, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []DeviceHour{
		{DeviceID: "1", Bucket: minute(10, 0), CoveredS: 20*60 + 30,
			CPUAvg: sql.NullFloat64{Float64: 10.5, Valid: true}, CPUMax: n64(20), CPUP95: n64(19),
			MemFreeMin: n64(981), NMSCmds: 103, ROSVersion: "7.20"},
		{DeviceID: "1", Bucket: minute(11, 0), CoveredS: 60,
			CPUAvg: sql.NullFloat64{Float64: 45, Valid: true}, CPUMax: n64(80), CPUP95: n64(45), NMSCmds: 6, ROSVersion: "7.20"},
		{DeviceID: "1", Bucket: minute(12, 0), ROSVersion: "7.20"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rollup =\n %+v\nwant\n %+v", got, want)
	}

	// Idempotent; a re-roll after an upgrade keeps the version the hour was
	// first rolled with.
	if err := UpdateDeviceInfo(db, "1", "MikroTik", "board", "7.21", "", "arm"); err != nil {
		t.Fatal(err)
	}
	roll()
	again, _ := ListDeviceHours(db, "1", minute(0, 0), minute(23, 0))
	if !slices.Equal(again, want) {
		t.Fatalf("re-roll changed rows:\n %+v", again)
	}

	// A late minute is picked up by the next re-roll.
	if err := UpsertDeviceMinutes(db, []DeviceMinute{{DeviceID: "1", Bucket: minute(11, 1), PolledS: 60, NMSCmds: 4}}); err != nil {
		t.Fatal(err)
	}
	roll()
	again, _ = ListDeviceHours(db, "1", minute(11, 0), minute(12, 0))
	if len(again) != 1 || again[0].CoveredS != 120 || again[0].NMSCmds != 10 {
		t.Errorf("re-roll after a late minute = %+v", again)
	}

	cov, err := GetDeviceCoverage(db, DeviceHours, "1", minute(10, 0), minute(13, 0))
	if err != nil || !cov.HasFirst || !cov.First.Equal(minute(10, 0)) || len(cov.Secs) != 2 || cov.Secs[minute(10, 0).Unix()] != 1230 {
		t.Errorf("hourly coverage = %+v err %v", cov, err)
	}
	if err := RollupDeviceHours(db, minute(12, 0), minute(12, 0)); err != nil {
		t.Errorf("empty window: %v", err)
	}
}

func TestDeviceLoadRetention(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	oldChunk, oldPause := portStatsDeleteChunk, portStatsDeletePause
	portStatsDeleteChunk, portStatsDeletePause = 2, 0
	t.Cleanup(func() { portStatsDeleteChunk, portStatsDeletePause = oldChunk, oldPause })

	var rows []DeviceMinute
	for i := 0; i < 7; i++ {
		rows = append(rows, DeviceMinute{DeviceID: "1", Bucket: minute(12, i), PolledS: 60})
	}
	if err := UpsertDeviceMinutes(db, rows); err != nil {
		t.Fatal(err)
	}
	if err := RollupDeviceHours(db, minute(12, 0), minute(13, 0)); err != nil {
		t.Fatal(err)
	}
	// Rows of a device that no longer exists (the tables have no foreign key).
	if _, err := db.Exec(`INSERT INTO device_minutes (device_id, bucket, polled_s) VALUES ('gone', ?, 60), ('gone', ?, 60), ('gone', ?, 60)`,
		minute(12, 5).Unix(), minute(12, 6).Unix(), minute(12, 7).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO device_hours (device_id, bucket, covered_s) VALUES ('gone', ?, 60)`, minute(12, 0).Unix()); err != nil {
		t.Fatal(err)
	}

	n, err := DeleteOldDeviceLoad(db, DeviceMinutes, minute(12, 5))
	if err != nil || n != 5 {
		t.Fatalf("DeleteOldDeviceLoad = %d, %v; want 5 rows in chunks", n, err)
	}
	if left := countRows(t, db, "device_minutes"); left != 5 {
		t.Errorf("device_minutes left = %d, want 2 + 3 orphans", left)
	}
	n, err = DeleteOrphanDeviceLoad(db)
	if err != nil || n != 4 {
		t.Fatalf("DeleteOrphanDeviceLoad = %d, %v; want 3 minute rows + 1 hour row", n, err)
	}
	if left := countRows(t, db, "device_minutes"); left != 2 {
		t.Errorf("device_minutes left = %d, want 2", left)
	}
	if n, err = DeleteOldDeviceLoad(db, DeviceHours, minute(13, 0)); err != nil || n != 1 {
		t.Errorf("hourly delete = %d, %v", n, err)
	}
	if _, err := DeleteOldDeviceLoad(db, DeviceLoadTable("users"), time.Now()); err == nil {
		t.Error("unknown table must be rejected")
	}
}

func TestUpdateDeviceLoadLeavesStatusAlone(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	msg := "ping failed"
	if err := MarkDeviceUnreachable(db, "1", "unknown", &msg); err != nil {
		t.Fatal(err)
	}
	total := int64(64 << 20)
	if _, err := db.Exec(`UPDATE devices SET memory_total = ? WHERE id = '1'`, total); err != nil {
		t.Fatal(err)
	}
	used := int64(40 << 20)
	if err := UpdateDeviceLoad(db, "1", 37, &used, "1d2h"); err != nil {
		t.Fatal(err)
	}
	d, err := GetDevice(db, "1")
	if err != nil {
		t.Fatal(err)
	}
	if d.CPULoad == nil || *d.CPULoad != 37 || d.MemoryUsed == nil || *d.MemoryUsed != used || d.Uptime == nil || *d.Uptime != "1d2h" {
		t.Errorf("load = cpu %v mem %v uptime %v", d.CPULoad, d.MemoryUsed, d.Uptime)
	}
	if d.Status != "unknown" || d.LastError == nil || d.LastSeen != nil || *d.MemoryTotal != total {
		t.Errorf("a load sample must not touch status, last_seen, last_error or memory_total: %+v", d)
	}
	// Memory unknown (total not read yet): cpu and uptime only.
	if err := UpdateDeviceLoad(db, "1", 12, nil, ""); err != nil {
		t.Fatal(err)
	}
	d, _ = GetDevice(db, "1")
	if *d.CPULoad != 12 || *d.MemoryUsed != used || *d.Uptime != "1d2h" {
		t.Errorf("partial sample overwrote what it did not carry: cpu %v mem %v uptime %v", *d.CPULoad, *d.MemoryUsed, *d.Uptime)
	}
}

func TestPortStatsDropColumnsAndHourlyCoverage(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")

	rows := []PortStatRow{
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(10, 0), RxBytes: 100, Samples: 4,
			RxDrop: n64(3), TxDrop: n64(120), RxError: n64(0), TxError: n64(1), TxQueueDrop: n64(7), LinkDowns: n64(1), CoveredS: n64(60)},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(10, 1), RxBytes: 50, Samples: 4,
			RxDrop: n64(1), TxDrop: n64(30), RxError: n64(2), TxError: n64(0), TxQueueDrop: n64(0), LinkDowns: n64(0), CoveredS: n64(45)},
		// An interface without the drop attributes: NULL, not 0.
		{DeviceID: "1", InterfaceName: "ether2", Bucket: minute(10, 0), RxBytes: 9, Samples: 4, LinkDowns: n64(0), CoveredS: n64(60)},
	}
	if err := InsertPortStats1m(db, rows); err != nil {
		t.Fatal(err)
	}
	got, err := GetPortStatsSeries(db, PortStats1m, "1", "ether1", minute(10, 0), minute(11, 0))
	if err != nil || len(got) != 2 || got[0] != rows[0] || got[1] != rows[1] {
		t.Fatalf("round trip = %+v err %v", got, err)
	}
	var txDrop any
	if err := db.QueryRow(`SELECT tx_drop FROM port_stats_1m WHERE interface_name = 'ether2'`).Scan(&txDrop); err != nil || txDrop != nil {
		t.Errorf("absent attribute stored as %v (err %v), want NULL", txDrop, err)
	}

	// Without device_minutes the hour's coverage is unknown (NULL).
	if err := RollupPortStats1h(db, minute(10, 0), minute(11, 0)); err != nil {
		t.Fatal(err)
	}
	hour, _ := GetPortStatsSeries(db, PortStats1h, "1", "ether1", minute(10, 0), minute(11, 0))
	if len(hour) != 1 || hour[0].RxDrop != n64(4) || hour[0].TxDrop != n64(150) || hour[0].RxError != n64(2) ||
		hour[0].TxError != n64(1) || hour[0].TxQueueDrop != n64(7) || hour[0].LinkDowns != n64(1) || hour[0].CoveredS.Valid {
		t.Fatalf("hourly drops = %+v", hour)
	}
	hour, _ = GetPortStatsSeries(db, PortStats1h, "1", "ether2", minute(10, 0), minute(11, 0))
	if len(hour) != 1 || hour[0].TxDrop.Valid || hour[0].LinkDowns != n64(0) {
		t.Fatalf("hourly row of a port without drop counters = %+v, want NULL drops", hour)
	}

	// With device_minutes the hour carries the device's covered seconds —
	// idle minutes included, which have no port row.
	if err := UpsertDeviceMinutes(db, []DeviceMinute{
		{DeviceID: "1", Bucket: minute(10, 0), PolledS: 60},
		{DeviceID: "1", Bucket: minute(10, 1), PolledS: 45},
		{DeviceID: "1", Bucket: minute(10, 2), PolledS: 60}, // idle on every port
		{DeviceID: "1", Bucket: minute(10, 3), GapReason: GapPaused},
		{DeviceID: "1", Bucket: minute(11, 0), PolledS: 60}, // next hour
	}); err != nil {
		t.Fatal(err)
	}
	if err := RollupPortStats1h(db, minute(10, 0), minute(11, 0)); err != nil {
		t.Fatal(err)
	}
	hour, _ = GetPortStatsSeries(db, PortStats1h, "1", "ether1", minute(10, 0), minute(11, 0))
	if len(hour) != 1 || hour[0].CoveredS != n64(165) {
		t.Fatalf("hourly covered_s = %+v, want 165", hour)
	}

	egress, err := ListPortEgressMinutes(db, minute(10, 0), minute(10, 1))
	if err != nil || len(egress) != 2 {
		t.Fatalf("ListPortEgressMinutes = %+v err %v", egress, err)
	}
	if e := egress[0]; e.DeviceID != "1" || e.InterfaceName != "ether1" || e.TxDrop != n64(120) || e.TxQueueDrop != n64(7) || e.CoveredS != n64(60) {
		t.Errorf("egress row = %+v", e)
	}
}
