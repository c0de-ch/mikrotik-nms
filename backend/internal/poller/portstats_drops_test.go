package poller

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// dropStats is a running port with every counter of the Tier 0 read.
func dropStats(name string, rxBytes, txBytes, rxDrop, txDrop, rxErr, txErr, txQueue, linkDowns int64) routeros.InterfaceStats {
	s := ifStats(name, rxBytes, txBytes, rxBytes/100, txBytes/100)
	s.RxDrops, s.TxDrops, s.RxErrors, s.TxErrors, s.TxQueueDrops = rxDrop, txDrop, rxErr, txErr, txQueue
	s.HasRxDrops, s.HasTxDrops, s.HasRxErrors, s.HasTxErrors, s.HasTxQueueDrops = true, true, true, true, true
	s.LinkDowns, s.HasLinkDowns = linkDowns, true
	return s
}

func rowFor(t *testing.T, rows []queries.PortStatRow, iface string, minute time.Time) queries.PortStatRow {
	t.Helper()
	for _, r := range rows {
		if r.InterfaceName == iface && r.Bucket.Equal(minute) {
			return r
		}
	}
	t.Fatalf("no row for %s at %s in %+v", iface, minute.Format("15:04"), rows)
	return queries.PortStatRow{}
}

func hasRow(rows []queries.PortStatRow, iface string, minute time.Time) bool {
	for _, r := range rows {
		if r.InterfaceName == iface && r.Bucket.Equal(minute) {
			return true
		}
	}
	return false
}

// The drop, error and link-down counters the Tier 0 read always fetched are
// stored as per-minute deltas, prorated across minutes like the bytes. A port
// without the attributes stores NULL; a minute is idle (no row) only when
// every byte and every drop / error / link-down delta is zero.
func TestPortStatsDropAndErrorDeltasPerMinute(t *testing.T) {
	c := NewPortStatsCollector(nil, nil, nil)
	const iv = 30 * time.Second

	down := func(linkDowns int64) routeros.InterfaceStats { // a port that is down: counters stand still
		s := ifStats("ether3", 700, 700, 7, 7)
		s.Running = false
		s.LinkDowns, s.HasLinkDowns = linkDowns, true
		return s
	}
	plain := ifStats("ether2", 5, 5, 0, 0) // no drop attributes at all, never changes
	plain.LinkDowns, plain.HasLinkDowns = 0, true

	c.ingest("d1", []routeros.InterfaceStats{dropStats("ether1", 1000, 1000, 5, 100, 0, 0, 10, 2), plain, down(1)}, at(12, 0, 5), iv)
	// ether1 moves no bytes but drops 60 on egress; ether3 goes down once more.
	c.ingest("d1", []routeros.InterfaceStats{dropStats("ether1", 1000, 1000, 5, 160, 3, 0, 10, 2), plain, down(2)}, at(12, 0, 35), iv)
	// The next interval straddles the minute: 25 s in 12:00, 5 s in 12:01.
	c.ingest("d1", []routeros.InterfaceStats{dropStats("ether1", 1000, 1000, 5, 190, 3, 6, 16, 2), plain, down(2)}, at(12, 1, 5), iv)

	rows := c.takeMinutesBefore(at(12, 1, 0))
	e1 := rowFor(t, rows, "ether1", at(12, 0, 0))
	if e1.RxBytes != 0 || e1.TxBytes != 0 {
		t.Errorf("ether1 bytes = %d/%d, want an idle port", e1.RxBytes, e1.TxBytes)
	}
	want := queries.PortStatRow{
		DeviceID: "d1", InterfaceName: "ether1", Bucket: at(12, 0, 0), Samples: 1,
		RxDrop: nullInt(0), TxDrop: nullInt(60 + 25), RxError: nullInt(3), TxError: nullInt(5),
		TxQueueDrop: nullInt(5), LinkDowns: nullInt(0), CoveredS: nullInt(55),
	}
	if e1 != want {
		t.Errorf("ether1 12:00 =\n %+v\nwant\n %+v", e1, want)
	}
	e3 := rowFor(t, rows, "ether3", at(12, 0, 0))
	if e3.LinkDowns != nullInt(1) || e3.TxDrop.Valid || e3.RxError.Valid || e3.CoveredS != nullInt(55) || e3.RxBytes != 0 {
		t.Errorf("ether3 12:00 = %+v, want one link-down, NULL drops, 55 s observed", e3)
	}
	if hasRow(rows, "ether2", at(12, 0, 0)) {
		t.Error("a port with no bytes and no drop, error or link-down delta is idle: no row")
	}

	// The rest of the straddling interval is in 12:01: 5 of the 30 drops.
	c.ingest("d1", []routeros.InterfaceStats{dropStats("ether1", 1000, 1000, 5, 190, 3, 6, 16, 2), plain, down(2)}, at(12, 1, 35), iv)
	rows = c.takeMinutesBefore(at(12, 2, 0))
	e1 = rowFor(t, rows, "ether1", at(12, 1, 0))
	if e1.TxDrop != nullInt(5) || e1.TxError != nullInt(1) || e1.TxQueueDrop != nullInt(1) || e1.CoveredS != nullInt(35) {
		t.Errorf("ether1 12:01 = %+v, want the 5 s share of the straddling interval", e1)
	}
}

// Resets and gaps follow the byte rules: a counter that went backwards
// contributes nothing for that interval, a reboot (bytes went backwards)
// nothing at all, and neither does a delta across a gap.
func TestPortStatsDropDeltasResetAndGap(t *testing.T) {
	c := NewPortStatsCollector(nil, nil, nil)
	const iv = 15 * time.Second
	ingest := func(ts time.Time, s routeros.InterfaceStats) {
		c.ingest("d1", []routeros.InterfaceStats{s}, ts, iv)
	}
	ingest(at(12, 0, 0), dropStats("ether1", 1000, 1000, 50, 500, 0, 0, 0, 3))
	// tx-drop was cleared on the device (reset-counters): skipped, rx-drop counts.
	ingest(at(12, 0, 15), dropStats("ether1", 2000, 2000, 54, 20, 0, 0, 0, 3))
	// Reboot: the byte counters restart, so nothing of this interval counts —
	// not even the drops that happen to be higher than before.
	ingest(at(12, 0, 30), dropStats("ether1", 10, 10, 90, 900, 0, 0, 0, 0))
	// Normal again.
	ingest(at(12, 0, 45), dropStats("ether1", 1010, 10, 91, 907, 0, 0, 0, 0))
	rows := c.takeMinutesBefore(at(12, 1, 0))
	r := rowFor(t, rows, "ether1", at(12, 0, 0))
	if r.RxDrop != nullInt(4+1) || r.TxDrop != nullInt(7) || r.LinkDowns != nullInt(0) {
		t.Errorf("drops = rx %v tx %v link-downs %v, want 5 / 7 / 0", r.RxDrop, r.TxDrop, r.LinkDowns)
	}
	if r.CoveredS != nullInt(45) {
		t.Errorf("covered_s = %v, want 45 (the port was observed across the reset)", r.CoveredS)
	}

	// A read more than three intervals after the last one is a gap.
	ingest(at(12, 5, 0), dropStats("ether1", 9_000_000, 10, 1000, 9000, 0, 0, 0, 0))
	ingest(at(12, 5, 15), dropStats("ether1", 9_000_000, 10, 1000, 9003, 0, 0, 0, 0))
	rows = c.takeMinutesBefore(at(12, 6, 0))
	for _, m := range []time.Time{at(12, 1, 0), at(12, 2, 0), at(12, 3, 0), at(12, 4, 0)} {
		if hasRow(rows, "ether1", m) {
			t.Errorf("a row at %s: the gap must not be filled", m.Format("15:04"))
		}
	}
	if r = rowFor(t, rows, "ether1", at(12, 5, 0)); r.TxDrop != nullInt(3) || r.RxDrop != nullInt(0) || r.CoveredS != nullInt(15) {
		t.Errorf("after the gap = %+v, want only the 3 drops of the first usable interval", r)
	}
}

func coverageOf(rows []DeviceMinuteCoverage, minute time.Time) (DeviceMinuteCoverage, bool) {
	for _, r := range rows {
		if r.Minute.Equal(minute) {
			return r, true
		}
	}
	return DeviceMinuteCoverage{}, false
}

// The collector knows which minutes each device's Tier 0 intervals spanned:
// that is the device's coverage. A read that pairs with no earlier one within
// the gap rule covers nothing, and the reads that could not run are counted
// by cause.
func TestPortStatsDeviceCoverage(t *testing.T) {
	c := NewPortStatsCollector(nil, nil, nil)
	const iv = 30 * time.Second
	row := []routeros.InterfaceStats{ifStats("ether1", 0, 0, 0, 0)}
	c.ingest("d1", row, at(12, 0, 5), iv)
	c.ingest("d1", row, at(12, 0, 35), iv)
	c.ingest("d1", row, at(12, 1, 5), iv)
	c.noteTier0Miss("d1", at(12, 3, 10), errNoClient)
	c.noteTier0Miss("d1", at(12, 4, 10), errors.New("timeout"))
	c.noteTier0Miss("d1", at(12, 4, 40), errors.New("timeout"))
	c.ingest("d1", row, at(12, 5, 0), iv) // 3 min 55 s after the last read: a gap
	c.ingest("d1", row, at(12, 5, 30), iv)

	cov := c.takeCoverageBefore(at(12, 6, 0))
	want := map[time.Time]DeviceMinuteCoverage{
		at(12, 0, 0): {DeviceID: "d1", Minute: at(12, 0, 0), PolledSeconds: 55, Reads: 2},
		at(12, 1, 0): {DeviceID: "d1", Minute: at(12, 1, 0), PolledSeconds: 5, Reads: 1},
		at(12, 3, 0): {DeviceID: "d1", Minute: at(12, 3, 0), NoSession: 1},
		at(12, 4, 0): {DeviceID: "d1", Minute: at(12, 4, 0), Failed: 2},
		at(12, 5, 0): {DeviceID: "d1", Minute: at(12, 5, 0), PolledSeconds: 30, Reads: 2},
	}
	if len(cov) != len(want) {
		t.Fatalf("coverage = %+v, want %d minutes", cov, len(want))
	}
	for m, w := range want {
		if got, ok := coverageOf(cov, m); !ok || got != w {
			t.Errorf("%s = %+v (found %v), want %+v", m.Format("15:04"), got, ok, w)
		}
	}
	if again := c.takeCoverageBefore(at(12, 6, 0)); len(again) != 0 {
		t.Errorf("taken coverage must be removed, got %+v", again)
	}
}

// recordingSink captures what a flush hands the coverage sink.
type recordingSink struct {
	mu       sync.Mutex
	through  []time.Time
	coverage []DeviceMinuteCoverage
}

func (s *recordingSink) FlushMinutes(through time.Time, coverage []DeviceMinuteCoverage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.through = append(s.through, through)
	s.coverage = append(s.coverage, coverage...)
}

// Every flush hands the sink the minutes that just became final, with the
// same boundary the port rows are written by, and a failed read is counted
// in the minute it failed in.
func TestPortStatsFlushFeedsTheCoverageSink(t *testing.T) {
	run := switchRunner("1000")
	c, clock := newTestPortStats(t, run)
	sink := &recordingSink{}
	c.UseCoverageSink(sink)
	const iv = 15 * time.Second

	c.cycle(t.Context(), iv) // 12:00:05
	clock.set(at(12, 0, 20))
	c.cycle(t.Context(), iv)
	// The device stops answering.
	run.byCmd["/interface/print"] = fakeReply{err: errors.New("i/o timeout")}
	clock.set(at(12, 0, 35))
	c.cycle(t.Context(), iv)
	clock.set(at(12, 1, 40))
	c.cycle(t.Context(), iv)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if n := len(sink.through); n == 0 || !sink.through[n-1].Equal(c.FlushedThrough()) || !c.FlushedThrough().Equal(at(12, 1, 0)) {
		t.Fatalf("sink boundaries = %v, collector flushed through %v; want both at 12:01", sink.through, c.FlushedThrough())
	}
	got, ok := coverageOf(sink.coverage, at(12, 0, 0))
	if !ok || got.PolledSeconds != 15 || got.Reads != 2 || got.Failed != 1 || got.DeviceID != "sw" {
		t.Errorf("12:00 coverage = %+v (found %v), want 15 s polled, 2 reads, 1 failed", got, ok)
	}
	if _, ok := coverageOf(sink.coverage, at(12, 1, 0)); ok {
		t.Error("12:01 is still open: it must not be handed over yet")
	}
}
