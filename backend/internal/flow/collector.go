package flow

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"maps"
	"net"
	"net/netip"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// writerWait bounds how long shutdown waits for the writer to drain.
const writerWait = 5 * time.Second

// Collector receives flow datagrams on the configured UDP sockets and turns
// them into flow_1m / flow_1h rows. Build it with New and start it with Run;
// every exported method is safe on a nil *Collector.
//
// Goroutines: one reader per socket (allow-list, rate limit, copy, enqueue),
// ONE worker that owns every decoder and aggregation map (no locks on the
// hot path), one writer for SQLite, and one maintenance loop (ifIndex sync,
// address plan, point facing) that talks to devices.
type Collector struct {
	db   *sql.DB
	pool *routeros.Pool
	hub  *ws.Hub
	cfg  Config

	listenPorts map[uint16]bool // configured listen ports (self-export drop)
	now         func() time.Time

	// Reader side.
	allow        atomic.Pointer[allowList]
	queue        chan datagram
	bufPool      sync.Pool
	global       globalCounters
	unknown      unknownRing
	globalBucket *tokenBucket
	boundPorts   map[uint16]bool // ports actually bound (kernel drops)

	// Worker-owned state (the worker goroutine only; tests drive it directly).
	exps      map[int64]*expState
	rows      []queries.FlowExporter
	devices   map[netip.Addr]deviceInfo
	natives   map[pointKey]bool
	ifaces    map[int64]map[uint32]queries.FlowIface
	settings  runtimeSettings
	nextFlush int64
	acceptLog map[netip.Addr]*logThrottle // auto-accept failures, per address

	// Writer-owned.
	pointLog map[int64]*logThrottle // new observation points, per exporter

	// Writer.
	writeCh    chan writeOp
	syncWrites bool
	drainBy    time.Time // shutdown: submit may wait until then

	// Maintenance loop.
	ifsyncReq chan int64

	// Status snapshot, rebuilt by the worker.
	statusMu     sync.Mutex
	status       Status
	startedAt    time.Time
	listenErrors []string
	rcvBuf       int

	flushedThrough atomic.Int64 // unix seconds, 0 = never
	rolledThrough  atomic.Int64 // unix seconds, 0 = never
	reload         chan struct{}

	started atomic.Bool   // Run began (enabled)
	done    chan struct{} // closed when Run has finished its shutdown
}

// New builds a collector. With an empty cfg.Listen it stays disabled: Run
// returns at once and Status reports enabled=false.
func New(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, cfg Config) *Collector {
	cfg.Listen = slices.Clone(cfg.Listen)
	c := &Collector{
		db: db, pool: pool, hub: hub, cfg: cfg,
		listenPorts:  map[uint16]bool{},
		now:          time.Now,
		queue:        make(chan datagram, queueCap),
		globalBucket: newTokenBucket(float64(globalRateFactor * 2000)),
		boundPorts:   map[uint16]bool{},
		exps:         map[int64]*expState{},
		devices:      map[netip.Addr]deviceInfo{},
		natives:      map[pointKey]bool{},
		ifaces:       map[int64]map[uint32]queries.FlowIface{},
		acceptLog:    map[netip.Addr]*logThrottle{},
		pointLog:     map[int64]*logThrottle{},
		settings:     runtimeSettings{topN: 50, maxRows: 1500, pps: 2000, autoAccept: true},
		writeCh:      make(chan writeOp, writerCap),
		ifsyncReq:    make(chan int64, 16),
		reload:       make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
	c.bufPool.New = func() any {
		b := make([]byte, pooledBufSize)
		return &b
	}
	for _, l := range cfg.Listen {
		if p := parseListenPort(l); p > 0 {
			c.listenPorts[uint16(p)] = true
		}
	}
	c.status = disabledStatus()
	c.status.Enabled = len(cfg.Listen) > 0
	c.status.Listen = slices.Clone(cfg.Listen)
	if c.status.Listen == nil {
		c.status.Listen = []string{}
	}
	return c
}

// Enabled reports whether a listen address is configured (nil-safe). It
// reflects the configuration, not whether the sockets could be bound.
func (c *Collector) Enabled() bool {
	return c != nil && len(c.cfg.Listen) > 0
}

// listen binds one UDP socket per listen entry; failures are logged and
// reported in the status, the other entries still bind.
func (c *Collector) listen() []*net.UDPConn {
	var conns []*net.UDPConn
	for _, entry := range c.cfg.Listen {
		addr, err := net.ResolveUDPAddr("udp", entry)
		var conn *net.UDPConn
		if err == nil {
			conn, err = net.ListenUDP("udp", addr)
		}
		if err != nil {
			log.Printf("flow collector: listen %s: %v", entry, err)
			c.listenErrors = append(c.listenErrors, entry+": "+err.Error())
			continue
		}
		if err := conn.SetReadBuffer(socketRcvBuf); err != nil {
			log.Printf("flow collector: %s: set receive buffer: %v", entry, err)
		}
		if n := socketRcvBufSize(conn); n > c.rcvBuf {
			c.rcvBuf = n
		}
		if la, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			c.boundPorts[uint16(la.Port)] = true
		}
		log.Printf("flow collector: listening on udp %s (receive buffer %d bytes)", conn.LocalAddr(), c.rcvBuf)
		conns = append(conns, conn)
	}
	return conns
}

// readLoop is one socket's reader.
func (c *Collector) readLoop(conn *net.UDPConn) {
	buf := make([]byte, readBufSize)
	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond) // transient (e.g. ICMP-induced) errors: never spin
			continue
		}
		c.handlePacket(buf[:n], src.Addr().Unmap(), c.now())
	}
}

// Run blocks until ctx is done; it returns at once when the collector is
// disabled. On shutdown it closes the sockets, drains the queue, flushes
// every open minute, persists the exporter status and waits up to 5 s for
// the writer.
func (c *Collector) Run(ctx context.Context) {
	if !c.Enabled() || !c.started.CompareAndSwap(false, true) {
		return
	}
	defer close(c.done)
	now := c.now()
	c.startedAt = now
	if c.cfg.CaptureDir != "" {
		if err := os.MkdirAll(c.cfg.CaptureDir, 0o750); err != nil {
			log.Printf("flow collector: capture dir: %v", err)
		}
	}
	conns := c.listen()
	c.guard("reconcile", func() { c.reconcile(now) })
	c.nextFlush = floorMinute(now.Unix()) - int64(maxOpenMinutes-1)*60
	c.rebuildStatus(now)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		c.writerLoop()
	}()
	c.submit(writeOp{startupRollup: now})

	maintCtx, stopMaint := context.WithCancel(ctx)
	defer stopMaint()
	go c.maintLoop(maintCtx)

	var readers sync.WaitGroup
	for _, conn := range conns {
		readers.Add(1)
		go func(conn *net.UDPConn) {
			defer readers.Done()
			c.readLoop(conn)
		}(conn)
	}

	c.workerLoop(ctx)

	for _, conn := range conns {
		_ = conn.Close()
	}
	readers.Wait()
	c.drainBy = time.Now().Add(writerWait)
	c.guard("shutdown", func() {
		c.drainQueue()
		end := c.now()
		c.flushAll(end)
		c.persistStatus()
	})
	close(c.writeCh)
	select {
	case <-writerDone:
	case <-time.After(max(time.Until(c.drainBy), 0)):
		log.Println("flow collector: writer did not finish within 5 s")
	}
	log.Println("flow collector: stopped")
}

// Wait blocks until a started collector has finished its shutdown (after
// Run's context is cancelled) or timeout elapses, and reports whether it
// finished. It returns true at once when Run was never started (nil-safe).
// main calls it after stopping the pollers so the final flush completes
// before the database is closed.
func (c *Collector) Wait(timeout time.Duration) bool {
	if c == nil || !c.started.Load() {
		return true
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-c.done:
		return true
	case <-t.C:
		return false
	}
}

// Status returns a deep copy of the collector's health (nil-safe, no DB I/O).
func (c *Collector) Status() Status {
	if !c.Enabled() {
		return disabledStatus()
	}
	c.statusMu.Lock()
	s := c.status
	s.Listen = slices.Clone(s.Listen)
	s.ListenErrors = slices.Clone(s.ListenErrors)
	if s.StartedAt != nil {
		t := *s.StartedAt
		s.StartedAt = &t
	}
	s.Exporters = make([]ExporterStatus, len(c.status.Exporters))
	for i, e := range c.status.Exporters {
		e.NATAddresses = slices.Clone(e.NATAddresses)
		e.Counters = maps.Clone(e.Counters)
		e.TimeSource = maps.Clone(e.TimeSource)
		if e.DeviceID != nil {
			d := *e.DeviceID
			e.DeviceID = &d
		}
		if e.LastSeen != nil {
			t := *e.LastSeen
			e.LastSeen = &t
		}
		if e.LastErrorAt != nil {
			t := *e.LastErrorAt
			e.LastErrorAt = &t
		}
		s.Exporters[i] = e
	}
	c.statusMu.Unlock()
	if s.Listen == nil {
		s.Listen = []string{}
	}
	if s.ListenErrors == nil {
		s.ListenErrors = []string{}
	}
	s.Global = c.global.snapshot()
	s.FlushedThrough = timePtr(c.FlushedThrough())
	s.RolledThrough = timePtr(c.RolledThrough())
	s.Unknown = c.unknown.snapshot()
	return s
}

func unixTimeOrZero(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// FlushedThrough is the exclusive end of the newest flushed minute; zero
// before the first flush (nil-safe).
func (c *Collector) FlushedThrough() time.Time {
	if c == nil {
		return time.Time{}
	}
	return unixTimeOrZero(c.flushedThrough.Load())
}

// RolledThrough is the exclusive end of the newest hour rolled into flow_1h;
// zero before the first rollup (nil-safe).
func (c *Collector) RolledThrough() time.Time {
	if c == nil {
		return time.Time{}
	}
	return unixTimeOrZero(c.rolledThrough.Load())
}

// Reload asks the collector to re-read exporters, points and settings now
// (nil-safe, non-blocking).
func (c *Collector) Reload() {
	if c == nil {
		return
	}
	select {
	case c.reload <- struct{}{}:
	default:
	}
}
