package hygiene

import (
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/devclass"
)

func stateOf(t *testing.T, checks []Check, cause string) string {
	t.Helper()
	for _, c := range checks {
		if c.Cause == cause {
			return c.State
		}
	}
	t.Fatalf("no check for %s among %+v", cause, checks)
	return ""
}

// diagnoseOf is a whole diagnose pass of the clean device with the given
// sections replaced.
func diagnoseOf(replace Sections) Sections {
	s := Sections{}
	for _, r := range Plan(ModeDiagnose) {
		s[r.Key] = cleanAudit()[r.Key]
	}
	for k, v := range replace {
		s[k] = v
	}
	return s
}

// A pass says of each usual cause what it saw — also where no rule makes a
// finding of it. A router that exports to this NMS and answers SNMP to its
// monitoring host has no finding, and neither traffic-flow nor SNMP is
// "absent" on it.
func TestChecksSayWhatThePassSaw(t *testing.T) {
	exporting := diagnoseOf(Sections{
		SecTrafficFlow:        sec(row("enabled", "true", "interfaces", "ether1")),
		SecTrafficFlowTargets: sec(row("dst-address", "192.0.2.10", "port", "2055", "version", "ipfix", "disabled", "false")),
		SecSNMP:               sec(row("enabled", "true")),
		SecSNMPCommunities:    sec(row("addresses", "192.0.2.20/32", "security", "none", "read-access", "true", "write-access", "false")),
		SecGraphingInterface:  sec(row("interface", "ether1", "allow-address", "192.0.2.0/24", "store-on-disk", "false")),
	})
	res := Evaluate(ModeDiagnose, exporting, router())
	wantKinds(t, res) // nothing is wrong with any of it on a router
	for cause, want := range map[string]string{
		CauseTrafficFlow: CheckPresent, CauseSNMP: CheckPresent, CauseGraphing: CheckPresent,
		CauseSniffer: CheckAbsent, CauseSwitchToCPU: CheckAbsent, CauseBondMode: CheckAbsent,
		CauseBridgeFirewall: CheckAbsent, CauseAPIClient: CheckAbsent,
	} {
		if got := stateOf(t, res.Checks, cause); got != want {
			t.Errorf("%s = %s, want %s", cause, got, want)
		}
	}
	if len(res.Checks) != len(Causes()) {
		t.Errorf("%d checks, want one per cause (%d)", len(res.Checks), len(Causes()))
	}

	// The clean device: everything was read and nothing is there.
	for _, c := range Evaluate(ModeDiagnose, diagnoseOf(nil), router()).Checks {
		if c.State != CheckAbsent {
			t.Errorf("clean device: %s = %s", c.Cause, c.State)
		}
	}

	// What the pass could not read, it does not call absent: a failed read,
	// a refused one, a read it did not get to, sessions it cannot tell apart
	// and a bond it cannot judge without the role.
	blind := diagnoseOf(Sections{SecSniffer: failedSec, SecSNMP: deniedSec, SecSNMPCommunities: skippedSec})
	delete(blind, SecBridgeSettings)
	f := router()
	f.NMSAddresses = nil
	f.Role, f.RoleMissing = devclass.RoleUnknown, true
	checks := Evaluate(ModeDiagnose, blind, f).Checks
	for _, cause := range []string{CauseSniffer, CauseSNMP, CauseBridgeFirewall, CauseAPIClient, CauseBondMode} {
		if got := stateOf(t, checks, cause); got != CheckUnknown {
			t.Errorf("%s = %s, want unknown", cause, got)
		}
	}
	if got := stateOf(t, checks, CauseGraphing); got != CheckAbsent {
		t.Errorf("graphing = %s, want absent (it was read)", got)
	}

	// The checks survive their way through the database.
	if back := ParseChecks(ChecksJSON(res.Checks)); len(back) != len(res.Checks) || back[1] != res.Checks[1] {
		t.Errorf("round trip: %+v", back)
	}
	if got := ParseChecks(""); len(got) != 0 {
		t.Errorf("no stored checks: %+v", got)
	}
}

// The sentence of a pass without findings never names as absent what the
// pass saw present, or could not read.
func TestNothingFoundNamesOnlyWhatIsAbsent(t *testing.T) {
	all := func(state string) []Check {
		var out []Check
		for _, c := range Causes() {
			out = append(out, Check{Cause: c, State: state})
		}
		return out
	}
	set := func(checks []Check, cause, state string) []Check {
		out := append([]Check(nil), checks...)
		for i := range out {
			if out[i].Cause == cause {
				out[i].State = state
			}
		}
		return out
	}

	clean := NothingFound(all(CheckAbsent), false)
	if clean != "found none of the usual device-side causes: no graphing, traffic-flow, running sniffer, mirror or switch rule "+
		"to the CPU, unsuitable bond mode, bridged traffic through the IP firewall, SNMP or other API client." {
		t.Errorf("clean = %q", clean)
	}

	// The exporter the alert is about.
	exporting := set(all(CheckAbsent), CauseTrafficFlow, CheckPresent)
	got := NothingFound(exporting, true)
	if !strings.HasPrefix(got, "found that traffic-flow is enabled — the export this alert is about — and nothing else: no graphing, running sniffer,") ||
		strings.Contains(got, "no graphing, traffic-flow") || strings.Contains(got, ", traffic-flow") {
		t.Errorf("exporting, for the export alert = %q", got)
	}
	// The same device, for an alert that is not about the export.
	got = NothingFound(exporting, false)
	if !strings.HasPrefix(got, "found that traffic-flow is enabled — no fault by itself on this device, but work for its CPU — and nothing else: no graphing,") ||
		strings.Contains(got, "this alert") || strings.Contains(got, ", traffic-flow") {
		t.Errorf("exporting, for another alert = %q", got)
	}
	// Two of them.
	got = NothingFound(set(exporting, CauseSNMP, CheckPresent), true)
	if !strings.HasPrefix(got, "found that traffic-flow is enabled (the export this alert is about) and SNMP is enabled — no faults by "+
		"themselves on this device, but work for its CPU — and nothing else: no graphing,") || strings.Contains(got, "SNMP or") {
		t.Errorf("two present = %q", got)
	}

	// What was not read is said, not passed off as absent.
	got = NothingFound(set(set(all(CheckAbsent), CauseSniffer, CheckUnknown), CauseSNMP, CheckUnknown), false)
	if !strings.Contains(got, "Not checked: running sniffer and SNMP.") || strings.Contains(got, "no graphing, traffic-flow, running sniffer") {
		t.Errorf("unknown = %q", got)
	}
	if got := NothingFound(all(CheckUnknown), false); !strings.HasPrefix(got, "could not check any of the usual device-side causes") {
		t.Errorf("nothing read = %q", got)
	}
	// A pass stored before the checks were: it claims nothing in particular.
	if got := NothingFound(nil, true); got != "found none of the usual device-side causes." {
		t.Errorf("no checks = %q", got)
	}
}
