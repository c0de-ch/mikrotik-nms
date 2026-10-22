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
	// TierCPU is Tier 1: /system/resource/print cpu-load,free-memory,uptime.
	TierCPU
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
	tierCount
)

// tierNames are the identifiers used in logs, metrics and settings.
var tierNames = [...]string{
	TierCounters:          "counters",
	TierCPU:               "cpu",
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
	},
	C2: {
		TierCounters:          30 * time.Second,
		TierCPU:               60 * time.Second,
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
	},
	C3: {
		TierCounters:          15 * time.Second,
		TierCPU:               60 * time.Second,
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
	step := int64(phase%jitterSteps) - (jitterSteps-1)/2 // -1000 … +1000
	jitter := time.Duration(int64(interval) / 10000 * step)
	return end.Add(interval + jitter)
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
