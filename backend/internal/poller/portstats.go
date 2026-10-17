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
	"github.com/mikrotik-nms/backend/internal/devclass"
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

	// ethernetInterval is the Tier 4a cadence: the proplisted
	// /interface/ethernet/print (loop-protect state, switch-chip drop and
	// pause counters) runs every 5 min per device whatever the counter
	// interval — the values change slowly, and on a switch the print costs
	// more than the general interface print.
	ethernetInterval = 5 * time.Minute
	// dueSlack lets a device whose next read is due within this margin
	// (capped at a tenth of its interval) ride along with the current
	// wake-up instead of costing one of its own: timers and replies jitter.
	dueSlack = time.Second
	// portStatsMaxWait caps how long a cycle waits for device replies. Slower
	// replies still land in the state when they arrive (see pollDevice).
	portStatsMaxWait = 10 * time.Second
	// portStatsStartupDelay gives the info loop time to dial the pool first;
	// until then GetLive has no dial parameters and returns nil.
	portStatsStartupDelay = 10 * time.Second
	// portHostsStartupDelay holds the first FDB dump back after startup.
	portHostsStartupDelay = 30 * time.Second
	// portHostsRetryDelay is how soon a device skipped for want of a pooled
	// connection gets its FDB dump and stacking prints again (see hostsTick).
	portHostsRetryDelay = noClientRetry
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

// PortStatsCollector owns the one scheduled interface read. Per device it
// runs Tier 0 — one proplisted /interface/print (inventory, link state,
// link-downs and counters) — each counter interval and Tier 4a — one
// proplisted /interface/ethernet/print — every 5 min, both on the device's
// own cadence (tierInterval). Tier 0 counter deltas become rates (the fleet
// snapshot, served by the API and published on WS) and 1-minute buckets
// (port_stats_1m) plus hourly rollups (port_stats_1h); the rows themselves
// are the per-device interface snapshot the other consumers read
// (DeviceSnapshot) and keep the interfaces table current. A second loop
// dumps each device's bridge FDB into port_hosts (Tier 3b), and its interface
// stacking (bond members, VLAN / PPPoE carriers) into interface_relations
// (Tier 3c), each on the device's cadence floored by port_hosts_interval.
//
// Devices are polled concurrently on their pooled clients (every print is
// short); a device whose previous poll has not returned yet is skipped, so a
// hung device never piles up goroutines.
type PortStatsCollector struct {
	db   *sql.DB
	pool *routeros.Pool
	hub  *ws.Hub

	// Test seams: the device link, the reads and the clock. runner returns
	// nil when the device has no pooled connection; the default reads run
	// on it through a counting wrapper (deviceRunner).
	runner         func(deviceID string) routeros.Runner
	fetchStats     func(deviceID string) ([]routeros.InterfaceStats, error)
	fetchEthernet  func(deviceID string) ([]routeros.EthernetStats, error)
	fetchHosts     func(deviceID string) ([]routeros.BridgeHost, error)
	fetchRelations func(deviceID string) ([]routeros.InterfaceRelation, error)
	now            func() time.Time

	// tierInterval decides how often a tier runs on a device; 0 = not at
	// all. The default is the fleet-wide schedule (fleetTierInterval);
	// UseCadence plugs in the per-class cadences and the polling pause, and
	// every interval-derived rule (delta gap, sample and snapshot expiry, the
	// flush hold) follows the interval it returns.
	tierInterval func(dev queries.Device, tier devclass.Tier, now time.Time) time.Duration
	// paused reports an admin pause: nothing is read, and the device's
	// snapshot and rates do not expire while it lasts.
	paused func(dev queries.Device, now time.Time) bool
	// cadence schedules the hosts loop's tiers (FDB dump, stacking prints).
	cadence CadenceSource
	// phase is each device's jitter phase (devclass.Phase; tests pin it).
	phase func(deviceID string) uint64
	// hostClock schedules the hosts loop per device; reg is where every
	// tier's next run is published (Schedule()).
	hostClock *tierClock
	reg       *ScheduleRegistry

	flushMu sync.Mutex // serializes flush (DB writes + rollup)

	mu             sync.Mutex
	interval       time.Duration // fleet-wide port_stats_interval of the current cycle
	lastCycle      time.Time
	ready          bool
	samples        map[portStatsKey]*portCounterSample
	minutes        map[int64]map[portStatsKey]*minuteAcc // keyed by minute start, Unix seconds
	devs           map[string]*deviceState
	inFlight       map[string]bool
	flushedThrough time.Time
	rolledThrough  time.Time
	lastLog        map[string]time.Time
}

// NewPortStatsCollector wires the collector; Run starts it.
func NewPortStatsCollector(db *sql.DB, pool *routeros.Pool, hub *ws.Hub) *PortStatsCollector {
	c := &PortStatsCollector{
		db:        db,
		pool:      pool,
		hub:       hub,
		now:       time.Now,
		interval:  defaultPortStatsInterval,
		samples:   make(map[portStatsKey]*portCounterSample),
		minutes:   make(map[int64]map[portStatsKey]*minuteAcc),
		devs:      make(map[string]*deviceState),
		inFlight:  make(map[string]bool),
		lastLog:   make(map[string]time.Time),
		cadence:   newFleetCadence(db),
		phase:     devclass.Phase,
		hostClock: newTierClock(Schedule()),
		reg:       Schedule(),
	}
	c.runner = c.poolRunner
	c.tierInterval = c.fleetTierInterval
	c.paused = func(dev queries.Device, now time.Time) bool { return dev.PollingPaused(now) }
	c.fetchStats = func(id string) ([]routeros.InterfaceStats, error) {
		run, err := c.deviceRunner(id)
		if err != nil {
			return nil, err
		}
		return routeros.GetInterfaceStats(run)
	}
	c.fetchEthernet = func(id string) ([]routeros.EthernetStats, error) {
		run, err := c.deviceRunner(id)
		if err != nil {
			return nil, err
		}
		return routeros.GetEthernetStats(run)
	}
	c.fetchHosts = func(id string) ([]routeros.BridgeHost, error) {
		run, err := c.deviceRunner(id)
		if err != nil {
			return nil, err
		}
		return routeros.GetBridgeHosts(run)
	}
	c.fetchRelations = func(id string) ([]routeros.InterfaceRelation, error) {
		run, err := c.deviceRunner(id)
		if err != nil {
			return nil, err
		}
		return routeros.GetInterfaceRelations(run)
	}
	return c
}

// poolRunner is the production device link: the pooled client, or nil while
// the pool holds no connection for the device.
func (c *PortStatsCollector) poolRunner(deviceID string) routeros.Runner {
	if c.pool == nil {
		return nil
	}
	client := c.pool.GetLive(deviceID)
	if client == nil {
		return nil
	}
	return routeros.ClientRunner{Client: client}
}

// UseCadence schedules the collector's tiers by cs: Tier 0 and Tier 4a
// through the tierInterval hook (0 while the device is paused), the FDB dump
// and the stacking prints in the hosts loop, and cs's pause for every expiry
// rule. Call before Run.
func (c *PortStatsCollector) UseCadence(cs CadenceSource) {
	if cs == nil {
		return
	}
	c.cadence = cs
	c.paused = cs.Paused
	c.tierInterval = func(dev queries.Device, tier devclass.Tier, now time.Time) time.Duration {
		return runInterval(cs, dev, tier, now)
	}
}

// fleetTierInterval is the schedule without a CadenceSource: Tier 0 at the
// fleet-wide port_stats_interval, Tier 4a every ethernetInterval, nothing
// else, and nothing while the device is paused.
func (c *PortStatsCollector) fleetTierInterval(dev queries.Device, tier devclass.Tier, now time.Time) time.Duration {
	if dev.PollingPaused(now) {
		return 0
	}
	switch tier {
	case devclass.TierCounters:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.interval
	case devclass.TierEthernet:
		return ethernetInterval
	}
	return 0
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

	if !sleepCtx(ctx, portStatsStartupDelay) {
		return
	}
	for {
		base := PortStatsInterval(c.db)
		next := c.safeCycle(ctx, base)

		// Sleep until the next device is due. At least a second, so a clock
		// step never spins the loop; at most one base interval, so settings,
		// the device list and pauses are re-read that often.
		wait := min(max(time.Until(next), time.Second), base)
		if !sleepCtx(ctx, wait) {
			return
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

func (c *PortStatsCollector) safeCycle(ctx context.Context, base time.Duration) (next time.Time) {
	next = c.now().Add(base)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller portstats: panic in cycle: %v\n%s", r, debug.Stack())
		}
	}()
	return c.cycle(ctx, base)
}

// tierPlan is what one poll of a device reads.
type tierPlan struct {
	stats, ethernet               bool
	statsInterval, ethernetPeriod time.Duration
}

// cycle polls, concurrently, every online device with a tier due at this
// wake-up, waits up to min(base, portStatsMaxWait) for the replies, then
// prunes, flushes finished minutes and publishes the snapshot. base is the
// fleet-wide port_stats_interval. It returns when the next device is due
// (at most one base interval ahead).
func (c *PortStatsCollector) cycle(ctx context.Context, base time.Duration) time.Time {
	c.mu.Lock()
	c.interval = base
	c.mu.Unlock()

	start := c.now().UTC()
	next := start.Add(base)
	devices, err := queries.ListDevices(c.db)
	if err != nil {
		log.Printf("poller portstats: list devices: %v", err)
	} else {
		c.forgetDevicesNotIn(devices)
	}

	var wg sync.WaitGroup
	for _, dev := range devices {
		c.notePause(dev, start)
		if dev.Status != "online" {
			c.reg.clear(dev.ID, devclass.TierCounters)
			c.reg.clear(dev.ID, devclass.TierEthernet)
			continue
		}
		plan, claimed, wake := c.planDevice(dev, start)
		if !wake.IsZero() && wake.Before(next) {
			next = wake
		}
		if !claimed {
			continue
		}
		wg.Add(1)
		go c.pollDevice(dev, plan, &wg)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	wctx, cancel := context.WithTimeout(ctx, min(base, portStatsMaxWait))
	select {
	case <-done:
	case <-wctx.Done():
	}
	cancel()
	if ctx.Err() != nil {
		return next // shutting down
	}

	c.finishCycle(c.now().UTC(), base)
	return next
}

// planDevice decides which tiers of dev are due at now and, when any is,
// claims the device (its poll is then in flight). A due tier is provisionally
// scheduled one interval (± the device's jitter) ahead for the loop's
// wake-up; the end of its read sets the real next run (scheduleNext), so a
// slow reply pushes the next one out instead of being caught up. A device
// whose previous poll has not returned is not claimed. wake is when the
// device is due next (zero while in flight or when no tier runs on it), for
// the loop's sleep. Every tier's next run is published to the registry.
func (c *PortStatsCollector) planDevice(dev queries.Device, now time.Time) (p tierPlan, claimed bool, wake time.Time) {
	p.statsInterval = c.tierInterval(dev, devclass.TierCounters, now)
	p.ethernetPeriod = c.tierInterval(dev, devclass.TierEthernet, now)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inFlight[dev.ID] {
		return tierPlan{}, false, time.Time{}
	}
	st := c.deviceLocked(dev.ID)
	p.stats = p.statsInterval > 0 && isDue(st.nextStats, now, p.statsInterval)
	p.ethernet = p.ethernetPeriod > 0 && isDue(st.nextEthernet, now, p.ethernetPeriod)
	if p.stats {
		st.nextStats = devclass.NextRun(now, p.statsInterval, c.phase(dev.ID))
	}
	if p.ethernet {
		st.nextEthernet = devclass.NextRun(now, p.ethernetPeriod, c.phase(dev.ID))
	}
	if p.statsInterval > 0 {
		wake = st.nextStats
	}
	if p.ethernetPeriod > 0 && (wake.IsZero() || st.nextEthernet.Before(wake)) {
		wake = st.nextEthernet
	}
	c.publishTierLocked(dev.ID, devclass.TierCounters, p.statsInterval, st.nextStats, now)
	c.publishTierLocked(dev.ID, devclass.TierEthernet, p.ethernetPeriod, st.nextEthernet, now)
	if p.stats || p.ethernet {
		c.inFlight[dev.ID] = true
		claimed = true
	}
	return p, claimed, wake
}

// publishTierLocked puts tier's next run (never run = due now) into the
// registry, or takes it out when the tier does not run. c.mu held.
func (c *PortStatsCollector) publishTierLocked(deviceID string, tier devclass.Tier, interval time.Duration, next, now time.Time) {
	switch {
	case interval <= 0:
		c.reg.clear(deviceID, tier)
	case next.IsZero():
		c.reg.set(deviceID, tier, now)
	default:
		c.reg.set(deviceID, tier, next)
	}
}

// scheduleNext sets tier's next run on deviceID from the end of its read.
func (c *PortStatsCollector) scheduleNext(deviceID string, tier devclass.Tier, end time.Time, interval time.Duration) {
	if interval <= 0 {
		return
	}
	next := devclass.NextRun(end, interval, c.phase(deviceID))
	c.mu.Lock()
	st := c.deviceLocked(deviceID)
	switch tier {
	case devclass.TierCounters:
		st.nextStats = next
	case devclass.TierEthernet:
		st.nextEthernet = next
	}
	c.mu.Unlock()
	c.reg.set(deviceID, tier, next)
}

// notePause remembers whether dev is paused, and until when: its snapshot and
// rates do not expire meanwhile (deviceState.fresh). Re-noted every cycle, so
// a pause lifted early is picked up within one.
func (c *PortStatsCollector) notePause(dev queries.Device, now time.Time) {
	paused := c.paused(dev, now)
	var until time.Time
	if paused && dev.PollingPausedUntil != nil {
		until = *dev.PollingPausedUntil
	}
	c.mu.Lock()
	if st := c.devs[dev.ID]; st != nil {
		st.paused, st.pausedUntil = paused, until
	}
	c.mu.Unlock()
}

// isDue reports whether a read scheduled for next (zero = never run) is due
// at now, allowing dueSlack — at most a tenth of the interval — of jitter.
func isDue(next, now time.Time, interval time.Duration) bool {
	return !now.Add(min(dueSlack, interval/10)).Before(next)
}

func (c *PortStatsCollector) release(deviceID string) {
	c.mu.Lock()
	delete(c.inFlight, deviceID)
	c.mu.Unlock()
}

// pollDevice runs one device's due tiers on its pooled client — Tier 0
// first, then Tier 4a — and ingests the replies. It may outlive the cycle
// that started it; a late reply still updates the state. A device that did
// not answer Tier 0 is not asked for Tier 4a: that read is retried at the
// next wake-up instead of an ethernet interval later.
func (c *PortStatsCollector) pollDevice(dev queries.Device, p tierPlan, wg *sync.WaitGroup) {
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

	if p.stats {
		stats, err := c.fetchStats(dev.ID)
		t := c.now().UTC()
		c.scheduleNext(dev.ID, devclass.TierCounters, t, p.statsInterval)
		if err != nil {
			if !errors.Is(err, errNoClient) {
				c.logThrottled("stats:"+dev.ID, "poller portstats: interface counters for %s: %v", dev.Identity, err)
			}
			if p.ethernet {
				c.retryEthernet(dev.ID)
			}
			return
		}
		c.ingest(dev.ID, stats, t, p.statsInterval)
		c.syncInventory(dev, stats, t)
	}
	if p.ethernet {
		eth, err := c.fetchEthernet(dev.ID)
		t := c.now().UTC()
		if err != nil {
			if errors.Is(err, errNoClient) {
				c.retryEthernet(dev.ID)
			} else {
				c.scheduleNext(dev.ID, devclass.TierEthernet, t, p.ethernetPeriod)
				c.logThrottled("ethernet:"+dev.ID, "poller portstats: ethernet ports of %s: %v", dev.Identity, err)
			}
			return
		}
		c.scheduleNext(dev.ID, devclass.TierEthernet, t, p.ethernetPeriod)
		c.ingestEthernet(dev.ID, eth, t, p.ethernetPeriod)
	}
}

// ingest turns one device's counter reading taken at t into rates and minute
// accumulations, and keeps the rows as the device's interface snapshot. The
// previous sample is usable when it exists and carried counters, 0 < dt <=
// 3*interval (the device's own Tier 0 interval), the interface is running
// with counters, and neither byte counter went backwards (reset/reboot).
// Otherwise the rates are 0 and nothing is accumulated. The reading always
// becomes the new previous sample. stats is retained: the caller must not
// modify it afterwards.
func (c *PortStatsCollector) ingest(deviceID string, stats []routeros.InterfaceStats, t time.Time, interval time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.deviceLocked(deviceID)
	st.rows, st.at, st.interval = stats, t, interval

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

// finishCycle ends a cycle at now: it drops rates and interface snapshots not
// refreshed for more than 3× their device's Tier 0 interval (offline device
// or removed interface; interval stands in for a device the collector has
// not read yet) — except a paused device's, which do not age while nobody
// reads it — stamps the snapshot, flushes finished minutes and publishes.
func (c *PortStatsCollector) finishCycle(now time.Time, interval time.Duration) {
	c.mu.Lock()
	for key, s := range c.samples {
		iv := interval
		st := c.devs[key.device]
		if st != nil && st.pausedAt(now) {
			continue
		}
		if st != nil && st.interval > 0 {
			iv = st.interval
		}
		if now.Sub(s.at) > 3*iv {
			delete(c.samples, key)
		}
	}
	for _, st := range c.devs {
		if !st.at.IsZero() && !st.fresh(now) {
			st.rows, st.at = nil, time.Time{}
		}
	}
	c.lastCycle = now
	c.mu.Unlock()

	c.flush(now)
	c.publish()
}

// flushBoundaryLocked is the exclusive end of the minutes a flush at now may
// finalise: the start of now's minute, held back to the minute of the oldest
// open interval. Devices poll on their own cadence and phase, so another
// device's cycle can flush while a device's interval is still open; its next
// delta starts at its last sample and must find every minute from there on
// still open, or its shares would pile into a later minute (a 5-minute
// interval read between other devices' cycles would comb into one minute of
// five minutes' bytes and four empty ones). An interval counts as open for as
// long as its device's next read is expected (deviceState.openFor: one
// interval with the full jitter plus the reply wait), so the newest final
// minute lags by at most about one interval — about a minute for the
// default cadences, as long as the old fleet-wide cycle at long intervals. A
// device whose read is overdue (failing, offline) no longer holds anything:
// its late shares are carried into the first open minute (accumulate). The
// pause does not extend the hold either: a paused device's next delta is a
// gap once its last read is past the gap rule, which stays on the wall
// clock. c.mu held.
func (c *PortStatsCollector) flushBoundaryLocked(now time.Time) time.Time {
	boundary := now.UTC().Truncate(time.Minute)
	for _, st := range c.devs {
		if st.at.IsZero() || now.Sub(st.at) > st.openFor() {
			continue
		}
		if m := st.at.Truncate(time.Minute); m.Before(boundary) {
			boundary = m
		}
	}
	return boundary
}

// flush writes every accumulated minute before the flush boundary (now's
// minute, or earlier while a device's open interval still credits one; see
// flushBoundaryLocked) to port_stats_1m and advances FlushedThrough; crossing
// an hour boundary rolls the hours before it into port_stats_1h. On a DB
// error the buckets are logged and dropped.
func (c *PortStatsCollector) flush(now time.Time) {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	c.mu.Lock()
	boundary := c.flushBoundaryLocked(now)
	c.mu.Unlock()
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
	snap, _ := c.SnapshotWithReadTimes()
	return snap
}

// SnapshotWithReadTimes is Snapshot together with when each device in it was
// last read (its Tier 0 read, DeviceIfaces.At), both taken under one lock.
// The snapshot carries one cycle time while devices are read on their own
// cadences, and a device whose reads fail keeps its last rates for up to
// three intervals: the read times tell such stale rates from fresh ones — the
// map links' fresher-end rule and the port grid's age — and, taken together,
// a reply that lands in between can never make a device's time look fresher
// than the rates served. A nil collector yields the empty snapshot and no
// times.
func (c *PortStatsCollector) SnapshotWithReadTimes() (PortSnapshot, map[string]time.Time) {
	if c == nil {
		return PortSnapshot{IntervalSeconds: int(defaultPortStatsInterval / time.Second), Ports: []PortRate{}}, nil
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
	times := make(map[string]time.Time, len(c.devs))
	for id, st := range c.devs {
		// Not st.fresh: a device's rates outlive its snapshot until the
		// cycle that prunes both, and must keep their true age meanwhile.
		if !st.at.IsZero() {
			times[id] = st.at
		}
	}
	c.mu.Unlock()

	sort.Slice(snap.Ports, func(i, j int) bool {
		a, b := snap.Ports[i], snap.Ports[j]
		if a.DeviceID != b.DeviceID {
			return a.DeviceID < b.DeviceID
		}
		return a.Iface < b.Iface
	})
	return snap, times
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

// hostsLoop runs each online device's FDB dump (Tier 3b) and stacking prints
// (Tier 3c) on their own per-device cadences, port_hosts_interval being the
// floor of both.
func (c *PortStatsCollector) hostsLoop(ctx context.Context) {
	if !sleepCtx(ctx, portHostsStartupDelay) {
		return
	}
	for {
		c.safeHostsTick(ctx)
		if !sleepCtx(ctx, pollerTick) {
			return
		}
	}
}

func (c *PortStatsCollector) safeHostsTick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller portstats: panic in hosts pass: %v\n%s", r, debug.Stack())
		}
	}()
	c.hostsTick(ctx)
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

// hostsTick runs, device by device, the FDB dumps and stacking prints that are
// due. Offline and paused devices run neither. A device without a pooled
// connection is retried after portHostsRetryDelay — on a cold start the pool
// has dialled only a few devices by the first pass, and without the retry
// most of the fleet would have no FDB rows (so no access/wireless roles) for a
// whole interval. Only port_hosts and interface_relations are written — never
// mac_lookup.
func (c *PortStatsCollector) hostsTick(ctx context.Context) {
	devices, err := queries.ListDevices(c.db)
	if err != nil {
		log.Printf("poller portstats: hosts list devices: %v", err)
		return
	}
	c.hostClock.retain(deviceIDs(devices))
	now := c.now()
	for _, dev := range devices {
		if ctx.Err() != nil {
			return
		}
		var fdbIv, relIv time.Duration
		if dev.Status == "online" {
			fdbIv = runInterval(c.cadence, dev, devclass.TierBridgeHosts, now)
			relIv = runInterval(c.cadence, dev, devclass.TierRelations, now)
		}
		fdbDue := c.hostClock.due(dev.ID, devclass.TierBridgeHosts, fdbIv, now)
		relDue := c.hostClock.due(dev.ID, devclass.TierRelations, relIv, now)
		if fdbDue || relDue {
			c.visitHosts(dev, fdbDue, relDue, fdbIv, relIv)
		}
	}
}

// visitHosts runs one device's due host tiers: the FDB dump first, then the
// stacking prints — which a device that just failed its dump is not asked
// for (they wait their interval), as the dump's failure says the session is
// unwell.
func (c *PortStatsCollector) visitHosts(dev queries.Device, fdbDue, relDue bool, fdbIv, relIv time.Duration) {
	if fdbDue {
		err := c.collectDeviceHosts(dev)
		end := c.now()
		if errors.Is(err, errNoClient) {
			c.hostClock.retryAt(dev.ID, devclass.TierBridgeHosts, end.Add(portHostsRetryDelay))
			if relDue {
				c.hostClock.retryAt(dev.ID, devclass.TierRelations, end.Add(portHostsRetryDelay))
			}
			return
		}
		c.hostClock.ran(dev.ID, devclass.TierBridgeHosts, fdbIv, end)
		if err != nil {
			if relDue {
				c.hostClock.ran(dev.ID, devclass.TierRelations, relIv, end)
			}
			return
		}
	}
	if relDue {
		err := c.collectDeviceRelations(dev)
		end := c.now()
		if errors.Is(err, errNoClient) {
			c.hostClock.retryAt(dev.ID, devclass.TierRelations, end.Add(portHostsRetryDelay))
			return
		}
		c.hostClock.ran(dev.ID, devclass.TierRelations, relIv, end)
	}
}

// collectDeviceHosts dumps one device's FDB into port_hosts. It returns
// errNoClient when the device had no pooled connection, and the read error
// (logged here) when the dump failed.
func (c *PortStatsCollector) collectDeviceHosts(dev queries.Device) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller portstats: panic reading bridge hosts of %s (%s): %v\n%s", dev.Identity, dev.Address, r, debug.Stack())
			if c.pool != nil {
				c.pool.Close(dev.ID)
			}
			err = errors.New("panic reading bridge hosts")
		}
	}()

	hosts, err := c.fetchHosts(dev.ID)
	if err != nil {
		if !errors.Is(err, errNoClient) {
			c.logThrottled("hosts:"+dev.ID, "poller portstats: bridge hosts for %s: %v", dev.Identity, err)
		}
		return err
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
	return nil
}

// collectDeviceRelations replaces one device's interface_relations with a
// fresh read, and keeps its VLAN rows in memory for the flow ifIndex sync
// (DeviceVLANs). On a failed read the previous rows stay: stacking rarely
// changes, and a partial answer must not drop a bond's members. It returns
// errNoClient when the device had no pooled connection.
func (c *PortStatsCollector) collectDeviceRelations(dev queries.Device) (err error) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller portstats: panic reading interface stacking of %s (%s): %v\n%s", dev.Identity, dev.Address, r, debug.Stack())
			if c.pool != nil {
				c.pool.Close(dev.ID)
			}
			err = errors.New("panic reading interface stacking")
		}
	}()
	rels, err := c.fetchRelations(dev.ID)
	if err != nil {
		if !errors.Is(err, errNoClient) {
			c.logThrottled("relations:"+dev.ID, "poller portstats: interface stacking for %s: %v", dev.Identity, err)
		}
		return err
	}
	c.storeVLANs(dev.ID, rels, c.now().UTC())
	rows := make([]queries.InterfaceRelation, 0, len(rels))
	for _, r := range rels {
		rows = append(rows, queries.InterfaceRelation{InterfaceName: r.Iface, Kind: r.Kind, Parent: r.Parent})
	}
	if err := queries.ReplaceInterfaceRelations(c.db, dev.ID, rows); err != nil {
		log.Printf("poller portstats: store interface stacking for %s: %v", dev.Identity, err)
	}
	return nil
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
