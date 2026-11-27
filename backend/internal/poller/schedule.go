package poller

import (
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/edgeproto"
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
	retry time.Time // non-zero: due at this time, off the grid (a run that could not start)
	// first is when the clock first looked at the tier on the device, run or
	// not: the tier's grid is anchored its lead after that.
	first time.Time
	// anchor, when set (anchorAt), anchors the grid instead.
	anchor time.Time
	// rejoin is the slot a tier that missed whole slots is due at, on the
	// grid of rejoinPeriod; cleared by the next run.
	rejoin       time.Time
	rejoinPeriod time.Duration
	// off: the tier was out of the device's plan (interval 0: paused, or a
	// tier that runs only at times) at the last check.
	off bool
}

// tierClock schedules the tiers one loop owns, per device, on absolute slots.
// A tier's grid on a device is anchor + k·period: the anchor is its first
// run — its lead (devclass.FirstRunDelay, stretched by the device's jitter
// factor) after the clock first looked at it on the device, run or not — and
// the period its interval stretched by the same factor (devclass.Period).
// Every tier of a device therefore keeps its place relative to the others for
// as long as the process runs: a run that starts late (the loop's 5 s tick,
// the reads before it) or takes long does not move the next one, where
// counting from the end of each run let that lateness add up and the groups
// of reads drift back into shared minutes.
//
// After a run the tier is due at the first slot at least half a period after
// the run's end (nextOnGrid): no catch-up — at most one run per slot, never
// two within half a period — and a run that ends more than half a period
// after its slot skips the next one. A tier that missed a whole period (the
// device was paused, offline, without a session) is read at once if its rows
// are judged by their age (devclass.CatchesUpAtOnce: neighbors, routes, the
// FDB dump, Tier 4a) and otherwise at the first slot of its grid after the
// moment it can run again, at most a period later; either way the run after
// it is back on its grid. A retry (retryAt: the run could not
// start) is a one-off time off the grid. The grid follows the interval in
// force at the check: a class, profile or floor change switches to the grid
// of the new interval — anchored the same way, so the device's reads take the
// places the new plan gives them — and the next run is its first slot at
// least half a period after the last run. Every check publishes the next run
// to the registry. Safe for concurrent use.
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

// nextOnGrid is when a tier on the grid anchor + k·period runs next: its
// first slot (the anchor) when it has not run (end zero), else the first slot
// at least half a period after end. missed reports that the tier missed its
// slot: a whole period has gone by at now since it without a run (an outage,
// no session), or — resumed — the slot passed while the tier was not in the
// device's plan (a pause; a tier that runs only at times, as the wifi tier of
// a switch for its capability probe). A tier that catches up at once is then
// due now, any other at the first slot of its grid at or after now.
func nextOnGrid(anchor time.Time, period time.Duration, end, now time.Time, catchUp, resumed bool) (next time.Time, missed bool) {
	next = anchor
	if !end.IsZero() {
		next = devclass.SlotAfter(anchor, period, end.Add(period/2))
	}
	if now.Before(next.Add(period)) && !(resumed && now.After(next)) {
		return next, false
	}
	if catchUp {
		return now, true
	}
	return devclass.SlotAfter(anchor, period, now), true
}

// nextLocked is when key is due next at interval; resumed: the tier was out
// of the plan (interval 0) at the check before. A tier that missed its slot
// and rejoins its grid remembers the slot it rejoins at (tierRun.rejoin):
// recomputed at a later check, the first slot at or after that check would
// move on each time a tick missed the slot by a moment. k.mu held.
func (k *tierClock) nextLocked(key tierKey, interval time.Duration, now time.Time, resumed bool) time.Time {
	r := k.runs[key]
	if !r.retry.IsZero() {
		return r.retry
	}
	phase := k.phase(key.device)
	period := devclass.Period(interval, phase)
	if !r.rejoin.IsZero() && r.rejoinPeriod == period {
		return r.rejoin
	}
	anchor := r.anchor
	if anchor.IsZero() {
		first := r.first
		if first.IsZero() {
			first = now
		}
		anchor = first.Add(devclass.Scale(k.spread(key.tier, interval), phase))
	}
	catchUp := devclass.CatchesUpAtOnce(key.tier)
	next, missed := nextOnGrid(anchor, period, r.end, now, catchUp, resumed)
	if missed && !catchUp {
		r.rejoin, r.rejoinPeriod = next, period
		k.runs[key] = r
	}
	return next
}

// due reports whether tier is due on deviceID at now. interval is the run
// interval (runInterval: 0 while paused or when the tier is not in the plan),
// for which the tier is never due and drops out of the registry.
func (k *tierClock) due(deviceID string, tier devclass.Tier, interval time.Duration, now time.Time) bool {
	key := tierKey{deviceID, tier}
	k.mu.Lock()
	r := k.runs[key]
	if r.first.IsZero() {
		r.first = now
	}
	resumed := r.off && interval > 0
	r.off = interval <= 0
	k.runs[key] = r
	if interval <= 0 {
		k.mu.Unlock()
		k.reg.clear(deviceID, tier)
		return false
	}
	next := k.nextLocked(key, interval, now, resumed)
	k.mu.Unlock()
	k.reg.set(deviceID, tier, next)
	return !now.Before(next)
}

// ran records a run of tier that ended at end: the next one is due at the
// first slot of its grid at least half a period later. A run on a tier the
// clock never looked at (the inventory read the liveness poll starts for a
// new device) anchors the grid at its end.
func (k *tierClock) ran(deviceID string, tier devclass.Tier, interval time.Duration, end time.Time) {
	key := tierKey{deviceID, tier}
	k.mu.Lock()
	r := k.runs[key]
	if r.first.IsZero() {
		r.first = end
	}
	r.end, r.retry, r.rejoin = end, time.Time{}, time.Time{}
	k.runs[key] = r
	next := k.nextLocked(key, interval, end, false)
	k.mu.Unlock()
	if interval > 0 {
		k.reg.set(deviceID, tier, next)
	}
}

// retryAt makes tier due again at at, off its grid: the run could not start
// (no pooled connection, a busy device), so it is not counted as one. The run
// after the retried one is back on the grid.
func (k *tierClock) retryAt(deviceID string, tier devclass.Tier, at time.Time) {
	key := tierKey{deviceID, tier}
	k.mu.Lock()
	r := k.runs[key]
	r.retry = at
	k.runs[key] = r
	k.mu.Unlock()
	k.reg.set(deviceID, tier, at)
}

// anchorAt anchors tier's grid on deviceID at at, instead of at its lead
// after the first look: the tier's first run is due then and every later one
// on the grid from there. The hygiene audit seeds its first pass this way
// (start-up spread, or six hours after the last pass the database holds).
func (k *tierClock) anchorAt(deviceID string, tier devclass.Tier, at time.Time) {
	key := tierKey{deviceID, tier}
	k.mu.Lock()
	r := k.runs[key]
	r.anchor = at
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

// specIDs is the keep-set of the device list a ConfigSource returned.
func specIDs(devices []edgeproto.DeviceSpec) map[string]bool {
	keep := make(map[string]bool, len(devices))
	for _, d := range devices {
		keep[d.ID] = true
	}
	return keep
}
