package poller

import (
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// ScheduleRegistry is the in-memory view of when each device's scheduled
// reads run next, published by the loops as they plan and finish their tiers
// and read by the API for the device page ("next counters read in 42 s"). A
// tier is absent while it does not run on the device (offline, paused, not in
// its plan). Safe for concurrent use.
type ScheduleRegistry struct {
	mu   sync.Mutex
	next map[string]map[devclass.Tier]time.Time
}

// NewScheduleRegistry returns an empty registry (tests; the loops publish into
// Schedule()).
func NewScheduleRegistry() *ScheduleRegistry {
	return &ScheduleRegistry{next: make(map[string]map[devclass.Tier]time.Time)}
}

var processSchedule = NewScheduleRegistry()

// Schedule returns the registry every loop of this process publishes into.
func Schedule() *ScheduleRegistry { return processSchedule }

func (r *ScheduleRegistry) set(deviceID string, tier devclass.Tier, at time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tiers := r.next[deviceID]
	if tiers == nil {
		tiers = make(map[devclass.Tier]time.Time)
		r.next[deviceID] = tiers
	}
	tiers[tier] = at
}

func (r *ScheduleRegistry) clear(deviceID string, tier devclass.Tier) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if tiers := r.next[deviceID]; tiers != nil {
		delete(tiers, tier)
		if len(tiers) == 0 {
			delete(r.next, deviceID)
		}
	}
}

// NextRuns returns deviceID's next run per tier name (a copy).
func (r *ScheduleRegistry) NextRuns(deviceID string) map[string]time.Time {
	out := map[string]time.Time{}
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for tier, at := range r.next[deviceID] {
		out[tier.String()] = at
	}
	return out
}

// Retain forgets the devices not in keep (deleted devices).
func (r *ScheduleRegistry) Retain(keep map[string]bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.next {
		if !keep[id] {
			delete(r.next, id)
		}
	}
}

// Polling is dev's polling view for the API: the effective class and profile,
// the pause in force, and the next run of every scheduled read, to the second.
// A paused device lists none: until the pause ends only liveness runs.
func (r *ScheduleRegistry) Polling(dev queries.Device, now time.Time) *queries.DevicePolling {
	p := &queries.DevicePolling{
		Class:    dev.EffectiveClass(),
		Profile:  dev.Profile(),
		NextRuns: map[string]time.Time{},
	}
	if dev.PollingPaused(now) {
		until := dev.PollingPausedUntil.UTC()
		p.PausedUntil = &until
		return p
	}
	for tier, at := range r.NextRuns(dev.ID) {
		p.NextRuns[tier] = at.UTC().Truncate(time.Second)
	}
	return p
}

// tierKey identifies one tier of one device.
type tierKey struct {
	device string
	tier   devclass.Tier
}

// tierRun is what a tierClock remembers of one (device, tier).
type tierRun struct {
	end   time.Time // end of the last run; zero = not run since startup
	retry time.Time // non-zero: due again at this time (a run that could not start)
	// first is when the clock first looked at the tier on the device, run or
	// not: its first run is due its lead (devclass.FirstRunDelay) after that.
	first time.Time
}

// tierClock schedules the tiers one loop owns, per device. A tier that has
// not run since startup is due its lead after the clock first looked at it on
// the device (devclass.FirstRunDelay: one group of reads a minute, at most
// half the interval and 10 minutes, so a device's reads do not all start —
// and then stay — together); after that it is due when its retry time has
// come, or at devclass.NextRun(end of the last run, interval, the device's
// phase) — with the interval in force at the check, so a class, profile or
// floor change applies at once. The lead counts from that first look, whether
// the tier could run then or not: a device that was offline or paused when
// the process started is read at once when it can be, as after any outage or
// pause. Every check publishes the next run to the registry. Safe for
// concurrent use.
type tierClock struct {
	mu    sync.Mutex
	runs  map[tierKey]tierRun
	phase func(deviceID string) uint64
	// spread is a tier's first-run lead at an interval (devclass.FirstRunDelay;
	// tests that are about something else pin it to none).
	spread func(tier devclass.Tier, interval time.Duration) time.Duration
	reg    *ScheduleRegistry
}

func newTierClock(reg *ScheduleRegistry) *tierClock {
	return &tierClock{runs: make(map[tierKey]tierRun), phase: devclass.Phase, spread: devclass.FirstRunDelay, reg: reg}
}

// nextLocked is when key is due next at interval. k.mu held.
func (k *tierClock) nextLocked(key tierKey, interval time.Duration, now time.Time) time.Time {
	r := k.runs[key]
	switch {
	case !r.retry.IsZero():
		return r.retry
	case r.end.IsZero():
		first := r.first
		if first.IsZero() {
			first = now
		}
		return first.Add(k.spread(key.tier, interval))
	}
	return devclass.NextRun(r.end, interval, k.phase(key.device))
}

// due reports whether tier is due on deviceID at now. interval is the run
// interval (runInterval: 0 while paused or when the tier is not in the plan),
// for which the tier is never due and drops out of the registry.
func (k *tierClock) due(deviceID string, tier devclass.Tier, interval time.Duration, now time.Time) bool {
	key := tierKey{deviceID, tier}
	k.mu.Lock()
	if r := k.runs[key]; r.first.IsZero() {
		r.first = now
		k.runs[key] = r
	}
	if interval <= 0 {
		k.mu.Unlock()
		k.reg.clear(deviceID, tier)
		return false
	}
	next := k.nextLocked(key, interval, now)
	k.mu.Unlock()
	k.reg.set(deviceID, tier, next)
	return !now.Before(next)
}

// ran records a run of tier that ended at end: the next one is due an
// interval (± jitter) later.
func (k *tierClock) ran(deviceID string, tier devclass.Tier, interval time.Duration, end time.Time) {
	key := tierKey{deviceID, tier}
	k.mu.Lock()
	k.runs[key] = tierRun{end: end}
	next := k.nextLocked(key, interval, end)
	k.mu.Unlock()
	if interval > 0 {
		k.reg.set(deviceID, tier, next)
	}
}

// retryAt makes tier due again at at: the run could not start (no pooled
// connection), so it is not counted as one.
func (k *tierClock) retryAt(deviceID string, tier devclass.Tier, at time.Time) {
	key := tierKey{deviceID, tier}
	k.mu.Lock()
	r := k.runs[key]
	r.retry = at
	k.runs[key] = r
	k.mu.Unlock()
	k.reg.set(deviceID, tier, at)
}

// lastEnd is when tier last ended on deviceID (zero = not since startup).
func (k *tierClock) lastEnd(deviceID string, tier devclass.Tier) time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.runs[tierKey{deviceID, tier}].end
}

// retain forgets the devices not in keep.
func (k *tierClock) retain(keep map[string]bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for key := range k.runs {
		if !keep[key.device] {
			delete(k.runs, key)
		}
	}
}

// deviceIDs is the keep-set of a device list.
func deviceIDs(devices []queries.Device) map[string]bool {
	keep := make(map[string]bool, len(devices))
	for _, d := range devices {
		keep[d.ID] = true
	}
	return keep
}
