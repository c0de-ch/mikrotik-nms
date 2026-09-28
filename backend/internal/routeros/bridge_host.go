package routeros

import (
	"strconv"
	"strings"

	ros "github.com/go-routeros/routeros/v3"
)

// FindBridgeHostPort looks a MAC up in the device's bridge host (FDB) table
// and returns the port it was dynamically learned on, or "" when absent.
// Local entries (the bridge's own MACs) are ignored.
func FindBridgeHostPort(client *ros.Client, mac string) (string, error) {
	reply, err := RunCommand(client, "/interface/bridge/host/print", "?mac-address="+mac)
	if err != nil {
		return "", err
	}
	for _, re := range reply.Re {
		m := GetSentenceMap(re)
		if m["local"] == "true" {
			continue
		}
		if iface := m["on-interface"]; iface != "" {
			return iface, nil
		}
		if iface := m["interface"]; iface != "" {
			return iface, nil
		}
	}
	return "", nil
}

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
// cold on an RB5009, ~40 ms after); CommandTimeout covers it.
func GetBridgeHosts(client *ros.Client) ([]BridgeHost, error) {
	reply, err := RunCommand(client, "/interface/bridge/host/print", "?local=false", bridgeHostProplist)
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
}

// GetInterfaceRelations reads the device's bond membership
// (/interface/bonding slaves), VLAN parents and PPPoE-client carriers
// (interface=) on the pooled client — three short prints. Any failing print
// fails the whole read, so a partial answer never replaces a complete one.
func GetInterfaceRelations(client *ros.Client) ([]InterfaceRelation, error) {
	var out []InterfaceRelation
	for _, q := range []struct {
		cmd, proplist string
		parse         func(map[string]string) []InterfaceRelation
	}{
		{"/interface/bonding/print", "=.proplist=name,slaves", parseBondSlaves},
		{"/interface/vlan/print", "=.proplist=name,interface", func(m map[string]string) []InterfaceRelation {
			return parseCarrier(m, RelationVLAN)
		}},
		{"/interface/pppoe-client/print", "=.proplist=name,interface", func(m map[string]string) []InterfaceRelation {
			return parseCarrier(m, RelationPPPoE)
		}},
	} {
		reply, err := RunCommand(client, q.cmd, q.proplist)
		if err != nil {
			return nil, err
		}
		for _, re := range reply.Re {
			out = append(out, q.parse(GetSentenceMap(re))...)
		}
	}
	return out, nil
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

// parseCarrier turns one /interface/vlan or /interface/pppoe-client row into
// its "runs over" relation.
func parseCarrier(m map[string]string, kind string) []InterfaceRelation {
	name, parent := strings.TrimSpace(m["name"]), strings.TrimSpace(m["interface"])
	if name == "" || parent == "" || name == parent {
		return nil
	}
	return []InterfaceRelation{{Iface: name, Parent: parent, Kind: kind}}
}
