package devclass

import (
	"fmt"
	"strings"
)

// Role is what a device does in the network, as far as the stored facts say
// it clearly. The CPU class keys how much the NMS may ask of a device; the
// role keys what may be asked for at all — above all whether flow export may
// be suggested for it or accepted from it.
type Role string

const (
	// RoleRouter is an L3 boundary — a device that routes an internet or VPN
	// egress of its own — with a capable CPU (class C3).
	RoleRouter Role = "router"
	// RoleWeakRouter is an L3 boundary on a small or weak CPU (C2, C1) or on
	// one that has not been probed yet.
	RoleWeakRouter Role = "weak-router"
	// RoleSwitch is switch hardware (CRS/CSS): it forwards in the switch
	// chip, whatever else is configured on it.
	RoleSwitch Role = "switch"
	// RoleAPCPE is an access point or CPE that sits behind another router.
	RoleAPCPE Role = "ap-cpe"
	// RoleUnknown is everything the facts do not settle: the conservative
	// answer, never upgraded to router by a guess.
	RoleUnknown Role = "unknown"
)

// ParseRole returns the Role for its string form.
func ParseRole(s string) (Role, bool) {
	switch r := Role(strings.ToLower(strings.TrimSpace(s))); r {
	case RoleRouter, RoleWeakRouter, RoleSwitch, RoleAPCPE, RoleUnknown:
		return r, true
	}
	return "", false
}

// RoutesTraffic reports the roles that are an L3 boundary (router and
// weak-router): the only ones flow export is ever suggested for.
func (r Role) RoutesTraffic() bool { return r == RoleRouter || r == RoleWeakRouter }

// NeverExports reports the roles that must never export flows: a switch
// forwards in hardware and an access point or CPE bridges, so the export
// shows next to nothing and costs a small CPU.
func (r Role) NeverExports() bool { return r == RoleSwitch || r == RoleAPCPE }

// Label is the role as an operator reads it.
func (r Role) Label() string {
	switch r {
	case RoleRouter:
		return "router"
	case RoleWeakRouter:
		return "weak router"
	case RoleSwitch:
		return "switch"
	case RoleAPCPE:
		return "access point / CPE"
	}
	return "unknown role"
}

// BoardFamily is what kind of hardware a board-name says the device is.
type BoardFamily string

const (
	FamilyUnknown BoardFamily = ""
	FamilyRouter  BoardFamily = "router"
	FamilySwitch  BoardFamily = "switch"
	FamilyAP      BoardFamily = "ap" // access points and CPEs
)

// Board-name prefixes (lower case) per family. Switches first: a CRS routes
// on paper, but its CPU is a management CPU.
var (
	switchBoards = []string{"crs", "css", "netpower", "netfiber", "fiberbox"}
	apBoards     = []string{"cap", "wap", "hap", "map", "audience", "ltap", "chateau", "knot", "lhg", "sxt", "disc",
		"ldf", "mant", "sextant", "netmetal", "netbox", "basebox", "omnitik", "groove", "metal", "qrt", "dynadish",
		"cube", "nray", "wireless wire", "wsap"}
	routerBoards = []string{"ccr", "rb", "hex", "l009", "chr", "powerbox"}
)

// BoardFamilyOf maps a RouterOS board-name ("CRS326-24G-2S+", "cAP ax",
// "RB5009UG+S+") and architecture to its hardware family. An x86 device is PC
// hardware and counts as a router board whatever its name. Unknown names are
// FamilyUnknown — never a router by default.
func BoardFamilyOf(board, arch string) BoardFamily {
	b := strings.ToLower(strings.TrimSpace(board))
	hasPrefix := func(list []string) bool {
		for _, p := range list {
			if strings.HasPrefix(b, p) {
				return true
			}
		}
		return false
	}
	switch {
	case b != "" && hasPrefix(switchBoards):
		return FamilySwitch
	case b != "" && hasPrefix(apBoards):
		return FamilyAP
	case b != "" && hasPrefix(routerBoards), isX86(normalizeArch(arch)):
		return FamilyRouter
	}
	return FamilyUnknown
}

// Egress is the strongest egress of its own a device has.
type Egress string

const (
	EgressNone     Egress = ""
	EgressVPN      Egress = "vpn"      // a running tunnel, or a default route into one
	EgressInternet Egress = "internet" // a default route to a public next hop or out a WAN interface
)

// RoleFacts are the stored facts the role is derived from. None of them costs
// a device a command: they come from the inventory read, the default-route
// read and the interface and bridge tables the pollers already keep.
type RoleFacts struct {
	Board        string // devices.board
	Architecture string // devices.architecture
	// Class is the probed CPU class — what the hardware is, whatever an
	// admin budgets it as; C0Unknown while the device has never been probed.
	Class Class
	// Egress is the device's own fresh egress (device_uplinks): never a
	// default route via a managed device or a private gateway — every
	// switch has one of those for its management address.
	Egress    Egress
	EgressVia string // the egress interface, for the reason
	// Gateway is the next hop of a fresh default route that points at
	// another router on the LAN (a private or managed address); "" when the
	// device has no such route. It is the positive sign that an access
	// point sits behind a router instead of being one.
	Gateway string
	// Radios counts the wireless interfaces in the stored interface list —
	// the device's wifi role. A controller holds the radios of its access
	// points there too, so the count is only read for boards whose family
	// is unknown.
	Radios int
	// BridgePorts counts the ports of its bridges; it only words the reason
	// for router hardware that shows no egress.
	BridgePorts int
}

// ClassifyRole derives a device's role and the reason an operator can read.
// The first matching rule wins:
//
//  1. Switch hardware (CRS/CSS family) is a switch, whatever its routes.
//  2. An access-point or CPE board — or a board of unknown family with
//     wireless interfaces — that has no internet egress of its own is ap-cpe
//     when its default route points at another router on the LAN, and
//     unknown while the NMS knows no default route on it (a router whose WAN
//     is down must not turn into an access point). A VPN tunnel alone does
//     not make such a board a router.
//  3. A device with an internet or VPN egress of its own is an L3 boundary:
//     router on class C3, weak-router on anything else.
//  4. Everything else is unknown. Router hardware without an egress of its
//     own may be working as a switch; it is never called a router.
//
// switch and ap-cpe are positive findings (hardware, or hardware plus a
// route), because senders with those roles are blocked and alarmed; router
// needs the egress, because routers are offered and accepted.
func ClassifyRole(f RoleFacts) (Role, string) {
	board := strings.TrimSpace(f.Board)
	name := board
	if name == "" {
		name = "this device"
	}
	family := BoardFamilyOf(board, f.Architecture)

	if family == FamilySwitch {
		reason := name + " is a switch (CRS/CSS hardware): it forwards in the switch chip, so a flow export sees " +
			"almost nothing and only loads its CPU"
		if f.Egress != EgressNone {
			reason += "; a default route of its own does not change that"
		}
		return RoleSwitch, reason
	}

	apLike := family == FamilyAP || (family == FamilyUnknown && f.Radios > 0)
	if apLike && f.Egress != EgressInternet {
		what := name + " is an access point / CPE board"
		if family != FamilyAP {
			what = fmt.Sprintf("%s has %d wireless interface(s)", name, f.Radios)
		}
		vpn := ""
		if f.Egress == EgressVPN {
			vpn = " (a VPN tunnel alone does not make it a router)"
		}
		if f.Gateway == "" {
			return RoleUnknown, what + ", but the NMS knows no default route on it right now, so it cannot tell " +
				"whether it routes" + vpn
		}
		return RoleAPCPE, what + " and its default route points at another router (" + f.Gateway +
			"), so it is treated as an access point or CPE" + vpn
	}

	if f.Egress != EgressNone {
		egress := "internet egress"
		if f.Egress == EgressVPN {
			egress = "VPN egress"
		}
		if f.EgressVia != "" {
			egress += " (" + f.EgressVia + ")"
		}
		switch f.Class {
		case C3:
			return RoleRouter, name + " routes its own " + egress + " and has a capable CPU (class C3)"
		case C2, C1:
			return RoleWeakRouter, fmt.Sprintf("%s routes its own %s, but has a %s CPU (class %s)", name, egress,
				f.Class.Label(), f.Class)
		}
		return RoleWeakRouter, name + " routes its own " + egress + ", but its CPU class has not been probed yet"
	}

	switch {
	case board == "" && family == FamilyUnknown:
		return RoleUnknown, "not read yet: the NMS has no board name for this device and knows no egress of its own on it"
	case family == FamilyRouter:
		reason := name + " is router hardware, but the NMS knows no internet or VPN egress of its own on it"
		if f.BridgePorts > 0 {
			reason += fmt.Sprintf(" and %d of its ports are bridged", f.BridgePorts)
		}
		return RoleUnknown, reason + ": it may be working as a switch"
	}
	return RoleUnknown, name + " is not a known router, switch or access-point board, and the NMS knows no internet " +
		"or VPN egress of its own on it"
}
