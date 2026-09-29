package flowview

import (
	"cmp"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// Suggestion rules.
const (
	RuleGatewayHost = "gateway-host"
	RuleTrunk       = "trunk"
)

// Adjacency is one physical up link from the Phase 1 role graph: DevA's IfA
// faces DevB's IfB (roles peer, uplink, or a downlink to a single device).
type Adjacency struct{ DevA, IfA, DevB, IfB string }

// SuggestInput is everything Suggest reads, loaded by the API layer.
type SuggestInput struct {
	Exporters []queries.FlowExporter
	Ifaces    []queries.FlowIface    // every exporter's ifIndex → name rows
	Points    []queries.FlowPoint    // existing points (for Exists)
	Uplinks   []queries.DeviceUplink // fresh device_uplinks rows
	VLANs     []queries.BridgeVLAN   // bridge VLAN table rows (all devices)
	Hosts     []queries.PortHost     // fresh port_hosts rows
	MACByIP   map[netip.Addr]string  // mac_lookup IP → MAC
	Devices   []queries.Device
	Adjacent  []Adjacency
	// IfaceTypes maps "<device id>\x00<iface>" to the interface type
	// (interfaces table ∪ live snapshot).
	IfaceTypes map[string]string
}

// Suggestion is one derived view the admin can add with one click. Point is
// nil when the rule matched but cannot be completed yet (Reason says why).
type Suggestion struct {
	Rule, Reason string
	Point        *queries.FlowPoint
	Exists       bool
}

// NoteGatewayHost is NOTE_G (contract §6.5) for exporter e and device d.
func NoteGatewayHost(e, d string) string {
	return "Derived from " + e + " NetFlow on its LAN interface: " + d + " cannot export flows, and " + e +
		" is the only device on this port, so this is the port's IP traffic. Non-IP frames (ARP, LLDP, STP) are not included."
}

// NoteTrunk is NOTE_T (contract §6.5) for exporter r, the names of the
// routed interfaces and the excluded ports.
func NoteTrunk(r string, l3Names, excludeNames []string) string {
	note := "Derived from " + r + " IPFIX on " + strings.Join(l3Names, ", ") +
		": routed and router-terminated traffic crossing this trunk. L2-switched VLAN traffic that " + r +
		" does not route is invisible"
	if len(excludeNames) > 0 {
		note += "; hosts on " + r + " " + strings.Join(excludeNames, ", ") + " are excluded"
	}
	return note + "."
}

// ListHas reports whether a RouterOS comma list contains name.
func ListHas(list, name string) bool {
	for _, item := range strings.Split(list, ",") {
		if strings.TrimSpace(item) == name {
			return true
		}
	}
	return false
}

// listItems splits a RouterOS comma list.
func listItems(list string) []string {
	var out []string
	for _, item := range strings.Split(list, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// maxVLANRange is the widest a-b range ExpandVLANIDs expands.
const maxVLANRange = 64

// ExpandVLANIDs expands a RouterOS vlan-ids value ("6,28", "10-12") into
// VIDs; ranges wider than 64 and anything outside 1..4094 are skipped.
func ExpandVLANIDs(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil || b < a || b-a+1 > maxVLANRange {
				continue
			}
		}
		for vid := a; vid <= b; vid++ {
			if vid >= 1 && vid <= 4094 {
				out = append(out, vid)
			}
		}
	}
	return out
}

// ifaceLabel is the iface name, or "if#N" when unnamed.
func ifaceLabel(f queries.FlowIface) string {
	if f.Name != "" {
		return f.Name
	}
	return "if#" + strconv.FormatUint(uint64(f.IfIndex), 10)
}

// suggester holds Suggest's indexes.
type suggester struct {
	in       SuggestInput
	devNames map[string]string
	ifaces   map[int64][]queries.FlowIface // by exporter, ascending ifIndex
	existing map[string]bool               // "<exporter>\x00<device>\x00<iface>" of derived points
}

func (s *suggester) devName(id string) string {
	if n := s.devNames[id]; n != "" {
		return n
	}
	return id
}

func existsKey(exporterID int64, deviceID, iface string) string {
	return strconv.FormatInt(exporterID, 10) + "\x00" + deviceID + "\x00" + iface
}

// Suggest computes the derived-view suggestions (contract §6.5): rule G
// (a managed switch port whose only device is a flow-exporting gateway
// without a device record, e.g. OPNsense) and rule T (a RouterOS exporter's
// bridge trunk to another managed device). Output order: rule G by exporter
// name, then rule T by exporter name and port.
func Suggest(in SuggestInput) []Suggestion {
	s := &suggester{in: in, devNames: map[string]string{}, ifaces: map[int64][]queries.FlowIface{},
		existing: map[string]bool{}}
	for _, d := range in.Devices {
		name := d.Identity
		if name == "" {
			name = d.Address
		}
		s.devNames[d.ID] = name
	}
	for _, f := range in.Ifaces {
		s.ifaces[f.ExporterID] = append(s.ifaces[f.ExporterID], f)
	}
	for _, list := range s.ifaces {
		slices.SortFunc(list, func(a, b queries.FlowIface) int { return cmp.Compare(a.IfIndex, b.IfIndex) })
	}
	for _, p := range in.Points {
		if p.Kind == "derived" {
			s.existing[existsKey(p.ExporterID, p.DeviceID, p.Iface)] = true
		}
	}
	exps := slices.Clone(in.Exporters)
	slices.SortStableFunc(exps, func(a, b queries.FlowExporter) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	out := []Suggestion{}
	for _, e := range exps {
		if e.DeviceID == "" {
			out = append(out, s.gatewayHost(e)...)
		}
	}
	for _, e := range exps {
		if e.DeviceID != "" {
			out = append(out, s.trunk(e)...)
		}
	}
	return out
}

// lanIndex picks exporter e's LAN interface for rule G: the iface with role
// lan, else the iface whose learned hint has a prefix containing addr; the
// highest last_seen wins, then the lowest ifIndex.
func (s *suggester) lanIndex(e queries.FlowExporter, addr netip.Addr) (queries.FlowIface, bool) {
	pick := func(match func(queries.FlowIface) bool) (queries.FlowIface, bool) {
		var best queries.FlowIface
		found := false
		for _, f := range s.ifaces[e.ID] {
			if f.IfIndex == 0 || !match(f) {
				continue
			}
			if !found || f.LastSeen.After(best.LastSeen) {
				best, found = f, true
			}
		}
		return best, found
	}
	if f, ok := pick(func(f queries.FlowIface) bool { return f.Role == "lan" }); ok {
		return f, true
	}
	return pick(func(f queries.FlowIface) bool {
		for _, p := range ParsePrefixList(f.Hint) {
			if p.Contains(addr) {
				return true
			}
		}
		return false
	})
}

// gatewayHost is rule G for one exporter without a device.
func (s *suggester) gatewayHost(e queries.FlowExporter) []Suggestion {
	addr, err := netip.ParseAddr(e.Address)
	if err != nil {
		return nil
	}
	addr = addr.Unmap()
	var out []Suggestion
	for _, u := range s.in.Uplinks {
		if u.Kind != "gateway-host" || u.Interface == "" {
			continue
		}
		gw, err := netip.ParseAddr(u.GatewayIP)
		if err != nil || gw.Unmap() != addr {
			continue
		}
		lan, ok := s.lanIndex(e, addr)
		if !ok {
			out = append(out, Suggestion{Rule: RuleGatewayHost, Reason: "waiting for first flows from " + e.Name})
			continue
		}
		dev := s.devName(u.DeviceID)
		note := NoteGatewayHost(e.Name, dev)
		own := strings.ToUpper(s.in.MACByIP[addr])
		others := map[string]bool{}
		for _, h := range s.in.Hosts {
			if h.DeviceID == u.DeviceID && h.InterfaceName == u.Interface {
				if mac := strings.ToUpper(h.MACAddress); mac != "" && mac != own {
					others[mac] = true
				}
			}
		}
		if k := len(others); k > 0 {
			note += fmt.Sprintf(" Note: the port also carries %d other MAC(s); the view shows only traffic to or from %s.",
				k, e.Name)
		}
		p := &queries.FlowPoint{
			ExporterID: e.ID, Name: dev + " · " + u.Interface, Kind: "derived",
			IfIndexes: []uint32{lan.IfIndex}, Facing: FacingDown, PortSide: "peer",
			DeviceID: u.DeviceID, Iface: u.Interface, LocalInternal: true,
			ExcludeAttach: []queries.PortRef{}, Note: note, Enabled: true,
		}
		out = append(out, Suggestion{
			Rule:   RuleGatewayHost,
			Reason: fmt.Sprintf("%s (%s) is the gateway host on %s %s; its %s interface is this port's traffic", e.Name, e.Address, dev, u.Interface, ifaceLabel(lan)),
			Point:  p,
			Exists: s.existing[existsKey(e.ID, u.DeviceID, u.Interface)],
		})
	}
	return out
}

// ifaceType is the type of (device, iface): interfaces table / snapshot,
// then the exporter's synced flow iface of that name.
func (s *suggester) ifaceType(exporterID int64, deviceID, iface string) string {
	if t := s.in.IfaceTypes[deviceID+"\x00"+iface]; t != "" {
		return t
	}
	for _, f := range s.ifaces[exporterID] {
		if f.Name == iface && f.Type != "" {
			return f.Type
		}
	}
	return ""
}

// trunk is rule T for one exporter with a device: every physical up link
// of that device gets a pair of trunk views (the far end, peer; the near
// end, same) over the routed interfaces of the VLANs the port carries.
func (s *suggester) trunk(e queries.FlowExporter) []Suggestion {
	var rows []queries.BridgeVLAN
	for _, v := range s.in.VLANs {
		if v.DeviceID == e.DeviceID {
			rows = append(rows, v)
		}
	}
	adj := []Adjacency{}
	for _, a := range s.in.Adjacent {
		if a.DevA == e.DeviceID && a.IfA != "" && a.DevB != "" && a.IfB != "" {
			adj = append(adj, a)
		}
	}
	slices.SortFunc(adj, func(a, b Adjacency) int {
		return cmp.Or(cmp.Compare(a.IfA, b.IfA), cmp.Compare(a.DevB, b.DevB), cmp.Compare(a.IfB, b.IfB))
	})
	rName := s.devName(e.DeviceID)
	var out []Suggestion
	seen := map[string]bool{}
	for _, a := range adj {
		q := a.IfA
		if seen[q+"\x00"+a.DevB+"\x00"+a.IfB] || !topology.IsPhysicalPort(q, s.ifaceType(e.ID, e.DeviceID, q)) {
			continue
		}
		seen[q+"\x00"+a.DevB+"\x00"+a.IfB] = true

		// 1. VIDs(q): VLANs of the rows that carry q (tagged or untagged).
		vids := map[int]bool{}
		var carrying []queries.BridgeVLAN
		for _, r := range rows {
			if ListHas(r.CurrentTagged, q) || ListHas(r.CurrentUntagged, q) {
				carrying = append(carrying, r)
				for _, vid := range ExpandVLANIDs(r.VLANIDs) {
					vids[vid] = true
				}
			}
		}
		if len(vids) == 0 {
			continue
		}
		hasVID := func(r queries.BridgeVLAN) bool {
			for _, vid := range ExpandVLANIDs(r.VLANIDs) {
				if vids[vid] {
					return true
				}
			}
			return false
		}
		// 2. L3(q): the VLAN interfaces on the carrying rows' bridges with a
		// VID of q, plus the bridge itself where it is an untagged member
		// of such a VLAN.
		l3 := map[uint32]queries.FlowIface{}
		bridges := map[string]bool{}
		for _, r := range carrying {
			bridges[r.BridgeName] = true
			for _, f := range s.ifaces[e.ID] {
				if f.IfIndex != 0 && f.Type == "vlan" && vids[f.VLANID] && f.Parent == r.BridgeName {
					l3[f.IfIndex] = f
				}
			}
		}
		for _, r := range rows {
			if !bridges[r.BridgeName] || !hasVID(r) || !ListHas(r.CurrentUntagged, r.BridgeName) {
				continue
			}
			for _, f := range s.ifaces[e.ID] {
				if f.IfIndex != 0 && f.Name == r.BridgeName {
					l3[f.IfIndex] = f
				}
			}
		}
		// 3. Nothing routed on this trunk: no view.
		if len(l3) == 0 {
			continue
		}
		idxs := make([]uint32, 0, len(l3))
		for idx := range l3 {
			idxs = append(idxs, idx)
		}
		slices.Sort(idxs)
		l3Names := make([]string, len(idxs))
		for i, idx := range idxs {
			l3Names[i] = ifaceLabel(l3[idx])
		}
		// 4. Exclude the hosts on the other physical ports of those VLANs
		// (switched locally, never crossing the trunk).
		excl := map[string]bool{}
		for _, r := range rows {
			if !hasVID(r) {
				continue
			}
			for _, x := range append(listItems(r.CurrentTagged), listItems(r.CurrentUntagged)...) {
				if x != q && topology.IsPhysicalPort(x, s.ifaceType(e.ID, e.DeviceID, x)) {
					excl[x] = true
				}
			}
		}
		exclNames := make([]string, 0, len(excl))
		for x := range excl {
			exclNames = append(exclNames, x)
		}
		slices.Sort(exclNames)
		exclude := make([]queries.PortRef, len(exclNames))
		for i, x := range exclNames {
			exclude[i] = queries.PortRef{DeviceID: e.DeviceID, Iface: x}
		}
		note := NoteTrunk(e.Name, l3Names, exclNames)
		dName := s.devName(a.DevB)
		reason := fmt.Sprintf("%s %s is a trunk to %s %s; %s routes %s", rName, q, dName, a.IfB, e.Name,
			strings.Join(l3Names, ", "))
		mk := func(name, side, dev, iface string) Suggestion {
			return Suggestion{
				Rule: RuleTrunk, Reason: reason,
				Point: &queries.FlowPoint{
					ExporterID: e.ID, Name: name, Kind: "derived", IfIndexes: slices.Clone(idxs),
					Facing: FacingDown, PortSide: side, DeviceID: dev, Iface: iface,
					ExcludeAttach: slices.Clone(exclude), Note: note, Enabled: true,
				},
				Exists: s.existing[existsKey(e.ID, dev, iface)],
			}
		}
		out = append(out,
			mk(dName+" · "+a.IfB, "peer", a.DevB, a.IfB),
			mk(rName+" · "+q, "same", e.DeviceID, q))
	}
	return out
}
