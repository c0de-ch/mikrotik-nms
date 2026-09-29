package flow

import (
	"bufio"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

func TestUnknownRingLRU(t *testing.T) {
	c := newTestCollector(t, testDB(t))
	c.reconcile(tLab)
	v9 := pktgen.OPNsenseV9Templates(1, 2, 3)
	for i := 1; i <= 17; i++ {
		c.handlePacket(v9, netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}), tLab.Add(time.Duration(i)*time.Second))
	}
	c.handlePacket([]byte("garbage!"), netip.MustParseAddr("10.0.0.5"), tLab.Add(time.Minute))
	u := c.Status().Unknown
	if len(u) != unknownRingCap {
		t.Fatalf("ring = %d entries, want %d", len(u), unknownRingCap)
	}
	if u[0].Address != "10.0.0.5" || u[0].Datagrams != 2 || u[0].Protocol != "netflow9" || u[1].Address != "10.0.0.17" {
		t.Fatalf("newest first / LRU refresh: %+v %+v", u[0], u[1])
	}
	for _, e := range u {
		if e.Address == "10.0.0.1" {
			t.Fatal("least recently seen sender not evicted")
		}
	}
	if len(c.queue) != 0 || c.global.unknownDatagrams.Load() != 18 {
		t.Fatalf("unknown senders must never be queued: queue %d unknown %d", len(c.queue), c.global.unknownDatagrams.Load())
	}
	// Once it becomes an exporter it leaves the ring.
	seedExporter(t, c.db, queries.FlowExporter{Name: "x", Address: "10.0.0.5", Kind: "other"})
	c.reconcile(tLab)
	for _, e := range c.Status().Unknown {
		if e.Address == "10.0.0.5" {
			t.Fatal("new exporter still listed as unknown")
		}
	}
}

func TestRateLimitAndQueueFull(t *testing.T) {
	db := testDB(t)
	seedExporter(t, db, queries.FlowExporter{Name: "fw", Address: opnAddr.String(), Kind: "opnsense"})
	c := newTestCollector(t, db)
	c.reconcile(tLab)
	pkt := pktgen.OPNsenseV9Templates(1, 2, 3)
	g := c.allow.Load().exporters[opnAddr]
	g.bucket = newTokenBucket(1) // burst 2
	for i := 0; i < 3; i++ {
		c.handlePacket(pkt, opnAddr, tLab)
	}
	if len(c.queue) != 2 || g.rateLimited.Load() != 1 || c.global.rateLimited.Load() != 1 {
		t.Fatalf("rate limit: queued %d limited %d/%d", len(c.queue), g.rateLimited.Load(), c.global.rateLimited.Load())
	}
	c.handlePacket(pkt, opnAddr, tLab.Add(time.Second)) // refilled 1 token
	if len(c.queue) != 3 {
		t.Fatalf("bucket did not refill: %d", len(c.queue))
	}
	c.drainQueue()

	g.bucket = newTokenBucket(1e6)
	for i := 0; i < queueCap+3; i++ {
		c.handlePacket(pkt, opnAddr, tLab)
	}
	if len(c.queue) != queueCap || c.global.queueDropped.Load() != 3 {
		t.Fatalf("queue: %d queued, %d dropped", len(c.queue), c.global.queueDropped.Load())
	}
	c.drainQueue()
	// Unrecognised versions from an exporter are rejected before the queue.
	c.handlePacket([]byte{0x45, 0, 0, 0, 1, 2, 3, 4}, opnAddr, tLab)
	if len(c.queue) != 0 || c.global.rejected.Load() != 1 {
		t.Fatalf("bad version: queued %d rejected %d", len(c.queue), c.global.rejected.Load())
	}
	// Datagrams larger than a pooled buffer are copied whole.
	big := append(pktgen.OPNsenseV9Templates(1, 2, 3), make([]byte, 3000)...)
	c.handlePacket(big, opnAddr, tLab)
	d := <-c.queue
	if d.n != len(big) || cap(*d.buf) == pooledBufSize {
		t.Fatalf("big datagram copy: n %d cap %d", d.n, cap(*d.buf))
	}
}

func TestTokenBucket(t *testing.T) {
	b := newTokenBucket(10) // burst 20
	n := 0
	for i := 0; i < 100; i++ {
		if b.take(tLab) {
			n++
		}
	}
	if n != 20 {
		t.Fatalf("burst = %d, want 20", n)
	}
	if !b.take(tLab.Add(100*time.Millisecond)) || b.take(tLab.Add(100*time.Millisecond)) {
		t.Fatal("refill of 1 token per 100 ms expected")
	}
	b.setRate(1)
	if b.burst != 2 || b.tokens > 2 {
		t.Fatalf("setRate: burst %v tokens %v", b.burst, b.tokens)
	}
}

func TestParseProcUDP(t *testing.T) {
	table := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
 1234: 00000000:0807 00000000:0000 07 00000000:00000000 00:00000000 00000000   998        0 123456 2 0000000000000000 17
 1235: 0100007F:2F17 00000000:0000 07 00000000:00000000 00:00000000 00000000   998        0 123457 2 0000000000000000 5
 1236: 00000000:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 123458 2 0000000000000000 99
`
	n, ok := parseProcUDP(bufio.NewScanner(strings.NewReader(table)), map[uint16]bool{2055: true, 12055: true})
	if !ok || n != 22 {
		t.Fatalf("drops = %d, %v; want 22 (ports 0x807, 0x2F17)", n, ok)
	}
	if _, ok := kernelDrops(nil); ok {
		t.Fatal("no ports: nothing to report")
	}
}

func TestIfacesFromStats(t *testing.T) {
	stats := []routeros.InterfaceStats{
		{ID: "*1", Name: "eth1-wan", Type: "ether"}, {ID: "*A", Name: "bridge", Type: "bridge"},
		{ID: "*D", Name: "net28", Type: "vlan"}, {ID: "*F", Name: "wg-labnet", Type: "wg"},
		{ID: "*3ED", Name: "cap-wifi1", Type: "cap"}, {ID: "bogus", Name: "x"}, {ID: "*0", Name: "y"},
	}
	vlans := map[string]routeros.VLANInfo{"net28": {Name: "net28", VLANID: 28, Parent: "bridge"}}
	got := ifacesFromStats(stats, vlans)
	if len(got) != 5 {
		t.Fatalf("ifaces = %+v", got)
	}
	want := map[uint32]string{1: "eth1-wan", 10: "bridge", 13: "net28", 15: "wg-labnet", 1005: "cap-wifi1"}
	for _, f := range got {
		if want[f.IfIndex] != f.Name {
			t.Errorf("ifIndex %d = %q", f.IfIndex, f.Name)
		}
		if f.IfIndex == 13 && (f.VLANID != 28 || f.Parent != "bridge") {
			t.Errorf("net28 vlan = %+v", f)
		}
	}
}
