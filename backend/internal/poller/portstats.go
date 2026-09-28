package poller

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// PortRatesTopic is the WS topic carrying the fleet-wide port-rate snapshot,
// published once per collector cycle while anyone is subscribed.
const PortRatesTopic = "traffic.ports"

const (
	// defaultPortStatsInterval / defaultPortHostsInterval apply when the
	// port_stats_interval / port_hosts_interval settings are unset or garbage.
	defaultPortStatsInterval = 15 * time.Second
	defaultPortHostsInterval = 300 * time.Second

	// portStatsMaxWait caps how long a cycle waits for device replies. Slower
	// replies still land in the state when they arrive (see pollDevice).
	portStatsMaxWait = 10 * time.Second
	// portStatsStartupDelay gives the info loop time to dial the pool first;
	// until then GetLive has no dial parameters and returns nil.
	portStatsStartupDelay = 10 * time.Second
	// portHostsStartupDelay holds the first FDB dump back after startup.
	portHostsStartupDelay = 30 * time.Second
	// portHostsRetryDelay is how soon devices skipped for want of a pooled
	// connection are retried within one port_hosts_interval. On a cold start
	// the pool has dialled only a few devices by the first pass; without the
	// retry most of the fleet would have no FDB rows (so no access/wireless
	// roles) for a whole interval.
	portHostsRetryDelay = 30 * time.Second
	// portStatsStartupRollback is how many complete hours a startup always
	// re-rolls into port_stats_1h.
	portStatsStartupRollback = 3 * time.Hour
	// portStatsMaxRollback bounds every re-roll of port_stats_1h. Never look
	// back further: an older hour may only be partially present in
	// port_stats_1m and would be overwritten with partial data. The sweep
	// keeps at least one day of 1m rows (port_stats_1m_days >= 1), so every
	// hour starting at or after floorHour(now)-23h is complete.
	portStatsMaxRollback = 23 * time.Hour
	// portStatsLogEvery rate-limits repeated per-device error logs.
	portStatsLogEvery = 10 * time.Minute
)

// errNoClient marks a device without a usable pooled connection this cycle
// (skipped silently; the pool logs its own redial failures).
var errNoClient = errors.New("no pooled connection")

// PortRate is one interface's latest computed rates (bits/s, packets/s).
type PortRate struct {
	DeviceID string `json:"device_id"`
	Iface    string `json:"iface"`
	Type     string `json:"type"`
	Comment  string `json:"comment,omitempty"`
	Running  bool   `json:"running"`
	Disabled bool   `json:"disabled"`
	RxBps    int64  `json:"rx_bps"`
	TxBps    int64  `json:"tx_bps"`
	RxPps    int64  `json:"rx_pps"`
	TxPps    int64  `json:"tx_pps"`
}

// PortSnapshot is the fleet-wide view published on PortRatesTopic and served
// by the API.
type PortSnapshot struct {
	TS              *time.Time `json:"ts"`               // end of the last completed cycle (UTC); nil before the first cycle
	IntervalSeconds int        `json:"interval_seconds"` // current port_stats_interval
	Ready           bool       `json:"ready"`            // true once any port has a computed rate (≥2 samples)
	Ports           []PortRate `json:"ports"`            // never nil; sorted by device_id, then iface
}

// portStatsKey identifies one interface of one device.
type portStatsKey struct {
	device, iface string
}

// portCounterSample is the previous counter reading of an interface plus the
// rates last derived from it.
type portCounterSample struct {
	at                   time.Time
	hasCounters          bool
	rxBytes, txBytes     int64
	rxPackets, txPackets int64
	rate                 PortRate
}

// minuteAcc accumulates, for one interface, the share of every usable poll
// interval that overlaps one minute (see accumulate).
type minuteAcc struct {
	rxBytes, txBytes     int64
	rxPackets, txPackets int64
	seconds              float64 // Σ overlap of the accumulated intervals with this minute
	rxMax, txMax         int64   // highest rate of any interval overlapping this minute
	samples              int64   // intervals that ENDED in this minute
}

// PortStatsCollector polls /interface/print counters on every online device
// each port_stats_interval, turns counter deltas into rates, keeps the latest
// fleet snapshot in memory, and writes 1-minute buckets (port_stats_1m) plus
// hourly rollups (port_stats_1h). A second loop dumps each device's bridge FDB
// into port_hosts, and its interface stacking (bond members, VLAN / PPPoE
// carriers) into interface_relations, every port_hosts_interval.
//
// Devices are polled concurrently on their pooled clients (both prints are
// short); a device whose previous poll has not returned yet is skipped, so a
// hung device never piles up goroutines.
type PortStatsCollector struct {
	db   *sql.DB
	pool *routeros.Pool
	hub  *ws.Hub

	// Test seams: the device reads and the clock.
	fetchStats     func(deviceID string) ([]routeros.InterfaceStats, error)
	fetchHosts     func(deviceID string) ([]routeros.BridgeHost, error)
	fetchRelations func(deviceID string) ([]routeros.InterfaceRelation, error)
	now            func() time.Time

	flushMu sync.Mutex // serializes flush (DB writes + rollup)

	mu             sync.Mutex
	interval       time.Duration
	lastCycle      time.Time
	ready          bool
	samples        map[portStatsKey]*portCounterSample
	minutes        map[int64]map[portStatsKey]*minuteAcc // keyed by minute start, Unix seconds
	inFlight       map[string]bool
	flushedThrough time.Time
	rolledThrough  time.Time
	lastLog        map[string]time.Time
}

// NewPortStatsCollector wires the collector; Run starts it.
func NewPortStatsCollector(db *sql.DB, pool *routeros.Pool, hub *ws.Hub) *PortStatsCollector {
	c := &PortStatsCollector{
		db:       db,
		pool:     pool,
		hub:      hub,
		now:      time.Now,
		interval: defaultPortStatsInterval,
		samples:  make(map[portStatsKey]*portCounterSample),
		minutes:  make(map[int64]map[portStatsKey]*minuteAcc),
		inFlight: make(map[string]bool),
		lastLog:  make(map[string]time.Time),
	}
	c.fetchStats = c.poolStats
	c.fetchHosts = c.poolHosts
	c.fetchRelations = c.poolRelations
	return c
}

func (c *PortStatsCollector) poolRelations(deviceID string) ([]routeros.InterfaceRelation, error) {
	client := c.pool.GetLive(deviceID)
	if client == nil {
		return nil, errNoClient
	}
	return routeros.GetInterfaceRelations(client)
}

func (c *PortStatsCollector) poolStats(deviceID string) ([]routeros.InterfaceStats, error) {
	client := c.pool.GetLive(deviceID)
	if client == nil {
		return nil, errNoClient
	}
	return routeros.GetInterfaceStats(client)
}

func (c *PortStatsCollector) poolHosts(deviceID string) ([]routeros.BridgeHost, error) {
	client := c.pool.GetLive(deviceID)
	if client == nil {
		return nil, errNoClient
	}
	return routeros.GetBridgeHosts(client)
}

// settingIntClamped reads an integer app_setting, clamped to lo..hi. A missing
// or non-numeric value yields def.
func settingIntClamped(db *sql.DB, key string, def, lo, hi int) int {
	if db == nil {
		return def
	}
	v, err := queries.GetSetting(db, key)
	if err != nil {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

// PortStatsInterval is the counter poll period: port_stats_interval seconds
// (default 15, clamp 5..300). Re-read every call.
func PortStatsInterval(db *sql.DB) time.Duration {
	return time.Duration(settingIntClamped(db, "port_stats_interval", 15, 5, 300)) * time.Second
}

// PortHostsInterval is the FDB dump period: port_hosts_interval seconds
// (default 300, clamp 60..3600). Re-read every call.
func PortHostsInterval(db *sql.DB) time.Duration {
	return time.Duration(settingIntClamped(db, "port_hosts_interval", 300, 60, 3600)) * time.Second
}

// Run drives the counters loop and starts the port-hosts loop; both return
// when ctx is done.
func (c *PortStatsCollector) Run(ctx context.Context) {
	go c.hostsLoop(ctx)

	c.startupRollup()

	select {
	case <-ctx.Done():
		return
	case <-time.After(portStatsStartupDelay):
	}
	for {
		start := time.Now()
		interval := PortStatsInterval(c.db)
		c.safeCycle(ctx, interval)

		// Keep a steady cadence: the cycle itself takes up to portStatsMaxWait.
		wait := max(interval-time.Since(start), time.Second)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// startupRollup re-rolls the complete hours since the newest hourly bucket
// (at least the last portStatsStartupRollback, at most portStatsMaxRollback),
// covering the hour the process stopped in when it was down for longer.
func (c *PortStatsCollector) startupRollup() {
	hour := c.now().UTC().Truncate(time.Hour)
	from := hour.Add(-portStatsMaxRollback)
	if newest, ok, err := queries.LatestPortStatsBucket(c.db, queries.PortStats1h); err != nil {
		log.Printf("poller portstats: newest hourly bucket: %v", err)
	} else if ok && newest.After(from) {
		from = newest
	}
	from = minTime(from, hour.Add(-portStatsStartupRollback))
	if err := queries.RollupPortStats1h(c.db, from, hour); err != nil {
		log.Printf("poller portstats: startup rollup: %v", err)
	}
	c.mu.Lock()
	if hour.After(c.rolledThrough) {
		c.rolledThrough = hour
	}
	c.mu.Unlock()
}

func (c *PortStatsCollector) safeCycle(ctx context.Context, interval time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller portstats: panic in cycle: %v\n%s", r, debug.Stack())
		}
	}()
	c.cycle(ctx, interval)
}

// cycle polls every online device concurrently, waits up to
// min(interval, portStatsMaxWait) for the replies, then prunes, flushes
// finished minutes and publishes the snapshot.
func (c *PortStatsCollector) cycle(ctx context.Context, interval time.Duration) {
	c.mu.Lock()
	c.interval = interval
	c.mu.Unlock()

	devices, err := queries.ListDevices(c.db)
	if err != nil {
		log.Printf("poller portstats: list devices: %v", err)
	}

	var wg sync.WaitGroup
	for _, dev := range devices {
		if dev.Status != "online" || !c.claim(dev.ID) {
			continue
		}
		wg.Add(1)
		go c.pollDevice(dev, interval, &wg)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	wctx, cancel := context.WithTimeout(ctx, min(interval, portStatsMaxWait))
	select {
	case <-done:
	case <-wctx.Done():
	}
	cancel()
	if ctx.Err() != nil {
		return // shutting down
	}

	c.finishCycle(c.now().UTC(), interval)
}

// claim marks a device's poll in flight; false when the previous one has not
// returned yet.
func (c *PortStatsCollector) claim(deviceID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight[deviceID] {
		return false
	}
	c.inFlight[deviceID] = true
	return true
}

func (c *PortStatsCollector) release(deviceID string) {
	c.mu.Lock()
	delete(c.inFlight, deviceID)
	c.mu.Unlock()
}

// pollDevice reads one device's counters and ingests them. It may outlive the
// cycle that started it; a late reply still updates the state.
func (c *PortStatsCollector) pollDevice(dev queries.Device, interval time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()
	defer c.release(dev.ID)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller portstats: panic polling %s (%s): %v\n%s", dev.Identity, dev.Address, r, debug.Stack())
			if c.pool != nil {
				c.pool.Close(dev.ID)
			}
		}
	}()

	stats, err := c.fetchStats(dev.ID)
	t := c.now().UTC()
	if err != nil {
		if !errors.Is(err, errNoClient) {
			c.logThrottled("stats:"+dev.ID, "poller portstats: interface counters for %s: %v", dev.Identity, err)
		}
		return
	}
	c.ingest(dev.ID, stats, t, interval)
}

// ingest turns one device's counter reading taken at t into rates and minute
// accumulations. The previous sample is usable when it exists and carried
// counters, 0 < dt <= 3*interval, the interface is running with counters, and
// neither byte counter went backwards (reset/reboot). Otherwise the rates are
// 0 and nothing is accumulated. The reading always becomes the new previous
// sample.
func (c *PortStatsCollector) ingest(deviceID string, stats []routeros.InterfaceStats, t time.Time, interval time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	maxGap := 3 * interval.Seconds()

	for _, s := range stats {
		key := portStatsKey{deviceID, s.Name}
		prev := c.samples[key]
		rate := PortRate{
			DeviceID: deviceID,
			Iface:    s.Name,
			Type:     s.Type,
			Comment:  s.Comment,
			Running:  s.Running,
			Disabled: s.Disabled,
		}

		if prev != nil && prev.hasCounters && s.Running && s.HasCounters {
			dt := t.Sub(prev.at).Seconds()
			dRx, dTx := s.RxBytes-prev.rxBytes, s.TxBytes-prev.txBytes
			if dt > 0 && dt <= maxGap && dRx >= 0 && dTx >= 0 {
				dRxP := max(s.RxPackets-prev.rxPackets, 0)
				dTxP := max(s.TxPackets-prev.txPackets, 0)
				rate.RxBps = roundRate(float64(dRx) * 8 / dt)
				rate.TxBps = roundRate(float64(dTx) * 8 / dt)
				rate.RxPps = roundRate(float64(dRxP) / dt)
				rate.TxPps = roundRate(float64(dTxP) / dt)
				c.ready = true
				c.accumulate(key, prev.at, t, portDelta{dRx, dTx, dRxP, dTxP}, rate.RxBps, rate.TxBps)
			}
		}

		if prev == nil {
			prev = &portCounterSample{}
			c.samples[key] = prev
		}
		prev.at = t
		prev.hasCounters = s.HasCounters
		prev.rxBytes, prev.txBytes = s.RxBytes, s.TxBytes
		prev.rxPackets, prev.txPackets = s.RxPackets, s.TxPackets
		prev.rate = rate
	}
}

// portDelta is one interval's counter increase.
type portDelta struct {
	rxBytes, txBytes     int64
	rxPackets, txPackets int64
}

// accumulate spreads one usable poll interval [from, to) over the minutes it
// overlaps, in proportion to the overlap: each minute gets that share of the
// byte and packet deltas and of the covered seconds, and the interval's rates
// as a candidate maximum. Crediting a whole interval to the minute it ended in
// would put, say, two minutes of bytes into one bucket at a 120 s interval (and
// 3 or 5 fifteen-second intervals into a minute when replies jitter across
// :00), so 1-minute charts would comb. Shares are rounded cumulatively, so they
// sum to the exact delta. The interval counts as a sample in the last minute
// it touches.
//
// A share for a minute that has already been flushed (a reply taken before a
// flush but ingested after it) is carried into the first open minute instead.
// Every cycle flushes only the minutes before its own, so in steady state the
// minutes an interval spans are all still open when it arrives. c.mu held.
func (c *PortStatsCollector) accumulate(key portStatsKey, from, to time.Time, d portDelta, rxRate, txRate int64) {
	total := to.Sub(from)
	if total <= 0 {
		return
	}
	var done time.Duration // overlap already credited
	var credited portDelta
	for m := from.Truncate(time.Minute); m.Before(to); m = m.Add(time.Minute) {
		overlap := minTime(to, m.Add(time.Minute)).Sub(maxTime(from, m))
		if overlap <= 0 {
			continue
		}
		done += overlap
		last := done >= total
		share := func(delta, sofar int64) int64 {
			if last {
				return delta - sofar
			}
			return int64(math.Round(float64(delta)*done.Seconds()/total.Seconds())) - sofar
		}
		slice := portDelta{
			rxBytes:   share(d.rxBytes, credited.rxBytes),
			txBytes:   share(d.txBytes, credited.txBytes),
			rxPackets: share(d.rxPackets, credited.rxPackets),
			txPackets: share(d.txPackets, credited.txPackets),
		}
		credited.rxBytes += slice.rxBytes
		credited.txBytes += slice.txBytes
		credited.rxPackets += slice.rxPackets
		credited.txPackets += slice.txPackets

		bucket := m
		if bucket.Before(c.flushedThrough) {
			bucket = c.flushedThrough
		}
		acc := c.minuteAccFor(bucket, key)
		acc.rxBytes += slice.rxBytes
		acc.txBytes += slice.txBytes
		acc.rxPackets += slice.rxPackets
		acc.txPackets += slice.txPackets
		acc.seconds += overlap.Seconds()
		acc.rxMax = max(acc.rxMax, rxRate)
		acc.txMax = max(acc.txMax, txRate)
		if last {
			acc.samples++
		}
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// minuteAccFor returns (creating) the accumulator of key in minute. c.mu held.
func (c *PortStatsCollector) minuteAccFor(minute time.Time, key portStatsKey) *minuteAcc {
	ports := c.minutes[minute.Unix()]
	if ports == nil {
		ports = make(map[portStatsKey]*minuteAcc)
		c.minutes[minute.Unix()] = ports
	}
	acc := ports[key]
	if acc == nil {
		acc = &minuteAcc{}
		ports[key] = acc
	}
	return acc
}

// finishCycle ends a cycle at now: it drops entries not refreshed for more
// than 3*interval (offline device or removed interface), stamps the snapshot,
// flushes finished minutes and publishes.
func (c *PortStatsCollector) finishCycle(now time.Time, interval time.Duration) {
	c.mu.Lock()
	for key, s := range c.samples {
		if now.Sub(s.at) > 3*interval {
			delete(c.samples, key)
		}
	}
	c.lastCycle = now
	c.mu.Unlock()

	c.flush(now)
	c.publish()
}

// flush writes every accumulated minute before now's minute to port_stats_1m
// and advances FlushedThrough; crossing an hour boundary rolls the hours
// before it into port_stats_1h. On a DB error the buckets are logged and
// dropped.
func (c *PortStatsCollector) flush(now time.Time) {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	boundary := now.UTC().Truncate(time.Minute)
	rows := c.takeMinutesBefore(boundary)
	if len(rows) > 0 {
		if err := queries.InsertPortStats1m(c.db, rows); err != nil {
			log.Printf("poller portstats: write %d minute rows: %v", len(rows), err)
		}
	}

	c.mu.Lock()
	if boundary.After(c.flushedThrough) {
		c.flushedThrough = boundary
	}
	hour := c.flushedThrough.Truncate(time.Hour)
	rollup := hour.After(c.rolledThrough)
	// Re-roll the last two hours (a late reply may have been carried into
	// the first) and every hour since the last rollup, so a stalled process
	// whose watermark jumps several hours skips none — bounded like the
	// startup re-roll.
	from := hour.Add(-2 * time.Hour)
	if !c.rolledThrough.IsZero() && c.rolledThrough.Before(from) {
		from = maxTime(c.rolledThrough, hour.Add(-portStatsMaxRollback))
	}
	c.mu.Unlock()
	if !rollup {
		return
	}

	if err := queries.RollupPortStats1h(c.db, from, hour); err != nil {
		log.Printf("poller portstats: hourly rollup to %s: %v", hour.Format(time.RFC3339), err)
	}
	c.mu.Lock()
	if hour.After(c.rolledThrough) {
		c.rolledThrough = hour
	}
	c.mu.Unlock()
}

// takeMinutesBefore removes the accumulated minutes before boundary and
// returns them as rows, skipping idle ones (0 bytes both ways).
func (c *PortStatsCollector) takeMinutesBefore(boundary time.Time) []queries.PortStatRow {
	c.mu.Lock()
	defer c.mu.Unlock()

	var rows []queries.PortStatRow
	for unix, ports := range c.minutes {
		minute := time.Unix(unix, 0).UTC()
		if !minute.Before(boundary) {
			continue
		}
		delete(c.minutes, unix)
		for key, acc := range ports {
			if acc.rxBytes == 0 && acc.txBytes == 0 {
				continue
			}
			rows = append(rows, acc.row(key, minute))
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.Bucket.Equal(b.Bucket) {
			return a.Bucket.Before(b.Bucket)
		}
		if a.DeviceID != b.DeviceID {
			return a.DeviceID < b.DeviceID
		}
		return a.InterfaceName < b.InterfaceName
	})
	return rows
}

// row converts an accumulated minute: avg = Σbytes*8/Σdt (not the mean of the
// interval rates), max = highest interval rate, pps = Σpackets/Σdt.
func (a *minuteAcc) row(key portStatsKey, minute time.Time) queries.PortStatRow {
	r := queries.PortStatRow{
		DeviceID:      key.device,
		InterfaceName: key.iface,
		Bucket:        minute,
		RxBpsMax:      a.rxMax,
		TxBpsMax:      a.txMax,
		RxBytes:       a.rxBytes,
		TxBytes:       a.txBytes,
		Samples:       a.samples,
	}
	if a.seconds > 0 {
		r.RxBpsAvg = roundRate(float64(a.rxBytes) * 8 / a.seconds)
		r.TxBpsAvg = roundRate(float64(a.txBytes) * 8 / a.seconds)
		r.RxPpsAvg = roundRate(float64(a.rxPackets) / a.seconds)
		r.TxPpsAvg = roundRate(float64(a.txPackets) / a.seconds)
	}
	return r
}

// publish sends the snapshot to traffic.ports subscribers. Hub.Publish never
// blocks (slow clients drop the message).
func (c *PortStatsCollector) publish() {
	if c.hub == nil || c.hub.TopicSubscriberCount(PortRatesTopic) == 0 {
		return
	}
	c.hub.Publish(PortRatesTopic, c.Snapshot())
}

// Snapshot returns a deep copy of the latest rates, safe for concurrent use.
// A nil collector yields an empty snapshot.
func (c *PortStatsCollector) Snapshot() PortSnapshot {
	if c == nil {
		return PortSnapshot{IntervalSeconds: int(defaultPortStatsInterval / time.Second), Ports: []PortRate{}}
	}
	c.mu.Lock()
	snap := PortSnapshot{
		IntervalSeconds: int(c.interval / time.Second),
		Ready:           c.ready,
		Ports:           make([]PortRate, 0, len(c.samples)),
	}
	if !c.lastCycle.IsZero() {
		ts := c.lastCycle
		snap.TS = &ts
	}
	for _, s := range c.samples {
		snap.Ports = append(snap.Ports, s.rate)
	}
	c.mu.Unlock()

	sort.Slice(snap.Ports, func(i, j int) bool {
		a, b := snap.Ports[i], snap.Ports[j]
		if a.DeviceID != b.DeviceID {
			return a.DeviceID < b.DeviceID
		}
		return a.Iface < b.Iface
	})
	return snap
}

// FlushedThrough is the exclusive end of the newest processed 1m bucket
// (minute-aligned UTC): every minute before it is final in port_stats_1m.
// Zero before the first flush.
func (c *PortStatsCollector) FlushedThrough() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushedThrough
}

// RolledThrough is the exclusive end of the newest rolled hour (hour-aligned
// UTC) in port_stats_1h. Zero before the first rollup.
func (c *PortStatsCollector) RolledThrough() time.Time {
	if c == nil {
		return time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rolledThrough
}

// hostsLoop dumps every online device's bridge FDB into port_hosts each
// port_hosts_interval (re-read every iteration).
func (c *PortStatsCollector) hostsLoop(ctx context.Context) {
	if !sleepCtx(ctx, portHostsStartupDelay) {
		return
	}
	for {
		pending := c.collectHosts(ctx, nil)
		next := time.Now().Add(PortHostsInterval(c.db))
		// Devices the pool had not dialled yet get another go soon rather
		// than a whole interval later.
		for len(pending) > 0 && time.Until(next) > portHostsRetryDelay {
			if !sleepCtx(ctx, portHostsRetryDelay) {
				return
			}
			pending = c.collectHosts(ctx, pending)
		}
		if !sleepCtx(ctx, time.Until(next)) {
			return
		}
	}
}

// sleepCtx waits for d, returning false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// collectHosts runs one FDB pass, device by device — only the devices in
// only when it is non-nil. A failing device is logged and skipped. It returns
// the online devices skipped for want of a pooled connection, so the caller
// can retry them. Only port_hosts is written — never mac_lookup.
func (c *PortStatsCollector) collectHosts(ctx context.Context, only map[string]bool) map[string]bool {
	devices, err := queries.ListDevices(c.db)
	if err != nil {
		log.Printf("poller portstats: hosts list devices: %v", err)
		return only
	}
	var noClient map[string]bool
	for _, dev := range devices {
		if ctx.Err() != nil {
			return nil
		}
		if dev.Status != "online" || (only != nil && !only[dev.ID]) {
			continue
		}
		if errors.Is(c.collectDeviceHosts(dev), errNoClient) {
			if noClient == nil {
				noClient = make(map[string]bool)
			}
			noClient[dev.ID] = true
		}
	}
	return noClient
}

// collectDeviceHosts dumps one device's FDB into port_hosts. It returns
// errNoClient when the device had no pooled connection; other failures are
// logged here and reported as nil.
func (c *PortStatsCollector) collectDeviceHosts(dev queries.Device) error {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller portstats: panic reading bridge hosts of %s (%s): %v\n%s", dev.Identity, dev.Address, r, debug.Stack())
			if c.pool != nil {
				c.pool.Close(dev.ID)
			}
		}
	}()

	hosts, err := c.fetchHosts(dev.ID)
	if err != nil {
		if errors.Is(err, errNoClient) {
			return err
		}
		c.logThrottled("hosts:"+dev.ID, "poller portstats: bridge hosts for %s: %v", dev.Identity, err)
		return nil
	}
	rows := make([]queries.PortHost, 0, len(hosts))
	for _, h := range hosts {
		rows = append(rows, queries.PortHost{
			InterfaceName: h.Interface,
			MACAddress:    h.MACAddress,
			VID:           h.VID,
			Bridge:        h.Bridge,
		})
	}
	if err := queries.UpsertPortHosts(c.db, dev.ID, rows, c.now()); err != nil {
		log.Printf("poller portstats: store bridge hosts for %s: %v", dev.Identity, err)
	}
	c.collectDeviceRelations(dev)
	return nil
}

// collectDeviceRelations replaces one device's interface_relations with a
// fresh read. On a failed read the previous rows stay: stacking rarely
// changes, and a partial answer must not drop a bond's members.
func (c *PortStatsCollector) collectDeviceRelations(dev queries.Device) {
	rels, err := c.fetchRelations(dev.ID)
	if err != nil {
		if !errors.Is(err, errNoClient) {
			c.logThrottled("relations:"+dev.ID, "poller portstats: interface stacking for %s: %v", dev.Identity, err)
		}
		return
	}
	rows := make([]queries.InterfaceRelation, 0, len(rels))
	for _, r := range rels {
		rows = append(rows, queries.InterfaceRelation{InterfaceName: r.Iface, Kind: r.Kind, Parent: r.Parent})
	}
	if err := queries.ReplaceInterfaceRelations(c.db, dev.ID, rows); err != nil {
		log.Printf("poller portstats: store interface stacking for %s: %v", dev.Identity, err)
	}
}

// logThrottled logs at most once per portStatsLogEvery per key, so a device
// that keeps failing doesn't flood the log every 15 s.
func (c *PortStatsCollector) logThrottled(key, format string, args ...any) {
	c.mu.Lock()
	now := c.now()
	last, seen := c.lastLog[key]
	if seen && now.Sub(last) < portStatsLogEvery {
		c.mu.Unlock()
		return
	}
	c.lastLog[key] = now
	c.mu.Unlock()
	log.Printf(format, args...)
}

func roundRate(f float64) int64 {
	return int64(math.Round(f))
}
