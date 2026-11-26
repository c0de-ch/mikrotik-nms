package poller

import (
	"context"
	"database/sql"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// CPU telemetry (design §2.5, Appendix B Tier 1 and 1c).
//
// Tier 1, the quiet-window sample: per device, once per minute, one
// /system/resource/print proplisted to cpu-load, free-memory and uptime. It
// is taken only once the device has been left alone for cpuQuietWindow —
// the NMS's own burst (a counter read is a 1–2 s spike on a small switch)
// must not be what the sample measures — and after cpuQuietMaxWait of
// waiting regardless, so a device that is never quiet still has a reading.
//
// Tier 1c, the post-poll sample: every 10 min, one cpu-load print
// cpuPostDelay after a Tier 0 read, stored apart (device_minutes.cpu_post).
// Next to the quiet-window line it shows what the NMS's read does to the CPU.
//
// That is the whole wire cost of this loop: one cheap command per device per
// minute plus one per 10 minutes. Neither runs on a paused device, an offline
// one, or one without a live pooled session.
const (
	cpuQuietWindow  = 3 * time.Second
	cpuQuietMaxWait = 10 * time.Second
	cpuPostDelay    = 3 * time.Second
	// cpuPostSlack is how late after cpuPostDelay the post-poll sample may
	// still be taken; a read the loop notices later than that is not
	// followed (the next one is).
	cpuPostSlack = 2 * time.Second
	// cpuFastTick is how often the loop looks again while a sample waits
	// for its moment; the device list is still re-read every pollerTick.
	cpuFastTick = time.Second
	// cpuStartupDelay lets the info loop dial the pool first.
	cpuStartupDelay = 12 * time.Second
	// Fleet-wide defaults of the two tiers (the class plan has the same
	// values for every class).
	defaultCPUInterval     = 60 * time.Second
	defaultCPUPostInterval = 10 * time.Minute
)

// cpuDevice is the sampler's per-device memory.
type cpuDevice struct {
	// slot is the Tier 1 slot last served (sampled, or passed over on first
	// sight); zero = the device was never seen with the tier running.
	slot time.Time
	// pendingSince is when the current slot's sample could first have run
	// (the quiet-window wait counts from it); zero = nothing pending.
	pendingSince time.Time
	// busy: a sample of this device is in flight.
	busy bool
	// ownEnd is when this sampler's last command to the device ended.
	ownEnd time.Time
	// postT0 is the Tier 0 read the last post-poll sample followed.
	postT0 time.Time
}

// CPUSampler runs Tier 1 and Tier 1c and feeds the LoadRecorder, the device
// row (cpu_load, memory_used, uptime) and WS device.health.
type CPUSampler struct {
	db   *sql.DB
	pool *routeros.Pool
	hub  *ws.Hub
	load *LoadRecorder

	cadence CadenceSource
	// Seams: the clock, the device link (nil = no live pooled session), when
	// the device's last command ended (the routeros observer, through the
	// recorder), when its last Tier 0 read ended (the port-stats collector),
	// the slot phase, and how a sample is run (a goroutine; tests run it
	// inline).
	now            func() time.Time
	runner         func(deviceID string) routeros.Runner
	lastCommandEnd func(deviceID string) time.Time
	tier0At        func(deviceID string) time.Time
	phase          func(deviceID string) uint64
	spawn          func(func())
	reg            *ScheduleRegistry
	postClock      *tierClock

	mu        sync.Mutex
	devs      map[string]*cpuDevice
	devices   []queries.Device // cached list
	devicesAt time.Time
	lastLog   map[string]time.Time
}

// NewCPUSampler wires the sampler; Run starts it. portStats may be nil (no
// post-poll samples then).
func NewCPUSampler(db *sql.DB, pool *routeros.Pool, hub *ws.Hub, load *LoadRecorder, portStats *PortStatsCollector) *CPUSampler {
	s := &CPUSampler{
		db: db, pool: pool, hub: hub, load: load,
		cadence:   newFleetCadence(db),
		now:       time.Now,
		phase:     devclass.Phase,
		spawn:     func(f func()) { go f() },
		reg:       Schedule(),
		postClock: newTierClock(Schedule()),
		devs:      make(map[string]*cpuDevice),
		lastLog:   make(map[string]time.Time),
	}
	s.runner = func(id string) routeros.Runner { return poolRunner(pool, id) }
	s.lastCommandEnd = func(id string) time.Time { return load.LastCommandEnd(id) }
	s.tier0At = portStats.LastTier0
	return s
}

// UseCadence schedules both tiers by cs (the per-class plan and the pause).
// Call before Run.
func (s *CPUSampler) UseCadence(cs CadenceSource) {
	if cs != nil {
		s.cadence = cs
	}
}

// Run drives the loop until ctx is done.
func (s *CPUSampler) Run(ctx context.Context) {
	if !sleepCtx(ctx, cpuStartupDelay) {
		return
	}
	for {
		now := s.now()
		wake := s.safeTick(now)
		wait := min(max(wake.Sub(now), 200*time.Millisecond), pollerTick)
		if !sleepCtx(ctx, wait) {
			return
		}
	}
}

func (s *CPUSampler) safeTick(now time.Time) (wake time.Time) {
	wake = now.Add(pollerTick)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("poller cpu: panic in tick: %v\n%s", r, debug.Stack())
		}
	}()
	return s.tick(now)
}

// deviceList returns the managed devices, re-read at most every pollerTick:
// the loop may wake every second while a sample waits, and that must not
// become a device-list query per second.
func (s *CPUSampler) deviceList(now time.Time) []queries.Device {
	s.mu.Lock()
	fresh := s.devices != nil && now.Sub(s.devicesAt) < pollerTick && !now.Before(s.devicesAt)
	devices := s.devices
	s.mu.Unlock()
	if fresh {
		return devices
	}
	devices, err := queries.ListDevices(s.db)
	if err != nil {
		log.Printf("poller cpu: list devices: %v", err)
		return nil
	}
	if devices == nil {
		devices = []queries.Device{}
	}
	keep := deviceIDs(devices)
	s.postClock.retain(keep)
	s.mu.Lock()
	s.devices, s.devicesAt = devices, now
	for id := range s.devs {
		if !keep[id] {
			delete(s.devs, id)
		}
	}
	s.mu.Unlock()
	return devices
}

func (s *CPUSampler) stateLocked(id string) *cpuDevice {
	st := s.devs[id]
	if st == nil {
		st = &cpuDevice{}
		s.devs[id] = st
	}
	return st
}

// tick runs, per device, the samples whose moment has come, and returns when
// the loop should look again: the next moment a waiting sample may run, at
// most a pollerTick ahead.
func (s *CPUSampler) tick(now time.Time) time.Time {
	wake := now.Add(pollerTick)
	sooner := func(t time.Time) {
		if t.Before(wake) {
			wake = t
		}
	}
	for _, dev := range s.deviceList(now) {
		if s.cadence.Paused(dev, now) {
			s.load.NotePaused(dev.ID, now)
		}
		var cpuIv, postIv time.Duration
		if dev.Status == "online" {
			cpuIv = runInterval(s.cadence, dev, devclass.TierCPU, now)
			postIv = runInterval(s.cadence, dev, devclass.TierCPUPost, now)
		}
		// The post-poll sample first: when both fall together, the
		// quiet-window sample then waits out its 3 s after it.
		s.tickPost(dev, postIv, now, sooner)
		s.tickQuiet(dev, cpuIv, now, sooner)
	}
	return wake
}

// tickQuiet runs dev's Tier 1 sample when its slot has come and the device
// is quiet (or was waited for long enough). A slot is served at most once; a
// device first seen mid-slot waits for its next one, so a start does not
// sample the whole fleet in one burst.
func (s *CPUSampler) tickQuiet(dev queries.Device, interval time.Duration, now time.Time, sooner func(time.Time)) {
	s.mu.Lock()
	st := s.stateLocked(dev.ID)
	if interval <= 0 {
		st.pendingSince = time.Time{}
		s.mu.Unlock()
		s.reg.clear(dev.ID, devclass.TierCPU)
		return
	}
	slot := devclass.SlotAt(now, interval, s.phase(dev.ID))
	next := slot.Add(interval)
	if st.slot.IsZero() {
		st.slot = slot
	}
	if !slot.After(st.slot) || st.busy {
		s.mu.Unlock()
		s.reg.set(dev.ID, devclass.TierCPU, next)
		sooner(next)
		return
	}
	ownEnd := st.ownEnd
	s.mu.Unlock()

	run := s.runner(dev.ID)
	if run == nil {
		// No live session: the slot stays open and is looked at again each
		// tick; the wait starts when a session is there.
		s.mu.Lock()
		st.pendingSince = time.Time{}
		s.mu.Unlock()
		s.reg.set(dev.ID, devclass.TierCPU, now.Add(pollerTick))
		return
	}

	s.mu.Lock()
	if st.pendingSince.IsZero() {
		st.pendingSince = now
	}
	quietAt := maxTime(s.lastCommandEnd(dev.ID), ownEnd).Add(cpuQuietWindow)
	deadline := st.pendingSince.Add(cpuQuietMaxWait)
	if now.Before(quietAt) && now.Before(deadline) {
		s.mu.Unlock()
		at := minTime(quietAt, deadline)
		s.reg.set(dev.ID, devclass.TierCPU, at)
		sooner(at)
		return
	}
	st.slot, st.pendingSince, st.busy = slot, time.Time{}, true
	s.mu.Unlock()
	s.reg.set(dev.ID, devclass.TierCPU, next)
	s.spawn(func() { s.sample(dev, run) })
}

// tickPost runs dev's post-poll sample when it is due and a Tier 0 read
// ended cpuPostDelay ago. While it is due and no such read is at hand, the
// loop looks again every second so the delay is met to the second.
func (s *CPUSampler) tickPost(dev queries.Device, interval time.Duration, now time.Time, sooner func(time.Time)) {
	if !s.postClock.due(dev.ID, devclass.TierCPUPost, interval, now) {
		return
	}
	s.mu.Lock()
	st := s.stateLocked(dev.ID)
	busy, followed := st.busy, st.postT0
	s.mu.Unlock()
	if busy {
		sooner(now.Add(cpuFastTick))
		return
	}
	t0 := s.tier0At(dev.ID)
	age := now.Sub(t0)
	switch {
	case t0.IsZero() || !t0.After(followed) || age >= cpuPostDelay+cpuPostSlack || age < 0:
		sooner(now.Add(cpuFastTick)) // wait for the next read
		return
	case age < cpuPostDelay:
		sooner(t0.Add(cpuPostDelay))
		return
	}
	run := s.runner(dev.ID)
	if run == nil {
		s.postClock.retryAt(dev.ID, devclass.TierCPUPost, now.Add(noClientRetry))
		return
	}
	s.mu.Lock()
	st.postT0, st.busy = t0, true
	s.mu.Unlock()
	s.spawn(func() { s.samplePost(dev, run, interval) })
}

// done marks the end of a sample command to deviceID at end.
func (s *CPUSampler) done(deviceID string, end time.Time) {
	s.mu.Lock()
	if st := s.devs[deviceID]; st != nil {
		st.busy = false
		st.ownEnd = end
	}
	s.mu.Unlock()
}

// recoverSample keeps a panic in one device's sample from taking the loop
// (or the process) down.
func (s *CPUSampler) recoverSample(dev queries.Device) {
	if r := recover(); r != nil {
		log.Printf("poller cpu: panic sampling %s (%s): %v\n%s", dev.Identity, dev.Address, r, debug.Stack())
		s.done(dev.ID, s.now())
		if s.pool != nil {
			s.pool.Close(dev.ID)
		}
	}
}

// sample takes dev's quiet-window sample: into the recorder, onto the device
// row, and out on device.health. A failed read is no sample — the minute
// simply has none.
func (s *CPUSampler) sample(dev queries.Device, run routeros.Runner) {
	defer s.recoverSample(dev)
	res, err := routeros.ReadLoadSample(run)
	end := s.now().UTC()
	s.done(dev.ID, end)
	if err != nil {
		s.logThrottled("cpu:"+dev.ID, "poller cpu: load sample of %s: %v", nonempty(dev.Identity, dev.Address), err)
		return
	}
	s.load.AddSample(dev.ID, end, res)

	// The device row: what the dashboard and the device page show. Status
	// and last_seen stay the liveness check's.
	var memUsed *int64
	payload := map[string]interface{}{
		"device_id": dev.ID,
		"status":    "online",
		"cpu_load":  res.CPULoad,
	}
	if res.HasFreeMemory && dev.MemoryTotal != nil && *dev.MemoryTotal > 0 {
		used := max(*dev.MemoryTotal-res.FreeMemory, 0)
		memUsed = &used
		payload["memory_pct"] = int(used * 100 / *dev.MemoryTotal)
	}
	if res.Uptime != "" {
		payload["uptime"] = res.Uptime
	}
	if s.db != nil {
		if err := queries.UpdateDeviceLoad(s.db, dev.ID, res.CPULoad, memUsed, res.Uptime); err != nil {
			s.logThrottled("store:"+dev.ID, "poller cpu: store load of %s: %v", nonempty(dev.Identity, dev.Address), err)
		}
	}
	if s.hub != nil {
		s.hub.Publish("device.health", payload)
	}
}

// samplePost takes dev's post-poll sample. The attempt counts as the tier's
// run either way: the next one is due an interval later.
func (s *CPUSampler) samplePost(dev queries.Device, run routeros.Runner, interval time.Duration) {
	defer s.recoverSample(dev)
	cpu, err := routeros.ReadCPULoad(run)
	end := s.now().UTC()
	s.done(dev.ID, end)
	s.postClock.ran(dev.ID, devclass.TierCPUPost, interval, end)
	if err != nil {
		s.logThrottled("post:"+dev.ID, "poller cpu: post-poll sample of %s: %v", nonempty(dev.Identity, dev.Address), err)
		return
	}
	s.load.AddPostSample(dev.ID, end, cpu)
}

// logThrottled logs at most once per portStatsLogEvery per key.
func (s *CPUSampler) logThrottled(key, format string, args ...any) {
	now := s.now()
	s.mu.Lock()
	last, seen := s.lastLog[key]
	if seen && now.Sub(last) < portStatsLogEvery {
		s.mu.Unlock()
		return
	}
	s.lastLog[key] = now
	s.mu.Unlock()
	log.Printf(format, args...)
}
