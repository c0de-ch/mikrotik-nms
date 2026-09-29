package flow

import (
	"net/netip"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/flow/decode"
)

func TestNormalizeRules(t *testing.T) {
	c := newTestCollector(t, nil)
	ros := &expState{addr: rosAddr}
	now := tLab
	base := decode.Flow{Proto: 6, SrcAddr: netip.MustParseAddr("142.250.203.100"), DstAddr: netip.MustParseAddr("100.114.241.213"),
		SrcPort: 443, DstPort: 50514, InIf: 1, OutIf: 13, Bytes: 100, Packets: 1, SamplingRate: 1, Start: now, End: now}

	var n normCounters
	f := base
	f.PostNATDst, f.PostNATDstPort = netip.MustParseAddr("192.168.28.33"), 50515
	nf, ok := c.normalize(ros, &f, now, &n)
	if !ok || keyAddr(nf.key.dst) != netip.MustParseAddr("192.168.28.33") || nf.key.port != 443 || n.natDst != 1 {
		t.Fatalf("NAT dst: %+v %+v", nf.key, n)
	}
	for _, post := range []netip.Addr{netip.IPv4Unspecified(), {}, base.DstAddr} {
		f = base
		f.PostNATDst = post
		nf, _ = c.normalize(ros, &f, now, &n)
		if keyAddr(nf.key.dst) != base.DstAddr {
			t.Errorf("226 = %v must not replace dst", post)
		}
	}
	if n.natDst != 1 {
		t.Fatalf("nat_dst_records = %d", n.natDst)
	}
	// The post-NAT port is used only when non-zero, and never the source.
	f = base
	f.SrcPort, f.DstPort, f.PostNATDst, f.PostNATDstPort = 50514, 8443, netip.MustParseAddr("10.0.0.1"), 443
	f.PostNATSrc = netip.MustParseAddr("100.114.241.213")
	nf, _ = c.normalize(ros, &f, now, &n)
	if nf.key.port != 443 || keyAddr(nf.key.src) != base.SrcAddr {
		t.Fatalf("NAT port/src: %+v", nf.key)
	}

	// Self-export: UDP from the exporter to a listen port only.
	f = base
	f.Proto, f.SrcAddr, f.DstPort = 17, rosAddr, 2055
	if _, ok := c.normalize(ros, &f, now, &n); ok || n.selfExport != 1 {
		t.Fatal("self-export not dropped")
	}
	f.DstPort = 2056
	if _, ok := c.normalize(ros, &f, now, &n); !ok {
		t.Fatal("UDP to another port dropped")
	}
	f.Proto, f.DstPort = 6, 2055
	if _, ok := c.normalize(ros, &f, now, &n); !ok {
		t.Fatal("TCP to the listen port dropped")
	}

	// Duplicates are dropped only for kind=opnsense.
	opn := &expState{addr: opnAddr, dup: newDupFilter()}
	f = base
	if _, ok := c.normalize(opn, &f, now, &n); !ok {
		t.Fatal("first copy dropped")
	}
	if _, ok := c.normalize(opn, &f, now.Add(time.Minute), &n); ok || n.dup != 1 {
		t.Fatal("duplicate within 3 min not dropped")
	}
	if _, ok := c.normalize(opn, &f, now.Add(4*time.Minute), &n); !ok {
		t.Fatal("copy after the TTL dropped")
	}
	for i := 0; i < 3; i++ {
		if _, ok := c.normalize(ros, &f, now, &n); !ok {
			t.Fatal("routeros flow dropped as duplicate")
		}
	}
	// A flow without addresses has no endpoints: only its pair's other row.
	f = decode.Flow{Proto: 0, InIf: 3, Bytes: 64, Packets: 1, SamplingRate: 512, Start: now, End: now}
	nf, ok = c.normalize(ros, &f, now, &n)
	if !ok || nf.endpoints || nf.key.port != 0 || nf.sampling != 512 {
		t.Fatalf("no-endpoint flow: %+v", nf)
	}
}

func TestDupFilterRingEviction(t *testing.T) {
	d := newDupFilter()
	for i := uint64(0); i < dupFilterCap+10; i++ {
		d.seen(i, tLab)
	}
	if len(d.seenAt) != dupFilterCap {
		t.Fatalf("filter holds %d keys, cap %d", len(d.seenAt), dupFilterCap)
	}
	if d.seen(3, tLab) {
		t.Fatal("evicted key still reported")
	}
	if !d.seen(dupFilterCap+5, tLab) {
		t.Fatal("recent key not reported")
	}
}

func TestHintTrackerCaps(t *testing.T) {
	h := newHintTracker(tLab)
	for i := 0; i < hintPrefixCap+10; i++ {
		h.add(1, netip.AddrFrom4([4]byte{10, byte(i), 0, 1}), int64(i+1), tLab)
	}
	if len(h.cur[1]) != hintPrefixCap {
		t.Fatalf("prefixes = %d", len(h.cur[1]))
	}
	if got := h.hint(1); got != "10.63.0.0/24,10.62.0.0/24,10.61.0.0/24" {
		t.Fatalf("hint = %q", got)
	}
	for _, a := range []string{"224.0.0.1", "0.0.0.0", "fe80::1", "127.0.0.1"} {
		h.add(2, netip.MustParseAddr(a), 10, tLab)
	}
	h.add(0, netip.MustParseAddr("1.1.1.1"), 10, tLab)
	if h.hint(2) != "" || h.hint(0) != "" {
		t.Fatal("non-unicast sources or ifIndex 0 learned")
	}
	// Hourly generations: the previous hour still counts, the one before not.
	h.add(5, netip.MustParseAddr("192.168.78.10"), 10, tLab)
	h.add(5, netip.MustParseAddr("192.168.79.10"), 5, tLab.Add(61*time.Minute))
	if got := h.hint(5); got != "192.168.78.0/24,192.168.79.0/24" {
		t.Fatalf("two generations = %q", got)
	}
	h.add(5, netip.MustParseAddr("192.168.79.10"), 5, tLab.Add(122*time.Minute))
	if got := h.hint(5); got != "192.168.79.0/24" {
		t.Fatalf("after two resets = %q", got)
	}
}
