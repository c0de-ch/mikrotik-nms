// Package flowguard holds the flow-export guardrails that are pure decisions
// and text: whether export may be set up on a device, whether a sender must
// never be accepted, whether one may be accepted automatically, and the
// commands and rollback steps the setup hints and the flow_export_on_switch
// alert show. It reads nothing and reaches no device.
package flowguard

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/firmware"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// FindingExportOnSwitch is the device_findings kind (and the alert rule name)
// for flow datagrams that arrive from the address of a managed device that
// must not export: the flow collector writes the finding, the alert rule
// reads it. Its severity is warn while unconfirmed and critical once a read
// of the device's traffic-flow settings confirmed the export.
const FindingExportOnSwitch = "flow_export_on_switch"

// FindingDetail is the stable detail of that finding for a sender address:
// one row per device and address, whatever the confirmation says.
func FindingDetail(address string) string { return "flow datagrams from " + address }

// AutoAcceptRouter is the one value of flow_auto_accept_classes that turns
// auto-accept on; anything else (the default is empty) is off.
const AutoAcceptRouter = "router"

// ParseAutoAccept normalises the flow_auto_accept_classes setting: "router",
// or "" (off) for everything else.
func ParseAutoAccept(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), AutoAcceptRouter) {
		return AutoAcceptRouter
	}
	return ""
}

// Verdict says whether flow export may be set up on a device.
type Verdict struct {
	// Eligible: the commands that enable export may be shown for it.
	Eligible bool
	// Warnings say why not, or — for an eligible weak router — what to
	// watch. Empty only for a strong router.
	Warnings []string
}

// Judge decides whether flow export may be suggested for a device: only for
// an L3 boundary (router, weak-router) budgeted as C3 or C2. A device
// budgeted as C1 — C1 hardware, an unprobed device, the light profile, an
// admin's C1 override — is never eligible, whatever its role.
func Judge(name string, r topology.DeviceRole) Verdict {
	if !r.Role.RoutesTraffic() {
		return Verdict{Warnings: []string{"No commands that enable flow export are shown for " + name + ": " + r.Reason + "."}}
	}
	switch {
	case r.Probed == devclass.C0Unknown:
		return Verdict{Warnings: []string{name + " has not been classified yet: flow export is only set up on a device " +
			"whose CPU class is known. Try again after its next inventory read."}}
	case r.Light:
		return Verdict{Warnings: []string{name + " is on the light polling profile, so the NMS treats it as a weak " +
			"device: flow export is not set up on it."}}
	case r.Budget == devclass.C1 && r.Overridden:
		return Verdict{Warnings: []string{name + " is budgeted as class C1 by an admin override: flow export is not " +
			"set up on a device treated as weak."}}
	case r.Budget == devclass.C1:
		return Verdict{Warnings: []string{name + " is a class C1 (weak) device: flow export is never set up on the " +
			"weakest class — every exported packet costs CPU it does not have."}}
	case r.Budget == devclass.C3 && r.Role == devclass.RoleRouter:
		return Verdict{Eligible: true}
	}
	why := "is a small device (class C2)"
	if r.Overridden {
		why = "is budgeted as class C2 by an admin override"
	}
	return Verdict{Eligible: true, Warnings: []string{name + " " + why + ": flow export costs CPU for every routed " +
		"packet. Enable it in a quiet period, watch the CPU on its device page, and roll back if it rises."}}
}

// NeverExports reports a device that must not export flows at all, with the
// reason shown as "blocked: <reason>": switch hardware, an access point or
// CPE behind a router, or C1 hardware. It reads the probed class, not the
// budget: a router an admin merely polls gently is not blocked or alarmed.
func NeverExports(name string, r topology.DeviceRole) (bool, string) {
	switch {
	case r.Role.NeverExports():
		return true, r.Reason
	case r.Probed == devclass.C1:
		return true, name + " is a class C1 (weak) device: flow export costs CPU for every packet, and the weakest " +
			"class has none to spare"
	}
	return false, ""
}

// AutoAccept reports whether a managed device's first datagram may create an
// exporter by itself under the flow_auto_accept_classes setting: only with
// "router", and only for a router-role device budgeted as C3 that has a
// fresh internet or VPN egress of its own.
func AutoAccept(classes string, r topology.DeviceRole) bool {
	return ParseAutoAccept(classes) == AutoAcceptRouter && r.Role == devclass.RoleRouter &&
		r.Budget == devclass.C3 && r.Egress != devclass.EgressNone
}

// ---- setup text ----

const (
	// Placeholder stands for the collector address while the admin has not
	// set flow_advertise_address: the hints never guess it.
	Placeholder = "<NMS-IP>"
	// InterfacesPlaceholder stands for the interface list the operator has
	// to type when the NMS cannot name the routed interfaces.
	InterfacesPlaceholder = "<routed-interfaces>"
)

func collectorOr(address string) string {
	if strings.TrimSpace(address) == "" {
		return Placeholder
	}
	return address
}

// CheckCommands are the read-only commands that show whether a device
// exports flows. They are the only commands shown for a device that is not
// eligible.
func CheckCommands() []string {
	return []string{"/ip traffic-flow print", "/ip traffic-flow target print"}
}

// plainName reports an interface name that needs no quoting in a RouterOS
// list.
func plainName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == '+':
		default:
			return false
		}
	}
	return true
}

// ApplyCommands are the two commands that enable IPFIX export toward the
// collector. interfaces are the routed interfaces to account (never "all":
// that also accounts bridged ports and costs CPU for traffic nobody asked
// about); without a usable list the command carries InterfacesPlaceholder
// for the operator to fill in. srcAddress pins the export source to the
// address the NMS manages the device by ("" = leave it to RouterOS).
func ApplyCommands(collector string, port int, srcAddress string, interfaces []string) []string {
	target := "/ip traffic-flow target add dst-address=" + collectorOr(collector) + " port=" + strconv.Itoa(port)
	if srcAddress != "" {
		target += " src-address=" + srcAddress
	}
	target += " version=ipfix v9-template-timeout=1m"
	list := InterfacesPlaceholder
	if len(interfaces) > 0 {
		ok := true
		for _, n := range interfaces {
			ok = ok && plainName(n)
		}
		if ok {
			list = strings.Join(interfaces, ",")
		}
	}
	return []string{target,
		"/ip traffic-flow set enabled=yes interfaces=" + list + " active-flow-timeout=1m inactive-flow-timeout=15s"}
}

// GraphingAdvice is the note every setup hint carries: per-port history is
// the NMS's job, not the switch's.
func GraphingAdvice(days1m, days1h int) string {
	return fmt.Sprintf("Do not enable Tools → Graphing on switches; the NMS keeps per-port history (1 min for %d d, 1 h for %d d).",
		days1m, days1h)
}

// ---- rollback ----

// fixedSince is the first RouterOS release whose changelog names the fix:
// 7.25, "traffic-flow - fix a kernel failure when disabling traffic flow on
// a busy system". The 7.25 pre-releases (the 7.25beta3 changelog has
// "improved system stability when disabling traffic flow on a busy system")
// are deliberately treated as not fixed: the careful text costs a
// pre-release nothing, the short one could cost it a reboot.
var fixedSince = firmware.Version{Nums: []int{7, 25}, Pre: 2} // 7.25 (release)

// DisableIsSafe reports whether a RouterOS version has that fix. known is
// false when the version cannot be parsed; it is then treated as not fixed.
func DisableIsSafe(rosVersion string) (fixed, known bool) {
	v, ok := firmware.ParseVersion(rosVersion)
	if !ok {
		return false, false
	}
	return v.Compare(fixedSince) >= 0, true
}

// RollbackStep is one numbered step of the rollback.
type RollbackStep struct {
	Text     string   `json:"text"`
	Commands []string `json:"commands"` // never nil
	// Warning is shown before the commands of the one risky step.
	Warning string `json:"warning,omitempty"`
	// Optional: do it only if it is still needed after the step before.
	Optional bool `json:"optional,omitempty"`
}

// RollbackPlan is how to stop a device's flow export toward this collector.
type RollbackPlan struct {
	// Fixed: the device's RouterOS has the 7.25 fix, so the plan carries no
	// kernel-failure warning and no download step.
	Fixed bool           `json:"fixed"`
	Note  string         `json:"note"`
	Steps []RollbackStep `json:"steps"`
}

// Rollback builds the rollback for a device on rosVersion that exports to
// collector:port. The order is fixed: profile first (the decision point),
// disable the target, then — only if still needed — switch traffic-flow off,
// remove the target, verify. Before RouterOS 7.25 switching traffic-flow off
// on a busy system can crash the kernel, so that step carries the warning
// and downloads the update first; an unknown version gets the same text.
// Every [find] matches the one target and nothing else.
func Rollback(rosVersion, collector string, port int) RollbackPlan {
	find := "[find dst-address=" + collectorOr(collector) + " port=" + strconv.Itoa(port) + "]"
	const profile = "/tool profile duration=30"
	fixed, known := DisableIsSafe(rosVersion)
	plan := RollbackPlan{Fixed: fixed}
	version := strings.TrimSpace(rosVersion)

	decide := RollbackStep{Commands: []string{profile}}
	off := RollbackStep{Optional: true}
	if fixed {
		plan.Note = "RouterOS " + version + " has the fix for disabling traffic-flow on a busy system (7.25)."
		decide.Text = "Decide first. Run the profiler over SSH and look at the traffic-flow and traffic-accounting " +
			"rows: they are what the export costs this device."
		off.Text = "Only if nothing else on the device uses traffic-flow: switch it off."
		off.Commands = []string{"/ip traffic-flow set enabled=no"}
	} else {
		plan.Note = "RouterOS " + version + " is older than 7.25: switching traffic-flow off on a busy system can " +
			"crash the kernel (fixed in 7.25)."
		if !known {
			plan.Note = "The device's RouterOS version is unknown, so it is treated as older than 7.25: switching " +
				"traffic-flow off on a busy system can crash the kernel (fixed in 7.25)."
		}
		decide.Text = "Decide first. Run the profiler over SSH. If the traffic-flow and traffic-accounting rows are " +
			"near 0 %, the export costs nothing worth the risk: disable and remove only the target (steps 1 and 3) " +
			"and leave enabled=yes with no target until the next planned upgrade to RouterOS 7.25."
		off.Text = "Only if enabled=no is still needed: download the update first, so that a kernel-failure reboot " +
			"installs the new version and the planned reboot is the only one. Then, in a maintenance window with a " +
			"console ready, switch traffic-flow off."
		off.Warning = "Before RouterOS 7.25, disabling traffic-flow on a busy system can crash the kernel and reboot " +
			"the device. Safe Mode does not protect against a kernel crash."
		off.Commands = []string{"/system package update download", "/ip traffic-flow set enabled=no"}
	}
	plan.Steps = []RollbackStep{
		decide,
		{Text: "Disable the target that points at this collector — only that one.",
			Commands: []string{"/ip traffic-flow target disable " + find}},
		off,
		{Text: "Remove the target.", Commands: []string{"/ip traffic-flow target remove " + find}},
		{Text: "Verify: the target list no longer names this collector, and the traffic-flow rows are gone from the profile.",
			Commands: []string{"/ip traffic-flow target print", profile}},
	}
	return plan
}

// DefaultPort is the collector port the hints use when none is configured.
const DefaultPort = 2055

// RollbackTarget picks the one traffic-flow target the rollback names. The
// device's own enabled targets (confirmed: what a read of the device found)
// win when they settle it — the one that is the advertise address, or the
// only one there is — because the [find] must match what is configured on
// the device. Otherwise it is the admin-approved advertise address with the
// collector's listen port. address is "" when nothing settles it (the hint
// then carries Placeholder): it is never guessed.
func RollbackTarget(advertise string, listenPort int, confirmed []netip.AddrPort) (address string, port int) {
	port = listenPort
	if port <= 0 || port > 65535 {
		port = DefaultPort
	}
	adv, err := netip.ParseAddr(strings.TrimSpace(advertise))
	if err == nil {
		adv = adv.Unmap()
		address = adv.String()
		for _, t := range confirmed {
			if t.Addr().Unmap() == adv {
				return address, int(t.Port())
			}
		}
	}
	if len(confirmed) == 1 {
		return confirmed[0].Addr().Unmap().String(), int(confirmed[0].Port())
	}
	return address, port
}

// Lines renders the plan as numbered plain-text lines, for an alert or a
// mail: the step text, its warning, then its commands indented.
func (p RollbackPlan) Lines() []string {
	lines := make([]string, 0, 4*len(p.Steps)+1)
	if p.Note != "" {
		lines = append(lines, p.Note)
	}
	for i, s := range p.Steps {
		lines = append(lines, strconv.Itoa(i)+". "+s.Text)
		if s.Warning != "" {
			lines = append(lines, "   WARNING: "+s.Warning)
		}
		for _, c := range s.Commands {
			lines = append(lines, "   "+c)
		}
	}
	return lines
}
