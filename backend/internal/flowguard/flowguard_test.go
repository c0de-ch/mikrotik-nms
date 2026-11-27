package flowguard

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/topology"
)

func role(r devclass.Role, probed, budget devclass.Class, egress devclass.Egress) topology.DeviceRole {
	return topology.DeviceRole{Role: r, Reason: "because of the facts", Probed: probed, Budget: budget, Egress: egress}
}

func TestJudge(t *testing.T) {
	for _, c := range []struct {
		name     string
		r        topology.DeviceRole
		eligible bool
		warning  string // substring of one warning ("" = no warnings at all)
	}{
		{"strong router", role(devclass.RoleRouter, devclass.C3, devclass.C3, devclass.EgressInternet), true, ""},
		{"weak router on C2", role(devclass.RoleWeakRouter, devclass.C2, devclass.C2, devclass.EgressInternet), true, "small device"},
		{"router budgeted as C2 by an override", topology.DeviceRole{Role: devclass.RoleRouter, Probed: devclass.C3,
			Budget: devclass.C2, Overridden: true, Egress: devclass.EgressInternet}, true, "override"},
		{"weak router on C1", role(devclass.RoleWeakRouter, devclass.C1, devclass.C1, devclass.EgressInternet), false, "C1"},
		{"unprobed router", role(devclass.RoleWeakRouter, devclass.C0Unknown, devclass.C1, devclass.EgressInternet), false, "not been classified"},
		{"router on the light profile", topology.DeviceRole{Role: devclass.RoleRouter, Probed: devclass.C3, Budget: devclass.C1,
			Light: true, Egress: devclass.EgressInternet}, false, "light"},
		{"router overridden to C1", topology.DeviceRole{Role: devclass.RoleRouter, Probed: devclass.C3, Budget: devclass.C1,
			Overridden: true, Egress: devclass.EgressInternet}, false, "override"},
		{"switch", role(devclass.RoleSwitch, devclass.C2, devclass.C2, devclass.EgressNone), false, "because of the facts"},
		{"strong access point", role(devclass.RoleAPCPE, devclass.C3, devclass.C3, devclass.EgressNone), false, "because of the facts"},
		{"unknown", role(devclass.RoleUnknown, devclass.C3, devclass.C3, devclass.EgressNone), false, "because of the facts"},
	} {
		v := Judge("dev-1", c.r)
		if v.Eligible != c.eligible {
			t.Errorf("%s: eligible = %v, want %v (%v)", c.name, v.Eligible, c.eligible, v.Warnings)
		}
		if c.warning == "" {
			if len(v.Warnings) != 0 {
				t.Errorf("%s: unexpected warnings %v", c.name, v.Warnings)
			}
			continue
		}
		if !strings.Contains(strings.Join(v.Warnings, "\n"), c.warning) {
			t.Errorf("%s: warnings %v do not mention %q", c.name, v.Warnings, c.warning)
		}
	}
}

// A C1 device is never eligible, whatever its role says.
func TestJudgeC1NeverEligible(t *testing.T) {
	for _, r := range []devclass.Role{devclass.RoleRouter, devclass.RoleWeakRouter, devclass.RoleSwitch, devclass.RoleAPCPE, devclass.RoleUnknown} {
		for _, probed := range []devclass.Class{devclass.C0Unknown, devclass.C1, devclass.C2, devclass.C3} {
			if v := Judge("d", role(r, probed, devclass.C1, devclass.EgressInternet)); v.Eligible {
				t.Errorf("role %s probed %s budgeted C1 is eligible", r, probed)
			}
		}
	}
}

func TestNeverExportsAndAutoAccept(t *testing.T) {
	for _, c := range []struct {
		name   string
		r      topology.DeviceRole
		never  bool
		accept bool // with flow_auto_accept_classes = router
	}{
		{"strong router with egress", role(devclass.RoleRouter, devclass.C3, devclass.C3, devclass.EgressInternet), false, true},
		{"strong router over a VPN", role(devclass.RoleRouter, devclass.C3, devclass.C3, devclass.EgressVPN), false, true},
		{"router on the light profile", topology.DeviceRole{Role: devclass.RoleRouter, Probed: devclass.C3, Budget: devclass.C1,
			Light: true, Egress: devclass.EgressInternet}, false, false},
		{"weak router", role(devclass.RoleWeakRouter, devclass.C2, devclass.C2, devclass.EgressInternet), false, false},
		{"C1 router", role(devclass.RoleWeakRouter, devclass.C1, devclass.C1, devclass.EgressInternet), true, false},
		{"switch", role(devclass.RoleSwitch, devclass.C2, devclass.C2, devclass.EgressNone), true, false},
		{"switch on C3", role(devclass.RoleSwitch, devclass.C3, devclass.C3, devclass.EgressInternet), true, false},
		{"access point", role(devclass.RoleAPCPE, devclass.C3, devclass.C3, devclass.EgressNone), true, false},
		{"unknown", role(devclass.RoleUnknown, devclass.C3, devclass.C3, devclass.EgressNone), false, false},
		{"unprobed unknown", role(devclass.RoleUnknown, devclass.C0Unknown, devclass.C1, devclass.EgressNone), false, false},
		// A class an admin only budgets as C1 is not C1 hardware: never
		// eligible, but not blocked or alarmed either.
		{"router overridden to C1", topology.DeviceRole{Role: devclass.RoleRouter, Probed: devclass.C3, Budget: devclass.C1,
			Overridden: true, Egress: devclass.EgressInternet}, false, false},
	} {
		never, reason := NeverExports("dev-1", c.r)
		if never != c.never || (never && reason == "") {
			t.Errorf("%s: NeverExports = %v %q, want %v", c.name, never, reason, c.never)
		}
		if got := AutoAccept(AutoAcceptRouter, c.r); got != c.accept {
			t.Errorf("%s: AutoAccept(router) = %v, want %v", c.name, got, c.accept)
		}
		if AutoAccept("", c.r) {
			t.Errorf("%s: accepted with auto-accept off", c.name)
		}
	}
	if ParseAutoAccept(" Router ") != AutoAcceptRouter || ParseAutoAccept("true") != "" || ParseAutoAccept("all") != "" {
		t.Error("ParseAutoAccept: only 'router' turns auto-accept on")
	}
}

func commandsOf(steps []RollbackStep) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Commands...)
	}
	return out
}

func indexOf(list []string, sub string) int {
	for i, s := range list {
		if strings.Contains(s, sub) {
			return i
		}
	}
	return -1
}

// The rollback of a device older than 7.25: profile first, disable the
// target, download before the risky disable (which carries the warning),
// remove the target, verify.
func TestRollbackBefore725(t *testing.T) {
	for _, version := range []string{"7.23.1 (stable)", "7.24.2", "7.25beta2", "7.25beta3", "7.25rc2", "7.16", "", "garbage"} {
		rb := Rollback(version, "198.51.100.5", 2055)
		if rb.Fixed {
			t.Fatalf("%q treated as having the 7.25 fix", version)
		}
		if len(rb.Steps) != 5 {
			t.Fatalf("%q: %d steps, want 5 (0–4)", version, len(rb.Steps))
		}
		cmds := commandsOf(rb.Steps)
		order := []string{
			"/tool profile duration=30",
			"/ip traffic-flow target disable [find dst-address=198.51.100.5 port=2055]",
			"/system package update download",
			"/ip traffic-flow set enabled=no",
			"/ip traffic-flow target remove [find dst-address=198.51.100.5 port=2055]",
			"/tool profile duration=30",
		}
		last := -1
		for _, want := range order {
			i := -1
			for j := last + 1; j < len(cmds); j++ {
				if cmds[j] == want {
					i = j
					break
				}
			}
			if i < 0 {
				t.Fatalf("%q: %q missing or out of order in %q", version, want, cmds)
			}
			last = i
		}
		// The decision point comes first and says when to stop early.
		if !strings.Contains(rb.Steps[0].Text, "steps 1 and 3") || !strings.Contains(rb.Steps[0].Text, "7.25") {
			t.Errorf("%q: step 0 = %q", version, rb.Steps[0].Text)
		}
		// Step 2 is the optional, risky one: it carries the warning and the download.
		s2 := rb.Steps[2]
		if !s2.Optional || !strings.Contains(s2.Warning, "kernel") || !strings.Contains(s2.Warning, "Safe Mode") ||
			!strings.Contains(s2.Text, "maintenance window") || len(s2.Commands) != 2 {
			t.Errorf("%q: step 2 = %+v", version, s2)
		}
		for i, s := range rb.Steps {
			if i != 2 && (s.Warning != "" || s.Optional) {
				t.Errorf("%q: step %d carries a warning or is optional: %+v", version, i, s)
			}
		}
		if version == "" || version == "garbage" {
			if !strings.Contains(rb.Note, "unknown") {
				t.Errorf("%q: note %q does not say the version is unknown", version, rb.Note)
			}
		}
	}
}

// From 7.25 the same order applies, without the warning and the download.
func TestRollbackFrom725(t *testing.T) {
	for _, version := range []string{"7.25", "7.25 (stable)", "7.25.1 (stable)", "7.26beta1", "7.26", "8.0"} {
		rb := Rollback(version, "198.51.100.5", 9995)
		if !rb.Fixed {
			t.Fatalf("%q not treated as fixed", version)
		}
		cmds := commandsOf(rb.Steps)
		if indexOf(cmds, "package update download") >= 0 {
			t.Errorf("%q: download step present: %q", version, cmds)
		}
		for _, s := range rb.Steps {
			if s.Warning != "" {
				t.Errorf("%q: warning present: %q", version, s.Warning)
			}
		}
		disable, off, remove := indexOf(cmds, "target disable"), indexOf(cmds, "enabled=no"), indexOf(cmds, "target remove")
		if cmds[0] != "/tool profile duration=30" || disable < 1 || off < disable || remove < off ||
			cmds[len(cmds)-1] != "/tool profile duration=30" {
			t.Errorf("%q: order = %q", version, cmds)
		}
		if !strings.Contains(cmds[disable], "port=9995") {
			t.Errorf("%q: port not used: %q", version, cmds[disable])
		}
	}
}

// No rollback command touches more than the one target: every [find] names
// the collector's address and port, and nothing else is found.
func TestRollbackFindsOnlyTheOneTarget(t *testing.T) {
	for _, version := range []string{"7.20", "7.25"} {
		for _, collector := range []string{"198.51.100.5", "2001:db8::5", ""} {
			rb := Rollback(version, collector, 2055)
			want := collector
			if want == "" {
				want = Placeholder
			}
			for _, c := range append(commandsOf(rb.Steps), rb.Lines()...) {
				if n := strings.Count(c, "[find"); n > 0 {
					if n != 1 || !strings.Contains(c, "[find dst-address="+want+" port=2055]") {
						t.Errorf("%s/%q: broad find in %q", version, collector, c)
					}
				}
				if strings.Contains(c, "interfaces=all") || strings.Contains(c, "[find]") {
					t.Errorf("%s/%q: %q", version, collector, c)
				}
			}
		}
	}
}

func TestRollbackLines(t *testing.T) {
	lines := Rollback("7.23", "198.51.100.5", 2055).Lines()
	text := strings.Join(lines, "\n")
	for _, want := range []string{"0. ", "1. ", "2. ", "3. ", "4. ", "WARNING", "/ip traffic-flow set enabled=no",
		"/ip traffic-flow target remove [find dst-address=198.51.100.5 port=2055]"} {
		if !strings.Contains(text, want) {
			t.Errorf("lines do not contain %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "target disable") > strings.Index(text, "enabled=no") {
		t.Errorf("target disable must come before enabled=no:\n%s", text)
	}
}

func TestApplyAndCheckCommands(t *testing.T) {
	apply := ApplyCommands("198.51.100.5", 2055, "192.0.2.1", []string{"ether1", "bridge", "vlan20-users"})
	want := []string{
		"/ip traffic-flow target add dst-address=198.51.100.5 port=2055 src-address=192.0.2.1 version=ipfix v9-template-timeout=1m",
		"/ip traffic-flow set enabled=yes interfaces=ether1,bridge,vlan20-users active-flow-timeout=1m inactive-flow-timeout=15s",
	}
	if len(apply) != 2 || apply[0] != want[0] || apply[1] != want[1] {
		t.Fatalf("apply = %q\nwant    %q", apply, want)
	}
	// Without known interfaces — or with a name that would need quoting —
	// the operator has to name them: never "all", never a guess.
	for _, ifaces := range [][]string{nil, {}, {"ether1", "vlan 20"}, {"ether1", "a,b"}, {"ether1", `q"x`}} {
		apply = ApplyCommands("", 2055, "", ifaces)
		if !strings.Contains(apply[0], "dst-address="+Placeholder) || strings.Contains(apply[0], "src-address") ||
			!strings.Contains(apply[1], "interfaces="+InterfacesPlaceholder) {
			t.Fatalf("placeholders: %q", apply)
		}
	}
	for _, c := range append(apply, want...) {
		if strings.Contains(c, "interfaces=all") {
			t.Errorf("interfaces=all in %q", c)
		}
	}
	check := CheckCommands()
	if len(check) != 2 || check[0] != "/ip traffic-flow print" || check[1] != "/ip traffic-flow target print" {
		t.Fatalf("check = %q", check)
	}
	if !strings.Contains(GraphingAdvice(2, 365), "Do not enable Tools → Graphing on switches") ||
		!strings.Contains(GraphingAdvice(2, 365), "1 min for 2 d, 1 h for 365 d") {
		t.Errorf("advice = %q", GraphingAdvice(2, 365))
	}
}

// The rollback names the device's own target when the read found it; the
// admin-approved advertise address otherwise; never a guess.
func TestRollbackTarget(t *testing.T) {
	ap := netip.MustParseAddrPort
	for _, c := range []struct {
		name      string
		advertise string
		listen    int
		confirmed []netip.AddrPort
		addr      string
		port      int
	}{
		{"nothing known", "", 0, nil, "", 2055},
		{"advertise address and listen port", "198.51.100.5", 9995, nil, "198.51.100.5", 9995},
		{"the confirmed target that is the advertise address", "198.51.100.5", 2055,
			[]netip.AddrPort{ap("203.0.113.9:9000"), ap("198.51.100.5:2056")}, "198.51.100.5", 2056},
		{"one confirmed target, no advertise address", "", 2055, []netip.AddrPort{ap("198.51.100.7:2057")}, "198.51.100.7", 2057},
		{"one confirmed target that is not the advertise address (NAT)", "198.51.100.5", 2055,
			[]netip.AddrPort{ap("192.0.2.200:2055")}, "192.0.2.200", 2055},
		{"several targets, none is the advertise address", "198.51.100.5", 2055,
			[]netip.AddrPort{ap("192.0.2.200:2055"), ap("192.0.2.201:2055")}, "198.51.100.5", 2055},
		{"several targets and no advertise address: the operator has to say", "", 2055,
			[]netip.AddrPort{ap("192.0.2.200:2055"), ap("192.0.2.201:2055")}, "", 2055},
		{"an advertise address that is not an address", "nms.example", 2055, nil, "", 2055},
	} {
		addr, port := RollbackTarget(c.advertise, c.listen, c.confirmed)
		if addr != c.addr || port != c.port {
			t.Errorf("%s: %q:%d, want %q:%d", c.name, addr, port, c.addr, c.port)
		}
	}
}
