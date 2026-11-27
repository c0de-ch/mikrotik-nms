package hygiene

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Severities, as device_findings stores them.
const (
	Info     = "info"
	Warn     = "warn"
	Critical = "critical"
)

func severityRank(s string) int {
	switch s {
	case Critical:
		return 3
	case Warn:
		return 2
	case Info:
		return 1
	}
	return 0
}

// Finding kinds the audit owns. A kind a pass evaluated and did not report
// is gone from the device: its stored finding is removed.
const (
	KindGraphing          = "graphing_enabled"
	KindTrafficFlow       = "traffic_flow_enabled"
	KindTrafficFlowTarget = "traffic_flow_foreign_target"
	KindSNMPOpen          = "snmp_open"
	KindSNMPWrite         = "snmp_write_access"
	KindSNMPWeak          = "snmp_on_weak_device"
	KindBridgeIPFirewall  = "bridge_ip_firewall"
	KindCPUForwarding     = "bridge_cpu_forwarding"
	KindBondNotOffloaded  = "bond_not_offloaded"
	KindPortNotOffloaded  = "bridge_port_not_offloaded"
	KindBridgeHorizon     = "bridge_horizon_set"
	KindMultipleBridges   = "multiple_hw_bridges"
	KindMirrorToCPU       = "switch_mirror_to_cpu"
	KindSwitchRulesToCPU  = "switch_rules_to_cpu"
	KindFlowControl       = "flow_control_enabled"
	KindLoopProtectTrunk  = "loop_protect_on_trunk"
	KindSwitchFilterRules = "switch_filter_rules"
	KindDebugLogging      = "debug_logging"
	KindFastScheduler     = "fast_scheduler"
	KindSnifferRunning    = "sniffer_running"
	KindCPUServices       = "cpu_services_on_switch"
	KindNMSAccount        = "nms_account_privileged"
	KindDeviceMode        = "device_mode_permissive"
	KindUnreadable        = "audit_unreadable"
	// KindParallelAPIPoller is shared with the login watch and not owned by
	// the audit: a pass may raise it, and never removes it.
	KindParallelAPIPoller = "parallel_api_poller"
)

// Kinds lists the finding kinds the audit owns, sorted.
func Kinds() []string {
	out := []string{KindGraphing, KindTrafficFlow, KindTrafficFlowTarget, KindSNMPOpen, KindSNMPWrite, KindSNMPWeak,
		KindBridgeIPFirewall, KindCPUForwarding, KindBondNotOffloaded, KindPortNotOffloaded, KindBridgeHorizon,
		KindMultipleBridges, KindMirrorToCPU, KindSwitchRulesToCPU, KindFlowControl, KindLoopProtectTrunk,
		KindSwitchFilterRules, KindDebugLogging, KindFastScheduler, KindSnifferRunning, KindCPUServices,
		KindNMSAccount, KindDeviceMode, KindUnreadable}
	sort.Strings(out)
	return out
}

// Owned reports a finding kind the audit owns (and may therefore remove).
func Owned(kind string) bool {
	for _, k := range Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

// FixStep is one step of a suggested fix. A fix is advice for an operator:
// the NMS never applies it. The rules every step follows are the ones of the
// network-health hints (frontend/src/lib/health-hints.ts):
//
//   - a command that changes a port is only shown for a port known to be an
//     end-device port — the audit's port findings are about links between
//     switches and whole devices, so they carry no command;
//   - a command names what it changes: never a bare [find];
//   - a step that interrupts traffic carries Window.
type FixStep struct {
	Text string `json:"text"`
	// Commands are RouterOS commands for the device's terminal.
	Commands []string `json:"commands,omitempty"`
	// ReadOnly: the commands only print (a check, not a change).
	ReadOnly bool `json:"read_only,omitempty"`
	// Window: the step interrupts traffic — plan a maintenance window.
	Window bool `json:"window,omitempty"`
}

// Finding is one thing a pass found on a device.
type Finding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	// Title names the finding in a few words.
	Title string `json:"title"`
	// Detail is what exactly was found. With Kind it identifies the
	// finding, so it holds configuration, not measurements.
	Detail string `json:"detail"`
	// Explanation says in plain language why it matters.
	Explanation string    `json:"explanation"`
	Fix         []FixStep `json:"fix,omitempty"`
	// Foreign marks a finding of a kind the audit does not own
	// (parallel_api_poller): it is recorded like the login watch records
	// it, and never removed by a pass.
	Foreign bool `json:"-"`
}

// FixJSON is the fix as stored with the finding ("" = none).
func (f Finding) FixJSON() string {
	if len(f.Fix) == 0 {
		return ""
	}
	b, err := json.Marshal(f.Fix)
	if err != nil {
		return ""
	}
	return string(b)
}

// Result is what Evaluate concluded.
type Result struct {
	// Findings, most severe first.
	Findings []Finding
	// Evaluated lists the owned kinds whose inputs were all at hand: one of
	// them that is not among the findings is not present on the device.
	// A kind that is not listed could not be judged — a read was refused or
	// failed, or was not part of the pass — and its stored finding stays.
	Evaluated []string
}

// Worst is the highest severity among the findings ("" = none).
func (r Result) Worst() string {
	worst := ""
	for _, f := range r.Findings {
		if severityRank(f.Severity) > severityRank(worst) {
			worst = f.Severity
		}
	}
	return worst
}

// listed joins up to max items; the rest are counted.
func listed(items []string, max int) string {
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + " and " + plural(len(items)-max, "more", "more")
}

// plural renders "1 rule" / "3 rules".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// plainValue reports a value that can stand inside double quotes in a
// RouterOS command without escaping. A command is only suggested for such a
// value; anything else is described in words.
func plainValue(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(" -_./:+@,", r):
		default:
			return false
		}
	}
	return true
}
