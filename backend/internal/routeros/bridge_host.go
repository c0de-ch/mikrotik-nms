package routeros

import (
	"slices"
	"strconv"
	"strings"
)

// BridgeHost is one learned (non-local) bridge FDB entry: a MAC seen behind a
// bridge port. Switch-chip-learned entries (External) are included — on CRS
// switches they are all of them.
type BridgeHost struct {
	MACAddress string // uppercase
	Interface  string // on-interface, falling back to interface
	Bridge     string
	VID        int // 0 when absent
	External   bool
	Dynamic    bool
}

// bridgeHostProplist limits the FDB dump to the attributes parseBridgeHost
// reads. RouterOS reports no "age" attribute on this table.
const bridgeHostProplist = "=.proplist=mac-address,on-interface,interface,bridge,vid," +
	"local,external,dynamic,invalid,disabled"

// GetBridgeHosts dumps the device's bridge FDB (local=false) in one print on
// the pooled client. The first print after boot can take a few seconds (4 s
// cold on an RB5009, ~40 ms after), longer the bigger the L2 domain: it is
// one of the slow reads (SlowCommandTimeout). The dump lands
// in port_hosts, which is also where gateway attachment looks MACs up: the
// NMS never queries a device's FDB per MAC.
func GetBridgeHosts(run Runner) ([]BridgeHost, error) {
	reply, err := run.Run("/interface/bridge/host/print", "?local=false", bridgeHostProplist)
	if err != nil {
		return nil, err
	}
	out := make([]BridgeHost, 0, len(reply.Re))
	for _, re := range reply.Re {
		if h, ok := parseBridgeHost(GetSentenceMap(re)); ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// parseBridgeHost converts one /interface/bridge/host sentence. It returns
// false for local, invalid or disabled entries (a disabled static entry does
// not forward, so it says nothing about where the MAC lives) and rows without
// a MAC or interface (the query already filters local=false; the check is a
// backstop).
func parseBridgeHost(m map[string]string) (BridgeHost, bool) {
	if m["local"] == "true" || m["invalid"] == "true" || m["disabled"] == "true" {
		return BridgeHost{}, false
	}
	mac := strings.ToUpper(strings.TrimSpace(m["mac-address"]))
	iface := m["on-interface"]
	if iface == "" {
		iface = m["interface"]
	}
	if mac == "" || iface == "" {
		return BridgeHost{}, false
	}
	h := BridgeHost{
		MACAddress: mac,
		Interface:  iface,
		Bridge:     m["bridge"],
		External:   m["external"] == "true",
		Dynamic:    m["dynamic"] == "true",
	}
	if v, err := strconv.Atoi(strings.TrimSpace(m["vid"])); err == nil {
		h.VID = v
	}
	return h, true
}

// Interface stacking kinds (InterfaceRelation.Kind).
const (
	RelationBond  = "bond"
	RelationVLAN  = "vlan"
	RelationPPPoE = "pppoe"
)

// InterfaceRelation is one interface-stacking fact: bond membership (Iface is
// a member of the bond Parent) or the interface a VLAN / PPPoE client runs
// over.
type InterfaceRelation struct {
	Iface  string
	Parent string
	Kind   string // RelationBond | RelationVLAN | RelationPPPoE
	// VLANID is a VLAN interface's vlan-id (RelationVLAN only; 0 when
	// absent): with Iface and Parent it is the device's /interface/vlan row,
	// which the flow ifIndex sync reads from here instead of printing it.
	VLANID int
}

// VLANs returns the /interface/vlan rows among rels.
func VLANs(rels []InterfaceRelation) []VLANInfo {
	out := []VLANInfo{}
	for _, r := range rels {
		if r.Kind == RelationVLAN {
			out = append(out, VLANInfo{Name: r.Iface, VLANID: r.VLANID, Parent: r.Parent})
		}
	}
	return out
}

// RelationMenus names the stacking menus a read prints (GetInterfaceRelations).
type RelationMenus struct {
	Bonding, VLAN, PPPoE bool
}

// AllRelationMenus prints every stacking menu: the read of a device whose
// interface list is not at hand.
var AllRelationMenus = RelationMenus{Bonding: true, VLAN: true, PPPoE: true}

// RelationMenusOf returns the stacking menus worth printing on a device whose
// /interface/print lists interfaces of these types: the bonding table only
// with a "bond", the VLAN table only with a "vlan", the PPPoE clients only
// with a "pppoe-out" — every bond, VLAN and PPPoE client is an interface, so
// a menu without one has nothing in it.
func RelationMenusOf(types ...string) RelationMenus {
	var m RelationMenus
	for _, t := range types {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "bond":
			m.Bonding = true
		case "vlan":
			m.VLAN = true
		case "pppoe-out":
			m.PPPoE = true
		}
	}
	return m
}

// RelationRead is one stacking read: the relations of every kind it settled
// — its menu printed, or not printed because the device has nothing in it —
// and the kinds whose print the device refused.
type RelationRead struct {
	Relations []InterfaceRelation
	// Refused lists the kinds (RelationBond, RelationVLAN, RelationPPPoE)
	// whose menu answered with a !trap: what is known of them is unchanged.
	Refused []string
}

// Settled reports whether the read settled kind: its relations are complete.
func (r RelationRead) Settled(kind string) bool {
	return !slices.Contains(r.Refused, kind)
}

// GetInterfaceRelations reads the device's bond membership
// (/interface/bonding slaves), VLAN parents and ids and PPPoE-client carriers
// (interface=) on the pooled client, one short print per menu in menus — a
// menu left out settles its kind as having nothing. A menu the device refuses
// (a !trap) is reported in Refused and the read goes on with the others; any
// other failure (no answer, a session that ended) fails the whole read, so a
// partial answer never replaces a complete one.
func GetInterfaceRelations(run Runner, menus RelationMenus) (RelationRead, error) {
	var read RelationRead
	for _, q := range []struct {
		print         bool
		kind          string
		cmd, proplist string
		parse         func(map[string]string) []InterfaceRelation
	}{
		{menus.Bonding, RelationBond, "/interface/bonding/print", "=.proplist=name,slaves", parseBondSlaves},
		{menus.VLAN, RelationVLAN, "/interface/vlan/print", "=.proplist=name,interface,vlan-id", parseVLANCarrier},
		{menus.PPPoE, RelationPPPoE, "/interface/pppoe-client/print", "=.proplist=name,interface", func(m map[string]string) []InterfaceRelation {
			return parseCarrier(m, RelationPPPoE)
		}},
	} {
		if !q.print {
			continue
		}
		reply, err := run.Run(q.cmd, q.proplist)
		switch {
		case err == nil:
		case isTrap(err) && !IsSessionFatal(err):
			read.Refused = append(read.Refused, q.kind)
			continue
		default:
			return RelationRead{}, err
		}
		for _, re := range reply.Re {
			read.Relations = append(read.Relations, q.parse(GetSentenceMap(re))...)
		}
	}
	return read, nil
}

// parseBondSlaves turns one /interface/bonding row ("slaves" is a comma
// list, e.g. "ether30,ether31") into one relation per member.
func parseBondSlaves(m map[string]string) []InterfaceRelation {
	bond := strings.TrimSpace(m["name"])
	if bond == "" {
		return nil
	}
	var out []InterfaceRelation
	for _, s := range strings.Split(m["slaves"], ",") {
		if s = strings.TrimSpace(s); s != "" && s != bond {
			out = append(out, InterfaceRelation{Iface: s, Parent: bond, Kind: RelationBond})
		}
	}
	return out
}

// parseVLANCarrier turns one /interface/vlan row into its "runs over"
// relation, with the VLAN id.
func parseVLANCarrier(m map[string]string) []InterfaceRelation {
	rels := parseCarrier(m, RelationVLAN)
	if len(rels) == 1 {
		rels[0].VLANID, _ = strconv.Atoi(strings.TrimSpace(m["vlan-id"]))
	}
	return rels
}

// parseCarrier turns one /interface/vlan or /interface/pppoe-client row into
// its "runs over" relation.
func parseCarrier(m map[string]string, kind string) []InterfaceRelation {
	name, parent := strings.TrimSpace(m["name"]), strings.TrimSpace(m["interface"])
	if name == "" || parent == "" || name == parent {
		return nil
	}
	return []InterfaceRelation{{Iface: name, Parent: parent, Kind: kind}}
}
