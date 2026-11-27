package hygiene

import (
	"encoding/json"
	"strings"
)

// The usual device-side causes of a high CPU, as a pass reports them next to
// its findings. A finding says "this is wrong"; a check says what the pass
// saw, wrong or not — traffic-flow on a router that may export, SNMP on a
// capable device and a tuned graphing rule are no findings, and are not
// "absent" either. The sentence of a pass that found nothing is built from
// the checks, so it never names as absent what the pass saw, or could not
// read.
const (
	CauseGraphing       = "graphing"
	CauseTrafficFlow    = "traffic_flow"
	CauseSniffer        = "sniffer"
	CauseSwitchToCPU    = "switch_to_cpu"
	CauseBondMode       = "bond_mode"
	CauseBridgeFirewall = "bridge_ip_firewall"
	CauseSNMP           = "snmp"
	CauseAPIClient      = "api_client"
)

// States of a check.
const (
	CheckAbsent  = "absent"  // read, and not there
	CheckPresent = "present" // read, and there — whether or not it is a finding
	CheckUnknown = "unknown" // not read: refused, failed, not reached, or the facts to judge it are missing
)

// Check is what a pass saw of one usual cause.
type Check struct {
	Cause string `json:"cause"`
	State string `json:"state"`
}

// cause is one usual cause: how it is named when absent, what is said when
// it is there, and how a pass tells.
type cause struct {
	key string
	// absent names it in "no …"; present is the clause for "found that …".
	absent, present string
	needs           []string
	// ready is any further precondition; nil = none.
	ready func(s Sections, f Facts) bool
	there func(s Sections, f Facts) bool
}

// causes, in the order a pass names them.
var causes = []cause{
	{key: CauseGraphing, absent: "graphing", present: "graphing rules are configured",
		needs: []string{SecGraphingInterface, SecGraphingResource, SecGraphingQueue},
		there: func(s Sections, _ Facts) bool {
			return len(enabledRows(s.rows(SecGraphingInterface)))+len(enabledRows(s.rows(SecGraphingResource)))+
				len(enabledRows(s.rows(SecGraphingQueue))) > 0
		}},
	{key: CauseTrafficFlow, absent: "traffic-flow", present: "traffic-flow is enabled",
		needs: []string{SecTrafficFlow},
		there: func(s Sections, _ Facts) bool { return on(s.first(SecTrafficFlow), "enabled") }},
	{key: CauseSniffer, absent: "running sniffer", present: "the packet sniffer is running",
		needs: []string{SecSniffer},
		there: func(s Sections, _ Facts) bool { return on(s.first(SecSniffer), "running") }},
	{key: CauseSwitchToCPU, absent: "mirror or switch rule to the CPU", present: "the switch chip sends packets to the CPU",
		needs: []string{SecSwitches, SecSwitchRules},
		there: func(s Sections, f Facts) bool { return len(mirrorToCPU(s, f))+len(switchRulesToCPU(s, f)) > 0 }},
	{key: CauseBondMode, absent: "unsuitable bond mode", present: "a bond is in a mode the switch chip cannot offload",
		needs: []string{SecBonding},
		ready: func(_ Sections, f Facts) bool { return !f.RoleMissing },
		there: func(s Sections, f Facts) bool { return len(bondModes(s, f)) > 0 }},
	{key: CauseBridgeFirewall, absent: "bridged traffic through the IP firewall", present: "bridged traffic goes through the IP firewall",
		needs: []string{SecBridgeSettings},
		there: func(s Sections, _ Facts) bool { return on(s.first(SecBridgeSettings), "use-ip-firewall") }},
	{key: CauseSNMP, absent: "SNMP", present: "SNMP is enabled",
		needs: []string{SecSNMP},
		there: func(s Sections, _ Facts) bool { return on(s.first(SecSNMP), "enabled") }},
	{key: CauseAPIClient, absent: "other API client", present: "another API client is logged in",
		needs: []string{SecSessions},
		ready: func(_ Sections, f Facts) bool { return len(f.NMSAddresses) > 0 },
		there: func(s Sections, f Facts) bool { return len(foreignSessions(s, f)) > 0 }},
}

// Causes lists the usual causes, in the order a pass names them.
func Causes() []string {
	out := make([]string, 0, len(causes))
	for _, c := range causes {
		out = append(out, c.key)
	}
	return out
}

// checksOf says, for every usual cause, what the pass saw of it.
func checksOf(s Sections, f Facts) []Check {
	out := make([]Check, 0, len(causes))
	for _, c := range causes {
		state := CheckUnknown
		if s.known(c.needs...) && (c.ready == nil || c.ready(s, f)) {
			state = CheckAbsent
			if c.there(s, f) {
				state = CheckPresent
			}
		}
		out = append(out, Check{Cause: c.key, State: state})
	}
	return out
}

// ChecksJSON is the checks as stored with a pass.
func ChecksJSON(checks []Check) string {
	if len(checks) == 0 {
		return "[]"
	}
	b, err := json.Marshal(checks)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ParseChecks reads the stored checks of a pass; an empty or damaged value
// is no checks.
func ParseChecks(raw string) []Check {
	var out []Check
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// NothingFound words what a pass without findings says about the usual
// causes: the rest of a sentence that begins "The diagnose pass …". It names
// as absent only what the pass read and found absent; what it saw without a
// rule making a finding of it is said, and what it could not read is listed
// as not checked.
//
// forExport: the pass answers an alert about the device's own flow export,
// so an enabled traffic-flow is "the export this alert is about". Without
// checks (a pass stored before they were) the sentence claims nothing in
// particular.
func NothingFound(checks []Check, forExport bool) string {
	byKey := make(map[string]string, len(checks))
	for _, c := range checks {
		byKey[c.Cause] = c.State
	}
	var absent, present, unknown []string
	for _, c := range causes {
		switch byKey[c.key] {
		case CheckAbsent:
			absent = append(absent, c.absent)
		case CheckPresent:
			present = append(present, c.present)
		case CheckUnknown:
			unknown = append(unknown, c.absent)
		}
	}
	export := forExport && byKey[CauseTrafficFlow] == CheckPresent

	var b strings.Builder
	switch {
	case len(checks) == 0:
		return "found none of the usual device-side causes."
	case len(present) == 0 && len(absent) == 0:
		b.WriteString("could not check any of the usual device-side causes.")
	case len(present) == 0:
		b.WriteString("found none of the usual device-side causes: no " + join(absent, "or") + ".")
	default:
		note := "no fault by itself on this device, but work for its CPU"
		switch {
		case export && len(present) == 1:
			note = "the export this alert is about"
		case len(present) > 1:
			note = "no faults by themselves on this device, but work for its CPU"
			for i, p := range present {
				if export && p == presentOf(CauseTrafficFlow) {
					present[i] = p + " (the export this alert is about)"
				}
			}
		}
		b.WriteString("found that " + join(present, "and") + " — " + note + " — and nothing else")
		if len(absent) > 0 {
			b.WriteString(": no " + join(absent, "or"))
		}
		b.WriteString(".")
	}
	if len(unknown) > 0 {
		b.WriteString(" Not checked: " + join(unknown, "and") + ".")
	}
	return b.String()
}

func presentOf(key string) string {
	for _, c := range causes {
		if c.key == key {
			return c.present
		}
	}
	return ""
}

// join renders "a", "a or b", "a, b or c".
func join(items []string, last string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " " + last + " " + items[len(items)-1]
}
