package devclass

import (
	"strings"
	"time"
)

// Tier is one scheduled collector read of the per-class call plan (design
// Appendix B). Each tier maps to one RouterOS print the pollers run on a
// cadence that depends on the device's class.
type Tier int

const (
	// TierCounters is Tier 0: the proplisted /interface/print that feeds
	// port minutes, link state, inventory and flow ifIndex names.
	TierCounters Tier = iota
	// TierCPU is Tier 1: /system/resource/print cpu-load,free-memory,uptime,
	// taken in a quiet window.
	TierCPU
	// TierCPUPost is 1c: the post-poll sample, /system/resource/print
	// cpu-load 3 s after a Tier 0 read — the NMS's own effect on the CPU.
	TierCPUPost
	// TierLogs is the shared /log/print reader (wifi and bridge parsers).
	TierLogs
	// TierBridgePorts is 2a: the proplisted /interface/bridge/port/print
	// (STP state), with 2b/2c sharing its cadence.
	TierBridgePorts
	// TierBridgeVLAN is 3a: /interface/bridge/vlan/print.
	TierBridgeVLAN
	// TierBridgeHosts is 3b: the bridge FDB (/interface/bridge/host).
	TierBridgeHosts
	// TierRelations is 3c: bonding, VLAN and PPPoE-client prints.
	TierRelations
	// TierNeighbors is 3d: /ip/neighbor/print.
	TierNeighbors
	// TierRoutes is 3e: the default-route print.
	TierRoutes
	// TierEthernet is 4a: the proplisted /interface/ethernet/print.
	TierEthernet
	// TierARP is 5a: /ip/arp/print.
	TierARP
	// TierDHCPLeases is 5b: /ip/dhcp-server/lease/print (C3 only).
	TierDHCPLeases
	// TierWifiRegistrations is 5c: the wifi registration table.
	TierWifiRegistrations
	// TierInventory is 6: static resource, routerboard, packages, addresses.
	TierInventory
	// TierAudit is 7: the scheduled hygiene audit — a pass of read-only,
	// proplisted configuration prints, spaced out on the device.
	TierAudit
	tierCount
)

// tierNames are the identifiers used in logs, metrics and settings.
var tierNames = [...]string{
	TierCounters:          "counters",
	TierCPU:               "cpu",
	TierCPUPost:           "cpu_post",
	TierLogs:              "logs",
	TierBridgePorts:       "bridge_ports",
	TierBridgeVLAN:        "bridge_vlan",
	TierBridgeHosts:       "bridge_hosts",
	TierRelations:         "relations",
	TierNeighbors:         "neighbors",
	TierRoutes:            "routes",
	TierEthernet:          "ethernet",
	TierARP:               "arp",
	TierDHCPLeases:        "dhcp_leases",
	TierWifiRegistrations: "wifi_registrations",
	TierInventory:         "inventory",
	TierAudit:             "audit",
}

// String returns the tier's identifier.
func (t Tier) String() string {
	if t >= 0 && t < tierCount {
		return tierNames[t]
	}
	return "unknown"
}

// Tiers lists every tier in plan order.
func Tiers() []Tier {
	out := make([]Tier, tierCount)
	for i := range out {
		out[i] = Tier(i)
	}
	return out
}

// cadences holds the scheduled interval per class and tier (Appendix B).
// 0 means the read is not run for that class. TierLogs has no row of its own
// in the plan; it takes the bridge-port cadence, the slowest reader that
// needs log lines (the syslog intake replaces it from M2).
var cadences = map[Class][tierCount]time.Duration{
	C1: {
		TierCounters:          60 * time.Second,
		TierCPU:               60 * time.Second,
		TierCPUPost:           10 * time.Minute,
		TierLogs:              5 * time.Minute,
		TierBridgePorts:       5 * time.Minute,
		TierBridgeVLAN:        15 * time.Minute,
		TierBridgeHosts:       15 * time.Minute,
		TierRelations:         15 * time.Minute,
		TierNeighbors:         5 * time.Minute,
		TierRoutes:            5 * time.Minute,
		TierEthernet:          5 * time.Minute,
		TierARP:               30 * time.Minute,
		TierDHCPLeases:        0,
		TierWifiRegistrations: 5 * time.Minute,
		TierInventory:         60 * time.Minute,
		TierAudit:             6 * time.Hour,
	},
	C2: {
		TierCounters:          30 * time.Second,
		TierCPU:               60 * time.Second,
		TierCPUPost:           10 * time.Minute,
		TierLogs:              2 * time.Minute,
		TierBridgePorts:       2 * time.Minute,
		TierBridgeVLAN:        5 * time.Minute,
		TierBridgeHosts:       10 * time.Minute,
		TierRelations:         15 * time.Minute,
		TierNeighbors:         2 * time.Minute,
		TierRoutes:            5 * time.Minute,
		TierEthernet:          5 * time.Minute,
		TierARP:               30 * time.Minute,
		TierDHCPLeases:        0,
		TierWifiRegistrations: 60 * time.Second,
		TierInventory:         60 * time.Minute,
		TierAudit:             6 * time.Hour,
	},
	C3: {
		TierCounters:          15 * time.Second,
		TierCPU:               60 * time.Second,
		TierCPUPost:           10 * time.Minute,
		TierLogs:              2 * time.Minute,
		TierBridgePorts:       2 * time.Minute,
		TierBridgeVLAN:        5 * time.Minute,
		TierBridgeHosts:       5 * time.Minute,
		TierRelations:         5 * time.Minute,
		TierNeighbors:         2 * time.Minute,
		TierRoutes:            60 * time.Second,
		TierEthernet:          5 * time.Minute,
		TierARP:               5 * time.Minute,
		TierDHCPLeases:        5 * time.Minute,
		TierWifiRegistrations: 30 * time.Second,
		TierInventory:         60 * time.Minute,
		TierAudit:             6 * time.Hour,
	},
}

// Cadence returns the scheduled interval of tier t for class c; 0 means the
// read is not run for that class. C0 and invalid classes use the C1 plan.
func Cadence(c Class, t Tier) time.Duration {
	if t < 0 || t >= tierCount {
		return 0
	}
	return cadences[c.Budgeted()][t]
}

// Profile selects which class's cadences a device runs on.
type Profile string

const (
	// ProfileAuto runs the cadences of the device's effective class.
	ProfileAuto Profile = "auto"
	// ProfileLight runs the C1 cadences whatever the class.
	ProfileLight Profile = "light"
)

// ParseProfile returns the Profile for its string form ("" is auto); ok is
// false for an unknown value.
func ParseProfile(s string) (Profile, bool) {
	switch Profile(strings.ToLower(strings.TrimSpace(s))) {
	case "", ProfileAuto:
		return ProfileAuto, true
	case ProfileLight:
		return ProfileLight, true
	}
	return "", false
}

// Valid reports whether p is a known profile.
func (p Profile) Valid() bool {
	_, ok := ParseProfile(string(p))
	return ok && p != ""
}

// EffectiveInterval is the interval tier t runs at for a device of class c
// on profile p: the class cadence, or the C1 cadence on the light profile.
func EffectiveInterval(c Class, p Profile, t Tier) time.Duration {
	if p == ProfileLight {
		return Cadence(C1, t)
	}
	return Cadence(c, t)
}

// Due reports whether a read last run at last (zero = never) is due at now
// for the given interval. A zero or negative interval is never due.
func Due(last, now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	if last.IsZero() {
		return true
	}
	return !now.Before(last.Add(interval))
}

// jitterSteps is the resolution of the ±10 % jitter: phase%jitterSteps maps
// onto [-10 %, +10 %] in 0.01 % steps.
const jitterSteps = 2001

// NextRun schedules the next run of a loop that just finished at end: end +
// interval, shifted by up to ±10 % of the interval. The shift is a pure
// function of phase (see Phase), so the same device always lands at the
// same offset and a fleet of devices spreads out instead of bunching on the
// minute. There is no catch-up: a slow run pushes the next one out.
func NextRun(end time.Time, interval time.Duration, phase uint64) time.Time {
	if interval <= 0 {
		return end
	}
	return end.Add(Scale(interval, phase))
}

// Scale stretches d by a device's jitter factor: d shifted by up to ±10 %,
// the shift a pure function of phase — the factor NextRun applies to an
// interval. Every duration of one device's schedule scaled by it keeps its
// ratio to the others: a device runs its whole plan a little slower or faster
// than the next one, never out of step with itself.
func Scale(d time.Duration, phase uint64) time.Duration {
	step := int64(phase%jitterSteps) - (jitterSteps-1)/2 // -1000 … +1000
	return d + time.Duration(int64(d)/10000*step)
}

// Period is the period of a device's reads at interval: the interval
// stretched by the device's jitter factor (Scale). The same factor for every
// tier of a device keeps their periods in the ratio of their intervals — a
// C1 device's 15-minute reads come round exactly once every third of its
// 5-minute ones — while a fleet of devices spreads out. 0 for a non-positive
// interval.
func Period(interval time.Duration, phase uint64) time.Duration {
	if interval <= 0 {
		return 0
	}
	return Scale(interval, phase)
}

// SlotAfter returns the first slot of the grid anchor + k·period (k ≥ 0) at
// or after t — anchor itself while t is not after it. A non-positive period
// has the one slot anchor.
func SlotAfter(anchor time.Time, period time.Duration, t time.Time) time.Time {
	if period <= 0 || !t.After(anchor) {
		return anchor
	}
	k := (t.Sub(anchor) + period - 1) / period
	return anchor.Add(k * period)
}

// CatchesUpAtOnce reports whether tier t, once it has missed its slot — the
// device was paused, offline or without a session — is read as soon as it can
// be, rather than at the next slot of its grid (up to a period later). Either
// the rows it keeps are judged current by their age — links and egress for
// three neighbor and route reads of the device, its FDB rows for three dumps
// and at least 15 min, the ethernet facts (loop-protect) for one Tier 4a
// period — and a paused device's rows, held during the pause, are judged by
// that age again once it ends: a reader that waited for its slot would find
// them stale and the map would drop links, egress and gateway attachments
// that are there; or the tier is the hygiene audit, which has its own pacing
// (one read at a time, spaced by class) and would otherwise leave the
// device's findings unjudged for up to another six hours. The other tiers'
// rows are not judged by an age a pause carries over (the network-health
// prunes give every tier a whole window after a pause; the wifi reads expire
// by their own count, never while paused; logs and stacking are read whenever
// they come), so they rejoin their grid at its next slot. Tier 0 is not on a
// grid: it counts from the end of its last read and is always read at once.
func CatchesUpAtOnce(t Tier) bool {
	switch t {
	case TierCounters, TierEthernet, TierNeighbors, TierRoutes, TierBridgeHosts, TierAudit:
		return true
	}
	return false
}

// firstRunLeads is how long each tier's first read waits after a loop first
// sees the device. The first read anchors the tier's slot grid (the tier
// clock's: anchor + k·Period), so the leads are the arrangement of a device's
// reads for as long as the process runs. On the C1 plan they make three
// groups 100 s apart in the five-minute cycle — the reads at once (logs,
// Tier 4a, wifi, every 30 and 60 minutes ARP and the inventory), at 100 s the
// bridge ports, neighbors and routes, at 190 s the 15-minute FDB dump, bridge
// VLANs and stacking — and the next cycle's first group 110 s after the last.
// Stretched by a device's jitter (Scale, ±10 %) the gaps stay at least 90 s:
// with the 25 s over which the loops first look at a device (5 to 30 s after
// start-up) and a tick's lateness, the last read of one group is still more
// than a minute before the first of the next, so no calendar minute holds
// reads of two groups — groups a minute apart would share one in every
// cycle, and reads that all started at once fall
// together for good (on a CRS354: 16 commands in one minute, against a
// budget of 12). The post-poll CPU sample (one command every 10 minutes)
// starts at 4 minutes, between the third group and the next cycle's first.
// A tier not listed is read at once:
//
//   - Tier 0 (port counters) and the CPU sample, which has a slot of its own
//     every minute (SlotAt);
//   - the inventory read: it opens the device's pooled session, which every
//     other loop waits for, and it probes the class of a new device;
//   - the hygiene audit, which has its own start-up spread;
//   - the shared log read (what a device logs before the reader first reads
//     it is history, handed to nobody), the wifi tier (its first visit is
//     the capability probe), Tier 4a (the ethernet properties the audit's
//     facts need before its first pass) and the client tables (ARP, DHCP
//     leases), which a client watch finds its client's address in.
var firstRunLeads = [tierCount]time.Duration{
	TierBridgePorts: 100 * time.Second,
	TierNeighbors:   100 * time.Second,
	TierRoutes:      100 * time.Second,
	TierBridgeHosts: 200 * time.Second,
	TierBridgeVLAN:  200 * time.Second,
	TierRelations:   200 * time.Second,
	TierCPUPost:     4 * time.Minute,
}

// maxFirstRunDelay caps how long any tier's first read waits.
const maxFirstRunDelay = 10 * time.Minute

// FirstRunBound is the longest a tier's first read may wait at interval: half
// the interval, and never more than 10 minutes. A device's rows are judged
// current for three of its reads (links, egress, FDB rows) or 30 minutes and
// more (the network-health prunes), so a first read within half an interval
// of a restart finds the previous process's rows still current: a restart
// does not look like an outage.
func FirstRunBound(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return min(interval/2, maxFirstRunDelay)
}

// FirstRunDelay is how long the first read of tier t waits after a loop first
// sees a device it runs at interval: the tier's lead (firstRunLeads), at most
// FirstRunBound(interval) — on a strong device, whose reads come round in a
// minute or two, the leads fold into the first half-interval. It is the same
// for every device: what spreads a fleet is the per-device jitter of every
// later run. 0 for a tier that is read at once.
func FirstRunDelay(t Tier, interval time.Duration) time.Duration {
	if t < 0 || t >= tierCount {
		return 0
	}
	return min(firstRunLeads[t], FirstRunBound(interval))
}

// SlotAt returns the start of the slot holding t on a device's grid: slots
// are exactly interval apart and begin at a fixed offset inside the interval,
// a pure function of phase within its first three quarters (the last quarter
// stays free for a read that waits before it runs). It schedules the reads
// that must land once in every interval — the CPU sample fills one
// device-minute each — where NextRun's "end of the last run ± 10 %" would
// drift across the boundaries and leave some minutes with two samples and
// others with none. A slot that was missed is not caught up: the caller runs
// at most once per slot. For a non-positive interval it returns t.
func SlotAt(t time.Time, interval time.Duration, phase uint64) time.Time {
	if interval <= 0 {
		return t
	}
	span := uint64(interval * 3 / 4 / time.Millisecond)
	if span == 0 {
		return t.Truncate(interval)
	}
	offset := time.Duration(phase%span) * time.Millisecond
	return t.Add(-offset).Truncate(interval).Add(offset)
}

// Budget is a class's device-load ceiling per minute (design §4.4): the
// sustained and burst command counts, the modelled device cost in seconds
// and the reply rows. The scheduler (M1) enforces it; today it documents
// what the cadences were sized for.
type Budget struct {
	CommandsPerMin int
	BurstCommands  int
	CostSecPerMin  float64
	RowsPerMin     int
}

var budgets = map[Class]Budget{
	C1: {CommandsPerMin: 12, BurstCommands: 6, CostSecPerMin: 2.0, RowsPerMin: 1500},
	C2: {CommandsPerMin: 30, BurstCommands: 10, CostSecPerMin: 4.0, RowsPerMin: 5000},
	C3: {CommandsPerMin: 120, BurstCommands: 30, CostSecPerMin: 12.0, RowsPerMin: 20000},
}

// BudgetOf returns the class's ceiling; C0 and invalid classes get C1's.
func BudgetOf(c Class) Budget {
	return budgets[c.Budgeted()]
}
