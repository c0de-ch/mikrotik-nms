package flow

import (
	"cmp"
	"encoding/binary"
	"hash/fnv"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/flow/decode"
	"github.com/mikrotik-nms/backend/internal/flow/svcport"
)

// Normalisation limits.
const (
	dupFilterCap     = 32 * 1024 // keys per exporter
	dupFilterTTL     = 3 * time.Minute
	hintPrefixCap    = 64  // source prefixes tracked per ingress interface
	hintIfaceCap     = 256 // ingress interfaces tracked per exporter
	hintTopK         = 3
	hintGenerationLn = time.Hour
)

// normCounters are what normalising one flow may count.
type normCounters struct {
	selfExport, dup, natDst int64
}

// normalize applies, in order: the self-export drop, the OPNsense duplicate
// filter, the NAT inside view and the service-port choice. ok is false when
// the flow is dropped.
func (c *Collector) normalize(st *expState, f *decode.Flow, now time.Time, n *normCounters) (normFlow, bool) {
	// 1. The exporter's own export datagrams (RouterOS interfaces=all,
	//    samplicate on OPNsense LAN egress).
	if f.Proto == 17 && f.SrcAddr == st.addr && c.listenPorts[f.DstPort] {
		n.selfExport++
		return normFlow{}, false
	}
	// 2. OPNsense captures a packet on every listening interface it crosses.
	if st.dup != nil && st.dup.seen(dupHash(f), now) {
		n.dup++
		return normFlow{}, false
	}
	// 3. NAT: store the inside view (RouterOS downloads carry the CGNAT
	//    address in dst and the internal host in IE 226).
	dst, dport := f.DstAddr, f.DstPort
	if f.PostNATDst.IsValid() && !f.PostNATDst.IsUnspecified() && f.PostNATDst != f.DstAddr {
		dst = f.PostNATDst
		if f.PostNATDstPort != 0 {
			dport = f.PostNATDstPort
		}
		n.natDst++
	}
	// 4. Service port (ephemeral client ports never become keys).
	port := svcport.Select(f.Proto, f.SrcPort, dport)

	nf := normFlow{
		key:      aggKey{inIf: f.InIf, outIf: f.OutIf, proto: f.Proto, port: port},
		bytes:    clampInt64(f.Bytes),
		packets:  clampInt64(f.Packets),
		sampling: int64(max(f.SamplingRate, 1)),
		start:    f.Start,
		end:      f.End,
	}
	src := f.SrcAddr.Unmap()
	dst = dst.Unmap()
	if src.IsValid() && dst.IsValid() {
		nf.endpoints = true
		nf.key.src, nf.key.dst = src.As16(), dst.As16()
	} else {
		nf.key.proto, nf.key.port = 0, 0 // no endpoints: only the pair's other row
	}
	return nf, true
}

func clampInt64(v uint64) int64 {
	if v > maxFlowValue {
		return maxFlowValue
	}
	return int64(v)
}

// dupHash is FNV-64 of (src, dst, sport, dport, proto, in_if, out_if, start,
// end, raw bytes, raw packets). start/end are the exporter-relative raw
// FIRST/LAST_SWITCHED when the record has them: every ng_netflow node of one
// OPNsense shares that uptime, while the converted times depend on each
// node's header unix/uptime phase (1 s resolution each) and differ by a
// second between two nodes' copies of the same flow about half the time.
func dupHash(f *decode.Flow) uint64 {
	h := fnv.New64a()
	var b [8]byte
	s, d := f.SrcAddr.As16(), f.DstAddr.As16()
	h.Write(s[:])
	h.Write(d[:])
	binary.BigEndian.PutUint16(b[:2], f.SrcPort)
	binary.BigEndian.PutUint16(b[2:4], f.DstPort)
	b[4] = f.Proto
	h.Write(b[:5])
	binary.BigEndian.PutUint32(b[:4], f.InIf)
	binary.BigEndian.PutUint32(b[4:], f.OutIf)
	h.Write(b[:])
	start, end := uint64(f.Start.UnixMilli()), uint64(f.End.UnixMilli())
	if f.RawStart != 0 || f.RawEnd != 0 {
		start, end = f.RawStart, f.RawEnd
	}
	for _, v := range []uint64{start, end, f.RawBytes, f.RawPackets} {
		binary.BigEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	return h.Sum64()
}

type dupEntry struct {
	at  int64 // unix ms
	idx int   // ring slot
}

// dupFilter remembers the last dupFilterCap flow hashes for dupFilterTTL
// (ring + map, bounded).
type dupFilter struct {
	seenAt map[uint64]dupEntry
	ring   []uint64
	pos    int
	full   bool
}

func newDupFilter() *dupFilter {
	return &dupFilter{seenAt: make(map[uint64]dupEntry), ring: make([]uint64, dupFilterCap)}
}

// seen reports whether h was recorded within the TTL, recording it if not.
func (d *dupFilter) seen(h uint64, now time.Time) bool {
	ms := now.UnixMilli()
	if e, ok := d.seenAt[h]; ok && ms-e.at < dupFilterTTL.Milliseconds() {
		return true
	}
	if d.full {
		old := d.ring[d.pos]
		if e, ok := d.seenAt[old]; ok && e.idx == d.pos {
			delete(d.seenAt, old)
		}
	}
	d.ring[d.pos] = h
	d.seenAt[h] = dupEntry{at: ms, idx: d.pos}
	d.pos++
	if d.pos == len(d.ring) {
		d.pos, d.full = 0, true
	}
	return false
}

// hintTracker learns, per ingress interface of an exporter without a device,
// the top source prefixes by bytes (v4 /24, v6 /64): the hint that names an
// interface ("192.168.78.0/24,192.168.79.0/24" is the LAN). Two hourly
// generations keep the hint stable across the reset.
type hintTracker struct {
	cur, prev map[uint32]map[netip.Prefix]int64
	genStart  time.Time
}

func newHintTracker(now time.Time) *hintTracker {
	return &hintTracker{cur: map[uint32]map[netip.Prefix]int64{}, genStart: now}
}

func hintPrefix(a netip.Addr) (netip.Prefix, bool) {
	a = a.Unmap()
	if !a.IsValid() || a.IsUnspecified() || a.IsMulticast() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return netip.Prefix{}, false
	}
	bitsLen := 24
	if a.Is6() {
		bitsLen = 64
	}
	p, err := a.Prefix(bitsLen)
	return p, err == nil
}

func (h *hintTracker) add(inIf uint32, src netip.Addr, bytes int64, now time.Time) {
	if inIf == 0 || bytes <= 0 {
		return
	}
	if now.Sub(h.genStart) >= hintGenerationLn {
		h.prev, h.cur, h.genStart = h.cur, map[uint32]map[netip.Prefix]int64{}, now
	}
	p, ok := hintPrefix(src)
	if !ok {
		return
	}
	m := h.cur[inIf]
	if m == nil {
		if len(h.cur) >= hintIfaceCap {
			return
		}
		m = map[netip.Prefix]int64{}
		h.cur[inIf] = m
	}
	if _, ok := m[p]; !ok && len(m) >= hintPrefixCap {
		return
	}
	m[p] = satAdd(m[p], bytes)
}

// hint returns the CSV of the interface's top source prefixes (bytes
// descending, then prefix), "" when nothing was learned.
func (h *hintTracker) hint(inIf uint32) string {
	sum := map[netip.Prefix]int64{}
	for _, gen := range []map[uint32]map[netip.Prefix]int64{h.prev, h.cur} {
		for p, b := range gen[inIf] {
			sum[p] = satAdd(sum[p], b)
		}
	}
	if len(sum) == 0 {
		return ""
	}
	ps := make([]netip.Prefix, 0, len(sum))
	for p := range sum {
		ps = append(ps, p)
	}
	slices.SortFunc(ps, func(a, b netip.Prefix) int {
		if c := cmp.Compare(sum[b], sum[a]); c != 0 {
			return c
		}
		return cmp.Compare(a.String(), b.String())
	})
	if len(ps) > hintTopK {
		ps = ps[:hintTopK]
	}
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

// ifaces returns the ingress interfaces with learned prefixes.
func (h *hintTracker) ifaces() []uint32 {
	seen := map[uint32]bool{}
	for _, gen := range []map[uint32]map[netip.Prefix]int64{h.prev, h.cur} {
		for i := range gen {
			seen[i] = true
		}
	}
	out := make([]uint32, 0, len(seen))
	for i := range seen {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}
