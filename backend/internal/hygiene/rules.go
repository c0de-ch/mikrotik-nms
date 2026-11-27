package hygiene

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/flowguard"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// rule is one row of the rule table (design §9.6): the kinds it may report,
// what it needs, and the judgement.
type rule struct {
	kinds []string
	// needs are the sections that must be known (read, absent on the
	// device, or deliberately skipped) for the rule to judge at all.
	needs []string
	// ready is any further precondition (the port facts, a previous
	// reading); nil = none. A rule that is not ready judges nothing: its
	// kinds are not evaluated and what was found before stays.
	ready func(s Sections, f Facts) bool
	// byRole: the judgement depends on the device's role — the rule applies
	// to one role only, or means something else on a switch. While the role
	// is not known (Facts.RoleMissing) the rule judges nothing: "no role"
	// is not "not a switch".
	byRole bool
	// wholeAudit: the rule reports on the audit itself, so it needs all of
	// it — every read of the audit's plan answered or deliberately not sent.
	// An audit that stopped early, or one of whose reads got no answer,
	// judges nothing here: what it did not get to may be what it reports.
	wholeAudit bool
	eval       func(s Sections, f Facts) []Finding
}

var rules = []rule{
	{kinds: []string{KindGraphing}, eval: graphing,
		needs: []string{SecGraphingInterface, SecGraphingResource, SecGraphingQueue}},
	// On C1 hardware traffic-flow is critical whatever the device does; on
	// anything else the role says whether it may export.
	{kinds: []string{KindTrafficFlow, KindTrafficFlowTarget}, eval: trafficFlow,
		needs: []string{SecTrafficFlow, SecTrafficFlowTargets},
		ready: func(_ Sections, f Facts) bool { return !f.RoleMissing || f.weak() }},
	{kinds: []string{KindSNMPWeak}, needs: []string{SecSNMP}, eval: snmpOnWeakDevice},
	{kinds: []string{KindSNMPOpen, KindSNMPWrite}, needs: []string{SecSNMP, SecSNMPCommunities}, eval: snmpCommunities},
	{kinds: []string{KindBridgeIPFirewall}, needs: []string{SecBridgeSettings}, byRole: true, eval: bridgeIPFirewall},
	{kinds: []string{KindCPUForwarding}, needs: []string{SecBridgeSettings}, byRole: true, ready: cpuForwardingReady, eval: cpuForwarding},
	{kinds: []string{KindBondNotOffloaded}, needs: []string{SecBonding}, byRole: true, eval: bondModes},
	{kinds: []string{KindPortNotOffloaded, KindBridgeHorizon, KindMultipleBridges}, byRole: true, ready: bridgePortsReady, eval: offload},
	{kinds: []string{KindMirrorToCPU}, needs: []string{SecSwitches}, eval: mirrorToCPU},
	{kinds: []string{KindSwitchRulesToCPU}, needs: []string{SecSwitches, SecSwitchRules}, eval: switchRulesToCPU},
	{kinds: []string{KindFlowControl}, byRole: true, ready: ethernetReady, eval: flowControl},
	{kinds: []string{KindLoopProtectTrunk}, ready: ethernetReady, eval: loopProtectOnTrunks},
	{kinds: []string{KindSwitchFilterRules}, needs: []string{SecFirewallForward, SecBridgeFilter}, byRole: true, eval: switchFilterRules},
	{kinds: []string{KindDebugLogging}, needs: []string{SecLogging, SecLoggingActions}, eval: debugLogging},
	{kinds: []string{KindFastScheduler}, needs: []string{SecScheduler}, eval: fastSchedulers},
	{kinds: []string{KindSnifferRunning}, needs: []string{SecSniffer}, eval: snifferRunning},
	{kinds: []string{KindCPUServices}, byRole: true, eval: cpuServices,
		needs: []string{SecDNS, SecDHCPServers, SecCAPsMAN, SecCAPsMANLegacy}},
	{kinds: []string{KindNMSAccount}, needs: []string{SecSessions, SecUserGroups}, ready: nmsGroupKnown, eval: nmsAccount},
	{kinds: []string{KindDeviceMode}, needs: []string{SecDeviceMode}, byRole: true, eval: deviceMode},
	{kinds: []string{KindParallelAPIPoller}, needs: []string{SecSessions}, eval: foreignSessions,
		ready: func(_ Sections, f Facts) bool { return len(f.NMSAddresses) > 0 }},
	{kinds: []string{KindUnreadable}, wholeAudit: true, eval: unreadable},
}

// Evaluate judges what a pass read. It is a pure function of the answers and
// the facts: the same input always yields the same findings.
func Evaluate(mode Mode, s Sections, f Facts) Result {
	var res Result
	whole := mode == ModeAudit && s.answered(Plan(ModeAudit))
	for _, r := range rules {
		if (r.wholeAudit && !whole) || (r.byRole && f.RoleMissing) || !s.known(r.needs...) || (r.ready != nil && !r.ready(s, f)) {
			continue
		}
		for _, k := range r.kinds {
			if Owned(k) {
				res.Evaluated = append(res.Evaluated, k)
			}
		}
		res.Findings = append(res.Findings, r.eval(s, f)...)
	}
	sort.SliceStable(res.Findings, func(i, j int) bool {
		a, b := res.Findings[i], res.Findings[j]
		if ra, rb := severityRank(a.Severity), severityRank(b.Severity); ra != rb {
			return ra > rb
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Detail < b.Detail
	})
	sort.Strings(res.Evaluated)
	res.Checks = checksOf(s, f)
	return res
}

func on(row map[string]string, key string) bool { return routeros.Bool(row[key]) }

// enabledRows returns the rows that are not disabled.
func enabledRows(rows []map[string]string) []map[string]string {
	var out []map[string]string
	for _, row := range rows {
		if !on(row, "disabled") {
			out = append(out, row)
		}
	}
	return out
}

// anyAddress reports an address list that lets everybody in: empty, or
// holding the IPv4 or IPv6 default prefix.
func anyAddress(list string) bool {
	if strings.TrimSpace(list) == "" {
		return true
	}
	for _, a := range strings.Split(list, ",") {
		switch strings.TrimSpace(a) {
		case "0.0.0.0/0", "::/0":
			return true
		}
	}
	return false
}

// ---- graphing ----------------------------------------------------------------

// graphing: Tools → Graphing makes the device sample its counters and draw
// graphs on its own CPU. Any enabled rule on a C1 device is critical; on
// other devices a rule is a warning when it graphs everything, writes to
// flash, or shows its graphs to any address.
func graphing(s Sections, f Facts) []Finding {
	type graphRule struct {
		menu, selector, value string // menu "interface", selector "interface", value "all"
		row                   map[string]string
	}
	var active []graphRule
	for _, row := range enabledRows(s.rows(SecGraphingInterface)) {
		active = append(active, graphRule{"interface", "interface", row["interface"], row})
	}
	for _, row := range enabledRows(s.rows(SecGraphingResource)) {
		active = append(active, graphRule{"resource", "allow-address", row["allow-address"], row})
	}
	for _, row := range enabledRows(s.rows(SecGraphingQueue)) {
		active = append(active, graphRule{"queue", "simple-queue", row["simple-queue"], row})
	}
	if len(active) == 0 {
		return nil
	}

	var described []string
	var remove []string
	costly := false
	for _, g := range active {
		parts := []string{g.menu + " rule"}
		if g.menu != "resource" && g.value != "" {
			parts = append(parts, g.selector+"="+g.value)
			if g.value == "all" {
				costly = true
			}
		}
		if on(g.row, "store-on-disk") {
			parts = append(parts, "store-on-disk=yes")
			costly = true
		}
		if anyAddress(g.row["allow-address"]) {
			parts = append(parts, "allow-address="+nonEmpty(g.row["allow-address"], "0.0.0.0/0"))
			costly = true
		}
		described = append(described, strings.Join(parts, " "))
		if plainValue(g.value) {
			remove = append(remove, fmt.Sprintf(`/tool graphing %s remove [find %s="%s"]`, g.menu, g.selector, g.value))
		}
	}
	sort.Strings(described)
	remove = dedupe(remove)

	fnd := Finding{Kind: KindGraphing, Detail: listed(described, 6)}
	removeStep := FixStep{Text: "Remove the graphing rules (the graphs kept on the device are lost; the NMS keeps " +
		"per-port history itself). Rules that are not listed here are removed the same way in Tools → Graphing."}
	if len(remove) > 0 && len(remove) <= 8 {
		removeStep.Text = "Remove the graphing rules (the graphs kept on the device are lost; the NMS keeps per-port history itself):"
		removeStep.Commands = remove
	}
	switch {
	case f.weak():
		fnd.Severity = Critical
		fnd.Title = "Graphing is enabled on a weak device"
		fnd.Explanation = "Tools → Graphing makes RouterOS sample its counters and draw graphs on the device's own CPU. " +
			f.name() + " is a weak (class C1) device: graphing alone can keep its CPU busy — a forgotten rule has pegged a " +
			"switch at 100 %. The NMS already keeps the history of every port; the device does not need to."
		fnd.Fix = []FixStep{removeStep}
	case costly:
		fnd.Severity = Warn
		fnd.Title = "Graphing rule with costly or open settings"
		fnd.Explanation = "Tools → Graphing makes RouterOS sample its counters and draw graphs on the device's own CPU. " +
			"interface=all graphs every interface, store-on-disk=yes writes the samples to flash every few minutes (flash " +
			"wears out), and allow-address=0.0.0.0/0 shows the graphs to anyone who reaches the device's web service."
		fnd.Fix = []FixStep{removeStep, {Text: "Or keep one rule for one interface, with store-on-disk=no and " +
			"allow-address set to the admin network."}}
	default:
		return nil // tuned rules on a capable device
	}
	return []Finding{fnd}
}

// ---- traffic-flow -------------------------------------------------------------

// flowState reads the traffic-flow sections into the shape the flow
// guardrails use.
func flowState(s Sections) routeros.TrafficFlowState {
	row := s.first(SecTrafficFlow)
	st := routeros.TrafficFlowState{Enabled: on(row, "enabled"), Interfaces: row["interfaces"]}
	for _, t := range s.rows(SecTrafficFlowTargets) {
		target := routeros.TrafficFlowTarget{DstAddress: t["dst-address"], Version: t["version"], Disabled: on(t, "disabled")}
		target.Port, _ = strconv.Atoi(strings.TrimSpace(t["port"]))
		st.Targets = append(st.Targets, target)
	}
	return st
}

// trafficFlow: flow export on a device that must never export (switch
// hardware, an access point or CPE, C1 hardware) is critical; on a device
// that may export, a target that is not this NMS is a warning.
func trafficFlow(s Sections, f Facts) []Finding {
	st := flowState(s)
	if !st.Enabled {
		return nil
	}
	var enabled []routeros.TrafficFlowTarget
	for _, t := range st.Targets {
		if !t.Disabled {
			enabled = append(enabled, t)
		}
	}

	if f.Role.NeverExports() || f.weak() {
		why := f.name() + " is a weak (class C1) device"
		if f.Role.NeverExports() {
			why = f.name() + " is " + roleWords(f.Role)
		}
		fnd := Finding{
			Kind: KindTrafficFlow, Severity: Critical,
			Title:  "Traffic-flow is enabled on a device that must not export flows",
			Detail: st.Summary(),
			Explanation: why + ". Flow accounting costs CPU for every packet the CPU handles, and on a device that " +
				"forwards in the switch chip or only bridges it shows next to nothing. interfaces=all also accounts " +
				"bridged ports. Stop it with the steps below, in this order.",
		}
		if len(enabled) > 0 {
			target := enabled[0]
			for _, t := range enabled {
				if f.Collector != "" && t.DstAddress == f.Collector {
					target = t
				}
			}
			plan := flowguard.Rollback(f.Version, target.DstAddress, target.Port)
			fnd.Fix = append(fnd.Fix, FixStep{Text: plan.Note})
			for _, step := range plan.Steps {
				fs := FixStep{Text: step.Text, Commands: step.Commands, Window: step.Warning != ""}
				if step.Warning != "" {
					fs.Text += " " + step.Warning
				}
				fnd.Fix = append(fnd.Fix, fs)
			}
			if len(enabled) > 1 {
				fnd.Fix = append(fnd.Fix, FixStep{Text: plural(len(enabled)-1, "other target is", "other targets are") +
					" enabled: disable and remove each the same way, naming its dst-address and port."})
			}
		} else {
			fnd.Fix = switchOffFlow(f.Version)
		}
		return []Finding{fnd}
	}

	if f.Collector == "" {
		return nil
	}
	var foreign, commands []string
	for _, t := range enabled {
		if t.DstAddress == f.Collector {
			continue
		}
		foreign = append(foreign, t.DstAddress+":"+strconv.Itoa(t.Port))
		if plainValue(t.DstAddress) {
			commands = append(commands, fmt.Sprintf("/ip traffic-flow target remove [find dst-address=%s port=%d]", t.DstAddress, t.Port))
		}
	}
	if len(foreign) == 0 {
		return nil
	}
	sort.Strings(foreign)
	return []Finding{{
		Kind: KindTrafficFlowTarget, Severity: Warn,
		Title:  "Flow export to a collector that is not this NMS",
		Detail: "targets: " + listed(foreign, 6),
		Explanation: "The device exports flow records to " + plural(len(foreign), "collector", "collectors") +
			" other than this NMS (" + f.Collector + "). Every target adds export work for each flow, and an old target " +
			"often points at a collector that no longer exists.",
		Fix: []FixStep{{Text: "If nothing listens there any more, remove the target — only that one. Removing a target " +
			"does not switch traffic-flow off:", Commands: commands}},
	}}
}

// switchOffFlow is the fix for traffic-flow that is enabled without any
// target: there is nothing to remove, only the switch itself — the step
// that can crash the kernel of a busy pre-7.25 system.
func switchOffFlow(version string) []FixStep {
	if fixed, _ := flowguard.DisableIsSafe(version); fixed {
		return []FixStep{{Text: "No target is configured, so nothing is exported — the device only accounts flows. " +
			"Switch traffic-flow off:", Commands: []string{"/ip traffic-flow set enabled=no"}}}
	}
	return []FixStep{{
		Text: "No target is configured, so nothing is exported — the device only accounts flows. Before RouterOS " +
			"7.25, switching traffic-flow off on a busy system can crash the kernel and reboot the device (Safe Mode " +
			"does not protect against that). Download the update first, so that such a reboot installs the fixed " +
			"version; then, in a maintenance window with a console ready, switch it off:",
		Commands: []string{"/system package update download", "/ip traffic-flow set enabled=no"},
		Window:   true,
	}}
}

func roleWords(r devclass.Role) string {
	switch r {
	case devclass.RoleSwitch:
		return "a switch"
	case devclass.RoleAPCPE:
		return "an access point or CPE"
	}
	return "a " + r.Label()
}

// ---- SNMP -----------------------------------------------------------------------

// snmpOnWeakDevice: any SNMP on a C1 device is worth knowing about — every
// poll is answered by its CPU.
func snmpOnWeakDevice(s Sections, f Facts) []Finding {
	if !on(s.first(SecSNMP), "enabled") || !f.weak() {
		return nil
	}
	return []Finding{{
		Kind: KindSNMPWeak, Severity: Info,
		Title:  "SNMP is enabled on a weak device",
		Detail: "SNMP enabled",
		Explanation: "Every SNMP request is answered by the device's CPU. A monitoring system that walks all " +
			"interfaces costs a weak (class C1) device about as much as the NMS's own counter read — and that load " +
			"comes on top of it. This is not a fault; it is where to look when the CPU is high.",
		Fix: []FixStep{{Text: "If nothing polls this device over SNMP, switch SNMP off:",
			Commands: []string{"/snmp set enabled=no"}}},
	}}
}

// snmpCommunities: a community that anyone may read without authentication,
// and write access. The community names are never read.
func snmpCommunities(s Sections, f Facts) []Finding {
	if !on(s.first(SecSNMP), "enabled") {
		return nil
	}
	var open, write int
	for _, c := range enabledRows(s.rows(SecSNMPCommunities)) {
		security := strings.ToLower(strings.TrimSpace(c["security"]))
		readable := c["read-access"] == "" || on(c, "read-access")
		if anyAddress(c["addresses"]) && (security == "" || security == "none") && (readable || on(c, "write-access")) {
			open++
		}
		if on(c, "write-access") {
			write++
		}
	}
	var out []Finding
	if open > 0 {
		out = append(out, Finding{
			Kind: KindSNMPOpen, Severity: Warn,
			Title:  "SNMP answers any address without authentication",
			Detail: plural(open, "community", "communities") + " with addresses=::/0 or 0.0.0.0/0 and security=none",
			Explanation: "Anyone who can reach the device and knows (or guesses) the community name can read its " +
				"configuration and counters over SNMP — and make its CPU work by polling it.",
			Fix: []FixStep{
				{Text: "Restrict each community to the hosts that poll it (set its addresses), and prefer SNMPv3 " +
					"(security=authorized or private). List the communities on the device — the NMS never reads their names:",
					Commands: []string{"/snmp community print"}, ReadOnly: true},
				{Text: "Or, if nothing polls this device over SNMP, switch SNMP off:",
					Commands: []string{"/snmp set enabled=no"}},
			},
		})
	}
	if write > 0 {
		out = append(out, Finding{
			Kind: KindSNMPWrite, Severity: Warn,
			Title:  "SNMP write access is enabled",
			Detail: plural(write, "community", "communities") + " with write-access=yes",
			Explanation: "A community with write access lets whoever knows its name change the device's " +
				"configuration over SNMP. Monitoring only needs read access.",
			Fix: []FixStep{{Text: "Take write access away, unless a tool really sets values over SNMP:",
				Commands: []string{"/snmp community set [find write-access=yes] write-access=no"}}},
		})
	}
	return out
}

// ---- bridge ---------------------------------------------------------------------

// bridgeIPFirewall: use-ip-firewall=yes hands every bridged packet to the
// IP firewall and switches the bridge fast path off. Critical on a switch;
// a warning elsewhere, where queues or firewalling of bridged traffic can
// need it.
func bridgeIPFirewall(s Sections, f Facts) []Finding {
	row := s.first(SecBridgeSettings)
	if !on(row, "use-ip-firewall") {
		return nil
	}
	detail := "use-ip-firewall=yes"
	if on(row, "use-ip-firewall-for-vlan") {
		detail += ", use-ip-firewall-for-vlan=yes"
	}
	fnd := Finding{
		Kind: KindBridgeIPFirewall, Severity: Warn,
		Title:  "Bridged traffic is sent through the IP firewall",
		Detail: detail,
		Explanation: "With use-ip-firewall=yes every bridged packet is handed to the IP firewall, and the bridge " +
			"fast path is switched off: the CPU works on traffic that would otherwise pass untouched. Some routers " +
			"need it for queues or for firewalling between bridged ports.",
		Fix: []FixStep{{
			Text: "Switch it off — after checking that no firewall rule or queue relies on seeing bridged traffic " +
				"(such rules stop matching). Never turn it on just to make traffic visible.",
			Commands: []string{"/interface bridge settings set use-ip-firewall=no use-ip-firewall-for-vlan=no"},
			Window:   true,
		}},
	}
	if f.Role == devclass.RoleSwitch {
		fnd.Severity = Critical
		fnd.Explanation = "With use-ip-firewall=yes every bridged packet is handed to the IP firewall, and the bridge " +
			"fast path is switched off. On a switch that puts the traffic the CPU has to forward — and everything " +
			"that misses the switch chip — through the firewall on a management CPU."
	}
	return []Finding{fnd}
}

// cpuForwardRate is the rate at which the fast-path counters make the rule
// speak: 1,000 packets a second on average since the previous pass. A
// starting value.
const cpuForwardRate = 1000

// minForwardSpan is the least time between two readings for a rate.
const minForwardSpan = time.Minute

func cpuForwardingReady(s Sections, f Facts) bool {
	if !f.offloadSwitch() {
		return true // the rule does not apply; it says so by reporting nothing
	}
	_, ok := forwardedPerSecond(s, f)
	return ok && f.InterfacesKnown && f.EthernetKnown && f.BridgePortsKnown
}

// forwardedPerSecond is the average rate of CPU-forwarded bridge packets
// since the previous pass; ok is false without two comparable readings
// (first pass, a reboot in between, too short a span).
func forwardedPerSecond(s Sections, f Facts) (rate float64, ok bool) {
	cur := RuntimeOf(s, f.Now)
	if f.Prev == nil || f.Prev.CPUForwarded == nil || cur.CPUForwarded == nil {
		return 0, false
	}
	span := f.Now.Sub(f.Prev.At)
	delta := *cur.CPUForwarded - *f.Prev.CPUForwarded
	if span < minForwardSpan || delta < 0 {
		return 0, false
	}
	return float64(delta) / span.Seconds(), true
}

// cpuForwarding: on a switch whose chip forwards in hardware, the bridge's
// fast-path counters only count what the CPU forwarded. A steady stream
// there means switched traffic is not in the chip — a warning, unless a
// running bridge member sits outside the chip, where CPU forwarding is
// expected (info).
func cpuForwarding(s Sections, f Facts) []Finding {
	if !f.offloadSwitch() {
		return nil
	}
	rate, _ := forwardedPerSecond(s, f)
	if rate < cpuForwardRate {
		return nil
	}
	outside := membersOutsideChip(s, f)
	measured := fmt.Sprintf("about %.0f packets a second on average since %s",
		rate, f.Prev.At.UTC().Format("2006-01-02 15:04 UTC"))
	fnd := Finding{
		Kind: KindCPUForwarding, Severity: Warn,
		Title:  "The CPU forwards bridged traffic",
		Detail: "the bridge fast path counts CPU-forwarded packets",
		Explanation: "The bridge forwarded " + measured + " on the CPU. Packets the switch chip forwards never " +
			"reach these counters, and every bridge member is a switch-chip port — so traffic that should be " +
			"switched in hardware is taking the CPU path.",
		Fix: []FixStep{{
			Text: "Find the port, feature or rule that keeps traffic off the switch chip and restore hardware " +
				"offload. A port without the H flag is forwarded by the CPU:",
			Commands: []string{"/interface bridge port print"}, ReadOnly: true,
		}},
	}
	if len(outside) > 0 {
		fnd.Severity = Info
		fnd.Detail = "the bridge fast path counts CPU-forwarded packets; members outside the switch chip: " + listed(outside, 6)
		fnd.Explanation = "The bridge forwarded " + measured + " on the CPU. That is expected here: " +
			listed(outside, 6) + " " + isAre(len(outside)) + " in the bridge but not on the switch chip, and whatever " +
			"crosses such a port is forwarded by the CPU."
		fnd.Fix = []FixStep{{Text: "Nothing to fix if that port is meant to be bridged. If the CPU is busy, keep " +
			"heavy traffic off it: a management port should carry management only."}}
	}
	return []Finding{fnd}
}

// bondSlaves maps each bond to its member ports, from the bonding section.
func bondSlaves(s Sections) map[string][]string {
	out := map[string][]string{}
	for _, row := range s.rows(SecBonding) {
		for _, m := range strings.Split(row["slaves"], ",") {
			if m = strings.TrimSpace(m); m != "" {
				out[row["name"]] = append(out[row["name"]], m)
			}
		}
	}
	return out
}

// membersOutsideChip lists the running, enabled bridge members that are not
// on a switch chip: the CPU-attached management port, wifi, VLAN, VPN and
// EoIP interfaces. A bond counts by its members (all on the chip = inside).
func membersOutsideChip(s Sections, f Facts) []string {
	chip := map[string]bool{}
	for _, p := range f.Ports {
		if p.Switch != "" {
			chip[p.Name] = true
		}
	}
	slaves := bondSlaves(s)
	var out []string
	for _, p := range f.Ports {
		if p.Bridge == "" || p.BridgeDisabled || !p.Running || p.Switch != "" {
			continue
		}
		if p.Type == "bond" {
			members, inside := slaves[p.Name], true
			for _, m := range members {
				inside = inside && chip[m]
			}
			if inside {
				continue // also a bond whose members are not known: assumed on the chip
			}
		}
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// offloadableBondModes are the bonding modes a 98DX switch chip forwards
// itself.
var offloadableBondModes = map[string]bool{"802.3ad": true, "balance-xor": true, "active-backup": true}

// bondModes: on a switch that offloads, a bond in any other mode is
// forwarded by the CPU.
func bondModes(s Sections, f Facts) []Finding {
	if !f.offloadSwitch() {
		return nil
	}
	var bad []string
	for _, row := range enabledRows(s.rows(SecBonding)) {
		if mode := strings.TrimSpace(row["mode"]); mode != "" && !offloadableBondModes[mode] {
			bad = append(bad, row["name"]+" (mode="+mode+")")
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return []Finding{{
		Kind: KindBondNotOffloaded, Severity: Critical,
		Title:  "Bond in a mode the switch chip cannot offload",
		Detail: listed(bad, 8),
		Explanation: "The switch chip of " + f.name() + " forwards bonds only in the modes 802.3ad, balance-xor and " +
			"active-backup. A bond in any other mode is forwarded by the CPU — all of its traffic.",
		Fix: []FixStep{{
			Text: "Change the bond to 802.3ad (LACP — the other end must use it too), balance-xor or active-backup. " +
				"The link is interrupted while both ends are changed.",
			Window: true,
		}},
	}}
}

func bridgePortsReady(_ Sections, f Facts) bool {
	return !f.offloadSwitch() || (f.InterfacesKnown && f.EthernetKnown && f.BridgePortsKnown)
}

// offload: on a switch whose chip offloads, a switch-chip port that is in a
// bridge with hardware offload requested, is up and carries traffic, but is
// not offloaded, is forwarded by the CPU (critical). Wifi, VLAN, VPN, EoIP
// and CPU-attached management ports are never judged: they cannot be
// offloaded. A horizon on a port and a second bridge with switch ports both
// take offload away (warnings).
func offload(_ Sections, f Facts) []Finding {
	if !f.offloadSwitch() {
		return nil
	}
	var notOffloaded, horizon []string
	bridges := map[string]bool{}
	for _, p := range f.Ports {
		if p.Switch == "" || p.Bridge == "" || p.BridgeDisabled {
			continue
		}
		bridges[p.Bridge] = true
		if h := strings.TrimSpace(p.Horizon); h != "" && h != "none" {
			horizon = append(horizon, p.Name+" (horizon="+h+")")
		}
		// hw-offload and "in the bridge" come from the same read of the
		// bridge port table; running and the rate from the counter read,
		// minutes apart. A port that was still down when the table was read
		// has hw-offload=false by nature, whatever the counters say now.
		if p.Running && !p.BridgeInactive && p.HWKnown && p.HW && p.HWOffloadKnown && !p.HWOffload && p.RateKnown && p.Bps > 0 {
			notOffloaded = append(notOffloaded, p.Name)
		}
	}
	var out []Finding
	if len(notOffloaded) > 0 {
		sort.Strings(notOffloaded)
		out = append(out, Finding{
			Kind: KindPortNotOffloaded, Severity: Critical,
			Title:  "Switch ports are forwarded by the CPU",
			Detail: listed(notOffloaded, 12),
			Explanation: plural(len(notOffloaded), "switch-chip port is", "switch-chip ports are") + " in a bridge with " +
				"hardware offload requested (hw=yes), up and carrying traffic, but not offloaded: the CPU forwards " +
				"everything that crosses " + itThem(len(notOffloaded)) + ".",
			Fix: []FixStep{
				{Text: "Find out what keeps the bridge off the switch chip. The usual causes: a second bridge with " +
					"switch ports, a horizon on a port, a bond in a mode the chip cannot offload, bridge settings the " +
					"chip does not support. Ports without the H flag are forwarded by the CPU:",
					Commands: []string{"/interface bridge port print"}, ReadOnly: true},
				{Text: "Restoring offload re-programs the switch chip: expect a short interruption.", Window: true},
			},
		})
	}
	if len(horizon) > 0 {
		sort.Strings(horizon)
		out = append(out, Finding{
			Kind: KindBridgeHorizon, Severity: Warn,
			Title:  "Bridge port with a horizon",
			Detail: listed(horizon, 12),
			Explanation: "A bridge port with a split horizon cannot be forwarded by the switch chip: RouterOS switches " +
				"hardware offload off for it, and the CPU forwards its traffic.",
			Fix: []FixStep{{Text: "Use port isolation in the switch chip instead of a bridge horizon, and set the " +
				"port's horizon back to none. The port is re-programmed when it changes.", Window: true}},
		})
	}
	if len(bridges) > 1 {
		names := make([]string, 0, len(bridges))
		for b := range bridges {
			names = append(names, b)
		}
		sort.Strings(names)
		out = append(out, Finding{
			Kind: KindMultipleBridges, Severity: Warn,
			Title:  "More than one bridge with switch ports",
			Detail: listed(names, 6),
			Explanation: "The switch chip offloads one bridge. With switch ports in " + plural(len(names), "bridge", "bridges") +
				", only one of them is forwarded in hardware; the others are forwarded by the CPU.",
			Fix: []FixStep{{Text: "Use one bridge with VLAN filtering for all switch ports instead of several " +
				"bridges. Moving ports between bridges interrupts their traffic.", Window: true}},
		})
	}
	return out
}

// ---- switch chip ------------------------------------------------------------------

// mirrorToCPU: a mirror whose target is the CPU copies every mirrored packet
// to it.
func mirrorToCPU(s Sections, _ Facts) []Finding {
	var found, commands []string
	for _, sw := range s.rows(SecSwitches) {
		for _, key := range []string{"mirror-target", "mirror-egress-target"} {
			if strings.EqualFold(strings.TrimSpace(sw[key]), "cpu") {
				d := sw["name"] + ": " + key + "=cpu"
				if src := strings.TrimSpace(sw["mirror-source"]); src != "" && src != "none" {
					d += " (mirror-source=" + src + ")"
				}
				found = append(found, d)
				if plainValue(sw["name"]) {
					commands = append(commands, fmt.Sprintf(`/interface ethernet switch set [find name="%s"] %s=none`, sw["name"], key))
				}
			}
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return []Finding{{
		Kind: KindMirrorToCPU, Severity: Critical,
		Title:  "The switch mirrors traffic to the CPU",
		Detail: listed(found, 4),
		Explanation: "A port mirror whose target is the CPU sends a copy of every mirrored packet to it. That is a " +
			"capture setup someone forgot: the CPU receives the traffic of the mirrored ports for as long as it stays.",
		Fix: []FixStep{{Text: "Remove the mirror (forwarding is not interrupted):", Commands: commands}},
	}}
}

// switchRulesToCPU: static switch rules that copy, redirect or mirror
// packets.
func switchRulesToCPU(s Sections, _ Facts) []Finding {
	var found []string
	for _, r := range enabledRows(s.rows(SecSwitchRules)) {
		if on(r, "dynamic") {
			continue
		}
		var actions []string
		for _, a := range []string{"copy-to-cpu", "redirect-to-cpu", "mirror"} {
			if on(r, a) {
				actions = append(actions, a)
			}
		}
		if len(actions) == 0 {
			continue
		}
		d := strings.Join(actions, "+")
		if ports := strings.TrimSpace(r["ports"]); ports != "" {
			d += " on " + ports
		}
		found = append(found, d)
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return []Finding{{
		Kind: KindSwitchRulesToCPU, Severity: Critical,
		Title:  "Switch rules send packets to the CPU",
		Detail: plural(len(found), "rule", "rules") + ": " + listed(found, 6),
		Explanation: "Switch rules with copy-to-cpu, redirect-to-cpu or mirror take matching packets out of the " +
			"hardware path. On a busy port that is a steady stream of packets for a management CPU.",
		Fix: []FixStep{{Text: "List the rules and remove or disable the ones that are not needed (forwarding is " +
			"not interrupted):",
			Commands: []string{"/interface ethernet switch rule print where copy-to-cpu=yes or redirect-to-cpu=yes or mirror=yes"},
			ReadOnly: true}},
	}}
}

func ethernetReady(_ Sections, f Facts) bool { return f.InterfacesKnown && f.EthernetKnown }

// manyFlowControlPorts is how many ports with flow control make the rule
// speak even when none of them is a link between switches.
const manyFlowControlPorts = 4

// flowControl: 802.3x flow control on many ports or on links between
// switches, on a switch. The finding carries no command: the ports are not
// end-device ports.
func flowControl(_ Sections, f Facts) []Finding {
	if f.Role != devclass.RoleSwitch {
		return nil
	}
	var flow, flowTrunks []string
	for _, p := range f.Ports {
		if p.Type != "ether" || !(flowControlOn(p.RxFlowControl) || flowControlOn(p.TxFlowControl)) {
			continue
		}
		flow = append(flow, p.Name)
		if p.Trunk {
			flowTrunks = append(flowTrunks, p.Name)
		}
	}
	if len(flowTrunks) == 0 && len(flow) <= manyFlowControlPorts {
		return nil
	}
	sort.Strings(flow)
	sort.Strings(flowTrunks)
	detail := "on " + plural(len(flow), "port", "ports") + ": " + listed(flow, 12)
	if len(flowTrunks) > 0 {
		detail += "; links between switches: " + listed(flowTrunks, 12)
	}
	return []Finding{{
		Kind: KindFlowControl, Severity: Warn,
		Title:  "802.3x flow control is enabled",
		Detail: detail,
		Explanation: "With flow control a slow receiver pauses the whole port — and on a link between switches " +
			"everything behind it. One stalled device can then hold up traffic that has nothing to do with it. " +
			"It is off by default for that reason.",
		Fix: []FixStep{{
			Text: "Switch flow control off on those ports (rx-flow-control=off tx-flow-control=off), the links " +
				"between switches first. Changing it can re-initialise the switch chip: every port drops for a " +
				"few seconds.",
			Window: true,
		}},
	}}
}

// loopProtectOnTrunks: loop-protect on a link between managed devices,
// whatever the device is. No command either: a trunk is not an end-device
// port.
func loopProtectOnTrunks(_ Sections, f Facts) []Finding {
	var loop []string
	for _, p := range f.Ports {
		if p.Type == "ether" && p.Trunk && strings.EqualFold(strings.TrimSpace(p.LoopProtect), "on") {
			loop = append(loop, p.Name)
		}
	}
	if len(loop) == 0 {
		return nil
	}
	sort.Strings(loop)
	return []Finding{{
		Kind: KindLoopProtectTrunk, Severity: Warn,
		Title:  "Loop-protect on a link between switches",
		Detail: listed(loop, 12),
		Explanation: "Loop-protect disables a port when its own probe frame comes back. On a link to another " +
			"managed device that takes the link — and everything behind it — down. Spanning tree protects links " +
			"between switches; loop-protect belongs on end-device ports only.",
		Fix: []FixStep{{
			Text: "Set loop-protect back to default on those ports. On CRS3xx switches changing loop-protect " +
				"re-initialises the switch chip: every port drops for about 4 seconds.",
			Window: true,
		}},
	}}
}

func flowControlOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "auto":
		return true
	}
	return false
}

// switchFilterRules: on a pure switch, forward-chain firewall rules and
// bridge filter rules only see what the CPU forwards — and invite settings
// that move switched traffic onto it. Input-chain rules are not counted.
func switchFilterRules(s Sections, f Facts) []Finding {
	if f.Role != devclass.RoleSwitch {
		return nil
	}
	forward, bridge := s.count(SecFirewallForward), s.count(SecBridgeFilter)
	if forward == 0 && bridge == 0 {
		return nil
	}
	var parts []string
	var commands []string
	if forward > 0 {
		parts = append(parts, plural(forward, "forward-chain firewall rule", "forward-chain firewall rules"))
		commands = append(commands, "/ip firewall filter print where chain=forward")
	}
	if bridge > 0 {
		parts = append(parts, plural(bridge, "bridge filter rule", "bridge filter rules"))
		commands = append(commands, "/interface bridge filter print")
	}
	return []Finding{{
		Kind: KindSwitchFilterRules, Severity: Warn,
		Title:  "Filter rules for forwarded traffic on a switch",
		Detail: strings.Join(parts, ", "),
		Explanation: "A switch forwards in the switch chip, where these rules do not apply: they only act on what " +
			"the CPU forwards, and making them effective means moving switched traffic onto the CPU. Input-chain " +
			"rules that protect the switch's own management are fine and are not counted.",
		Fix: []FixStep{{Text: "Review them; filter between networks on the router, or with switch rules in the chip:",
			Commands: commands, ReadOnly: true}},
	}}
}

// ---- logging and schedulers ---------------------------------------------------------

// debugLogging: a rule that logs debug topics to a remote action or to disk
// produces a line for every packet or event of its topic.
func debugLogging(s Sections, _ Facts) []Finding {
	targets := map[string]string{"memory": "memory", "disk": "disk", "echo": "echo", "remote": "remote"}
	for _, a := range s.rows(SecLoggingActions) {
		if a["name"] != "" && a["target"] != "" {
			targets[a["name"]] = a["target"]
		}
	}
	var found []string
	for _, r := range enabledRows(s.rows(SecLogging)) {
		if !hasDebugTopic(r["topics"]) {
			continue
		}
		switch target := targets[r["action"]]; target {
		case "remote", "disk":
			found = append(found, "topics="+r["topics"]+" → "+r["action"]+" ("+target+")")
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return []Finding{{
		Kind: KindDebugLogging, Severity: Warn,
		Title:  "Debug topics are logged to a remote server or to disk",
		Detail: listed(found, 6),
		Explanation: "A debug topic logs every packet or event of its subsystem. Sent to a remote server, each line " +
			"is a packet the CPU builds; written to disk, each line wears the flash. Debug rules are usually left " +
			"over from troubleshooting.",
		Fix: []FixStep{{Text: "Take debug out of the rule's topics (add !debug, and !packet and !raw where they are " +
			"logged), or log it to memory while you need it:",
			Commands: []string{"/system logging print"}, ReadOnly: true}},
	}}
}

// fastSchedulerLimit: a scheduler that fires more often than this is a
// constant load.
const fastSchedulerLimit = time.Minute

// fastSchedulers: schedulers with an interval under a minute.
func fastSchedulers(s Sections, _ Facts) []Finding {
	var found []string
	for _, r := range enabledRows(s.rows(SecScheduler)) {
		d, ok := parseInterval(r["interval"])
		if !ok || d <= 0 || d >= fastSchedulerLimit {
			continue // 0 = runs once at startup
		}
		found = append(found, nonEmpty(r["name"], "(unnamed)")+" (every "+d.String()+")")
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	return []Finding{{
		Kind: KindFastScheduler, Severity: Warn,
		Title:  "Scheduler that runs more than once a minute",
		Detail: listed(found, 6),
		Explanation: "A script that runs every few seconds is a constant load on the device, and one that prints or " +
			"changes configuration also fills the log and wears the flash.",
		Fix: []FixStep{{Text: "Review what each of them does and whether it needs to run that often (the NMS reads " +
			"their names and intervals only, never the scripts):",
			Commands: []string{"/system scheduler print"}, ReadOnly: true}},
	}}
}

// parseInterval reads a RouterOS interval: "30s", "1m30s", "1d" (the API's
// form) or "00:00:30" (the console's).
func parseInterval(v string) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if secs, ok := routeros.ParseUptime(v); ok {
		return time.Duration(secs) * time.Second, true
	}
	days := 0
	if d, rest, found := strings.Cut(v, "d "); found {
		n, err := strconv.Atoi(strings.TrimSpace(d))
		if err != nil {
			return 0, false
		}
		days, v = n, strings.TrimSpace(rest)
	}
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return 0, false
	}
	total := time.Duration(days) * 24 * time.Hour
	for i, unit := range []time.Duration{time.Hour, time.Minute, time.Second} {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return 0, false
		}
		total += time.Duration(n) * unit
	}
	return total, true
}

// ---- tools and services ---------------------------------------------------------------

// snifferRunning: a packet sniffer left running copies packets on the CPU.
func snifferRunning(s Sections, _ Facts) []Finding {
	if !on(s.first(SecSniffer), "running") {
		return nil
	}
	return []Finding{{
		Kind: KindSnifferRunning, Severity: Warn,
		Title:  "The packet sniffer is running",
		Detail: "Tools → Packet Sniffer is running",
		Explanation: "A running sniffer copies every packet it matches on the CPU, and on a switch it pulls " +
			"traffic out of the hardware path to do so. It is a troubleshooting tool that was not stopped.",
		Fix: []FixStep{{Text: "Stop it:", Commands: []string{"/tool sniffer stop"}}},
	}}
}

// cpuServices: services that answer clients from the CPU, on a switch whose
// CPU has nothing to spare.
func cpuServices(s Sections, f Facts) []Finding {
	if !isWeakSwitch(f, nil) {
		return nil
	}
	var found []string
	if on(s.first(SecCAPsMAN), "enabled") || on(s.first(SecCAPsMANLegacy), "enabled") {
		found = append(found, "CAPsMAN controller")
	}
	if on(s.first(SecDNS), "allow-remote-requests") {
		found = append(found, "DNS for clients (allow-remote-requests=yes)")
	}
	if n := len(enabledRows(s.rows(SecDHCPServers))); n > 0 {
		found = append(found, plural(n, "DHCP server", "DHCP servers"))
	}
	if len(found) == 0 {
		return nil
	}
	return []Finding{{
		Kind: KindCPUServices, Severity: Warn,
		Title:  "Client services run on a weak switch",
		Detail: strings.Join(found, ", "),
		Explanation: f.name() + " is a switch with a small management CPU. Services that answer every client — " +
			"DHCP, DNS, a wireless controller — run on that CPU, next to spanning tree and the management plane, and " +
			"a burst of clients makes all of them slow.",
		Fix: []FixStep{{Text: "Move them to a router. Clients lose the service while it moves: plan it.", Window: true}},
	}}
}

// dangerousPolicies are the rights a monitoring account must not have: with
// any of them the stored login can change the device.
var dangerousPolicies = []string{"write", "policy", "password", "sensitive", "ftp", "reboot"}

// nmsGroup is the group of the NMS's own account, read from its session.
func nmsGroup(s Sections, f Facts) string {
	if f.NMSUser == "" {
		return ""
	}
	for _, row := range s.rows(SecSessions) {
		if row["name"] == f.NMSUser && strings.TrimSpace(row["group"]) != "" {
			return strings.TrimSpace(row["group"])
		}
	}
	return ""
}

func nmsGroupKnown(s Sections, f Facts) bool { return nmsGroup(s, f) != "" }

// nmsAccount: the account the NMS logs in with can do more than read.
func nmsAccount(s Sections, f Facts) []Finding {
	group := nmsGroup(s, f)
	var policies string
	found := false
	for _, g := range s.rows(SecUserGroups) {
		if g["name"] == group {
			policies, found = g["policy"], true
		}
	}
	if !found {
		return nil
	}
	has := map[string]bool{}
	for _, p := range strings.Split(policies, ",") {
		if p = strings.TrimSpace(p); p != "" && !strings.HasPrefix(p, "!") {
			has[p] = true
		}
	}
	var extra []string
	for _, p := range dangerousPolicies {
		if has[p] {
			extra = append(extra, p)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	address := "<NMS-IP>"
	if len(f.NMSAddresses) > 0 {
		address = f.NMSAddresses[0].Unmap().String()
	}
	return []Finding{{
		Kind: KindNMSAccount, Severity: Warn,
		Title:  "The NMS account can change the device",
		Detail: "user " + f.NMSUser + " in group " + group + ": " + strings.Join(extra, ", "),
		Explanation: "The NMS only reads. The login it stores for this device also has the rights " +
			strings.Join(extra, ", ") + ": whoever gets hold of the NMS host or its database can change or reboot " +
			"the device with it. Firmware actions ask for an admin login when they run; they do not use this one.",
		Fix: []FixStep{
			{Text: "Create a read-only group and an account of its own for the NMS (choose a password; the " +
				"address limits where it may log in from):",
				Commands: []string{
					"/user group add name=nms-read policy=read,api,test",
					"/user add name=nms-poll group=nms-read address=" + address + " password=<new-password>",
				}},
			{Text: "Then enter that account for this device in the NMS (Devices → the device → Edit connection). " +
				"Leave the existing account as it is."},
		},
	}}
}

// deviceModeFeatures are the device-mode switches the audit reports on a
// switch or an access point.
var deviceModeFeatures = []string{"fetch", "bandwidth-test", "email", "traffic-gen", "sniffer"}

// deviceMode: on switches and access points, device-mode still allows tools
// they never need.
func deviceMode(s Sections, f Facts) []Finding {
	if !f.Role.NeverExports() {
		return nil
	}
	row := s.first(SecDeviceMode)
	var allowed []string
	for _, feature := range deviceModeFeatures {
		if on(row, feature) {
			allowed = append(allowed, feature)
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	detail := "allows " + strings.Join(allowed, ", ")
	if mode := strings.TrimSpace(row["mode"]); mode != "" {
		detail = "mode=" + mode + "; " + detail
	}
	args := make([]string, 0, len(allowed))
	for _, a := range allowed {
		args = append(args, a+"=no")
	}
	return []Finding{{
		Kind: KindDeviceMode, Severity: Info,
		Title:  "Device-mode allows tools this device does not need",
		Detail: detail,
		Explanation: "Device-mode is RouterOS's last line of defence: what it forbids cannot be switched on by " +
			"anyone who logs in, only by someone standing at the device. " + strings.ToUpper(roleWords(f.Role)[:1]) +
			roleWords(f.Role)[1:] + " has no use for " + strings.Join(allowed, ", ") + ".",
		Fix: []FixStep{{
			Text: "At the next site visit: the change must be confirmed at the device itself (the reset button or " +
				"a power cycle) within a few minutes, and the device restarts.",
			Commands: []string{"/system device-mode update " + strings.Join(args, " ")},
			Window:   true,
		}},
	}}
}

// foreignSessions: API sessions from addresses that are not the NMS's — the
// same finding the login watch raises from the log, here from the sessions
// open right now.
func foreignSessions(s Sections, f Facts) []Finding {
	seen := map[string]bool{}
	var out []Finding
	for _, row := range s.rows(SecSessions) {
		via := strings.ToLower(strings.TrimSpace(row["via"]))
		if (via != "api" && via != "api-ssl") || row["address"] == "" || f.isSelf(row["address"]) {
			continue
		}
		detail := "user " + row["name"] + " from " + row["address"] + " via " + via
		if seen[detail] {
			continue
		}
		seen[detail] = true
		out = append(out, Finding{
			Kind: KindParallelAPIPoller, Severity: Warn, Foreign: true,
			Title:  "Another API client is logged in",
			Detail: detail,
			Explanation: "Something other than this NMS has an API session open on the device — another monitoring " +
				"system, a script or an app. Its reads add load this NMS does not budget for.",
			Fix: []FixStep{{Text: "Find what runs at that address and stop it, or let it read less often. If the " +
				"address is this NMS's own (behind NAT, or a second address), add it to NMS source addresses in Settings."}},
		})
	}
	return out
}

// unreadable: menus the NMS account may not read, and reads the device's
// RouterOS version refused with an error of its own. That part of the audit
// did not happen — which is a finding, never "clean".
func unreadable(s Sections, _ Facts) []Finding {
	denied, refused := s.Denied(), s.Unsupported()
	if len(denied)+len(refused) == 0 {
		return nil
	}
	fnd := Finding{Kind: KindUnreadable, Severity: Info}
	var detail, why []string
	if len(denied) > 0 {
		fnd.Title = "The NMS account may not read part of the configuration"
		detail = append(detail, strings.Join(denied, ", "))
		why = append(why, "The device refused "+plural(len(denied), "read", "reads")+" with \"not enough permissions\".")
		fnd.Fix = append(fnd.Fix, FixStep{Text: "Give the group of the NMS account the read policy for these menus, " +
			"or accept that they are not audited."})
	}
	if len(refused) > 0 {
		if fnd.Title == "" {
			fnd.Title = "The device's RouterOS refused part of the audit"
		}
		detail = append(detail, "refused by this RouterOS version: "+strings.Join(refused, ", "))
		why = append(why, "The device answered "+plural(len(refused), "read", "reads")+" with an error of its own: "+
			"this RouterOS version does not take the read the way the NMS sends it.")
		fnd.Fix = append(fnd.Fix, FixStep{Text: "A read the RouterOS version refuses is for the NMS to fix, not for " +
			"the device: report the menu and the RouterOS version."})
	}
	fnd.Detail = strings.Join(detail, "; ")
	fnd.Explanation = strings.Join(why, " ") + " The audit could not check what is configured there — that is not " +
		"the same as nothing being wrong."
	return []Finding{fnd}
}

// ---- small helpers ------------------------------------------------------------------------

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func dedupe(list []string) []string {
	seen := map[string]bool{}
	out := list[:0]
	for _, v := range list {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

func itThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}
