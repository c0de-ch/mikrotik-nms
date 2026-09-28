package queries

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

// seedPortStatsDevice inserts a devices row (port_stats/port_hosts rows
// reference it through a foreign key).
func seedPortStatsDevice(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if err := CreateDevice(db, &Device{ID: id, Address: "192.0.2." + id, Username: "u", APIPort: 8728, Status: "online"}); err != nil {
		t.Fatalf("create device %s: %v", id, err)
	}
}

// minute returns 2026-09-28 hh:mm:00 UTC.
func minute(hh, mm int) time.Time {
	return time.Date(2026, 9, 28, hh, mm, 0, 0, time.UTC)
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestInsertPortStats1mAndSeriesRoundTrip(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")

	rows := []PortStatRow{
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(12, 0), RxBpsAvg: 800, TxBpsAvg: 1600, RxBpsMax: 900, TxBpsMax: 2000, RxBytes: 6000, TxBytes: 12000, RxPpsAvg: 3, TxPpsAvg: 4, Samples: 4},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(12, 1), RxBpsAvg: 80, TxBpsAvg: 160, RxBpsMax: 90, TxBpsMax: 200, RxBytes: 600, TxBytes: 1200, Samples: 4},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(12, 3), RxBytes: 1, Samples: 1},
		{DeviceID: "1", InterfaceName: "ether2", Bucket: minute(12, 0), RxBytes: 7, Samples: 1},
		// Device deleted since the collector sampled it: skipped, not an error.
		{DeviceID: "gone", InterfaceName: "ether1", Bucket: minute(12, 0), RxBytes: 5, Samples: 1},
	}
	if err := InsertPortStats1m(db, rows); err != nil {
		t.Fatalf("InsertPortStats1m: %v", err)
	}
	if n := countRows(t, db, "port_stats_1m"); n != 4 {
		t.Fatalf("port_stats_1m rows = %d, want 4 (row for unknown device skipped)", n)
	}

	// Stored as UTC text in the CURRENT_TIMESTAMP layout.
	var raw string
	if err := db.QueryRow(`SELECT CAST(bucket AS TEXT) FROM port_stats_1m WHERE interface_name = 'ether2'`).Scan(&raw); err != nil {
		t.Fatalf("read raw bucket: %v", err)
	}
	if raw != "2026-09-28 12:00:00" {
		t.Errorf("raw bucket = %q, want 2026-09-28 12:00:00", raw)
	}

	// Window bounds given in a non-UTC zone must be converted, [from, to).
	cest := time.FixedZone("CEST", 2*3600)
	got, err := GetPortStatsSeries(db, PortStats1m, "1", "ether1", minute(12, 0).In(cest), minute(12, 3).In(cest))
	if err != nil {
		t.Fatalf("GetPortStatsSeries: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("series len = %d, want 2: %+v", len(got), got)
	}
	if got[0] != rows[0] || got[1] != rows[1] {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, rows[:2])
	}
	if got[0].Bucket.Location() != time.UTC {
		t.Errorf("bucket location = %v, want UTC", got[0].Bucket.Location())
	}

	// INSERT OR REPLACE on the PK.
	upd := rows[0]
	upd.RxBytes = 42
	if err := InsertPortStats1m(db, []PortStatRow{upd}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, _ = GetPortStatsSeries(db, PortStats1m, "1", "ether1", minute(12, 0), minute(12, 1))
	if len(got) != 1 || got[0].RxBytes != 42 {
		t.Errorf("replace not applied: %+v", got)
	}

	if err := InsertPortStats1m(db, nil); err != nil {
		t.Errorf("empty insert: %v", err)
	}
	if _, err := GetPortStatsSeries(db, PortStatsTable("users"), "1", "ether1", minute(12, 0), minute(13, 0)); err == nil {
		t.Error("unknown table must be rejected")
	}
}

func TestRollupPortStats1h(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")

	if err := InsertPortStats1m(db, []PortStatRow{
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(9, 59), RxBytes: 999, Samples: 4},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(10, 0), RxBpsAvg: 48000, RxBpsMax: 60000, TxBpsMax: 10, RxBytes: 360_000, TxBytes: 900, RxPpsAvg: 40, TxPpsAvg: 1, Samples: 4},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(10, 30), RxBpsAvg: 96000, RxBpsMax: 50000, TxBpsMax: 30, RxBytes: 720_000, TxBytes: 900, RxPpsAvg: 80, TxPpsAvg: 1, Samples: 3},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(11, 5), RxBytes: 450, Samples: 4},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(12, 0), RxBytes: 1, Samples: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	roll := func() {
		t.Helper()
		// Unaligned bounds are truncated to the hour: [10:00, 12:00).
		if err := RollupPortStats1h(db, minute(10, 20), minute(12, 40)); err != nil {
			t.Fatalf("RollupPortStats1h: %v", err)
		}
	}
	roll()
	got, err := GetPortStatsSeries(db, PortStats1h, "1", "ether1", minute(0, 0), minute(23, 0))
	if err != nil {
		t.Fatalf("series 1h: %v", err)
	}
	want := []PortStatRow{
		{
			DeviceID: "1", InterfaceName: "ether1", Bucket: minute(10, 0),
			RxBpsAvg: 2400, TxBpsAvg: 4, // 1_080_000*8/3600, 1800*8/3600
			RxBpsMax: 60000, TxBpsMax: 30,
			RxBytes: 1_080_000, TxBytes: 1800,
			RxPpsAvg: 2, TxPpsAvg: 0, // round(120/60), round(2/60)
			Samples: 7,
		},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(11, 0), RxBpsAvg: 1, RxBytes: 450, Samples: 4},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("rollup =\n %+v\nwant\n %+v", got, want)
	}

	// Idempotent: re-rolling yields the same rows.
	roll()
	again, _ := GetPortStatsSeries(db, PortStats1h, "1", "ether1", minute(0, 0), minute(23, 0))
	if !slices.Equal(again, want) {
		t.Fatalf("re-roll changed rows:\n %+v", again)
	}

	// A late minute in hour 10 is picked up by the next re-roll (replace, not add).
	if err := InsertPortStats1m(db, []PortStatRow{{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(10, 59), RxBpsMax: 70000, RxBytes: 360_000, Samples: 2}}); err != nil {
		t.Fatalf("late minute: %v", err)
	}
	roll()
	again, _ = GetPortStatsSeries(db, PortStats1h, "1", "ether1", minute(10, 0), minute(11, 0))
	if len(again) != 1 || again[0].RxBytes != 1_440_000 || again[0].RxBpsAvg != 3200 || again[0].RxBpsMax != 70000 || again[0].Samples != 9 {
		t.Errorf("re-roll after late minute = %+v", again)
	}
	if n := countRows(t, db, "port_stats_1h"); n != 2 {
		t.Errorf("port_stats_1h rows = %d, want 2", n)
	}

	// Empty/inverted window is a no-op.
	if err := RollupPortStats1h(db, minute(12, 0), minute(12, 0)); err != nil {
		t.Errorf("empty window: %v", err)
	}
}

func TestAggregatePortStats(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	seedPortStatsDevice(t, db, "2")

	if err := InsertPortStats1m(db, []PortStatRow{
		{DeviceID: "1", InterfaceName: "ether2", Bucket: minute(12, 0), RxBytes: 10, TxBytes: 1, RxBpsMax: 5, TxBpsMax: 50, Samples: 1},
		{DeviceID: "1", InterfaceName: "ether2", Bucket: minute(12, 1), RxBytes: 20, TxBytes: 2, RxBpsMax: 7, TxBpsMax: 40, Samples: 1},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(12, 1), RxBytes: 3, Samples: 1},
		{DeviceID: "2", InterfaceName: "sfp1", Bucket: minute(12, 2), TxBytes: 9, TxBpsMax: 9, Samples: 1},
		{DeviceID: "2", InterfaceName: "sfp1", Bucket: minute(12, 5), TxBytes: 1000, Samples: 1}, // outside
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := AggregatePortStats(db, PortStats1m, minute(12, 0), minute(12, 5), "", nil)
	if err != nil {
		t.Fatalf("AggregatePortStats: %v", err)
	}
	want := []PortAggregate{
		{DeviceID: "1", InterfaceName: "ether1", RxBytes: 3},
		{DeviceID: "1", InterfaceName: "ether2", RxBytes: 30, TxBytes: 3, RxBpsMax: 7, TxBpsMax: 50},
		{DeviceID: "2", InterfaceName: "sfp1", TxBytes: 9, TxBpsMax: 9},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("aggregate =\n %+v\nwant\n %+v", got, want)
	}

	got, err = AggregatePortStats(db, PortStats1m, minute(12, 0), minute(12, 5), "2", nil)
	if err != nil || len(got) != 1 || got[0].InterfaceName != "sfp1" {
		t.Fatalf("device filter = %+v, err %v", got, err)
	}

	got, err = AggregatePortStats(db, PortStats1h, minute(12, 0), minute(13, 0), "", nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty 1h aggregate = %+v, err %v", got, err)
	}
}

func TestPortStatsCoverageStart(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")

	if _, ok, err := PortStatsCoverageStart(db, PortStats1m, minute(0, 0), minute(23, 0)); err != nil || ok {
		t.Fatalf("empty table: ok=%v err=%v, want ok=false", ok, err)
	}

	if err := InsertPortStats1m(db, []PortStatRow{
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(11, 0), RxBytes: 1, Samples: 1},
		{DeviceID: "1", InterfaceName: "ether2", Bucket: minute(12, 7), RxBytes: 1, Samples: 1},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(12, 9), RxBytes: 1, Samples: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, ok, err := PortStatsCoverageStart(db, PortStats1m, minute(12, 0), minute(13, 0))
	if err != nil || !ok {
		t.Fatalf("coverage: ok=%v err=%v", ok, err)
	}
	if !got.Equal(minute(12, 7)) || got.Location() != time.UTC {
		t.Errorf("coverage start = %v, want %v UTC", got, minute(12, 7))
	}

	if _, ok, _ := PortStatsCoverageStart(db, PortStats1m, minute(13, 0), minute(14, 0)); ok {
		t.Error("window without rows must report ok=false")
	}
}

func TestDeleteOldPortStats(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")

	if err := InsertPortStats1m(db, []PortStatRow{
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(10, 0), RxBytes: 1, Samples: 1},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(10, 59), RxBytes: 1, Samples: 1},
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(11, 0), RxBytes: 1, Samples: 1},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := RollupPortStats1h(db, minute(10, 0), minute(12, 0)); err != nil {
		t.Fatalf("rollup: %v", err)
	}

	n, err := DeleteOldPortStats(db, PortStats1m, minute(11, 0))
	if err != nil || n != 2 {
		t.Fatalf("delete 1m: n=%d err=%v, want 2", n, err)
	}
	if c := countRows(t, db, "port_stats_1m"); c != 1 {
		t.Errorf("1m rows left = %d, want 1", c)
	}

	n, err = DeleteOldPortStats(db, PortStats1h, minute(10, 30))
	if err != nil || n != 1 {
		t.Fatalf("delete 1h: n=%d err=%v, want 1", n, err)
	}
	if c := countRows(t, db, "port_stats_1h"); c != 1 {
		t.Errorf("1h rows left = %d, want 1", c)
	}

	if _, err := DeleteOldPortStats(db, PortStatsTable("devices"), minute(23, 0)); err == nil {
		t.Error("unknown table must be rejected")
	}
	if c := countRows(t, db, "devices"); c != 1 {
		t.Errorf("devices rows = %d, want 1", c)
	}
}

// DeleteOldPortStats removes a backlog in bounded chunks: the count it
// returns is every row removed, and nothing at or after the cutoff goes.
func TestDeleteOldPortStatsChunked(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	defer func(chunk int, pause time.Duration) {
		portStatsDeleteChunk, portStatsDeletePause = chunk, pause
	}(portStatsDeleteChunk, portStatsDeletePause)
	portStatsDeleteChunk, portStatsDeletePause = 7, 0

	var rows []PortStatRow
	for m := 0; m < 4*60; m += 5 { // 10:00 .. 13:55, every 5 minutes, two ports
		for _, iface := range []string{"ether1", "ether2"} {
			rows = append(rows, PortStatRow{DeviceID: "1", InterfaceName: iface, Bucket: minute(10, 0).Add(time.Duration(m) * time.Minute), RxBytes: 1, Samples: 1})
		}
	}
	if err := InsertPortStats1m(db, rows); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cutoff := minute(12, 30) // 30 buckets × 2 ports before it: several chunks
	n, err := DeleteOldPortStats(db, PortStats1m, cutoff)
	if err != nil || n != 60 {
		t.Fatalf("delete: n=%d err=%v, want 60", n, err)
	}
	left, err := GetPortStatsSeries(db, PortStats1m, "1", "ether1", minute(0, 0), minute(23, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 18 || !left[0].Bucket.Equal(cutoff) {
		t.Errorf("rows left = %d starting %v, want 18 from the cutoff", len(left), left[0].Bucket)
	}
	if c := countRows(t, db, "port_stats_1m"); c != 36 {
		t.Errorf("1m rows left = %d, want 36", c)
	}
	// Exactly a multiple of the chunk size still terminates.
	if n, err := DeleteOldPortStats(db, PortStats1m, minute(23, 0)); err != nil || n != 36 {
		t.Errorf("delete rest: n=%d err=%v, want 36", n, err)
	}
	if n, err := DeleteOldPortStats(db, PortStats1m, minute(23, 0)); err != nil || n != 0 {
		t.Errorf("delete on empty: n=%d err=%v", n, err)
	}
}

func TestLatestPortStatsBucket(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	if _, ok, err := LatestPortStatsBucket(db, PortStats1h); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	if err := InsertPortStats1m(db, []PortStatRow{
		{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(9, 10), RxBytes: 1, Samples: 1},
		{DeviceID: "1", InterfaceName: "ether2", Bucket: minute(11, 10), RxBytes: 1, Samples: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := RollupPortStats1h(db, minute(0, 0), minute(23, 0)); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LatestPortStatsBucket(db, PortStats1h)
	if err != nil || !ok || !got.Equal(minute(11, 0)) {
		t.Errorf("latest = %v ok=%v err=%v, want 11:00", got, ok, err)
	}
}

func TestPortStatsCascadeOnDeviceDelete(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "1")
	if err := InsertPortStats1m(db, []PortStatRow{{DeviceID: "1", InterfaceName: "ether1", Bucket: minute(10, 0), RxBytes: 1, Samples: 1}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := RollupPortStats1h(db, minute(10, 0), minute(11, 0)); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if err := UpsertPortHosts(db, "1", []PortHost{{InterfaceName: "ether1", MACAddress: "AA:AA:AA:AA:AA:AA"}}, minute(10, 0)); err != nil {
		t.Fatalf("hosts: %v", err)
	}
	if err := DeleteDevice(db, "1"); err != nil {
		t.Fatalf("delete device: %v", err)
	}
	for _, table := range []string{"port_stats_1m", "port_stats_1h", "port_hosts"} {
		if c := countRows(t, db, table); c != 0 {
			t.Errorf("%s rows after device delete = %d, want 0", table, c)
		}
	}
}

func TestDownsampleSeries(t *testing.T) {
	rows := []PortStatRow{
		{Bucket: minute(11, 59), RxBytes: 1_000_000},              // before from: ignored
		{Bucket: minute(12, 0), RxBytes: 30_000, TxBytes: 3_000},  // bucket 0
		{Bucket: minute(12, 4), RxBytes: 7_500, TxBytes: 0},       // bucket 0
		{Bucket: minute(12, 11), RxBytes: 100, TxBytes: 1},        // bucket 2
		{Bucket: minute(12, 15), RxBytes: 1_000_000, TxBytes: 99}, // at to: ignored
	}
	got := DownsampleSeries(rows, minute(12, 0), minute(12, 15), 5*time.Minute)
	want := []SeriesPoint{
		{TS: minute(12, 0), RxBps: 1000, TxBps: 80}, // 37_500*8/300, 3_000*8/300
		{TS: minute(12, 5), RxBps: 0, TxBps: 0},     // zero-filled
		{TS: minute(12, 10), RxBps: 3, TxBps: 0},    // round(800/300), round(8/300)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("DownsampleSeries =\n %+v\nwant\n %+v", got, want)
	}

	// Native step: one point per minute, zero-filled.
	got = DownsampleSeries(rows, minute(12, 0), minute(12, 5), time.Minute)
	if len(got) != 5 || got[0].RxBps != 4000 || got[1].RxBps != 0 || got[4].RxBps != 1000 {
		t.Errorf("native step = %+v", got)
	}

	for _, tc := range []struct {
		name     string
		from, to time.Time
		step     time.Duration
	}{
		{"from == to", minute(12, 0), minute(12, 0), time.Minute},
		{"to before from", minute(12, 5), minute(12, 0), time.Minute},
		{"zero step", minute(12, 0), minute(12, 5), 0},
		{"window shorter than step", minute(12, 0), minute(12, 3), 5 * time.Minute},
	} {
		if got := DownsampleSeries(rows, tc.from, tc.to, tc.step); got == nil || len(got) != 0 {
			t.Errorf("%s: got %v, want empty non-nil", tc.name, got)
		}
	}
}

func TestComputeSeriesStats(t *testing.T) {
	from, to := minute(12, 0), minute(12, 20) // 20 native buckets
	rows := []PortStatRow{
		{Bucket: minute(12, 1), RxBpsAvg: 100, TxBpsAvg: 10, RxBpsMax: 150, TxBpsMax: 11, RxBytes: 750, TxBytes: 75},
		{Bucket: minute(12, 7), RxBpsAvg: 300, TxBpsAvg: 30, RxBpsMax: 900, TxBpsMax: 31, RxBytes: 2250, TxBytes: 225},
		{Bucket: minute(12, 9), RxBpsAvg: 200, TxBpsAvg: 20, RxBpsMax: 250, TxBpsMax: 21, RxBytes: 1500, TxBytes: 150},
		{Bucket: minute(12, 20), RxBpsAvg: 9999, RxBpsMax: 99999, RxBytes: 99999}, // at to: ignored
	}
	got := ComputeSeriesStats(rows, from, to, time.Minute)
	want := SeriesStats{
		RxAvg: 30, TxAvg: 3, // 4500*8/1200, 450*8/1200
		RxMax: 900, TxMax: 31,
		RxP95: 200, TxP95: 20, // 17 zeros + {100,200,300}: idx ceil(19)-1 = 18 → 200
		RxBytes: 4500, TxBytes: 450,
	}
	if got != want {
		t.Fatalf("ComputeSeriesStats =\n %+v\nwant\n %+v", got, want)
	}

	if z := ComputeSeriesStats(rows, from, from, time.Minute); z != (SeriesStats{}) {
		t.Errorf("from == to: %+v, want zero", z)
	}

	// Raw 1 s samples converted to rows without an avg: the percentile falls
	// back to bytes*8/native; two rows in one second keep the higher rate.
	live := []PortStatRow{
		{Bucket: from, RxBytes: 125, RxBpsMax: 1000},
		{Bucket: from.Add(time.Second), RxBytes: 250, RxBpsMax: 2000},
		{Bucket: from.Add(time.Second), RxBytes: 50, RxBpsMax: 400},
	}
	s := ComputeSeriesStats(live, from, from.Add(2*time.Second), time.Second)
	if s.RxP95 != 2000 || s.RxMax != 2000 || s.RxBytes != 425 || s.RxAvg != 1700 {
		t.Errorf("live stats = %+v", s)
	}
}

func TestPercentile95(t *testing.T) {
	seq := func(n int) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = int64(n - i) // descending, so sorting matters
		}
		return out
	}
	tests := []struct {
		name string
		in   []int64
		want int64
	}{
		{"empty", nil, 0},
		{"single", []int64{7}, 7},
		{"1..20", seq(20), 19},   // ceil(19)-1 = 18 → 19
		{"1..100", seq(100), 95}, // ceil(95)-1 = 94 → 95
		{"1..10", seq(10), 10},   // ceil(9.5)-1 = 9 → 10
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := slices.Clone(tt.in)
			if got := Percentile95(in); got != tt.want {
				t.Errorf("Percentile95 = %d, want %d", got, tt.want)
			}
			if !slices.Equal(in, tt.in) {
				t.Error("input was modified")
			}
		})
	}
}
