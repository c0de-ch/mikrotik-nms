package poller

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// ifStats builds a running interface reading with counters.
func ifStats(name string, rxBytes, txBytes, rxPkts, txPkts int64) routeros.InterfaceStats {
	return routeros.InterfaceStats{
		Name: name, Type: "ether", Running: true, HasCounters: true,
		RxBytes: rxBytes, TxBytes: txBytes, RxPackets: rxPkts, TxPackets: txPkts,
	}
}

// at returns 2026-09-28 hh:mm:ss UTC.
func at(hh, mm, ss int) time.Time {
	return time.Date(2026, 9, 28, hh, mm, ss, 0, time.UTC)
}

func snapshotRate(t *testing.T, c *PortStatsCollector, dev, iface string) PortRate {
	t.Helper()
	for _, p := range c.Snapshot().Ports {
		if p.DeviceID == dev && p.Iface == iface {
			return p
		}
	}
	t.Fatalf("snapshot has no %s/%s", dev, iface)
	return PortRate{}
}

func minuteSamples(c *PortStatsCollector, minute time.Time, dev, iface string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if acc := c.minutes[minute.Unix()][portStatsKey{dev, iface}]; acc != nil {
		return acc.samples
	}
	return 0
}

func TestPortStatsIngestDeltaResetAndGap(t *testing.T) {
	c := NewPortStatsCollector(nil, nil, nil)
	const iv = 15 * time.Second

	steps := []struct {
		name       string
		t          time.Time
		row        routeros.InterfaceStats
		wantRx     int64
		wantTx     int64
		wantRxPps  int64
		accumulate bool
	}{
		{"first sample has no rate", at(12, 0, 0), ifStats("ether1", 1_000, 0, 10, 0), 0, 0, 0, false},
		{"delta over 10s", at(12, 0, 10), ifStats("ether1", 13_500, 2_500, 110, 5), 10_000, 2_000, 10, true},
		{"counter went backwards (reboot)", at(12, 0, 20), ifStats("ether1", 500, 100, 1, 1), 0, 0, 0, false},
		{"rates resume after reset", at(12, 0, 30), ifStats("ether1", 1_750, 100, 11, 1), 1_000, 0, 1, true},
		{"gap over 3x interval", at(12, 1, 16), ifStats("ether1", 9_000_000, 100, 12, 1), 0, 0, 0, false},
		{"same timestamp (dt 0)", at(12, 1, 16), ifStats("ether1", 9_100_000, 100, 12, 1), 0, 0, 0, false},
		{"not running", at(12, 1, 30), func() routeros.InterfaceStats {
			s := ifStats("ether1", 9_200_000, 200, 13, 2)
			s.Running = false
			return s
		}(), 0, 0, 0, false},
		{"running again, delta vs the down sample", at(12, 1, 45), ifStats("ether1", 9_200_000+1_875, 200, 13, 2), 1_000, 0, 0, true},
		{"row without counters", at(12, 1, 50), routeros.InterfaceStats{Name: "ether1", Type: "ether", Running: true}, 0, 0, 0, false},
		{"no rate after a counterless sample", at(12, 1, 55), ifStats("ether1", 9_300_000, 300, 14, 3), 0, 0, 0, false},
	}
	for _, st := range steps {
		before := minuteSamples(c, st.t.Truncate(time.Minute), "d1", "ether1")
		c.ingest("d1", []routeros.InterfaceStats{st.row}, st.t, iv)
		got := snapshotRate(t, c, "d1", "ether1")
		if got.RxBps != st.wantRx || got.TxBps != st.wantTx || got.RxPps != st.wantRxPps {
			t.Errorf("%s: rates rx=%d tx=%d rxpps=%d, want %d/%d/%d", st.name, got.RxBps, got.TxBps, got.RxPps, st.wantRx, st.wantTx, st.wantRxPps)
		}
		after := minuteSamples(c, st.t.Truncate(time.Minute), "d1", "ether1")
		if accumulated := after > before; accumulated != st.accumulate {
			t.Errorf("%s: accumulated=%v, want %v", st.name, accumulated, st.accumulate)
		}
	}
	if !c.Snapshot().Ready {
		t.Error("Ready must latch after the first computed rate")
	}
}

func TestPortStatsReadyLatchesOnlyAfterTwoSamples(t *testing.T) {
	c := NewPortStatsCollector(nil, nil, nil)
	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 0, 0, 0, 0)}, at(12, 0, 0), 15*time.Second)
	if c.Snapshot().Ready {
		t.Fatal("one sample must not make the snapshot ready")
	}
	// An idle port (zero delta) still counts as a computed rate.
	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 0, 0, 0, 0)}, at(12, 0, 15), 15*time.Second)
	if !c.Snapshot().Ready {
		t.Fatal("second sample must make the snapshot ready")
	}
}

func TestPortStatsMinuteBucketMath(t *testing.T) {
	c := NewPortStatsCollector(nil, nil, nil)
	const iv = 30 * time.Second
	ingest := func(ts time.Time, rx, tx int64) {
		c.ingest("d1", []routeros.InterfaceStats{
			ifStats("ether1", rx, tx, rx/100, tx/100),
			ifStats("ether2", 5, 5, 0, 0), // idle: never changes
		}, ts, iv)
	}
	ingest(at(12, 0, 5), 0, 0)
	ingest(at(12, 0, 15), 10_000, 0)   // dt 10: rx 8000 bps
	ingest(at(12, 0, 45), 13_000, 600) // dt 30: rx 800 bps, tx 160 bps
	ingest(at(12, 0, 50), 13_000, 600) // dt 5: idle interval
	ingest(at(12, 1, 5), 14_500, 600)  // dt 15 across :00: 10 s (1000 B) in 12:00, 5 s (500 B) in 12:01

	rows := c.takeMinutesBefore(at(12, 1, 0))
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly the ether1 12:00 row (idle ether2 skipped)", rows)
	}
	r := rows[0]
	want := queries.PortStatRow{
		DeviceID: "d1", InterfaceName: "ether1", Bucket: at(12, 0, 0),
		// Σbytes*8/Σdt over the 55 s covered in 12:00 — NOT the mean of the
		// interval rates.
		RxBpsAvg: 2036, // 14_000*8/55
		TxBpsAvg: 87,   // 600*8/55
		RxBpsMax: 8000, TxBpsMax: 160,
		RxBytes: 14_000, TxBytes: 600,
		RxPpsAvg: 3, // 140/55
		TxPpsAvg: 0, // 6/55
		Samples:  3, // the interval ending at 12:01:05 counts in 12:01
	}
	if r != want {
		t.Fatalf("minute row =\n %+v\nwant\n %+v", r, want)
	}

	// The 12:01 minute is still open and holds the rest of the last interval.
	if minuteSamples(c, at(12, 1, 0), "d1", "ether1") != 1 {
		t.Error("12:01 accumulation must survive a flush at 12:01")
	}
	c.mu.Lock()
	acc := *c.minutes[at(12, 1, 0).Unix()][portStatsKey{"d1", "ether1"}]
	c.mu.Unlock()
	if acc.rxBytes != 500 || acc.seconds != 5 || acc.rxMax != 800 || acc.rxPackets != 5 {
		t.Errorf("12:01 share = %+v, want 500 B over 5 s at max 800 bps", acc)
	}
	if again := c.takeMinutesBefore(at(12, 1, 0)); len(again) != 0 {
		t.Errorf("flushed minutes must be removed, got %+v", again)
	}
}

func TestPortStatsFlushWritesAndRollsUp(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "sw", Address: "192.168.78.10"})
	c := NewPortStatsCollector(db, nil, nil)
	const iv = 15 * time.Second

	// Startup rollup marks the current hour as rolled.
	c.now = func() time.Time { return at(10, 58, 30) }
	c.startupRollup()
	if got := c.RolledThrough(); !got.Equal(at(10, 0, 0)) {
		t.Fatalf("RolledThrough after startup = %v, want 10:00", got)
	}

	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 0, 0, 0, 0)}, at(10, 58, 50), iv)
	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 90_000, 9_000, 600, 60)}, at(10, 59, 5), iv)    // 10 s → 10:58, 5 s → 10:59
	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 180_000, 9_000, 1200, 60)}, at(10, 59, 20), iv) // → 10:59
	c.finishCycle(at(10, 59, 25), iv)
	if got := c.FlushedThrough(); !got.Equal(at(10, 59, 0)) {
		t.Fatalf("FlushedThrough = %v, want 10:59", got)
	}
	series, err := queries.GetPortStatsSeries(db, queries.PortStats1m, "d1", "ether1", at(10, 0, 0), at(11, 0, 0))
	// 10:58 holds its 10 s share of the first interval; 10:59 is still open.
	if err != nil || len(series) != 1 || !series[0].Bucket.Equal(at(10, 58, 0)) || series[0].RxBytes != 60_000 ||
		series[0].TxBytes != 6_000 || series[0].RxBpsAvg != 48_000 || series[0].Samples != 0 {
		t.Fatalf("after the 10:59:25 flush: %+v err %v", series, err)
	}

	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 180_000, 9_000, 1200, 60)}, at(11, 0, 2), iv) // idle: 40 s → 10:59, 2 s → 11:00
	c.finishCycle(at(11, 0, 3), iv)
	if got := c.FlushedThrough(); !got.Equal(at(11, 0, 0)) {
		t.Fatalf("FlushedThrough = %v, want 11:00", got)
	}
	if got := c.RolledThrough(); !got.Equal(at(11, 0, 0)) {
		t.Fatalf("RolledThrough = %v, want 11:00 after crossing the hour", got)
	}

	series, err = queries.GetPortStatsSeries(db, queries.PortStats1m, "d1", "ether1", at(10, 59, 0), at(11, 0, 0))
	if err != nil || len(series) != 1 {
		t.Fatalf("1m series = %+v err %v", series, err)
	}
	m := series[0]
	// 120_000 bytes over the full minute (5 + 15 + 40 idle seconds) → 16_000
	// bps avg; both busy intervals ran at 48_000.
	if !m.Bucket.Equal(at(10, 59, 0)) || m.RxBytes != 120_000 || m.RxBpsAvg != 16_000 || m.RxBpsMax != 48_000 ||
		m.TxBytes != 3_000 || m.TxBpsAvg != 400 || m.RxPpsAvg != 13 || m.Samples != 2 {
		t.Errorf("1m row = %+v", m)
	}

	hourly, err := queries.GetPortStatsSeries(db, queries.PortStats1h, "d1", "ether1", at(0, 0, 0), at(23, 0, 0))
	if err != nil || len(hourly) != 1 {
		t.Fatalf("1h series = %+v err %v", hourly, err)
	}
	h := hourly[0]
	if !h.Bucket.Equal(at(10, 0, 0)) || h.RxBytes != 180_000 || h.RxBpsAvg != 400 || h.RxBpsMax != 48_000 || h.Samples != 2 {
		t.Errorf("1h row = %+v (avg must be bytes*8/3600)", h)
	}

	// Another flush in the same hour neither rewrites nor re-rolls.
	c.finishCycle(at(11, 0, 20), iv)
	if got := c.RolledThrough(); !got.Equal(at(11, 0, 0)) {
		t.Errorf("RolledThrough moved to %v", got)
	}
}

// A constant rate must chart flat at 1-minute resolution whatever the poll
// interval: each interval is spread over the minutes it overlaps instead of
// landing whole in the minute it ended in (which drew 0/20/0/20 Mbit/s at a
// 120 s interval, 50/0/0/0/0 at 300 s, and 12.5/10/7.5 at 15 s when replies
// jitter across :00).
func TestPortStatsConstantRateChartsFlat(t *testing.T) {
	const rateBps = 10_000_000
	for _, tc := range []struct {
		name   string
		iv     time.Duration
		phase  time.Duration // offset of the poll grid from :00
		jitter time.Duration // ± alternating reply jitter
	}{
		{"15s on the minute with jitter", 15 * time.Second, 0, 20 * time.Millisecond},
		{"15s off the minute", 15 * time.Second, 7 * time.Second, 0},
		{"60s with jitter", time.Minute, 0, 30 * time.Millisecond},
		{"120s", 2 * time.Minute, 11 * time.Second, 0},
		{"300s", 5 * time.Minute, 37 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := pollerTestDB(t)
			mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "sw", Address: "192.168.78.10"})
			c := NewPortStatsCollector(db, nil, nil)
			start := at(10, 0, 0).Add(tc.phase)
			end := at(11, 0, 0)
			for k := 0; ; k++ {
				ts := start.Add(time.Duration(k) * tc.iv)
				if k%2 == 1 {
					ts = ts.Add(tc.jitter)
				} else {
					ts = ts.Add(-tc.jitter)
				}
				// Run two intervals past the end: a sample just before a
				// minute boundary holds that minute open for the next one.
				if ts.After(end.Add(2 * tc.iv)) {
					break
				}
				bytes := int64(ts.Sub(at(9, 0, 0)).Seconds() * rateBps / 8)
				c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", bytes, 0, 0, 0)}, ts, tc.iv)
				c.finishCycle(ts.Add(50*time.Millisecond), tc.iv)
			}
			// Skip the minutes before the second sample (no rate yet).
			from := start.Add(tc.iv).Truncate(time.Minute).Add(time.Minute)
			rows, err := queries.GetPortStatsSeries(db, queries.PortStats1m, "d1", "ether1", from, end)
			if err != nil {
				t.Fatal(err)
			}
			points := queries.DownsampleSeries(rows, from, end, time.Minute)
			if len(points) < 50 {
				t.Fatalf("only %d points checked", len(points))
			}
			for _, p := range points {
				if dev := float64(p.RxBps-rateBps) / rateBps; dev > 0.01 || dev < -0.01 {
					t.Errorf("%s: %d bps, want %d ±1%%", p.TS.Format("15:04"), p.RxBps, rateBps)
				}
			}
		})
	}
}

// A process that stopped at HH:50 (before rolling hour HH) and restarts more
// than 3 h later still rolls HH into port_stats_1h: the startup re-roll starts
// at the newest hourly bucket (bounded by 23 h, which the ≥ 1 day 1m
// retention keeps complete).
func TestPortStatsStartupRollupAfterLongDowntime(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "sw", Address: "192.168.78.10"})
	var rows []queries.PortStatRow
	for m := 0; m < 50; m++ { // 04:00..04:49
		rows = append(rows, queries.PortStatRow{DeviceID: "d1", InterfaceName: "ether1", Bucket: at(4, m, 0), RxBytes: 100, Samples: 4})
	}
	rows = append(rows, queries.PortStatRow{DeviceID: "d1", InterfaceName: "ether1", Bucket: at(3, 30, 0), RxBytes: 7, Samples: 4})
	if err := queries.InsertPortStats1m(db, rows); err != nil {
		t.Fatal(err)
	}
	if err := queries.RollupPortStats1h(db, at(3, 0, 0), at(4, 0, 0)); err != nil { // rolled before the stop
		t.Fatal(err)
	}

	c := NewPortStatsCollector(db, nil, nil)
	c.now = func() time.Time { return at(10, 30, 0) } // 6 h later
	c.startupRollup()
	hourly, err := queries.GetPortStatsSeries(db, queries.PortStats1h, "d1", "ether1", at(0, 0, 0), at(23, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(hourly) != 2 || !hourly[1].Bucket.Equal(at(4, 0, 0)) || hourly[1].RxBytes != 5_000 {
		t.Fatalf("hourly after restart = %+v, want 03:00 and the stopped 04:00 hour", hourly)
	}

	// An empty hourly table re-rolls the last 23 h, never further.
	db2 := pollerTestDB(t)
	mustCreateDevice(t, db2, &queries.Device{ID: "d1", Identity: "sw", Address: "192.168.78.10"})
	if err := queries.InsertPortStats1m(db2, []queries.PortStatRow{
		{DeviceID: "d1", InterfaceName: "ether1", Bucket: at(10, 5, 0).Add(-24 * time.Hour), RxBytes: 1, Samples: 1},
		{DeviceID: "d1", InterfaceName: "ether1", Bucket: at(10, 5, 0).Add(-22 * time.Hour), RxBytes: 1, Samples: 1},
	}); err != nil {
		t.Fatal(err)
	}
	c2 := NewPortStatsCollector(db2, nil, nil)
	c2.now = func() time.Time { return at(10, 30, 0) }
	c2.startupRollup()
	hourly, _ = queries.GetPortStatsSeries(db2, queries.PortStats1h, "d1", "ether1", at(0, 0, 0).Add(-48*time.Hour), at(23, 0, 0))
	if len(hourly) != 1 || !hourly[0].Bucket.Equal(at(12, 0, 0).Add(-24*time.Hour)) {
		t.Errorf("hourly = %+v, want only the hour within 23 h", hourly)
	}
}

// A process frozen for hours (flushedThrough jumps) rolls every hour since
// the last rollup, not just the last two.
func TestPortStatsRuntimeRollupAfterStall(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "sw", Address: "192.168.78.10"})
	c := NewPortStatsCollector(db, nil, nil)
	c.now = func() time.Time { return at(10, 50, 0) }
	c.startupRollup() // rolledThrough 10:00
	c.flush(at(10, 50, 30))
	var rows []queries.PortStatRow
	for _, h := range []int{10, 11, 12} {
		rows = append(rows, queries.PortStatRow{DeviceID: "d1", InterfaceName: "ether1", Bucket: at(h, 20, 0), RxBytes: int64(h), Samples: 1})
	}
	if err := queries.InsertPortStats1m(db, rows); err != nil {
		t.Fatal(err)
	}
	c.flush(at(13, 5, 0))
	if got := c.RolledThrough(); !got.Equal(at(13, 0, 0)) {
		t.Fatalf("RolledThrough = %v, want 13:00", got)
	}
	hourly, err := queries.GetPortStatsSeries(db, queries.PortStats1h, "d1", "ether1", at(0, 0, 0), at(23, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(hourly) != 3 || !hourly[0].Bucket.Equal(at(10, 0, 0)) || !hourly[2].Bucket.Equal(at(12, 0, 0)) {
		t.Errorf("hourly = %+v, want 10:00, 11:00 and 12:00", hourly)
	}
}

// A flush keeps the minute a device's open interval starts in: a reply taken
// at 12:00:59 but ingested after the 12:01:00 flush still lands in 12:00.
func TestPortStatsFlushHoldsMinuteOfOpenInterval(t *testing.T) {
	c := NewPortStatsCollector(pollerTestDB(t), nil, nil)
	const iv = 15 * time.Second
	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 0, 0, 0, 0)}, at(12, 0, 40), iv)
	c.finishCycle(at(12, 1, 0), iv)
	if got := c.FlushedThrough(); !got.Equal(at(12, 0, 0)) {
		t.Fatalf("FlushedThrough = %v, want 12:00 held for d1's open interval", got)
	}

	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 1_000, 0, 0, 0)}, at(12, 0, 59), iv)
	if minuteSamples(c, at(12, 0, 0), "d1", "ether1") != 1 {
		t.Error("the late reply must land in its own minute")
	}
	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 2_500, 0, 0, 0)}, at(12, 1, 14), iv)
	c.finishCycle(at(12, 1, 15), iv)
	if got := c.FlushedThrough(); !got.Equal(at(12, 1, 0)) {
		t.Fatalf("FlushedThrough = %v, want 12:01 once d1 moved on", got)
	}
}

// The hold is capped at a minute. A share for a minute older than that — a
// device on a long interval — is carried into the first open minute, so
// the bytes still add up exactly.
func TestPortStatsShareBeyondHoldCarriedIntoOpenMinute(t *testing.T) {
	c := NewPortStatsCollector(pollerTestDB(t), nil, nil)
	const iv = 5 * time.Minute
	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 0, 0, 0, 0)}, at(12, 0, 10), iv)
	c.finishCycle(at(12, 3, 0), iv) // another device's cycle
	if got := c.FlushedThrough(); !got.Equal(at(12, 2, 0)) {
		t.Fatalf("FlushedThrough = %v, want 12:02 (held at most a minute)", got)
	}

	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 30_000, 0, 0, 0)}, at(12, 5, 10), iv) // 100 B/s
	if minuteSamples(c, at(12, 0, 0), "d1", "ether1") != 0 {
		t.Error("a flushed minute must not be reopened")
	}
	c.mu.Lock()
	carried := *c.minutes[at(12, 2, 0).Unix()][portStatsKey{"d1", "ether1"}]
	c.mu.Unlock()
	// 12:00 (50 s) and 12:01 (60 s) carried into 12:02, plus its own 60 s.
	if carried.seconds != 170 || carried.rxBytes != 17_000 {
		t.Errorf("12:02 = %+v, want 170 s and 17000 B", carried)
	}
	var total int64
	c.mu.Lock()
	for _, ports := range c.minutes {
		total += ports[portStatsKey{"d1", "ether1"}].rxBytes
	}
	c.mu.Unlock()
	if total != 30_000 {
		t.Errorf("accumulated %d B, want the whole 30000 B delta", total)
	}
}

// Devices on different Tier 0 intervals interleave their cycles, so the
// fast device's flushes happen while the slow device's interval is open.
// Each minute must still hold exactly the shares of the intervals that
// overlap it (proration by overlap), not a share pushed into a later minute.
// The rate steps up mid-run, which is what tells the two apart.
func TestPortStatsPerDeviceIntervalsProrateExactly(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "fast", Identity: "fast", Address: "192.0.2.10"})
	mustCreateDevice(t, db, &queries.Device{ID: "slow", Identity: "slow", Address: "192.0.2.11"})
	c := NewPortStatsCollector(db, nil, nil)

	// Idle until 10:30, 1 MB/s until 10:45, 3 MB/s after.
	bytesAt := func(ts time.Time) int64 {
		s := ts.Sub(at(10, 30, 0)).Seconds()
		switch {
		case s <= 0:
			return 0
		case s <= 900:
			return int64(s * 1e6)
		}
		return int64(900e6 + (s-900)*3e6)
	}
	type poll struct {
		dev string
		iv  time.Duration
		t   time.Time
	}
	var polls []poll
	for ts := at(10, 0, 5); !ts.After(at(11, 2, 0)); ts = ts.Add(15 * time.Second) {
		polls = append(polls, poll{"fast", 15 * time.Second, ts})
	}
	for ts := at(10, 0, 30); !ts.After(at(11, 2, 0)); ts = ts.Add(time.Minute) {
		polls = append(polls, poll{"slow", time.Minute, ts})
	}
	sort.Slice(polls, func(i, j int) bool { return polls[i].t.Before(polls[j].t) })

	want := map[string]map[time.Time]float64{"fast": {}, "slow": {}}
	last := map[string]time.Time{}
	for _, p := range polls {
		c.ingest(p.dev, []routeros.InterfaceStats{ifStats("ether1", bytesAt(p.t), 0, 0, 0)}, p.t, p.iv)
		c.finishCycle(p.t.Add(50*time.Millisecond), 15*time.Second)

		// The reference: this interval's delta spread over its minutes by
		// overlap.
		if from, ok := last[p.dev]; ok {
			delta := float64(bytesAt(p.t) - bytesAt(from))
			for m := from.Truncate(time.Minute); m.Before(p.t); m = m.Add(time.Minute) {
				overlap := minTime(p.t, m.Add(time.Minute)).Sub(maxTime(from, m))
				want[p.dev][m] += delta * overlap.Seconds() / p.t.Sub(from).Seconds()
			}
		}
		last[p.dev] = p.t
	}

	for _, dev := range []string{"fast", "slow"} {
		rows, err := queries.GetPortStatsSeries(db, queries.PortStats1m, dev, "ether1", at(10, 2, 0), at(11, 0, 0))
		if err != nil {
			t.Fatal(err)
		}
		got := map[time.Time]int64{}
		for _, r := range rows {
			got[r.Bucket] = r.RxBytes
		}
		for m := at(10, 2, 0); m.Before(at(11, 0, 0)); m = m.Add(time.Minute) {
			if d := float64(got[m]) - want[dev][m]; d > 2 || d < -2 {
				t.Errorf("%s %s: %d B, want %.0f B", dev, m.Format("15:04"), got[m], want[dev][m])
			}
		}
	}
	// The step at 10:30 straddles the slow device's 10:29:30–10:30:30
	// interval: half of its 30 MB belongs to 10:29.
	if w := want["slow"][at(10, 29, 0)]; w != 15e6 {
		t.Fatalf("reference for slow 10:29 = %.0f, the fixture no longer exercises the hold", w)
	}
}

func TestPortStatsSnapshotPruneAndShape(t *testing.T) {
	var nilCollector *PortStatsCollector
	empty := nilCollector.Snapshot()
	if empty.TS != nil || empty.Ports == nil || len(empty.Ports) != 0 || empty.IntervalSeconds != 15 {
		t.Fatalf("nil snapshot = %+v", empty)
	}
	if !nilCollector.FlushedThrough().IsZero() || !nilCollector.RolledThrough().IsZero() {
		t.Fatal("nil collector accessors must return zero times")
	}

	c := NewPortStatsCollector(pollerTestDB(t), nil, nil)
	b, _ := json.Marshal(c.Snapshot())
	if string(b) != `{"ts":null,"interval_seconds":15,"ready":false,"ports":[]}` {
		t.Fatalf("pre-cycle JSON = %s", b)
	}

	const iv = 15 * time.Second
	c.ingest("d2", []routeros.InterfaceStats{ifStats("sfp1", 0, 0, 0, 0), ifStats("ether2", 0, 0, 0, 0)}, at(12, 0, 0), iv)
	c.ingest("d1", []routeros.InterfaceStats{{Name: "wifi1", Type: "wifi", Comment: "5 GHz", Disabled: true}}, at(12, 0, 30), iv)
	c.ingest("d1", []routeros.InterfaceStats{ifStats("ether1", 0, 0, 0, 0)}, at(12, 0, 50), iv)

	// d2 was last refreshed 50 s before the cycle end: > 3×15 s → dropped.
	c.finishCycle(at(12, 0, 50), iv)
	snap := c.Snapshot()
	if snap.TS == nil || !snap.TS.Equal(at(12, 0, 50)) {
		t.Fatalf("TS = %v", snap.TS)
	}
	var keys []string
	for _, p := range snap.Ports {
		keys = append(keys, p.DeviceID+"/"+p.Iface)
	}
	if len(keys) != 2 || keys[0] != "d1/ether1" || keys[1] != "d1/wifi1" {
		t.Fatalf("snapshot ports = %v, want [d1/ether1 d1/wifi1] sorted, d2 pruned", keys)
	}
	w := snap.Ports[1]
	if w.Type != "wifi" || w.Comment != "5 GHz" || !w.Disabled || w.Running {
		t.Errorf("wifi1 = %+v", w)
	}

	// Deep copy: mutating the returned slice leaves the collector alone.
	snap.Ports[0].RxBps = 999
	*snap.TS = time.Time{}
	if again := c.Snapshot(); again.Ports[0].RxBps != 0 || !again.TS.Equal(at(12, 0, 50)) {
		t.Error("Snapshot must return a deep copy")
	}
}

func TestPortStatsIntervalSettings(t *testing.T) {
	db := pollerTestDB(t)

	if got := PortStatsInterval(db); got != 15*time.Second {
		t.Errorf("seeded port_stats_interval = %v, want 15s", got)
	}
	if got := PortHostsInterval(db); got != 300*time.Second {
		t.Errorf("seeded port_hosts_interval = %v, want 300s", got)
	}
	if got := PortStatsInterval(nil); got != 15*time.Second {
		t.Errorf("nil db = %v, want default", got)
	}

	tests := []struct {
		key, value string
		read       func() time.Duration
		want       time.Duration
	}{
		{"port_stats_interval", "2", func() time.Duration { return PortStatsInterval(db) }, 5 * time.Second},
		{"port_stats_interval", "99999999999", func() time.Duration { return PortStatsInterval(db) }, 300 * time.Second},
		{"port_stats_interval", "99999999999999999999", func() time.Duration { return PortStatsInterval(db) }, 15 * time.Second}, // overflows Atoi → default
		{"port_stats_interval", "9999", func() time.Duration { return PortStatsInterval(db) }, 300 * time.Second},
		{"port_stats_interval", " 30 ", func() time.Duration { return PortStatsInterval(db) }, 30 * time.Second},
		{"port_stats_interval", "abc", func() time.Duration { return PortStatsInterval(db) }, 15 * time.Second},
		{"port_hosts_interval", "10", func() time.Duration { return PortHostsInterval(db) }, 60 * time.Second},
		{"port_hosts_interval", "7200", func() time.Duration { return PortHostsInterval(db) }, 3600 * time.Second},
		{"port_hosts_interval", "", func() time.Duration { return PortHostsInterval(db) }, 300 * time.Second},
	}
	for _, tt := range tests {
		if err := queries.SetSetting(db, tt.key, tt.value); err != nil {
			t.Fatalf("set %s: %v", tt.key, err)
		}
		if got := tt.read(); got != tt.want {
			t.Errorf("%s=%q → %v, want %v", tt.key, tt.value, got, tt.want)
		}
	}
}

func TestSweepPortStatsRetention(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "d1", Identity: "sw", Address: "192.168.78.10"})
	now := time.Now().UTC().Truncate(time.Minute)

	if err := queries.InsertPortStats1m(db, []queries.PortStatRow{
		{DeviceID: "d1", InterfaceName: "ether1", Bucket: now.AddDate(0, 0, -400), RxBytes: 1, Samples: 1},
		{DeviceID: "d1", InterfaceName: "ether1", Bucket: now.AddDate(0, 0, -20), RxBytes: 1, Samples: 1},
		{DeviceID: "d1", InterfaceName: "ether1", Bucket: now.AddDate(0, 0, -3), RxBytes: 1, Samples: 1},
		{DeviceID: "d1", InterfaceName: "ether1", Bucket: now.Add(-time.Hour), RxBytes: 1, Samples: 1},
	}); err != nil {
		t.Fatalf("seed 1m: %v", err)
	}
	if err := queries.RollupPortStats1h(db, now.AddDate(0, 0, -401), now); err != nil {
		t.Fatalf("seed 1h: %v", err)
	}
	if err := queries.UpsertPortHosts(db, "d1", []queries.PortHost{{InterfaceName: "ether1", MACAddress: "AA:AA:AA:AA:AA:01"}}, now.AddDate(0, 0, -10)); err != nil {
		t.Fatalf("seed stale host: %v", err)
	}
	if err := queries.UpsertPortHosts(db, "d1", []queries.PortHost{{InterfaceName: "ether2", MACAddress: "AA:AA:AA:AA:AA:02"}}, now); err != nil {
		t.Fatalf("seed fresh host: %v", err)
	}

	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	// Defaults: 1m 2 d, 1h 365 d, hosts 7 d.
	sweepPortStats(db, time.Now())
	if n := count("port_stats_1m"); n != 1 {
		t.Errorf("1m rows = %d, want 1 (-1h)", n)
	}
	if n := count("port_stats_1h"); n != 3 {
		t.Errorf("1h rows = %d, want 3 (-20d, -3d, -1h hours)", n)
	}
	if n := count("port_hosts"); n != 1 {
		t.Errorf("port_hosts rows = %d, want 1", n)
	}

	// A longer explicit 1m retention is honoured.
	if err := queries.InsertPortStats1m(db, []queries.PortStatRow{
		{DeviceID: "d1", InterfaceName: "ether1", Bucket: now.AddDate(0, 0, -3), RxBytes: 1, Samples: 1},
	}); err != nil {
		t.Fatalf("re-seed 1m: %v", err)
	}
	_ = queries.SetSetting(db, "port_stats_1m_days", "5")
	sweepPortStats(db, time.Now())
	if n := count("port_stats_1m"); n != 2 {
		t.Errorf("1m rows with 5-day retention = %d, want 2 (-3d, -1h)", n)
	}

	// Out-of-range values clamp: 1m "0" → 1 day, 1h "1" → 7 days.
	_ = queries.SetSetting(db, "port_stats_1m_days", "0")
	_ = queries.SetSetting(db, "port_stats_1h_days", "1")
	_ = queries.SetSetting(db, "port_hosts_stale_days", "junk") // → default 7
	sweepPortStats(db, time.Now())
	if n := count("port_stats_1m"); n != 1 {
		t.Errorf("1m rows after clamp to 1 day = %d, want 1", n)
	}
	if n := count("port_stats_1h"); n != 2 {
		t.Errorf("1h rows after clamp to 7 days = %d, want 2", n)
	}
	if n := count("port_hosts"); n != 1 {
		t.Errorf("port_hosts rows = %d, want 1", n)
	}
}

func TestPortStatsCycleConcurrentWithInFlightGuard(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "fast", Identity: "fast", Address: "192.168.78.10"})
	mustCreateDevice(t, db, &queries.Device{ID: "slow", Identity: "slow", Address: "192.168.78.11"})
	mustCreateDevice(t, db, &queries.Device{ID: "down", Identity: "down", Address: "192.168.78.12", Status: "offline"})
	mustCreateDevice(t, db, &queries.Device{ID: "nocl", Identity: "nocl", Address: "192.168.78.13"})

	c := NewPortStatsCollector(db, nil, ws.NewHub())
	c.phase = noJitter
	c.reg = NewScheduleRegistry()
	release := make(chan struct{})
	var calls sync.Map // device → *int32
	var fastCount atomic.Int64
	c.fetchStats = func(id string) ([]routeros.InterfaceStats, error) {
		v, _ := calls.LoadOrStore(id, new(int32))
		atomic.AddInt32(v.(*int32), 1)
		switch id {
		case "slow":
			<-release
			return []routeros.InterfaceStats{ifStats("ether1", 1, 1, 1, 1)}, nil
		case "nocl":
			return nil, errNoClient
		case "down":
			t.Error("offline device must not be polled")
		}
		n := fastCount.Add(1)
		return []routeros.InterfaceStats{ifStats("ether1", n*1000, n*1000, n, n)}, nil
	}
	callCount := func(id string) int32 {
		if v, ok := calls.Load(id); ok {
			return atomic.LoadInt32(v.(*int32))
		}
		return 0
	}

	const iv = 150 * time.Millisecond
	start := time.Now()
	c.cycle(context.Background(), iv)
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("cycle blocked on the slow device for %v", el)
	}
	c.cycle(context.Background(), iv) // slow still in flight → not polled again

	if got := callCount("slow"); got != 1 {
		t.Errorf("slow device polled %d times while in flight, want 1", got)
	}
	if got := callCount("fast"); got != 2 {
		t.Errorf("fast device polled %d times, want 2", got)
	}
	snap := c.Snapshot()
	if snap.TS == nil || !snap.Ready {
		t.Errorf("snapshot after two cycles = %+v", snap)
	}
	for _, p := range snap.Ports {
		if p.DeviceID == "slow" {
			t.Fatalf("slow device reported before its reply: %+v", p)
		}
	}

	// The late reply still lands in the state.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		found := false
		for _, p := range c.Snapshot().Ports {
			found = found || p.DeviceID == "slow"
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late reply from the slow device never reached the snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The next read counts from the end of the slow one: right after the
	// late reply it is not due — no catch-up — one interval later it is.
	c.cycle(context.Background(), iv)
	if got := callCount("slow"); got != 1 {
		t.Errorf("slow device polled %d times right after its late reply, want 1 (no catch-up)", got)
	}
	time.Sleep(iv + iv/2)
	c.cycle(context.Background(), iv)
	if got := callCount("slow"); got != 2 {
		t.Errorf("slow device polled %d times an interval after returning, want 2", got)
	}
}

func TestPortStatsCycleShutdownSkipsFinish(t *testing.T) {
	db := pollerTestDB(t)
	c := NewPortStatsCollector(db, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.cycle(ctx, time.Second)
	if c.Snapshot().TS != nil {
		t.Error("a cycle interrupted by shutdown must not stamp the snapshot")
	}
}

// hostsCadence runs the FDB dump and the stacking prints every 5 min.
func hostsCadence() *stubCadence {
	return &stubCadence{iv: map[devclass.Tier]time.Duration{
		devclass.TierBridgeHosts: 5 * time.Minute, devclass.TierRelations: 5 * time.Minute,
	}}
}

func TestPortStatsCollectHosts(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "sw", Identity: "sw", Address: "192.168.78.10"})
	mustCreateDevice(t, db, &queries.Device{ID: "bad", Identity: "bad", Address: "192.168.78.11"})
	mustCreateDevice(t, db, &queries.Device{ID: "off", Identity: "off", Address: "192.168.78.12", Status: "offline"})

	c := NewPortStatsCollector(db, nil, nil)
	clock := &testClock{t: time.Now().UTC().Truncate(time.Second)}
	c.now = clock.now
	c.hostClock = newTierClock(NewScheduleRegistry())
	c.UseCadence(hostsCadence())
	hostCalls := map[string]int{}
	c.fetchHosts = func(id string) ([]routeros.BridgeHost, error) {
		hostCalls[id]++
		switch id {
		case "sw":
			return []routeros.BridgeHost{
				{MACAddress: "AA:BB:CC:00:00:01", Interface: "ether7", Bridge: "bridge", VID: 28, Dynamic: true},
				{MACAddress: "AA:BB:CC:00:00:02", Interface: "wifi2", Bridge: "bridge", External: true},
			}, nil
		case "bad":
			return nil, errors.New("boom")
		case "off":
			t.Error("offline device must not be dumped")
		}
		return nil, nil
	}
	relCalls := map[string]int{}
	c.fetchRelations = func(id string) ([]routeros.InterfaceRelation, error) {
		relCalls[id]++
		return []routeros.InterfaceRelation{
			{Iface: "ether7", Parent: "bond1", Kind: routeros.RelationBond},
			{Iface: "pppoe-out1", Parent: "ether1", Kind: routeros.RelationPPPoE},
		}, nil
	}
	c.hostsTick(context.Background())
	rels, err := queries.ListInterfaceRelations(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 || rels[0] != (queries.InterfaceRelation{DeviceID: "sw", InterfaceName: "ether7", Kind: "bond", Parent: "bond1"}) {
		t.Errorf("relations = %+v", rels)
	}
	if relCalls["bad"] != 0 || relCalls["sw"] != 1 {
		t.Errorf("relation reads = %v: a device failing its dump is not asked for its stacking", relCalls)
	}

	// Nothing is due again within the interval; a failing dump is not
	// retried early either.
	clock.set(clock.now().Add(pollerTick))
	c.hostsTick(context.Background())
	if hostCalls["sw"] != 1 || hostCalls["bad"] != 1 || relCalls["sw"] != 1 || relCalls["bad"] != 0 {
		t.Errorf("reads within the interval: hosts %v, relations %v", hostCalls, relCalls)
	}

	// A failed read keeps the previous rows.
	clock.set(clock.now().Add(6 * time.Minute))
	c.fetchRelations = func(string) ([]routeros.InterfaceRelation, error) { return nil, errors.New("trap") }
	c.hostsTick(context.Background())
	if rels, _ := queries.ListInterfaceRelations(db); len(rels) != 2 {
		t.Errorf("relations after a failed read = %+v, want the previous 2", rels)
	}

	hosts, err := queries.ListPortHosts(db, clock.now().Add(-time.Minute), "")
	if err != nil {
		t.Fatalf("ListPortHosts: %v", err)
	}
	if len(hosts) != 2 || hosts[0].DeviceID != "sw" || hosts[0].InterfaceName != "ether7" || hosts[0].VID != 28 ||
		hosts[1].InterfaceName != "wifi2" || !hosts[1].LastSeen.Equal(clock.now()) {
		t.Fatalf("hosts = %+v", hosts)
	}

	var macLookups int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mac_lookup`).Scan(&macLookups); err != nil {
		t.Fatalf("count mac_lookup: %v", err)
	}
	if macLookups != 0 {
		t.Errorf("mac_lookup rows = %d: the FDB dump must never write mac_lookup", macLookups)
	}
}

// On a cold start the pool has dialled only some devices by the first FDB
// pass. The ones without a connection are retried after portHostsRetryDelay,
// not a whole interval later, and the others are not dumped again.
func TestPortStatsCollectHostsRetriesDevicesWithoutClient(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "sw", Identity: "sw", Address: "192.168.78.10"})
	mustCreateDevice(t, db, &queries.Device{ID: "late", Identity: "late", Address: "192.168.78.11"})

	c := NewPortStatsCollector(db, nil, nil)
	clock := &testClock{t: time.Now().UTC().Truncate(time.Second)}
	c.now = clock.now
	c.hostClock = newTierClock(NewScheduleRegistry())
	c.UseCadence(hostsCadence())
	c.fetchRelations = func(string) ([]routeros.InterfaceRelation, error) { return nil, nil }
	dialled := map[string]bool{"sw": true}
	calls := map[string]int{}
	c.fetchHosts = func(id string) ([]routeros.BridgeHost, error) {
		calls[id]++
		if !dialled[id] {
			return nil, errNoClient
		}
		return []routeros.BridgeHost{{MACAddress: "AA:BB:CC:00:00:0" + string(rune('0'+calls[id])), Interface: "ether" + id}}, nil
	}

	start := clock.now()
	c.hostsTick(context.Background())
	dialled["late"] = true
	clock.set(start.Add(portHostsRetryDelay - time.Second))
	c.hostsTick(context.Background())
	if calls["late"] != 1 {
		t.Fatalf("late retried after %v: calls = %v", portHostsRetryDelay-time.Second, calls)
	}
	clock.set(start.Add(portHostsRetryDelay))
	c.hostsTick(context.Background())
	if calls["sw"] != 1 || calls["late"] != 2 {
		t.Errorf("fetch calls = %v, want sw once (not re-dumped by the retry) and late twice", calls)
	}
	hosts, err := queries.ListPortHosts(db, clock.now().Add(-time.Minute), "late")
	if err != nil {
		t.Fatalf("ListPortHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0].InterfaceName != "etherlate" {
		t.Errorf("late hosts = %+v", hosts)
	}
}

// The hosts loop runs each device on its class plan floored by
// port_hosts_interval — C2: the FDB every 10 min, the stacking every 15 —
// and a paused device on neither.
func TestPortStatsHostsPerClassCadenceAndPause(t *testing.T) {
	db := pollerTestDB(t)
	mustCreateDevice(t, db, &queries.Device{ID: "sw", Identity: "sw", Address: "192.0.2.20"})
	mustCreateDevice(t, db, &queries.Device{ID: "held", Identity: "held", Address: "192.0.2.21"})
	t0 := time.Now().UTC().Truncate(time.Second)
	if err := queries.SetDeviceClass(db, "sw", devclass.C2, devclass.ResourceFacts{Architecture: "arm", CPUCount: 1}, t0); err != nil {
		t.Fatal(err)
	}
	if err := queries.PauseDevicePolling(db, "held", t0.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	c := NewPortStatsCollector(db, nil, nil)
	clock := &testClock{t: t0}
	c.now = clock.now
	reg := NewScheduleRegistry()
	c.hostClock = newTierClock(reg)
	c.hostClock.phase = noJitter
	c.UseCadence(NewCadence(db, nil))
	fdb, rel := map[string]int{}, map[string]int{}
	c.fetchHosts = func(id string) ([]routeros.BridgeHost, error) { fdb[id]++; return nil, nil }
	c.fetchRelations = func(id string) ([]routeros.InterfaceRelation, error) { rel[id]++; return nil, nil }

	for _, step := range []struct {
		at       time.Duration
		fdb, rel int
	}{
		{0, 1, 1},
		{5 * time.Minute, 1, 1},
		{10 * time.Minute, 2, 1},
		{15 * time.Minute, 2, 2},
		{20 * time.Minute, 3, 2},
		{30 * time.Minute, 4, 3},
	} {
		clock.set(t0.Add(step.at))
		c.hostsTick(context.Background())
		if fdb["sw"] != step.fdb || rel["sw"] != step.rel {
			t.Errorf("+%v: FDB dumps %d, stacking reads %d; want %d, %d", step.at, fdb["sw"], rel["sw"], step.fdb, step.rel)
		}
	}
	if fdb["held"] != 0 || rel["held"] != 0 {
		t.Errorf("a paused device was read: FDB %d, stacking %d", fdb["held"], rel["held"])
	}
	if runs := reg.NextRuns("held"); len(runs) != 0 {
		t.Errorf("a paused device has no next runs, got %v", runs)
	}
	if next := reg.NextRuns("sw")["bridge_hosts"]; !next.Equal(t0.Add(40 * time.Minute)) {
		t.Errorf("next FDB dump = %v, want 10 min after the last", next)
	}
}

func TestPortStatsPublishWithoutSubscribers(t *testing.T) {
	// No hub and a hub with no subscribers are both no-ops (collection and DB
	// writes don't depend on subscribers).
	NewPortStatsCollector(nil, nil, nil).publish()
	NewPortStatsCollector(nil, nil, ws.NewHub()).publish()
}
