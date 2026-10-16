package devclass

import "time"

// LiveCaps are the ceilings on the UI-driven live stream of one device: the
// per-interface /interface/monitor-traffic samples the traffic page's port
// detail opens (design §9.5). Every other live view (the map's link overlay
// and port grid) reads the port-stats snapshot and costs the device nothing.
//
// A lease is one viewer — one browser connection — streaming from the
// device; every lease on a device shares one sampling loop, so the device
// load is the union of streamed interfaces read once per period.
type LiveCaps struct {
	// MinPeriod is the shortest sampling period a stream may run at.
	MinPeriod time.Duration
	// MaxIfaces caps the interfaces streamed from the device at once,
	// counted across every viewer (the union one sample reads).
	MaxIfaces int
	// Leases caps how many viewers may stream from the device at once.
	Leases int
	// AdminOnly refuses the stream to every role but admin.
	AdminOnly bool
	// FastCounters is the shortest counter interval a fast-counters lease
	// may shorten a visible device's Tier 0 read to; 0 means the class gets
	// no such lease. Nothing grants one yet (P0-5b); the row records the
	// ceiling that work must stay under.
	FastCounters time.Duration
}

// liveCaps holds the per-class ceilings. C1 is what the budget allows on a
// single 650 MHz MIPS core: one lease of at most four interfaces at 15 s is
// four monitor-traffic reads a minute on top of the ≈4 scheduled ones,
// inside the class's 12 commands/min. C2 and C3 are first cuts, tuned in
// P0-5b; C3 keeps the 1 s stream the page has always had.
var liveCaps = map[Class]LiveCaps{
	C1: {MinPeriod: 15 * time.Second, MaxIfaces: 4, Leases: 1, AdminOnly: true},
	C2: {MinPeriod: 5 * time.Second, MaxIfaces: 8, Leases: 2, FastCounters: 10 * time.Second},
	C3: {MinPeriod: time.Second, MaxIfaces: 16, Leases: 4, FastCounters: 5 * time.Second},
}

// LiveCapsOf returns the class's live-stream ceilings; C0 (unprobed) and
// invalid classes get C1's.
func LiveCapsOf(c Class) LiveCaps {
	return liveCaps[c.Budgeted()]
}

// EffectiveLiveCaps are the ceilings for a device of class c on profile p:
// the light profile asks for C1 treatment whatever the class, for live views
// as for the scheduled cadences (see EffectiveInterval).
func EffectiveLiveCaps(c Class, p Profile) LiveCaps {
	if p == ProfileLight {
		return LiveCapsOf(C1)
	}
	return LiveCapsOf(c)
}

// Period is the sampling period a stream asking for requested actually runs
// at: never shorter than MinPeriod.
func (l LiveCaps) Period(requested time.Duration) time.Duration {
	return max(requested, l.MinPeriod)
}
