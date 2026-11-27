package devclass

import (
	"strings"
	"testing"
)

func TestBoardFamily(t *testing.T) {
	for _, c := range []struct {
		board, arch string
		want        BoardFamily
	}{
		{"CRS326-24G-2S+", "arm", FamilySwitch},
		{"CRS354-48G-4S+2Q+", "mipsbe", FamilySwitch},
		{"CSS610-8G-2S+IN", "", FamilySwitch},
		{"netPower 16P", "arm", FamilySwitch},
		{"RB5009UG+S+", "arm64", FamilyRouter},
		{"CCR2004-16G-2S+", "arm64", FamilyRouter},
		{"RB4011iGS+5HacQ2HnD", "arm", FamilyRouter},
		{"hEX S", "mmips", FamilyRouter},
		{"L009UiGS-RM", "arm", FamilyRouter},
		{"CHR", "x86_64", FamilyRouter},
		{"Some PC board", "x86_64", FamilyRouter}, // x86 is PC hardware whatever the name
		{"cAP ax", "arm64", FamilyAP},
		{"wAP ac", "arm", FamilyAP},
		{"hAP ax3", "arm64", FamilyAP},
		{"Audience", "arm", FamilyAP},
		{"LHG 5", "mipsbe", FamilyAP},
		{"SXTsq 5 ac", "arm", FamilyAP},
		{"mANTBox 52 15s", "arm", FamilyAP},
		{"", "", FamilyUnknown},
		{"SomeOtherBoard", "arm64", FamilyUnknown},
	} {
		if got := BoardFamilyOf(c.board, c.arch); got != c.want {
			t.Errorf("BoardFamilyOf(%q, %q) = %q, want %q", c.board, c.arch, got, c.want)
		}
	}
}

func TestClassifyRole(t *testing.T) {
	for _, c := range []struct {
		name   string
		facts  RoleFacts
		want   Role
		reason string // substring the reason must contain
	}{
		{"switch board", RoleFacts{Board: "CRS326-24G-2S+", Class: C2}, RoleSwitch, "switch"},
		{"switch board stays a switch with a default route to the internet",
			RoleFacts{Board: "CRS317-1G-16S+", Class: C2, Egress: EgressInternet, EgressVia: "sfp-sfpplus1"}, RoleSwitch, "switch"},
		{"strong router board with internet egress",
			RoleFacts{Board: "RB5009UG+S+", Class: C3, Egress: EgressInternet, EgressVia: "ether1"}, RoleRouter, "ether1"},
		{"strong router board with VPN egress only",
			RoleFacts{Board: "CCR2004-16G-2S+", Class: C3, Egress: EgressVPN, EgressVia: "wg0"}, RoleRouter, "VPN"},
		{"router board without egress is not clearly an L3 boundary",
			RoleFacts{Board: "RB5009UG+S+", Class: C3, BridgePorts: 8}, RoleUnknown, "8"},
		{"router board without egress, even as a CAPsMAN controller with wifi interfaces",
			RoleFacts{Board: "RB5009UG+S+", Class: C3, Radios: 6}, RoleUnknown, "router hardware"},
		{"small router is a weak router", RoleFacts{Board: "RB4011iGS+", Class: C2, Egress: EgressInternet, EgressVia: "ether1"},
			RoleWeakRouter, "C2"},
		{"C1 router is a weak router", RoleFacts{Board: "hEX S", Class: C1, Egress: EgressInternet, EgressVia: "ether1"},
			RoleWeakRouter, "C1"},
		{"unprobed device with egress is a weak router until its class is known",
			RoleFacts{Egress: EgressInternet, EgressVia: "lte1"}, RoleWeakRouter, "not been probed"},
		{"access point behind a router", RoleFacts{Board: "cAP ax", Class: C3, Radios: 2, BridgePorts: 3, Gateway: "192.0.2.1"},
			RoleAPCPE, "192.0.2.1"},
		{"access point with a VPN tunnel is still an access point",
			RoleFacts{Board: "cAP ax", Class: C3, Egress: EgressVPN, EgressVia: "back-to-home-vpn", Gateway: "192.0.2.1"},
			RoleAPCPE, "VPN"},
		{"AP board with no default route known (a router whose WAN is down) is not called an access point",
			RoleFacts{Board: "hAP ax3", Class: C3, Radios: 2}, RoleUnknown, "no default route"},
		{"AP board that is the internet edge routes", RoleFacts{Board: "hAP ax3", Class: C3, Egress: EgressInternet,
			EgressVia: "ether1"}, RoleRouter, "internet"},
		{"home router board behind another router (double NAT, or used as an access point) is not called an access point",
			RoleFacts{Board: "hAP ax3", Class: C3, Radios: 2, BridgePorts: 4, Gateway: "192.0.2.1"}, RoleUnknown, "cannot tell"},
		{"the same for an LTE home router whose default route goes to a LAN router",
			RoleFacts{Board: "Chateau LTE12", Class: C2, Radios: 2, Gateway: "192.0.2.1"}, RoleUnknown, "192.0.2.1"},
		{"a home router board with only a VPN tunnel of its own is not called an access point either",
			RoleFacts{Board: "hAP ax2", Class: C3, Egress: EgressVPN, EgressVia: "wg0", Gateway: "192.0.2.1"}, RoleUnknown, "cannot tell"},
		{"LTE CPE that is the internet edge is a weak router", RoleFacts{Board: "LHG LTE6 kit", Class: C1,
			Egress: EgressInternet, EgressVia: "lte1"}, RoleWeakRouter, "lte1"},
		{"unknown board with radios behind a router", RoleFacts{Board: "SomeOtherBoard", Class: C2, Radios: 2,
			Gateway: "192.0.2.1"}, RoleAPCPE, "wireless"},
		{"unknown board with radios but only a VPN tunnel", RoleFacts{Board: "SomeOtherBoard", Class: C3, Radios: 1,
			Egress: EgressVPN, EgressVia: "wg0", Gateway: "192.0.2.1"}, RoleAPCPE, "wireless"},
		{"unknown board with radios and no route known", RoleFacts{Board: "SomeOtherBoard", Class: C2, Radios: 2},
			RoleUnknown, "no default route"},
		{"x86 with wifi interfaces (a controller) is never called an access point",
			RoleFacts{Board: "CHR", Architecture: "x86_64", Class: C3, Radios: 4}, RoleUnknown, "router hardware"},
		{"unknown board without egress", RoleFacts{Board: "SomeOtherBoard", Class: C3}, RoleUnknown, "SomeOtherBoard"},
		{"unknown board with internet egress", RoleFacts{Board: "SomeOtherBoard", Class: C3, Egress: EgressInternet,
			EgressVia: "ether1"}, RoleRouter, "ether1"},
		{"nothing read yet", RoleFacts{}, RoleUnknown, "not read yet"},
	} {
		got, reason := ClassifyRole(c.facts)
		if got != c.want {
			t.Errorf("%s: role = %q (%s), want %q", c.name, got, reason, c.want)
		}
		if reason == "" || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: reason %q does not mention %q", c.name, reason, c.reason)
		}
	}
}

// A role is "router" only for a strong device: whatever the facts, C2, C1 and
// unprobed devices never come out as router.
func TestClassifyRoleNeverRouterBelowC3(t *testing.T) {
	for _, board := range []string{"RB5009UG+S+", "CCR2004-16G-2S+", "hAP ax3", "SomeOtherBoard", "CRS326-24G-2S+", ""} {
		for _, class := range []Class{C0Unknown, C1, C2} {
			for _, eg := range []Egress{EgressNone, EgressVPN, EgressInternet} {
				if r, _ := ClassifyRole(RoleFacts{Board: board, Class: class, Egress: eg, EgressVia: "x"}); r == RoleRouter {
					t.Errorf("%q class %s egress %q classified as router", board, class, eg)
				}
			}
		}
	}
}

// Without an egress of its own a device is never a router or weak router.
func TestClassifyRoleNeedsEgress(t *testing.T) {
	for _, board := range []string{"RB5009UG+S+", "CCR2004-16G-2S+", "hAP ax3", "CHR", "SomeOtherBoard", ""} {
		for _, class := range []Class{C0Unknown, C1, C2, C3} {
			for _, gw := range []string{"", "192.0.2.1"} {
				r, _ := ClassifyRole(RoleFacts{Board: board, Class: class, BridgePorts: 4, Gateway: gw})
				if r.RoutesTraffic() {
					t.Errorf("%q class %s without egress (gateway %q) classified as %s", board, class, gw, r)
				}
			}
		}
	}
}

// switch and ap-cpe get a sender blocked and alarmed, so they need positive
// evidence: no fact that merely went missing (stale routes, a WAN that is
// down, a device not read yet) may produce them.
func TestClassifyRoleNeverExportsNeedsEvidence(t *testing.T) {
	for _, board := range []string{"RB5009UG+S+", "CCR2004-16G-2S+", "hAP ax3", "cAP ax", "CHR", "SomeOtherBoard", ""} {
		for _, class := range []Class{C0Unknown, C1, C2, C3} {
			for _, radios := range []int{0, 3} {
				r, reason := ClassifyRole(RoleFacts{Board: board, Class: class, Radios: radios, BridgePorts: 5})
				if r.NeverExports() {
					t.Errorf("%q class %s radios %d with no route facts at all = %s (%s)", board, class, radios, r, reason)
				}
			}
		}
	}
}

func TestParseRole(t *testing.T) {
	for _, r := range []Role{RoleRouter, RoleWeakRouter, RoleSwitch, RoleAPCPE, RoleUnknown} {
		if got, ok := ParseRole(string(r)); !ok || got != r {
			t.Errorf("ParseRole(%q) = %q, %v", r, got, ok)
		}
	}
	if _, ok := ParseRole("gateway"); ok {
		t.Error("ParseRole accepted an unknown role")
	}
	if !RoleRouter.RoutesTraffic() || !RoleWeakRouter.RoutesTraffic() || RoleSwitch.RoutesTraffic() ||
		RoleAPCPE.RoutesTraffic() || RoleUnknown.RoutesTraffic() {
		t.Error("RoutesTraffic: only router and weak-router route")
	}
	if !RoleSwitch.NeverExports() || !RoleAPCPE.NeverExports() || RoleRouter.NeverExports() || RoleUnknown.NeverExports() {
		t.Error("NeverExports: only switch and ap-cpe")
	}
}

// A home router board (hAP, Chateau, …) can be an access point behind the
// real router, or a router behind an ISP modem that hands out a private
// address. A private default gateway does not tell the two apart, and
// "ap-cpe" gets a sender blocked and alarmed: such a board is unknown —
// neither offered nor blocked — while a pure access point or CPE board with
// the same route is ap-cpe.
func TestHomeRouterBoardsAreNotAccessPointsOnARouteAlone(t *testing.T) {
	for _, board := range []string{"hAP ax3", "hAP ac^2", "hAP ax lite LTE6", "Chateau 5G ax", "Audience", "LtAP mini LTE kit", "KNOT LR8 kit"} {
		r, reason := ClassifyRole(RoleFacts{Board: board, Class: C2, Radios: 2, Gateway: "192.0.2.1"})
		if r != RoleUnknown || r.NeverExports() {
			t.Errorf("%q behind a private gateway = %s (%s), want unknown", board, r, reason)
		}
	}
	for _, board := range []string{"cAP ax", "wAP ac", "mAP lite", "SXTsq 5 ax", "LHG 5 ax", "Disc Lite5 ac"} {
		if r, reason := ClassifyRole(RoleFacts{Board: board, Class: C2, Radios: 1, Gateway: "192.0.2.1"}); r != RoleAPCPE {
			t.Errorf("%q behind a private gateway = %s (%s), want ap-cpe", board, r, reason)
		}
	}
}
