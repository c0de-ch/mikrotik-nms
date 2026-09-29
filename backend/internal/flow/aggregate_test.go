package flow

import (
	"net/netip"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) // a minute boundary

func flowOf(inIf, outIf uint32, src, dst string, port uint16, bytes, packets int64, start, end time.Time) normFlow {
	k := aggKey{inIf: inIf, outIf: outIf, proto: 6, port: port}
	k.src = netip.MustParseAddr(src).As16()
	k.dst = netip.MustParseAddr(dst).As16()
	return normFlow{key: k, endpoints: true, bytes: bytes, packets: packets, sampling: 1, start: start, end: end}
}

func sumMinute(m *minuteAgg) (b, p, f int64) {
	for _, v := range m.keys {
		b, p, f = b+v.bytes, p+v.packets, f+v.flows
	}
	for _, v := range m.other {
		b, p, f = b+v.bytes, p+v.packets, f+v.flows
	}
	return
}

func TestSplitAcrossMinutesSumsExactly(t *testing.T) {
	a := newAggregator()
	now := t0.Add(2*time.Minute + 30*time.Second) // open: 12:00, 12:01, 12:02
	next := floorMinute(now.Unix()) - 120
	// 12:00:40 .. 12:02:10 = 90 s: 20 s / 60 s / 10 s.
	f := flowOf(1, 13, "1.1.1.1", "192.168.28.33", 443, 1_000_003, 997, t0.Add(40*time.Second), t0.Add(130*time.Second))
	if a.add(f, now, next) {
		t.Fatal("not late")
	}
	var tb, tp, tf int64
	// cumulative floor(total*cum/dur): 222222, 888891, 1000003
	want := map[int64]int64{t0.Unix(): 222_222, t0.Unix() + 60: 666_669, t0.Unix() + 120: 111_112}
	for m, wb := range want {
		b, p, fl := sumMinute(a.minutes[m])
		tb, tp, tf = tb+b, tp+p, tf+fl
		if b != wb {
			t.Errorf("minute %d bytes = %d, want %d", (m-t0.Unix())/60, b, wb)
		}
	}
	if tb != 1_000_003 || tp != 997 || tf != 1 {
		t.Fatalf("totals = %d/%d/%d, want exact 1000003/997/1", tb, tp, tf)
	}
	if _, _, fl := sumMinute(a.minutes[t0.Unix()+120]); fl != 1 {
		t.Fatal("flows += 1 must land in the minute holding the end")
	}
	if a.minutes[t0.Unix()+120].records != 1 || a.minutes[t0.Unix()].records != 0 {
		t.Fatal("records count in the end minute only")
	}
	// A flow ending exactly on a minute boundary still counts its record.
	a = newAggregator()
	a.add(flowOf(1, 13, "1.1.1.1", "10.0.0.1", 443, 100, 1, t0.Add(30*time.Second), t0.Add(time.Minute)), now, next)
	b0, _, f0 := sumMinute(a.minutes[t0.Unix()])
	b1, _, f1 := sumMinute(a.minutes[t0.Unix()+60])
	if b0 != 100 || b1 != 0 || f0+f1 != 1 || f1 != 1 {
		t.Fatalf("boundary end: %d/%d bytes, %d/%d flows", b0, b1, f0, f1)
	}
	// start == end: one minute.
	a = newAggregator()
	a.add(flowOf(1, 13, "1.1.1.1", "10.0.0.1", 443, 7, 1, t0.Add(61*time.Second), t0.Add(61*time.Second)), now, next)
	if b, _, f := sumMinute(a.minutes[t0.Unix()+60]); b != 7 || f != 1 || len(a.minutes) != 1 {
		t.Fatalf("instant flow: %d bytes %d flows in %d minutes", b, f, len(a.minutes))
	}
}

func TestLateAndFutureShares(t *testing.T) {
	a := newAggregator()
	now := t0.Add(10*time.Minute + 5*time.Second) // hi 12:10, lo 12:08
	next := floorMinute(now.Unix()) - 120
	lo := next
	// 12:02:00 .. 12:09:30 (450 s; 360 s before the window): the old part lands in lo.
	f := flowOf(1, 13, "1.1.1.1", "10.0.0.1", 443, 4500, 45, t0.Add(2*time.Minute), t0.Add(9*time.Minute+30*time.Second))
	if !a.add(f, now, next) {
		t.Fatal("share older than the oldest open minute must be late")
	}
	if len(a.minutes) != 2 || a.minutes[lo] == nil || a.minutes[lo].late != 1 {
		t.Fatalf("minutes %v, late %+v", len(a.minutes), a.minutes[lo])
	}
	b, _, _ := sumMinute(a.minutes[lo])
	b9, _, f9 := sumMinute(a.minutes[lo+60])
	if b != 3600+600 || b9 != 300 || f9 != 1 {
		t.Fatalf("late split = %d + %d (flows %d)", b, b9, f9)
	}
	// Entirely before the window: all of it (and the record) in lo.
	a = newAggregator()
	a.add(flowOf(1, 13, "1.1.1.1", "10.0.0.1", 443, 50, 5, t0, t0.Add(time.Minute)), now, next)
	if b, _, f := sumMinute(a.minutes[lo]); b != 50 || f != 1 || len(a.minutes) != 1 {
		t.Fatalf("old flow = %d bytes %d flows", b, f)
	}
	// An end more than 1 min in the future is clamped to now; shares after
	// the current minute land in it: never a 4th open minute.
	a = newAggregator()
	a.add(flowOf(1, 13, "1.1.1.1", "10.0.0.1", 443, 60, 6, now.Add(-3*time.Second), now.Add(10*time.Minute)), now, next)
	a.add(flowOf(1, 13, "1.1.1.1", "10.0.0.2", 443, 60, 6, now.Add(50*time.Second), now.Add(58*time.Second)), now, next)
	if len(a.minutes) != 1 {
		t.Fatalf("future shares opened %d minutes", len(a.minutes))
	}
	if b, _, f := sumMinute(a.minutes[floorMinute(now.Unix())]); b != 120 || f != 2 {
		t.Fatalf("current minute = %d bytes %d flows", b, f)
	}
}

func TestFlushTopNOtherExactAndGlobalCap(t *testing.T) {
	a := newAggregator()
	now := t0.Add(30 * time.Second)
	next := floorMinute(now.Unix()) - 120
	perPair := map[pairKey]int64{}
	add := func(f normFlow) {
		a.add(f, now, next)
		perPair[pairKey{f.key.inIf, f.key.outIf}] += f.bytes
	}
	for i := 1; i <= 30; i++ { // 30 conversations on pair 1/13
		add(flowOf(1, 13, "1.1.1.1", netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}).String(), 443, int64(i*100), 1, now, now))
	}
	for i := 1; i <= 5; i++ { // 5 on pair 13/1
		add(flowOf(13, 1, netip.AddrFrom4([4]byte{10, 0, 1, byte(i)}).String(), "1.1.1.1", 443, int64(i), 1, now, now))
	}
	// A flow without endpoints goes straight to its pair's other row.
	nf := normFlow{key: aggKey{inIf: 7, outIf: 0}, bytes: 9, packets: 1, sampling: 3, start: now, end: now}
	add(nf)
	a.minute(t0.Unix()).alive = true

	out := a.flush(42, t0.Unix(), 10, 12)
	var others, tops int
	sums := map[pairKey]int64{}
	for _, r := range out.rows {
		sums[pairKey{r.InIf, r.OutIf}] += r.Bytes
		if !r.Src.IsValid() && !r.Dst.IsValid() {
			others++
			if r.Proto != 0 || r.Port != 0 {
				t.Errorf("other row with proto/port: %+v", r)
			}
		} else {
			tops++
		}
		if r.ExporterID != 42 || !r.Bucket.Equal(t0) {
			t.Errorf("row identity: %+v", r)
		}
	}
	for p, want := range perPair {
		if sums[p] != want {
			t.Errorf("pair %v = %d, want %d (Σ per pair == input)", p, sums[p], want)
		}
	}
	// top-10 of pair 1/13 + 5 of pair 13/1 = 15 > global cap 12: the 3
	// smallest (pair 13/1 bytes 1..3) fold into their other row.
	if tops != 12 || others != 3 {
		t.Fatalf("rows: %d top + %d other, want 12 + 3", tops, others)
	}
	if out.rows[0].Bytes != 3000 {
		t.Fatalf("first row = %+v, want the largest (3000)", out.rows[0])
	}
	if out.meta == nil || out.meta.Records != 36 || out.meta.Sampling != 3 || out.meta.Bytes != 46_524 {
		t.Fatalf("meta = %+v", out.meta)
	}
	if !out.ifaces[1] || !out.ifaces[13] || !out.ifaces[7] || out.ifaces[0] {
		t.Fatalf("ifaces = %v", out.ifaces)
	}
	if len(a.minutes) != 0 {
		t.Fatal("flushed minute must be removed")
	}
}

func TestKeyCapOverflowKeepsTotals(t *testing.T) {
	a := newAggregator()
	now := t0.Add(30 * time.Second)
	next := floorMinute(now.Unix()) - 120
	var total int64
	n := maxKeysPerMinute + 500
	for i := 0; i < n; i++ {
		f := flowOf(1, 13, "1.1.1.1", netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)}).String(), 443,
			int64(i%97+1), 1, now, now)
		a.add(f, now, next)
		total += f.bytes
	}
	m := a.minutes[t0.Unix()]
	if len(m.keys) != maxKeysPerMinute || m.overflow != 500 {
		t.Fatalf("keys %d overflow %d", len(m.keys), m.overflow)
	}
	out := a.flush(1, t0.Unix(), 50, 1500)
	var sum, flows int64
	for _, r := range out.rows {
		sum += r.Bytes
		flows += r.Flows
	}
	if sum != total || flows != int64(n) || out.meta.Overflow != 500 {
		t.Fatalf("after overflow: bytes %d/%d flows %d/%d overflow %d", sum, total, flows, n, out.meta.Overflow)
	}
}

func TestMetaOnlyForAliveMinutes(t *testing.T) {
	a := newAggregator()
	now := t0.Add(2*time.Minute + 10*time.Second)
	next := floorMinute(now.Unix()) - 120
	a.markAlive(now, next) // alive 12:00, 12:01, 12:02; datagram counted in 12:02
	for _, m := range []int64{0, 60, 120} {
		out := a.flush(1, t0.Unix()+m, 50, 1500)
		if out.meta == nil || out.meta.Records != 0 || len(out.rows) != 0 {
			t.Fatalf("alive minute +%ds: meta %+v rows %d", m, out.meta, len(out.rows))
		}
		if wantDG := map[int64]int64{0: 0, 60: 0, 120: 1}[m]; out.meta.Datagrams != wantDG {
			t.Fatalf("minute +%ds datagrams = %d, want %d", m, out.meta.Datagrams, wantDG)
		}
	}
	if out := a.flush(1, t0.Unix()+180, 50, 1500); out.meta != nil {
		t.Fatalf("dead minute got meta %+v", out.meta)
	}
}

func TestFlushOrderDeterministic(t *testing.T) {
	build := func() []queries.FlowRow {
		a := newAggregator()
		now := t0.Add(30 * time.Second)
		for i := 0; i < 20; i++ { // equal bytes: order by key bytes
			a.add(flowOf(1, 13, "1.1.1.1", netip.AddrFrom4([4]byte{10, 0, 0, byte(20 - i)}).String(), 443, 10, 1, now, now),
				now, floorMinute(now.Unix())-120)
		}
		return a.flush(1, t0.Unix(), 5, 1500).rows
	}
	x, y := build(), build()
	for i := range x {
		if x[i] != y[i] {
			t.Fatalf("row %d differs between runs", i)
		}
	}
	if x[0].Dst != netip.MustParseAddr("10.0.0.1") || x[4].Dst != netip.MustParseAddr("10.0.0.5") {
		t.Fatalf("tie-break by key bytes: %v .. %v", x[0].Dst, x[4].Dst)
	}
}
