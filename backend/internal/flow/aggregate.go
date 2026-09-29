package flow

import (
	"bytes"
	"cmp"
	"math"
	"math/bits"
	"net/netip"
	"slices"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// Aggregation limits (contract §8.13).
const (
	maxKeysPerMinute = 20_000 // conversation keys per (exporter, open minute)
	maxOpenMinutes   = 3
	flushDelay       = 180 // s: minute M flushes at M + 60 s + 120 s
	maxFlowValue     = 1 << 48
)

// aggKey is one conversation of an exporter-minute. Addresses are 16 bytes
// (IPv4 v4-mapped) in memory and 4 or 16 bytes in the database.
type aggKey struct {
	inIf, outIf uint32
	proto       uint8
	port        uint16
	src, dst    [16]byte
}

type pairKey struct{ inIf, outIf uint32 }

type aggVal struct{ bytes, packets, flows int64 }

func satAdd(a, b int64) int64 {
	if s := a + b; s >= a {
		return s
	}
	return math.MaxInt64
}

func (v *aggVal) add(b, p, f int64) {
	v.bytes, v.packets, v.flows = satAdd(v.bytes, b), satAdd(v.packets, p), satAdd(v.flows, f)
}

// minuteAgg accumulates one exporter-minute.
type minuteAgg struct {
	start    int64 // unix seconds of the minute start
	keys     map[aggKey]*aggVal
	other    map[pairKey]*aggVal
	alive    bool  // the exporter sent datagrams covering this minute
	dgrams   int64 // datagrams received in this minute
	records  int64 // flow records whose end landed here
	bytes    int64 // pre-fold total
	sampling int64 // max 1-in-N
	overflow int64 // records beyond the key cap
	late     int64 // records with a share older than the oldest open minute
}

func newMinuteAgg(start int64) *minuteAgg {
	return &minuteAgg{start: start, keys: map[aggKey]*aggVal{}, other: map[pairKey]*aggVal{}}
}

func (m *minuteAgg) otherFor(p pairKey) *aggVal {
	v := m.other[p]
	if v == nil {
		if len(m.other) >= maxKeysPerMinute { // wire-controlled pairs: bounded too
			p = pairKey{}
			if v = m.other[p]; v != nil {
				return v
			}
		}
		v = &aggVal{}
		m.other[p] = v
	}
	return v
}

// aggregator holds one exporter's open minutes.
type aggregator struct {
	minutes map[int64]*minuteAgg
}

func newAggregator() *aggregator { return &aggregator{minutes: map[int64]*minuteAgg{}} }

func (a *aggregator) minute(start int64) *minuteAgg {
	m := a.minutes[start]
	if m == nil {
		m = newMinuteAgg(start)
		a.minutes[start] = m
	}
	return m
}

func floorMinute(unix int64) int64 { return unix - ((unix%60)+60)%60 }

// window returns the open-minute range [lo, hi] for a worker time: lo is the
// oldest minute not yet flushed, hi the current minute. When the clock
// stepped back behind nextFlush (an NTP step on the host), everything goes
// into the oldest still-open minute: flushDue only visits minutes from
// nextFlush on, so a share below it would stay in memory until shutdown.
func window(now time.Time, nextFlush int64) (lo, hi int64) {
	hi = floorMinute(now.Unix())
	if hi < nextFlush {
		return nextFlush, nextFlush
	}
	lo = max(hi-int64(maxOpenMinutes-1)*60, nextFlush)
	return lo, hi
}

// markAlive records a datagram received at now: minutes [floor(now)−2m,
// floor(now)] are alive, and the datagram counts in floor(now).
func (a *aggregator) markAlive(now time.Time, nextFlush int64) {
	lo, hi := window(now, nextFlush)
	for m := lo; m <= hi; m += 60 {
		a.minute(m).alive = true
	}
	a.minute(hi).dgrams++
}

// normFlow is a flow after normalisation, ready to aggregate.
type normFlow struct {
	key            aggKey
	endpoints      bool // false: no usable src/dst — only the pair's other row
	bytes, packets int64
	sampling       int64
	start, end     time.Time
}

// mulDiv returns floor(x*num/den) without overflow (num <= den, den > 0).
func mulDiv(x, num, den uint64) uint64 {
	hi, lo := bits.Mul64(x, num)
	q, _ := bits.Div64(hi, lo, den)
	return q
}

// add splits f across the open minutes in proportion to its overlap with
// each (cumulative rounding: the shares sum exactly to the flow's bytes and
// packets). The part before the oldest open minute goes to that minute and
// counts the record late; a share after the current minute is added to the
// current minute; an end more than 1 min after now is clamped to now.
// flows += 1 goes to the minute holding the end. Returns true when late.
func (a *aggregator) add(f normFlow, now time.Time, nextFlush int64) bool {
	lo, hi := window(now, nextFlush)
	start, end := f.start, f.end
	if end.After(now.Add(time.Minute)) {
		end = now
	}
	if start.After(end) {
		start = end
	}
	b, p := min(max(f.bytes, 0), maxFlowValue), min(max(f.packets, 0), maxFlowValue)
	clampMin := func(m int64) int64 { return min(max(m, lo), hi) }
	endMin := floorMinute(end.Unix())
	late := endMin < lo || floorMinute(start.Unix()) < lo

	put := func(m int64, sb, sp, sf int64) {
		mm := a.minute(clampMin(m))
		mm.bytes = satAdd(mm.bytes, sb)
		mm.sampling = max(mm.sampling, f.sampling)
		if sf > 0 {
			mm.records++
		}
		if !f.endpoints {
			mm.otherFor(pairKey{f.key.inIf, f.key.outIf}).add(sb, sp, sf)
			return
		}
		if v := mm.keys[f.key]; v != nil {
			v.add(sb, sp, sf)
			return
		}
		if len(mm.keys) >= maxKeysPerMinute {
			if sf > 0 {
				mm.overflow++
			}
			mm.otherFor(pairKey{f.key.inIf, f.key.outIf}).add(sb, sp, sf)
			return
		}
		v := &aggVal{}
		v.add(sb, sp, sf)
		mm.keys[f.key] = v
	}

	startMs, endMs := start.UnixMilli(), end.UnixMilli()
	if endMs <= startMs {
		put(endMin, b, p, 1)
	} else {
		dur := uint64(endMs - startMs)
		var cum, doneB, doneP uint64
		share := func(m int64, ov int64) {
			cum = min(cum+uint64(ov), dur)
			nb, np := mulDiv(uint64(b), cum, dur), mulDiv(uint64(p), cum, dur)
			if nb > doneB || np > doneP {
				put(m, int64(nb-doneB), int64(np-doneP), 0)
			}
			doneB, doneP = nb, np
		}
		first := floorMinute(start.Unix())
		if first < lo { // everything before lo in one share: it all lands in lo
			share(lo, min(endMs, lo*1000)-startMs)
			first = lo
		}
		for m := first; m <= endMin && cum < dur; m += 60 {
			if ov := min(endMs, (m+60)*1000) - max(startMs, m*1000); ov > 0 {
				share(m, ov)
			}
		}
		if rb, rp := uint64(b)-doneB, uint64(p)-doneP; rb > 0 || rp > 0 { // never with exact overlaps
			put(endMin, int64(rb), int64(rp), 0)
		}
		put(endMin, 0, 0, 1) // the record counts in the minute holding its end
	}
	if late {
		a.minute(lo).late++
	}
	return late
}

// oldest returns the oldest open minute, ok=false when none.
func (a *aggregator) oldest() (int64, bool) {
	best, ok := int64(0), false
	for m := range a.minutes {
		if !ok || m < best {
			best, ok = m, true
		}
	}
	return best, ok
}

// flushed is the result of flushing one exporter-minute.
type flushed struct {
	rows   []queries.FlowRow
	meta   *queries.FlowMeta
	ifaces map[uint32]bool // non-zero ifIndexes with bytes > 0
}

func keyAddr(b [16]byte) netip.Addr { return netip.AddrFrom16(b).Unmap() }

// compareAggKeys orders keys by their key bytes ascending: in_if, out_if,
// proto, port, src, dst.
func compareAggKeys(a, b aggKey) int {
	if c := cmp.Compare(a.inIf, b.inIf); c != 0 {
		return c
	}
	if c := cmp.Compare(a.outIf, b.outIf); c != 0 {
		return c
	}
	if c := cmp.Compare(a.proto, b.proto); c != 0 {
		return c
	}
	if c := cmp.Compare(a.port, b.port); c != 0 {
		return c
	}
	if c := bytes.Compare(a.src[:], b.src[:]); c != 0 {
		return c
	}
	return bytes.Compare(a.dst[:], b.dst[:])
}

type keyed struct {
	k aggKey
	v *aggVal
}

func byBytesThenKey(x, y keyed) int {
	if c := cmp.Compare(y.v.bytes, x.v.bytes); c != 0 {
		return c
	}
	return compareAggKeys(x.k, y.k)
}

// flush removes minute m and returns its rows: per (in_if, out_if) the top
// topN conversations by bytes (ties: key bytes ascending) with the rest
// folded into the pair's other row (src = dst = x”, proto 0, port 0), then
// at most maxRows non-other rows overall (the global top by bytes; the rest
// fold into their pair's other row). The meta row is emitted when the minute
// was alive or has rows. Per-pair totals are exact.
func (a *aggregator) flush(exporterID int64, m int64, topN, maxRows int) flushed {
	mm := a.minutes[m]
	delete(a.minutes, m)
	out := flushed{ifaces: map[uint32]bool{}}
	if mm == nil {
		return out
	}
	byPair := map[pairKey][]keyed{}
	for k, v := range mm.keys {
		p := pairKey{k.inIf, k.outIf}
		byPair[p] = append(byPair[p], keyed{k, v})
	}
	var kept []keyed
	for p, list := range byPair {
		slices.SortFunc(list, byBytesThenKey)
		for i, kv := range list {
			if i < topN {
				kept = append(kept, kv)
				continue
			}
			mm.otherFor(p).add(kv.v.bytes, kv.v.packets, kv.v.flows)
		}
	}
	slices.SortFunc(kept, byBytesThenKey)
	if len(kept) > maxRows {
		for _, kv := range kept[maxRows:] {
			mm.otherFor(pairKey{kv.k.inIf, kv.k.outIf}).add(kv.v.bytes, kv.v.packets, kv.v.flows)
		}
		kept = kept[:maxRows]
	}
	bucket := time.Unix(m, 0).UTC()
	note := func(inIf, outIf uint32, b int64) {
		if b <= 0 {
			return
		}
		if inIf != 0 {
			out.ifaces[inIf] = true
		}
		if outIf != 0 {
			out.ifaces[outIf] = true
		}
	}
	for _, kv := range kept {
		out.rows = append(out.rows, queries.FlowRow{ExporterID: exporterID, Bucket: bucket, InIf: kv.k.inIf,
			OutIf: kv.k.outIf, Src: keyAddr(kv.k.src), Dst: keyAddr(kv.k.dst), Proto: kv.k.proto, Port: kv.k.port,
			Bytes: kv.v.bytes, Packets: kv.v.packets, Flows: kv.v.flows})
		note(kv.k.inIf, kv.k.outIf, kv.v.bytes)
	}
	pairs := make([]pairKey, 0, len(mm.other))
	for p := range mm.other {
		pairs = append(pairs, p)
	}
	slices.SortFunc(pairs, func(x, y pairKey) int {
		if c := cmp.Compare(x.inIf, y.inIf); c != 0 {
			return c
		}
		return cmp.Compare(x.outIf, y.outIf)
	})
	for _, p := range pairs {
		v := mm.other[p]
		if v.bytes == 0 && v.packets == 0 && v.flows == 0 {
			continue
		}
		out.rows = append(out.rows, queries.FlowRow{ExporterID: exporterID, Bucket: bucket, InIf: p.inIf,
			OutIf: p.outIf, Bytes: v.bytes, Packets: v.packets, Flows: v.flows})
		note(p.inIf, p.outIf, v.bytes)
	}
	if mm.alive || len(out.rows) > 0 {
		out.meta = &queries.FlowMeta{ExporterID: exporterID, Bucket: bucket, Datagrams: mm.dgrams,
			Records: mm.records, Bytes: mm.bytes, Sampling: max(mm.sampling, 1), Overflow: mm.overflow, Late: mm.late}
	}
	return out
}
