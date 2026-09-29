package flow

import (
	"bufio"
	"log"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mikrotik-nms/backend/internal/flow/decode"
)

// Reader-side limits (contract §8.13).
const (
	queueCap         = 1024    // datagrams between the readers and the worker
	pooledBufSize    = 2048    // pooled datagram copies; larger ones allocate
	maxDatagram      = 9216    // larger datagrams are rejected (exporters send <= path MTU; 9216 = jumbo)
	readBufSize      = 65535   // one read buffer per socket (so oversized datagrams are seen whole and rejected)
	socketRcvBuf     = 4 << 20 // SetReadBuffer request
	unknownRingCap   = 16      // unknown senders listed in the status
	unknownLogEvery  = 10 * time.Minute
	unknownLogBudget = 10 // unknown-sender log lines per minute, all sources
	globalRateFactor = 5  // the global token bucket is 5x the per-exporter rate
)

// tokenBucket is a mutex-protected token bucket (readers of several sockets
// may share an exporter).
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rate float64) *tokenBucket {
	return &tokenBucket{rate: rate, burst: 2 * rate, tokens: 2 * rate}
}

// setRate changes the refill rate (burst 2x) keeping the current tokens.
func (b *tokenBucket) setRate(rate float64) {
	b.mu.Lock()
	b.rate, b.burst = rate, 2*rate
	b.tokens = min(b.tokens, b.burst)
	b.mu.Unlock()
}

// take removes one token, reporting false when the bucket is empty.
func (b *tokenBucket) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.last.IsZero() {
		if dt := now.Sub(b.last).Seconds(); dt > 0 {
			b.tokens = min(b.burst, b.tokens+dt*b.rate)
		}
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// exporterGate is the reader's view of one exporter: kept across reloads
// while the exporter id and address are unchanged, so its bucket and counter
// survive.
type exporterGate struct {
	id          int64
	enabled     atomic.Bool
	bucket      *tokenBucket
	rateLimited atomic.Int64
	oversized   atomic.Int64 // datagrams > maxDatagram
}

// candidateGate is a managed device's address, accepted automatically on its
// first datagram (flow_auto_accept_devices).
type candidateGate struct {
	deviceID, name string
	bucket         *tokenBucket
}

// deviceInfo names a managed device.
type deviceInfo struct {
	id, name string
}

// allowList is the copy-on-write view the readers consult before any copy.
type allowList struct {
	exporters  map[netip.Addr]*exporterGate
	candidates map[netip.Addr]*candidateGate // only while auto-accept is on
	devices    map[netip.Addr]deviceInfo     // every managed device (unknown-sender labels)
}

// datagram is one accepted datagram on its way to the worker.
type datagram struct {
	addr       netip.Addr
	exporterID int64          // 0 for an auto-accept candidate
	candidate  *candidateGate // non-nil for a candidate
	buf        *[]byte        // pooled when cap == pooledBufSize
	n          int
	received   time.Time
}

func (d *datagram) bytes() []byte { return (*d.buf)[:d.n] }

// globalCounters are the collector-wide counters (GlobalStats), updated
// lock-free by readers, worker and writer.
type globalCounters struct {
	datagrams, unknownDatagrams, rejected, rateLimited atomic.Int64
	queueDropped, kernelDropped, writerDropped         atomic.Int64
	lateRecords, overflowRecords                       atomic.Int64
	selfExportDropped, dupDropped, panics              atomic.Int64
}

func (g *globalCounters) snapshot() GlobalStats {
	return GlobalStats{
		Datagrams: g.datagrams.Load(), UnknownDatagrams: g.unknownDatagrams.Load(), Rejected: g.rejected.Load(),
		RateLimited: g.rateLimited.Load(), QueueDropped: g.queueDropped.Load(), KernelDropped: g.kernelDropped.Load(),
		WriterDropped: g.writerDropped.Load(), LateRecords: g.lateRecords.Load(),
		OverflowRecords: g.overflowRecords.Load(), SelfExportDropped: g.selfExportDropped.Load(),
		DupDropped: g.dupDropped.Load(), Panics: g.panics.Load(),
	}
}

// handlePacket is the reader's per-datagram path: allow-list, size cap, rate
// limit, then (only then) a copy into a pooled buffer and a non-blocking
// enqueue. b is the socket's read buffer and is not retained. The size cap
// bounds the queue at queueCap x maxDatagram (~9 MB) and keeps kernel-
// reassembled 64 KB datagrams away from the decoder.
func (c *Collector) handlePacket(b []byte, addr netip.Addr, now time.Time) {
	c.global.datagrams.Add(1)
	kind := decode.Sniff(b)
	al := c.allow.Load()
	if al == nil {
		al = &allowList{}
	}
	if g := al.exporters[addr]; g != nil {
		if !g.enabled.Load() || kind == decode.KindUnknown || len(b) < 4 {
			c.global.rejected.Add(1)
			return
		}
		if len(b) > maxDatagram {
			g.oversized.Add(1)
			c.global.rejected.Add(1)
			return
		}
		if !g.bucket.take(now) || !c.globalBucket.take(now) {
			g.rateLimited.Add(1)
			c.global.rateLimited.Add(1)
			return
		}
		c.enqueue(datagram{addr: addr, exporterID: g.id, received: now}, b)
		return
	}
	if cg := al.candidates[addr]; cg != nil && kind != decode.KindUnknown && len(b) >= 4 {
		if len(b) > maxDatagram {
			c.global.rejected.Add(1)
			return
		}
		if !cg.bucket.take(now) || !c.globalBucket.take(now) {
			c.global.rateLimited.Add(1)
			return
		}
		c.enqueue(datagram{addr: addr, candidate: cg, received: now}, b)
		return
	}
	c.global.unknownDatagrams.Add(1)
	dev, isDev := al.devices[addr]
	c.unknown.record(addr, kind, now, dev, isDev)
}

// enqueue copies b and hands it to the worker without blocking; a full queue
// drops the datagram (queue_dropped).
func (c *Collector) enqueue(d datagram, b []byte) {
	if len(b) <= pooledBufSize {
		d.buf = c.bufPool.Get().(*[]byte)
	} else {
		buf := make([]byte, len(b))
		d.buf = &buf
	}
	d.n = copy((*d.buf)[:cap(*d.buf)], b)
	select {
	case c.queue <- d:
	default:
		c.global.queueDropped.Add(1)
		c.recycle(&d)
	}
}

// recycle returns a pooled buffer.
func (c *Collector) recycle(d *datagram) {
	if d.buf != nil && cap(*d.buf) == pooledBufSize {
		c.bufPool.Put(d.buf)
	}
	d.buf = nil
}

// unknownSender is one ring entry.
type unknownSender struct {
	addr                netip.Addr
	firstSeen, lastSeen time.Time
	datagrams           int64
	protocol            string
	device              deviceInfo
	isDevice            bool
	lastLog             time.Time
}

// unknownRing remembers the last unknownRingCap senders (LRU by address)
// and rate-limits their log lines.
type unknownRing struct {
	mu        sync.Mutex
	entries   []*unknownSender // most recently seen first
	logWindow time.Time
	logCount  int
}

func (r *unknownRing) record(addr netip.Addr, kind decode.Kind, now time.Time, dev deviceInfo, isDev bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var e *unknownSender
	for i, x := range r.entries {
		if x.addr == addr {
			e = x
			r.entries = slices.Delete(r.entries, i, i+1)
			break
		}
	}
	if e == nil {
		e = &unknownSender{addr: addr, firstSeen: now}
		if len(r.entries) >= unknownRingCap {
			r.entries = r.entries[:unknownRingCap-1]
		}
	}
	e.lastSeen = now
	e.datagrams++
	if p := kind.String(); p != "" {
		e.protocol = p
	}
	e.device, e.isDevice = dev, isDev
	r.entries = slices.Insert(r.entries, 0, e)

	if now.Sub(e.lastLog) < unknownLogEvery {
		return
	}
	if now.Sub(r.logWindow) >= time.Minute {
		r.logWindow, r.logCount = now, 0
	}
	if r.logCount >= unknownLogBudget {
		return
	}
	r.logCount++
	e.lastLog = now
	proto := e.protocol
	if proto == "" {
		proto = "unrecognised"
	}
	log.Printf("flow collector: datagrams from unknown sender %s (%s) — add it as an exporter to accept", addr, proto)
}

// snapshot returns the ring newest first.
func (r *unknownRing) snapshot() []UnknownSender {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]UnknownSender, 0, len(r.entries))
	for _, e := range r.entries {
		u := UnknownSender{Address: e.addr.String(), FirstSeen: e.firstSeen.UTC(), LastSeen: e.lastSeen.UTC(),
			Datagrams: e.datagrams, Protocol: e.protocol}
		if e.isDevice {
			id := e.device.id
			u.DeviceID, u.DeviceName = &id, e.device.name
		}
		out = append(out, u)
	}
	return out
}

// forget drops an address from the ring (it became an exporter).
func (r *unknownRing) forget(addr netip.Addr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = slices.DeleteFunc(r.entries, func(e *unknownSender) bool { return e.addr == addr })
}

// kernelDrops sums the drops column of /proc/self/net/udp and udp6 for the
// given local ports (the sockets' receive-buffer overflows).
func kernelDrops(ports map[uint16]bool) (int64, bool) {
	if len(ports) == 0 {
		return 0, false
	}
	var total int64
	found := false
	for _, path := range []string{"/proc/self/net/udp", "/proc/self/net/udp6"} {
		n, ok := procUDPDrops(path, ports)
		total += n
		found = found || ok
	}
	return total, found
}

func procUDPDrops(path string, ports map[uint16]bool) (int64, bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return parseProcUDP(bufio.NewScanner(f), ports)
}

// parseProcUDP reads a /proc/net/udp table: local_address is "HEXIP:HEXPORT"
// (field 2) and drops is the last field.
func parseProcUDP(sc *bufio.Scanner, ports map[uint16]bool) (int64, bool) {
	var total int64
	found := false
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // header
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 13 {
			continue
		}
		_, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		port, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil || !ports[uint16(port)] {
			continue
		}
		drops, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err != nil {
			continue
		}
		total += drops
		found = true
	}
	return total, found
}
