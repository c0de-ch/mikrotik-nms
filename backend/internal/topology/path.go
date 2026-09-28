package topology

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// This file derives the traffic "role graph": which managed device hangs off
// which port of which other device, what every (device, port) is for (WAN,
// uplink, downlink, access, …), the Internet → port path, and an estimated
// source → sink Sankey built from port counters. It is pure — no DB, no I/O;
// the API layer loads PathInput from the queries package.
//
// MNDP neighbours on a flat L2 form a near-mesh (every device hears every
// other through the switches), so a BFS over links would hang everything off
// the router. The tree is instead inferred per device pair from the port on
// which X hears Y ("nearest ancestor"): see BuildRoleGraph.

// ---------- inputs ----------

// PathDevice is one managed device.
type PathDevice struct {
	ID      string
	Name    string // identity, or address when identity is empty
	Type    string // router|switch|ap|unknown (inferDeviceType(board)); informational
	Status  string
	Address string // used to recognise managed next-hops
}

// PathLink is one links-table row. Interface names are raw MNDP values
// ("ether1,bridge", "net28", "unknown"); only Status "up" rows are used.
type PathLink struct {
	DeviceA, IfaceA string
	DeviceB, IfaceB string
	Status          string
}

// PathUplink is one fresh device_uplinks row.
type PathUplink struct {
	DeviceID, Kind, Interface, IfaceType, GatewayIP string // Kind: default-route | vpn | gateway-host
}

// PathIface is one known interface (interfaces table ∪ live port snapshot).
type PathIface struct {
	DeviceID, Name, Type, MAC string
	Running, Disabled         bool
}

// PathHost is one fresh bridge FDB entry (port_hosts row).
type PathHost struct {
	DeviceID, Iface, MAC string // MAC uppercase
	VID                  int
}

// Interface stacking kinds (PathRelation.Kind).
const (
	RelationBond  = "bond"  // Iface is a member (slave) of the bond Parent
	RelationVLAN  = "vlan"  // Iface is a VLAN interface running over Parent
	RelationPPPoE = "pppoe" // Iface is a PPPoE client running over Parent
)

// PathRelation is one interface-stacking fact read from the device: bond
// membership, or the interface a VLAN / PPPoE client runs over.
type PathRelation struct {
	DeviceID, Iface, Parent, Kind string
}

// PathInput is everything BuildRoleGraph needs.
type PathInput struct {
	Devices       []PathDevice
	Links         []PathLink
	Uplinks       []PathUplink
	Interfaces    []PathIface
	Hosts         []PathHost
	Relations     []PathRelation    // optional: bond members, VLAN/PPPoE carriers
	GatewayLabels map[string]string // gateway IP → display label (optional; fallback = IP)
}

// RateFunc returns the current (or range-average) rates for a port, in bits/s.
// ok=false means "not measured" (unknown port / no data).
type RateFunc func(deviceID, iface string) (rxBps, txBps int64, ok bool)

// ---------- role graph ----------

// Port roles. Download is rx on the upstream-facing roles (wan, uplink, vpn)
// and tx on the downstream-facing ones (downlink, access, wireless); peer,
// virtual and idle ports have no download/upload reading.
const (
	RoleWAN      = "wan"
	RoleUplink   = "uplink"
	RoleDownlink = "downlink"
	// RolePeer is a physical port on an up link to another managed device
	// that is not an edge of the inferred tree: a cable between two anchored
	// roots (a second router with its own egress), or a redundant / ring /
	// parallel link the tree did not choose.
	RolePeer     = "peer"
	RoleAccess   = "access"
	RoleWireless = "wireless"
	RoleVPN      = "vpn"
	RoleVirtual  = "virtual"
	RoleIdle     = "idle"
)

// Hop kinds, Sankey node types and synthetic node kinds share these strings.
const (
	kindInternet = "internet"
	kindGateway  = "gateway"
	kindVPN      = "vpn"
	kindDevice   = "device"
	kindClients  = "clients"
	kindPort     = "port"
	kindOther    = "other"

	internetID = "internet"
)

// PortRoleInfo is the derived role of one (device, interface).
//
// A bond member takes its bond's role and neighbour (a member of a WAN bond
// is an uplink toward the same upstream) with Master set and no clients of
// its own. The physical port under a WAN that runs over a VLAN or PPPoE
// client is an uplink toward that WAN's upstream node.
type PortRoleInfo struct {
	DeviceID         string `json:"device_id"`
	Iface            string `json:"iface"`
	Role             string `json:"role"`
	NeighborDeviceID string `json:"neighbor_device_id,omitempty"` // uplink: parent device; downlink: first child by name; peer: the device across the link
	NeighborName     string `json:"neighbor_name,omitempty"`      // device name, or the gw/vpn/internet label for a synthetic parent
	NeighborIface    string `json:"neighbor_iface,omitempty"`     // port on the neighbor facing this port
	NeighborCount    int    `json:"neighbor_count"`               // downlink: # child devices on this port; else 0
	ClientCount      int    `json:"client_count"`                 // # non-managed MACs ATTACHED here (access/wireless only; else 0)
	MACCount         int    `json:"mac_count"`                    // # distinct MACs learned on this port (any role)
	GatewayIP        string `json:"gateway_ip,omitempty"`         // wan via gateway-host
	UpstreamNode     string `json:"upstream_node,omitempty"`      // wan/vpn/uplink-to-synthetic: "internet" | "gw:<ip>" | "vpn:<dev>:<iface>"
	Master           string `json:"master,omitempty"`             // bond members: the bond this port belongs to
}

// DeviceTreeInfo is one device's place in the inferred tree.
type DeviceTreeInfo struct {
	DeviceID    string `json:"device_id"`
	Name        string `json:"name"`
	ParentID    string `json:"parent_id"`    // device id, synthetic node id ("internet", "gw:<ip>", "vpn:<dev>:<iface>") or "" (orphan)
	ParentIface string `json:"parent_iface"` // port on the parent DEVICE facing this device ("" when parent is synthetic/none)
	UplinkIface string `json:"uplink_iface"` // this device's port facing its parent ("" when unknown)
	Depth       int    `json:"depth"`        // 0 = directly under a synthetic node or orphan root
	Anchor      bool   `json:"anchor"`
}

// RoleGraph is the inferred physical tree plus per-port roles. Build it with
// BuildRoleGraph; it is read-only afterwards and safe for concurrent use.
type RoleGraph struct {
	devs     map[string]PathDevice
	byID     []string // device ids, sorted
	byName   []string // device ids ordered by name, then id
	anchored bool
	gwLabels map[string]string

	anchor     map[string]bool
	edgePorts  map[string][]string // anchor → its ports toward its synthetic parent (all WAN ports of a dual-WAN edge)
	parent     map[string]string   // device → device id, synthetic node id, or "" (orphan)
	parentPort map[string]string   // device → port on its parent DEVICE facing it
	uplink     map[string]string   // device → its own port facing its parent (anchor: the anchor port)
	logical    map[string]bool     // parent is a routed-only gw:<ip> (no port, unmeasured)
	depth      map[string]int
	children   map[string][]string // node id (device or synthetic) → child device ids, by name
	synth      map[string]synthNode
	synthKids  map[string][]string // synthetic id → synthetic child ids, sorted
	kidsOnPort map[portKey][]string

	ports      map[string][]string // device → every known iface, sorted
	roles      map[portKey]PortRoleInfo
	attached   map[portKey][]string // access/wireless port → sorted client MACs
	attachment map[string]portKey   // client MAC → the edge port it is attached to
}

type portKey struct{ dev, iface string }

type synthNode struct {
	kind   string // internet | gateway | vpn
	label  string
	parent string // "" for the internet node
}

// anchorRow is a device whose upstream is known from its egress data.
type anchorRow struct {
	device, port, parent, parentLabel string
}

// pairPorts[x][y] is a set of ports on x related to device y.
type pairPorts map[string]map[string]map[string]bool

func (pp pairPorts) add(x, y, port string) {
	if pp[x] == nil {
		pp[x] = make(map[string]map[string]bool)
	}
	if pp[x][y] == nil {
		pp[x][y] = make(map[string]bool)
	}
	pp[x][y][port] = true
}

// portIndex is the per-port view of the inputs used while building.
type portIndex struct {
	types    map[portKey]string
	managed  map[string]string // interface MAC → owning device
	universe map[portKey]bool  // every (device, iface) that gets a role
	hostsAt  map[portKey]map[string]bool
	po       map[string]map[string]string // portOf(X, Y)
	sCount   map[portKey]int              // S(X, p) = |{Y : portOf(X, Y) == p}|
	master   map[portKey]string           // bond member → its bond
	carrier  map[portKey]string           // VLAN / PPPoE client → the interface it runs over
	// linkPeer[(X, p)][Y] is the set of Y's ports on up links X.p ↔ Y (bond
	// members folded into their bond; empty when Y's end is unknown).
	linkPeer map[portKey]map[string]map[string]bool
}

// bondOf maps a bond member to its bond; any other port to itself.
func (ix *portIndex) bondOf(dev, iface string) string {
	if m := ix.master[portKey{dev, iface}]; m != "" {
		return m
	}
	return iface
}

// typeOf is the interface's RouterOS type, guessed from its name when unknown.
func (ix *portIndex) typeOf(k portKey) string {
	if t := ix.types[k]; t != "" {
		return t
	}
	return guessIfaceType(k.iface)
}

func (ix *portIndex) portOf(x, y string) string { return ix.po[x][y] }

// egress is the parsed uplink data.
type egress struct {
	anchors   []anchorRow         // first anchor per device, in precedence order
	wanUp     map[portKey]string  // wan port → upstream synthetic node
	logicalGW map[string]string   // device → routed-only private gateway (no gateway-host row)
	defGW     map[string][]string // device → its default-route next-hop IPs, sorted
	gwHolder  map[string]string   // gateway IP → device whose FDB found it (gateway-host row)
}

// BuildRoleGraph infers the physical tree and every port's role:
//
//  1. portOf(X, Y) — the adjacency port on X facing Y — from MNDP links (L)
//     and bridge-FDB entries for Y's interface MACs (F).
//  2. Anchors — devices whose upstream is known from egress data: a public or
//     interface-only default route (→ internet), the FDB attachment port of an
//     unmanaged gateway (→ gw:<ip>), an interface-only route over a tunnel
//     (→ vpn:<dev>:<iface>).
//  3. uplink[D] — the anchor port, else the port on which D hears the first
//     anchor, else the port on which D hears the most devices.
//  4. X is an ancestor of D when X hears D on a non-uplink port and D hears X
//     on its uplink; D's parent is its deepest ancestor.
//  5. Defensive cycle breaking, then synthetic / routed-gateway / orphan roots.
//  6. Per-port roles, then each client MAC's attachment port.
func BuildRoleGraph(in PathInput) *RoleGraph {
	g := &RoleGraph{
		devs:       make(map[string]PathDevice),
		gwLabels:   make(map[string]string),
		anchor:     make(map[string]bool),
		edgePorts:  make(map[string][]string),
		parent:     make(map[string]string),
		parentPort: make(map[string]string),
		uplink:     make(map[string]string),
		logical:    make(map[string]bool),
		depth:      make(map[string]int),
		children:   make(map[string][]string),
		synth:      make(map[string]synthNode),
		synthKids:  make(map[string][]string),
		kidsOnPort: make(map[portKey][]string),
		ports:      make(map[string][]string),
		roles:      make(map[portKey]PortRoleInfo),
		attached:   make(map[portKey][]string),
		attachment: make(map[string]portKey),
	}
	for ip, label := range in.GatewayLabels {
		g.gwLabels[ip] = label
	}
	for _, d := range in.Devices {
		if d.ID == "" {
			continue
		}
		if _, dup := g.devs[d.ID]; dup {
			continue
		}
		if d.Name == "" {
			d.Name = d.Address
		}
		if d.Name == "" {
			d.Name = d.ID
		}
		g.devs[d.ID] = d
		g.byID = append(g.byID, d.ID)
	}
	sort.Strings(g.byID)
	g.byName = append([]string(nil), g.byID...)
	sort.SliceStable(g.byName, func(i, j int) bool {
		return g.devs[g.byName[i]].Name < g.devs[g.byName[j]].Name
	})

	ix := g.indexPorts(in)
	eg := g.parseEgress(in.Uplinks)
	g.inferTree(ix, eg)
	g.assignRoles(ix, eg)
	return g
}

func (g *RoleGraph) known(id string) bool { _, ok := g.devs[id]; return ok }

// indexPorts collects interface types, managed MACs, FDB contents and the
// port on each device facing each other device (portOf).
func (g *RoleGraph) indexPorts(in PathInput) *portIndex {
	ix := &portIndex{
		types:    make(map[portKey]string),
		managed:  make(map[string]string),
		universe: make(map[portKey]bool),
		hostsAt:  make(map[portKey]map[string]bool),
		po:       make(map[string]map[string]string),
		sCount:   make(map[portKey]int),
		master:   make(map[portKey]string),
		carrier:  make(map[portKey]string),
		linkPeer: make(map[portKey]map[string]map[string]bool),
	}
	for _, r := range in.Relations {
		iface, parent := strings.TrimSpace(r.Iface), strings.TrimSpace(r.Parent)
		if !g.known(r.DeviceID) || iface == "" || parent == "" || iface == parent {
			continue
		}
		k := portKey{r.DeviceID, iface}
		switch r.Kind {
		case RelationBond:
			if cur := ix.master[k]; cur == "" || parent < cur {
				ix.master[k] = parent
			}
		case RelationVLAN, RelationPPPoE:
			if cur := ix.carrier[k]; cur == "" || parent < cur {
				ix.carrier[k] = parent
			}
		}
	}
	// A bond nested in another bond is not a thing RouterOS allows; keep
	// membership one level deep so bondOf never chains.
	for k, m := range ix.master {
		if ix.master[portKey{k.dev, m}] != "" {
			delete(ix.master, k)
		}
	}
	for _, i := range in.Interfaces {
		name := strings.TrimSpace(i.Name)
		if !g.known(i.DeviceID) || name == "" {
			continue
		}
		k := portKey{i.DeviceID, name}
		if ix.types[k] == "" {
			ix.types[k] = i.Type
		}
		ix.universe[k] = true
		if mac := normMAC(i.MAC); mac != "" {
			if _, ok := ix.managed[mac]; !ok {
				ix.managed[mac] = i.DeviceID
			}
		}
	}
	for _, u := range in.Uplinks {
		iface := strings.TrimSpace(u.Interface)
		if !g.known(u.DeviceID) || iface == "" {
			continue
		}
		k := portKey{u.DeviceID, iface}
		if ix.types[k] == "" {
			ix.types[k] = u.IfaceType
		}
		ix.universe[k] = true
	}
	adjacent := func(k portKey) bool { return isAdjacencyPort(k.iface, ix.typeOf(k)) }

	// L: adjacency ports on X over which X hears Y via MNDP, both directions.
	// VLAN/bridge ports are never adjacency ports — on a flat L2 they hear
	// everything and would collapse the tree into a star. A bond member
	// stands for its bond: the bond carries (and counts) the traffic.
	L := pairPorts{}
	notePeer := func(x, p, y, q string) {
		k := portKey{x, p}
		if ix.linkPeer[k] == nil {
			ix.linkPeer[k] = make(map[string]map[string]bool)
		}
		if ix.linkPeer[k][y] == nil {
			ix.linkPeer[k][y] = make(map[string]bool)
		}
		if q != "" {
			ix.linkPeer[k][y][q] = true
		}
	}
	for _, l := range in.Links {
		if l.Status != "up" || l.DeviceA == l.DeviceB || !g.known(l.DeviceA) || !g.known(l.DeviceB) {
			continue
		}
		pa, pb := normPort(l.IfaceA), normPort(l.IfaceB)
		if pa != "" {
			ix.universe[portKey{l.DeviceA, pa}] = true
			pa = ix.bondOf(l.DeviceA, pa)
			if !adjacent(portKey{l.DeviceA, pa}) {
				pa = ""
			}
		}
		if pb != "" {
			ix.universe[portKey{l.DeviceB, pb}] = true
			pb = ix.bondOf(l.DeviceB, pb)
			if !adjacent(portKey{l.DeviceB, pb}) {
				pb = ""
			}
		}
		if pa != "" {
			L.add(l.DeviceA, l.DeviceB, pa)
			notePeer(l.DeviceA, pa, l.DeviceB, pb)
		}
		if pb != "" {
			L.add(l.DeviceB, l.DeviceA, pb)
			notePeer(l.DeviceB, pb, l.DeviceA, pa)
		}
	}

	// F: adjacency ports on X whose FDB learned one of Y's interface MACs.
	F := pairPorts{}
	for _, h := range in.Hosts {
		iface, mac := strings.TrimSpace(h.Iface), normMAC(h.MAC)
		if !g.known(h.DeviceID) || iface == "" || mac == "" {
			continue
		}
		k := portKey{h.DeviceID, iface}
		ix.universe[k] = true
		if ix.hostsAt[k] == nil {
			ix.hostsAt[k] = make(map[string]bool)
		}
		ix.hostsAt[k][mac] = true
		fk := portKey{h.DeviceID, ix.bondOf(h.DeviceID, iface)}
		if y, ok := ix.managed[mac]; ok && y != h.DeviceID && g.known(y) && adjacent(fk) {
			F.add(h.DeviceID, y, fk.iface)
		}
	}

	for k := range ix.universe {
		g.ports[k.dev] = append(g.ports[k.dev], k.iface)
	}
	for _, ps := range g.ports {
		sort.Strings(ps)
	}

	for _, x := range g.byID {
		wired := func(p string) bool { return IsPhysicalPort(p, ix.typeOf(portKey{x, p})) }
		for _, y := range g.byID {
			if x == y {
				continue
			}
			p := choosePort(L[x][y], F[x][y], wired)
			if p == "" {
				continue
			}
			if ix.po[x] == nil {
				ix.po[x] = make(map[string]string)
			}
			ix.po[x][y] = p
			ix.sCount[portKey{x, p}]++
		}
	}
	return ix
}

// parseEgress finds the anchors — groups a (internet edge), b (gateway-host
// attachment), c (VPN egress), each ordered by device name then iface, the
// first anchor per device winning — plus WAN ports and routed-only gateways.
func (g *RoleGraph) parseEgress(uplinks []PathUplink) egress {
	eg := egress{wanUp: make(map[portKey]string), logicalGW: make(map[string]string),
		defGW: make(map[string][]string), gwHolder: make(map[string]string)}
	managedIP := make(map[string]bool)
	for _, d := range g.devs {
		if d.Address != "" {
			managedIP[d.Address] = true
		}
	}
	gwHosted := make(map[string]bool)
	for _, u := range uplinks {
		if u.Kind == "gateway-host" && u.GatewayIP != "" {
			gwHosted[u.GatewayIP] = true
		}
	}
	var grpA, grpB, grpC []anchorRow
	for _, u := range uplinks {
		if !g.known(u.DeviceID) {
			continue
		}
		iface := strings.TrimSpace(u.Interface)
		switch u.Kind {
		case "default-route":
			if gw := strings.TrimSpace(u.GatewayIP); gw != "" && !slices.Contains(eg.defGW[u.DeviceID], gw) {
				eg.defGW[u.DeviceID] = append(eg.defGW[u.DeviceID], gw)
			}
			switch classifyDefaultRoute(u.GatewayIP, iface, u.IfaceType, managedIP) {
			case egressInternet:
				grpA = append(grpA, anchorRow{u.DeviceID, iface, internetID, "Internet"})
			case egressVPN:
				grpC = append(grpC, anchorRow{u.DeviceID, iface, vpnNodeID(u.DeviceID, iface), iface})
			case egressGateway:
				if gw := u.GatewayIP; !gwHosted[gw] {
					if cur, ok := eg.logicalGW[u.DeviceID]; !ok || gw < cur {
						eg.logicalGW[u.DeviceID] = gw
					}
				}
			}
		case "gateway-host":
			if u.GatewayIP != "" {
				grpB = append(grpB, anchorRow{u.DeviceID, iface, gatewayNodeID(u.GatewayIP), g.gatewayLabel(u.GatewayIP)})
			}
		}
	}
	seen := make(map[string]bool)
	for _, grp := range [][]anchorRow{grpA, grpB, grpC} {
		sort.SliceStable(grp, func(i, j int) bool {
			a, b := grp[i], grp[j]
			if na, nb := g.devs[a.device].Name, g.devs[b.device].Name; na != nb {
				return na < nb
			}
			if a.device != b.device {
				return a.device < b.device
			}
			if a.port != b.port {
				return a.port < b.port
			}
			return a.parent < b.parent
		})
		for _, a := range grp {
			if !seen[a.device] {
				seen[a.device] = true
				eg.anchors = append(eg.anchors, a)
			}
		}
	}
	for _, grp := range [][]anchorRow{grpA, grpB} {
		for _, a := range grp {
			if k := (portKey{a.device, a.port}); a.port != "" && eg.wanUp[k] == "" {
				eg.wanUp[k] = a.parent
			}
		}
	}
	for _, a := range grpB { // sorted by device name: the first holder wins
		if ip := strings.TrimPrefix(a.parent, "gw:"); eg.gwHolder[ip] == "" {
			eg.gwHolder[ip] = a.device
		}
	}
	for _, gws := range eg.defGW {
		sort.Strings(gws)
	}
	return eg
}

// inferTree sets uplinks, parents, roots, children and depths.
func (g *RoleGraph) inferTree(ix *portIndex, eg egress) {
	anchorOf := make(map[string]anchorRow, len(eg.anchors))
	for _, a := range eg.anchors {
		anchorOf[a.device] = a
		g.anchor[a.device] = true
		g.uplink[a.device] = a.port
	}
	g.anchored = len(eg.anchors) > 0
	// A dual-WAN edge reaches its synthetic parent over every WAN port that
	// leads there; the anchor port alone for a tunnel.
	for k, up := range eg.wanUp {
		if a, ok := anchorOf[k.dev]; ok && a.parent == up {
			g.edgePorts[k.dev] = append(g.edgePorts[k.dev], k.iface)
		}
	}
	for _, a := range eg.anchors {
		if len(g.edgePorts[a.device]) == 0 && a.port != "" {
			g.edgePorts[a.device] = []string{a.port}
		}
		sort.Strings(g.edgePorts[a.device])
	}

	// uplink[D]: the port toward D's own egress — the managed device its
	// default route points at, or the device whose FDB found that gateway —
	// else the port facing the first anchor D hears, else the port on which
	// D hears the most devices. Egress first: with two edges in different
	// branches, "the first anchor by name" would turn whole subtrees upside
	// down.
	addrDev := make(map[string]string)
	for _, d := range g.byID {
		if a := g.devs[d].Address; a != "" && addrDev[a] == "" {
			addrDev[a] = d
		}
	}
	for _, d := range g.byID {
		if g.anchor[d] {
			continue
		}
		up := ""
		for _, gw := range eg.defGW[d] {
			next := addrDev[gw]
			if next == "" {
				next = eg.gwHolder[gw]
			}
			if next == "" || next == d {
				continue
			}
			if p := ix.portOf(d, next); p != "" {
				up = p
				break
			}
		}
		for _, a := range eg.anchors {
			if up != "" {
				break
			}
			if p := ix.portOf(d, a.device); p != "" {
				up = p
			}
		}
		if up == "" {
			best := 0
			for _, p := range g.ports[d] {
				if n := ix.sCount[portKey{d, p}]; n > best {
					up, best = p, n
				}
			}
		}
		g.uplink[d] = up
	}

	// Nearest-ancestor parent selection.
	isAnc := func(x, d string) bool {
		if x == d {
			return false
		}
		p := ix.portOf(x, d)
		return p != "" && p != g.uplink[x] && g.uplink[d] != "" && ix.portOf(d, x) == g.uplink[d]
	}
	devParent := make(map[string]string)
	for _, d := range g.byID {
		if g.anchor[d] {
			continue
		}
		var cands []string
		for _, x := range g.byID {
			if isAnc(x, d) {
				cands = append(cands, x)
			}
		}
		best, bestDepth, bestSize := "", -1, 0
		for _, x := range cands { // byID order: equal candidates keep the smaller id
			depth := 0
			for _, y := range cands {
				if isAnc(y, x) {
					depth++
				}
			}
			size := ix.sCount[portKey{x, ix.portOf(x, d)}]
			if depth > bestDepth || (depth == bestDepth && size < bestSize) {
				best, bestDepth, bestSize = x, depth, size
			}
		}
		if best != "" {
			devParent[d] = best
		}
	}
	breakCycles(g.byID, devParent)

	// Roots: anchors hang off their synthetic node, other roots off a routed
	// gateway when they have one, the rest are orphans.
	for _, d := range g.byID {
		switch {
		case g.anchor[d]:
			a := anchorOf[d]
			g.ensureSynth(a.parent, a.parentLabel)
			g.parent[d] = a.parent
		case devParent[d] != "":
			g.parent[d] = devParent[d]
			g.parentPort[d] = ix.portOf(devParent[d], d)
		case eg.logicalGW[d] != "":
			id := gatewayNodeID(eg.logicalGW[d])
			g.ensureSynth(id, g.gatewayLabel(eg.logicalGW[d]))
			g.parent[d] = id
			g.logical[d] = true
		default:
			g.parent[d] = ""
		}
	}
	for _, d := range g.byName {
		p := g.parent[d]
		if p == "" {
			continue
		}
		g.children[p] = append(g.children[p], d)
		if g.known(p) {
			k := portKey{p, g.parentPort[d]}
			g.kidsOnPort[k] = append(g.kidsOnPort[k], d)
		}
	}
	for _, d := range g.byID {
		n, x := 0, d
		for i := 0; i < len(g.byID); i++ { // parents are acyclic; the bound is defensive
			p := g.parent[x]
			if !g.known(p) {
				break
			}
			n++
			x = p
		}
		g.depth[d] = n
	}
}

// assignRoles classifies every known port (first matching rule wins) and
// attaches each client MAC to the deepest edge port that learned it. Bond
// members are classified after their bond and take its role.
func (g *RoleGraph) assignRoles(ix *portIndex, eg egress) {
	carrierUp := g.wanCarriers(ix, eg)
	var members []portKey
	for k := range ix.universe {
		if ix.master[k] != "" {
			members = append(members, k)
			continue
		}
		info := PortRoleInfo{DeviceID: k.dev, Iface: k.iface, MACCount: len(ix.hostsAt[k])}
		t := ix.typeOf(k)
		switch {
		case eg.wanUp[k] != "":
			up := eg.wanUp[k]
			info.Role = RoleWAN
			info.UpstreamNode = up
			if strings.HasPrefix(up, "gw:") {
				info.GatewayIP = strings.TrimPrefix(up, "gw:")
				info.NeighborName = g.gatewayLabel(info.GatewayIP)
			} else {
				info.NeighborName = "Internet"
			}
		case IsVPNIfaceType(t):
			info.Role = RoleVPN
			info.UpstreamNode = vpnNodeID(k.dev, k.iface)
		case carrierUp[k] != "":
			// The physical port under a WAN that runs over a VLAN or PPPoE
			// client: it faces the same upstream, but the logical WAN port
			// already measures that edge (so it is not a second wan port).
			up := carrierUp[k]
			info.Role = RoleUplink
			info.UpstreamNode = up
			info.NeighborName = g.upstreamLabel(up)
		case k.iface == g.uplink[k.dev] && !g.anchor[k.dev]:
			info.Role = RoleUplink
			if p := g.parent[k.dev]; g.known(p) {
				info.NeighborDeviceID = p
				info.NeighborName = g.devs[p].Name
				info.NeighborIface = g.parentPort[k.dev]
			} else if p != "" {
				info.NeighborName = g.synth[p].label
				info.UpstreamNode = p
			}
		case len(g.kidsOnPort[k]) > 0:
			kids := g.kidsOnPort[k]
			info.Role = RoleDownlink
			info.NeighborDeviceID = kids[0]
			info.NeighborName = g.devs[kids[0]].Name
			info.NeighborIface = g.uplink[kids[0]]
			info.NeighborCount = len(kids)
		case len(ix.linkPeer[k]) > 0 && isAdjacencyPort(k.iface, t):
			// Up link to another managed device that is not a tree edge.
			// What it learned is that side's network, not clients here.
			y, q := g.peerOf(ix, k)
			info.Role = RolePeer
			info.NeighborDeviceID = y
			info.NeighborName = g.devs[y].Name
			info.NeighborIface = q
		default:
			var clients []string
			// A port facing another anchor links two roots (e.g. a second
			// router with its own egress): what it learned is that root's
			// side of the network, not clients attached here.
			if !g.facesOtherAnchor(ix, k) {
				for mac := range ix.hostsAt[k] {
					if _, isManaged := ix.managed[mac]; !isManaged {
						clients = append(clients, mac)
					}
				}
			}
			wireless := IsWirelessIfaceType(t)
			switch {
			case wireless && len(clients) > 0:
				info.Role = RoleWireless
			case !wireless && len(clients) > 0 && isAdjacencyPort(k.iface, t):
				info.Role = RoleAccess
			case wireless || isAdjacencyPort(k.iface, t):
				info.Role = RoleIdle
			default:
				info.Role = RoleVirtual
			}
			if info.Role == RoleWireless || info.Role == RoleAccess {
				sort.Strings(clients)
				info.ClientCount = len(clients)
				g.attached[k] = clients
			}
		}
		g.roles[k] = info
	}

	// A bond member carries a share of its bond's traffic in the same
	// direction, so it takes the bond's role and neighbour. Clients are
	// learned (and counted) on the bond. A member of a WAN bond is an uplink
	// toward the same upstream: the bond already measures the WAN edge.
	for _, k := range members {
		bond := ix.master[k]
		info, ok := g.roles[portKey{k.dev, bond}]
		if !ok {
			info = g.Role(k.dev, bond)
		}
		info.DeviceID, info.Iface, info.Master = k.dev, k.iface, bond
		info.ClientCount = 0
		info.MACCount = len(ix.hostsAt[k])
		if info.Role == RoleWAN {
			info.Role, info.GatewayIP = RoleUplink, ""
		}
		if info.Role == RoleVirtual || info.Role == RoleVPN {
			info.Role = RoleIdle
		}
		g.roles[k] = info
	}

	for k, macs := range g.attached {
		for _, mac := range macs {
			if cur, ok := g.attachment[mac]; !ok || g.deeperPort(k, cur) {
				g.attachment[mac] = k
			}
		}
	}
}

// wanCarriers maps every physical port (or bond) a WAN port runs over —
// following VLAN / PPPoE-client stacking, e.g. pppoe-out1 → vlan-wan → ether1
// — to that WAN's upstream node. Logical links in the chain stay virtual.
func (g *RoleGraph) wanCarriers(ix *portIndex, eg egress) map[portKey]string {
	out := make(map[portKey]string)
	for w, up := range eg.wanUp {
		k := w
		for range 4 { // stacking is shallow; the bound only guards a bad loop
			parent := ix.carrier[k]
			if parent == "" {
				break
			}
			k = portKey{k.dev, parent}
			if eg.wanUp[k] != "" {
				break // itself a WAN port
			}
			if isAdjacencyPort(k.iface, ix.typeOf(k)) && out[k] == "" {
				out[k] = up
			}
		}
	}
	return out
}

// upstreamLabel is the display label of a synthetic upstream node id.
func (g *RoleGraph) upstreamLabel(id string) string {
	if n, ok := g.synth[id]; ok {
		return n.label
	}
	if strings.HasPrefix(id, "gw:") {
		return g.gatewayLabel(strings.TrimPrefix(id, "gw:"))
	}
	return "Internet"
}

// peerOf picks the device across a peer port and its port on that device.
// MNDP on a flat L2 hears everything behind the link, so the neighbour is
// the candidate nearest the root of its tree (an anchor before others, then
// name); its port is the far end of an up link from this port, else the port
// it faces this device on.
func (g *RoleGraph) peerOf(ix *portIndex, k portKey) (dev, iface string) {
	var cands []string
	for y := range ix.linkPeer[k] {
		cands = append(cands, y)
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if g.depth[a] != g.depth[b] {
			return g.depth[a] < g.depth[b]
		}
		if g.anchor[a] != g.anchor[b] {
			return g.anchor[a]
		}
		if na, nb := g.devs[a].Name, g.devs[b].Name; na != nb {
			return na < nb
		}
		return a < b
	})
	y := cands[0]
	if q := smallestPreferWired(ix.linkPeer[k][y], func(p string) bool {
		return IsPhysicalPort(p, ix.typeOf(portKey{y, p}))
	}); q != "" {
		return y, q
	}
	return y, ix.portOf(y, k.dev)
}

// facesOtherAnchor reports whether k is portOf(k.dev, A) for an anchor A other
// than k.dev.
func (g *RoleGraph) facesOtherAnchor(ix *portIndex, k portKey) bool {
	for y, p := range ix.po[k.dev] {
		if p == k.iface && y != k.dev && g.anchor[y] {
			return true
		}
	}
	return false
}

// choosePort picks portOf(X, Y) from the MNDP ports (l) and FDB ports (f) on
// X related to Y. Parallel MNDP edges collapse to the one the FDB agrees
// with, else the smallest wired port.
func choosePort(l, f map[string]bool, wired func(string) bool) string {
	switch {
	case len(l) == 1:
		for p := range l {
			return p
		}
	case len(l) > 1:
		var both []string
		for p := range l {
			if f[p] {
				both = append(both, p)
			}
		}
		if len(both) > 0 {
			sort.Strings(both)
			return both[0]
		}
		return smallestPreferWired(l, wired)
	case len(f) > 0:
		return smallestPreferWired(f, wired)
	}
	return ""
}

// smallestPreferWired returns the smallest wired port in set, else the
// smallest port; "" for an empty set.
func smallestPreferWired(set map[string]bool, wired func(string) bool) string {
	all := make([]string, 0, len(set))
	for p := range set {
		all = append(all, p)
	}
	if len(all) == 0 {
		return ""
	}
	sort.Strings(all)
	for _, p := range all {
		if wired(p) {
			return p
		}
	}
	return all[0]
}

// breakCycles removes parent pointers that close a loop. Walks start from
// devices in id order; when a walk reaches a node already on the same walk,
// the parent of the node where that was detected is cut.
func breakCycles(byID []string, parent map[string]string) {
	const (
		onWalk = 1
		done   = 2
	)
	state := make(map[string]int, len(byID))
	for _, start := range byID {
		if state[start] == done {
			continue
		}
		var walk []string
		for n := start; ; {
			state[n] = onWalk
			walk = append(walk, n)
			p := parent[n]
			if p == "" || state[p] == done {
				break
			}
			if state[p] == onWalk {
				delete(parent, n)
				break
			}
			n = p
		}
		for _, n := range walk {
			state[n] = done
		}
	}
}

func (g *RoleGraph) ensureSynth(id, label string) {
	if _, ok := g.synth[id]; ok {
		return
	}
	if id == internetID {
		g.synth[id] = synthNode{kind: kindInternet, label: "Internet"}
		return
	}
	kind := kindGateway
	if strings.HasPrefix(id, "vpn:") {
		kind = kindVPN
	}
	g.ensureSynth(internetID, "")
	g.synth[id] = synthNode{kind: kind, label: label, parent: internetID}
	kids := append(g.synthKids[internetID], id)
	sort.Strings(kids)
	g.synthKids[internetID] = kids
}

func (g *RoleGraph) gatewayLabel(ip string) string {
	if l := g.gwLabels[ip]; l != "" {
		return l
	}
	return ip
}

// deeperPort orders attachment candidates: deepest device, then smallest
// (device, iface).
func (g *RoleGraph) deeperPort(a, b portKey) bool {
	if da, db := g.depth[a.dev], g.depth[b.dev]; da != db {
		return da > db
	}
	if a.dev != b.dev {
		return a.dev < b.dev
	}
	return a.iface < b.iface
}

// inIface is the device's port facing the previous path hop: the anchor port
// or the uplink to a device parent. Orphan roots and devices behind a routed-
// only gateway have no such port.
func (g *RoleGraph) inIface(d string) string {
	if g.anchor[d] {
		return g.uplink[d]
	}
	if _, ok := g.devs[g.parent[d]]; ok {
		return g.uplink[d]
	}
	return ""
}

// Anchored reports whether any device's upstream is known from egress data.
// When false the tree is a best-effort guess from L2 adjacency alone.
func (g *RoleGraph) Anchored() bool { return g != nil && g.anchored }

// Role returns the role of any (device, iface); unknown ports are classified
// from the interface name alone. Never returns an empty role.
func (g *RoleGraph) Role(deviceID, iface string) PortRoleInfo {
	if g != nil {
		if r, ok := g.roles[portKey{deviceID, iface}]; ok {
			return r
		}
	}
	info := PortRoleInfo{DeviceID: deviceID, Iface: iface, Role: RoleVirtual}
	switch t := guessIfaceType(iface); {
	case IsVPNIfaceType(t):
		info.Role = RoleVPN
		info.UpstreamNode = vpnNodeID(deviceID, iface)
	case isAdjacencyPort(iface, t):
		info.Role = RoleIdle
	}
	return info
}

// Roles returns every known (device, iface), sorted by device name, then iface.
func (g *RoleGraph) Roles() []PortRoleInfo {
	out := []PortRoleInfo{}
	if g == nil {
		return out
	}
	for _, d := range g.byName {
		for _, p := range g.ports[d] {
			out = append(out, g.roles[portKey{d, p}])
		}
	}
	return out
}

// Devices returns every input device's place in the tree, sorted by depth,
// then name.
func (g *RoleGraph) Devices() []DeviceTreeInfo {
	out := []DeviceTreeInfo{}
	if g == nil {
		return out
	}
	for _, d := range g.byName {
		out = append(out, DeviceTreeInfo{
			DeviceID:    d,
			Name:        g.devs[d].Name,
			ParentID:    g.parent[d],
			ParentIface: g.parentPort[d],
			UplinkIface: g.uplink[d],
			Depth:       g.depth[d],
			Anchor:      g.anchor[d],
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Depth < out[j].Depth })
	return out
}

// AttachmentOf returns the edge port (role access or wireless) a client MAC
// is attached to: the deepest device that learned it, then the smallest
// (device, iface).
func (g *RoleGraph) AttachmentOf(mac string) (deviceID, iface string, ok bool) {
	if g == nil {
		return "", "", false
	}
	k, ok := g.attachment[normMAC(mac)]
	return k.dev, k.iface, ok
}

// AttachedMACs returns the sorted client MACs attached at an access or
// wireless port; empty for any other role.
func (g *RoleGraph) AttachedMACs(deviceID, iface string) []string {
	if g == nil {
		return []string{}
	}
	return append([]string{}, g.attached[portKey{deviceID, iface}]...)
}

// ---------- path ----------

// Hop is one step of an Internet → port path.
type Hop struct {
	Kind        string `json:"kind"`                   // internet | gateway | vpn | device | clients
	ID          string `json:"id"`                     // "internet" | "gw:<ip>" | "vpn:<dev>:<iface>" | device id | "clients:<dev>:<iface>"
	Label       string `json:"label"`                  // "Internet" | gw label | vpn iface | device name | "" for clients
	DeviceID    string `json:"device_id,omitempty"`    // device hops
	InIface     string `json:"in_iface,omitempty"`     // device hop: its port facing the previous hop (uplink / anchor port)
	OutIface    string `json:"out_iface,omitempty"`    // device hop: its port facing the next hop (target device: the requested iface)
	ClientCount int    `json:"client_count,omitempty"` // clients hop
	Sink        bool   `json:"sink"`                   // hops after the requested port
	DownBps     int64  `json:"down_bps"`               // segment ENTERING this hop, download direction
	UpBps       int64  `json:"up_bps"`                 // same segment, upload direction
	Measured    bool   `json:"measured"`               // false for the first hop or when no rates are found
}

// ErrUnknownDevice is returned by Path for a device not in the graph.
var ErrUnknownDevice = errors.New("topology: unknown device")

// Path returns the hops from the root of deviceID's tree (Internet, then any
// gateway/VPN node, then device ancestors) down to deviceID/iface, followed
// by the sink behind that port: the first child device of a downlink, or the
// clients of an access/wireless port. iface may be "" (no sinks). An orphan
// device's path starts at the device itself.
//
// Segment rates: the previous device's out port tx/rx (download/upload),
// else this device's in port rx/tx.
func (g *RoleGraph) Path(deviceID, iface string, rates RateFunc) ([]Hop, error) {
	if g == nil {
		return nil, ErrUnknownDevice
	}
	if _, ok := g.devs[deviceID]; !ok {
		return nil, ErrUnknownDevice
	}

	chain := []string{deviceID}
	for x := deviceID; len(chain) <= len(g.devs); {
		p := g.parent[x]
		if _, ok := g.devs[p]; !ok {
			break
		}
		chain = append(chain, p)
		x = p
	}
	var synth []string
	for s := g.parent[chain[len(chain)-1]]; s != "" && len(synth) < 4; s = g.synth[s].parent {
		synth = append(synth, s)
	}

	var hops []Hop
	for i := len(synth) - 1; i >= 0; i-- {
		n := g.synth[synth[i]]
		hops = append(hops, Hop{Kind: n.kind, ID: synth[i], Label: n.label})
	}
	for i := len(chain) - 1; i >= 0; i-- {
		d := chain[i]
		h := Hop{Kind: kindDevice, ID: d, Label: g.devs[d].Name, DeviceID: d, InIface: g.inIface(d)}
		if i > 0 {
			h.OutIface = g.parentPort[chain[i-1]]
		} else {
			h.OutIface = iface
			if slices.Contains(g.edgePorts[d], iface) {
				h.InIface = iface // the requested port is one of a dual-WAN edge's WAN ports
			}
		}
		hops = append(hops, h)
	}
	if iface != "" {
		// A bond member leads where its bond does (child device or clients).
		sinkPort, r := iface, g.Role(deviceID, iface)
		if r.Master != "" {
			sinkPort, r = r.Master, g.Role(deviceID, r.Master)
		}
		switch r.Role {
		case RoleDownlink:
			if kids := g.kidsOnPort[portKey{deviceID, sinkPort}]; len(kids) > 0 {
				c := kids[0]
				hops = append(hops, Hop{Kind: kindDevice, ID: c, Label: g.devs[c].Name, DeviceID: c,
					InIface: g.uplink[c], Sink: true})
			}
		case RoleAccess, RoleWireless:
			hops = append(hops, Hop{Kind: kindClients, ID: "clients:" + deviceID + ":" + sinkPort,
				ClientCount: r.ClientCount, Sink: true})
		}
	}

	for k := 1; k < len(hops); k++ {
		prev, cur := hops[k-1], &hops[k]
		// Several children behind one parent port (an unmanaged switch, a
		// PtMP radio): the port's total belongs to all of them, so this
		// child's own uplink measures the segment better.
		shared := prev.Kind == kindDevice && cur.Kind == kindDevice &&
			len(g.kidsOnPort[portKey{prev.DeviceID, prev.OutIface}]) > 1
		if shared && cur.InIface != "" {
			if rx, tx, ok := measure(rates, cur.DeviceID, cur.InIface); ok {
				cur.DownBps, cur.UpBps, cur.Measured = rx, tx, true
				continue
			}
		}
		if prev.Kind == kindDevice && prev.OutIface != "" {
			if rx, tx, ok := measure(rates, prev.DeviceID, prev.OutIface); ok {
				cur.DownBps, cur.UpBps, cur.Measured = tx, rx, true
				continue
			}
		}
		if cur.Kind == kindDevice && cur.InIface != "" {
			if rx, tx, ok := measure(rates, cur.DeviceID, cur.InIface); ok {
				cur.DownBps, cur.UpBps, cur.Measured = rx, tx, true
			}
		}
	}
	return hops, nil
}

func measure(rates RateFunc, deviceID, iface string) (rx, tx int64, ok bool) {
	if rates == nil || deviceID == "" || iface == "" {
		return 0, 0, false
	}
	rx, tx, ok = rates(deviceID, iface)
	return max(rx, 0), max(tx, 0), ok
}

// ---------- estimated sankey ----------

// SankeyNode is one node of the estimated flow diagram.
type SankeyNode struct {
	ID          string `json:"id"`                     // unique: "internet" | "gw:<ip>" | "vpn:<dev>:<iface>" | <device id> | "port:<dev>:<iface>" | "other:<dev>"
	Name        string `json:"name"`                   // display label (device name, iface name, "+N ports", …)
	Type        string `json:"type"`                   // internet | gateway | vpn | device | port | other
	DeviceID    string `json:"device_id,omitempty"`    // device, port and other nodes
	Iface       string `json:"iface,omitempty"`        // port leaves
	ClientCount int    `json:"client_count,omitempty"` // port leaves
}

// SankeyLink is one flow; Source/Target index Sankey.Nodes.
type SankeyLink struct {
	Source      int    `json:"source"`
	Target      int    `json:"target"`
	Value       int64  `json:"value"`                  // bits/s, always >= MinBps
	SourceIface string `json:"source_iface,omitempty"` // port on the source device carrying the flow
}

// Sankey is an estimated single-direction flow forest (acyclic by construction).
type Sankey struct {
	Estimated bool         `json:"estimated"` // always true
	Direction string       `json:"direction"` // download | upload
	Nodes     []SankeyNode `json:"nodes"`     // never nil
	Links     []SankeyLink `json:"links"`     // never nil
}

// SankeyOptions tunes Sankey.
type SankeyOptions struct {
	Direction          string // "download" (default) | "upload"
	RootDeviceID       string // "" = whole forest; else only that device's subtree (device is a root node)
	MinBps             int64  // default 1000 when <= 0
	MaxLeavesPerDevice int    // default 8 when <= 0
}

type sankeyItem struct {
	node     SankeyNode
	value    int64  // value of the link entering this item
	srcIface string // port on the parent device carrying that link
	kids     []*sankeyItem
	local    int64 // east-west surplus fed in by a "local:<dev>" source node
}

type sankeyBuilder struct {
	g         *RoleGraph
	rates     RateFunc
	upload    bool
	minBps    int64
	maxLeaves int
	seen      map[string]bool
}

// Sankey estimates one direction of traffic over the tree from port rates:
// parent→child = the parent's port toward the child (tx for download, rx for
// upload), else the child's uplink (rx / tx), else the sum of what the child
// forwards on. When several children share one parent port, each child's own
// uplink comes first (the port's total belongs to all of them). Access /
// wireless / idle ports carrying ≥ MinBps become leaves (top
// MaxLeavesPerDevice, the rest folded into "+N ports"); bond members, peer
// links and other non-tree ports never do. A link below MinBps is dropped
// with its whole subtree, and nodes left without links are dropped.
//
// A device with east-west traffic (a NAS, a hypervisor) sends out more than
// it receives from its parent. Every non-root device whose outflow exceeds
// its inflow by ≥ MinBps gets that surplus from a synthetic source node
// "local:<dev>" (type other, "Local / east-west"), so every node conserves
// flow. The result is a DAG: such a device has two incoming links.
func (g *RoleGraph) Sankey(rates RateFunc, opt SankeyOptions) Sankey {
	out := Sankey{Estimated: true, Direction: "download", Nodes: []SankeyNode{}, Links: []SankeyLink{}}
	b := &sankeyBuilder{g: g, rates: rates, minBps: opt.MinBps, maxLeaves: opt.MaxLeavesPerDevice, seen: map[string]bool{}}
	if opt.Direction == "upload" {
		out.Direction, b.upload = "upload", true
	}
	if b.minBps <= 0 {
		b.minBps = 1000
	}
	if b.maxLeaves <= 0 {
		b.maxLeaves = 8
	}
	if g == nil {
		return out
	}

	var roots []*sankeyItem
	if opt.RootDeviceID != "" {
		if _, ok := g.devs[opt.RootDeviceID]; !ok {
			return out
		}
		roots = append(roots, b.device(opt.RootDeviceID))
	} else {
		if _, ok := g.synth[internetID]; ok {
			roots = append(roots, b.synthetic(internetID))
		}
		for _, d := range g.byName {
			if g.parent[d] == "" {
				roots = append(roots, b.device(d))
			}
		}
	}
	for _, r := range roots {
		b.balance(r, true)
		emitSankey(&out, r, -1)
	}
	return out
}

// flow is a port's rate in the chosen direction. facingDown: the port faces
// away from the Internet (download leaves through tx).
func (b *sankeyBuilder) flow(deviceID, iface string, facingDown bool) (int64, bool) {
	rx, tx, ok := measure(b.rates, deviceID, iface)
	if !ok {
		return 0, false
	}
	if facingDown != b.upload {
		return tx, true
	}
	return rx, true
}

func (b *sankeyBuilder) device(d string) *sankeyItem {
	g := b.g
	it := &sankeyItem{node: SankeyNode{ID: d, Name: g.devs[d].Name, Type: kindDevice, DeviceID: d}}
	if b.seen[d] {
		return it
	}
	b.seen[d] = true

	shares := b.sharedPortShares(d)
	for _, c := range g.children[d] {
		ci := b.device(c)
		v, ok := int64(0), false
		if sv, shared := shares[c]; shared {
			v, ok = sv.value, sv.ok
		} else {
			v, ok = b.flow(d, g.parentPort[c], true)
			if !ok {
				v, ok = b.flow(c, g.uplink[c], false)
			}
		}
		if !ok {
			v = ci.outflow()
		}
		if v < b.minBps {
			continue
		}
		ci.value, ci.srcIface = v, g.parentPort[c]
		it.kids = append(it.kids, ci)
	}

	type leaf struct {
		iface string
		value int64
	}
	var leaves []leaf
	for _, p := range g.ports[d] {
		ri := g.roles[portKey{d, p}]
		switch ri.Role {
		case RoleAccess, RoleWireless, RoleIdle:
		default:
			continue
		}
		if ri.Master != "" {
			continue // its bond carries (and counts) this traffic
		}
		if v, ok := b.flow(d, p, true); ok && v >= b.minBps {
			leaves = append(leaves, leaf{p, v})
		}
	}
	sort.Slice(leaves, func(i, j int) bool {
		if leaves[i].value != leaves[j].value {
			return leaves[i].value > leaves[j].value
		}
		return leaves[i].iface < leaves[j].iface
	})
	var rest []leaf
	if len(leaves) > b.maxLeaves {
		leaves, rest = leaves[:b.maxLeaves], leaves[b.maxLeaves:]
	}
	for _, l := range leaves {
		it.kids = append(it.kids, &sankeyItem{
			node: SankeyNode{ID: "port:" + d + ":" + l.iface, Name: l.iface, Type: kindPort, DeviceID: d,
				Iface: l.iface, ClientCount: g.roles[portKey{d, l.iface}].ClientCount},
			value: l.value, srcIface: l.iface,
		})
	}
	var restSum int64
	for _, l := range rest {
		restSum += l.value
	}
	if len(rest) > 0 && restSum >= b.minBps {
		name := fmt.Sprintf("+%d ports", len(rest))
		if len(rest) == 1 {
			name = "+1 port"
		}
		it.kids = append(it.kids, &sankeyItem{
			node:  SankeyNode{ID: "other:" + d, Name: name, Type: kindOther, DeviceID: d},
			value: restSum,
		})
	}
	return it
}

// shareValue is a child's edge value on a shared parent port.
type shareValue struct {
	value int64
	ok    bool
}

// sharedPortShares values the edges to children that share a parent port of
// d with other children: each child's own uplink when measured; the parent
// port's remaining measured total split evenly across the children whose
// uplink is not; unmeasured (outflow fallback) when neither side is.
func (b *sankeyBuilder) sharedPortShares(d string) map[string]shareValue {
	g := b.g
	out := make(map[string]shareValue)
	for _, c := range g.children[d] {
		port := portKey{d, g.parentPort[c]}
		kids := g.kidsOnPort[port]
		if len(kids) < 2 || kids[0] != c {
			continue // single child, or this port was already handled
		}
		var measured int64
		var unmeasured []string
		for _, k := range kids {
			if v, ok := b.flow(k, g.uplink[k], false); ok {
				out[k] = shareValue{v, true}
				measured += v
			} else {
				unmeasured = append(unmeasured, k)
			}
		}
		total, ok := b.flow(d, port.iface, true)
		for _, k := range unmeasured {
			if !ok {
				out[k] = shareValue{}
				continue
			}
			out[k] = shareValue{max(total-measured, 0) / int64(len(unmeasured)), true}
		}
	}
	return out
}

func (b *sankeyBuilder) synthetic(id string) *sankeyItem {
	g := b.g
	n := g.synth[id]
	it := &sankeyItem{node: SankeyNode{ID: id, Name: n.label, Type: n.kind}}
	for _, c := range g.children[id] {
		ci := b.device(c)
		// internet→edge (every WAN port toward it), gw→attachment port,
		// vpn→tunnel. A routed-only gateway edge has no port: use what the
		// child forwards.
		var v int64
		ok := false
		if !g.logical[c] {
			for _, p := range g.edgePorts[c] {
				if pv, pok := b.flow(c, p, false); pok {
					v, ok = v+pv, true
				}
			}
		}
		if !ok {
			v = ci.outflow()
		}
		if v < b.minBps {
			continue
		}
		ci.value = v
		it.kids = append(it.kids, ci)
	}
	for _, s := range g.synthKids[id] {
		si := b.synthetic(s)
		if v := si.outflow(); v >= b.minBps {
			si.value = v
			it.kids = append(it.kids, si)
		}
	}
	return it
}

func (it *sankeyItem) outflow() int64 {
	var sum int64
	for _, k := range it.kids {
		sum += k.value
	}
	return sum
}

// balance sets, on every device below a root, the east-west surplus its
// outflow has over its inflow when that is at least minBps. Roots are
// skipped: their inflow is unknown.
func (b *sankeyBuilder) balance(it *sankeyItem, isRoot bool) {
	for _, k := range it.kids {
		b.balance(k, false)
	}
	if isRoot || it.node.Type != kindDevice {
		return
	}
	if surplus := it.outflow() - it.value; surplus >= b.minBps {
		it.local = surplus
	}
}

// emitSankey appends the item (pre-order) and the link from its parent, then
// its "local:<dev>" source when it has an east-west surplus. A root without
// links is omitted; every other item has its incoming link.
func emitSankey(out *Sankey, it *sankeyItem, parentIdx int) {
	if parentIdx < 0 && len(it.kids) == 0 {
		return
	}
	idx := len(out.Nodes)
	out.Nodes = append(out.Nodes, it.node)
	if parentIdx >= 0 {
		out.Links = append(out.Links, SankeyLink{Source: parentIdx, Target: idx, Value: it.value, SourceIface: it.srcIface})
	}
	if it.local > 0 {
		li := len(out.Nodes)
		out.Nodes = append(out.Nodes, SankeyNode{ID: "local:" + it.node.DeviceID, Name: "Local / east-west",
			Type: kindOther, DeviceID: it.node.DeviceID})
		out.Links = append(out.Links, SankeyLink{Source: li, Target: idx, Value: it.local})
	}
	for _, k := range it.kids {
		emitSankey(out, k, idx)
	}
}

// ---------- helpers ----------

// IsPhysicalPort reports whether an interface is a faceplate port (ethernet,
// SFP, QSFP, combo) — the same rule as the device port grid.
func IsPhysicalPort(name, typ string) bool {
	if typ == "ether" {
		return true
	}
	n := strings.ToLower(name)
	for _, p := range []string{"ether", "sfp", "qsfp", "combo"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// IsWirelessIfaceType reports whether a RouterOS interface type is a radio
// (legacy wireless, wifi/wifiwave2, 60 GHz, or a CAPsMAN cap interface).
func IsWirelessIfaceType(typ string) bool {
	return isWirelessType(typ) || strings.EqualFold(typ, "cap")
}

// nonAdjacencyTypes are interface types that never face a single neighbour:
// on a flat L2 they hear the whole network.
var nonAdjacencyTypes = map[string]bool{
	"vlan": true, "bridge": true, "loopback": true, "veth": true, "vrrp": true, "macvlan": true,
}

// isAdjacencyPort reports whether a port can face exactly one neighbour
// (physical, radio or bond) and so can carry a tree edge.
func isAdjacencyPort(name, typ string) bool {
	t := strings.ToLower(typ)
	if nonAdjacencyTypes[t] || IsVPNIfaceType(t) {
		return false
	}
	return IsPhysicalPort(name, typ) || IsWirelessIfaceType(typ) || t == "bond"
}

// guessIfaceType infers a RouterOS interface type from its default-style
// name, for ports the interfaces table does not know. "" = no idea.
func guessIfaceType(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case IsPhysicalPort(n, ""):
		return "ether"
	case strings.HasPrefix(n, "wlan") || strings.HasPrefix(n, "wifi"):
		return "wifi"
	case strings.HasPrefix(n, "bond"):
		return "bond"
	case strings.HasPrefix(n, "bridge"):
		return "bridge"
	case strings.HasPrefix(n, "vlan"):
		return "vlan"
	case nameToken(n, "lo") || nameToken(n, "loopback"):
		return "loopback"
	}
	for _, p := range []struct{ prefix, typ string }{
		{"wireguard", "wg"}, {"wg", "wg"}, {"eoip", "eoip"}, {"gre", "gre"}, {"ipip", "ipip"},
		{"vxlan", "vxlan"}, {"ovpn-out", "ovpn-out"}, {"l2tp-out", "l2tp-out"},
		{"sstp-out", "sstp-out"}, {"pptp-out", "pptp-out"}, {"zerotier", "zerotier"},
	} {
		if nameToken(n, p.prefix) {
			return p.typ
		}
	}
	return ""
}

// nameToken reports whether n is prefix, optionally followed by a digit or a
// separator ("wg0", "gre-site2" but not "green").
func nameToken(n, prefix string) bool {
	if !strings.HasPrefix(n, prefix) {
		return false
	}
	if len(n) == len(prefix) {
		return true
	}
	c := n[len(prefix)]
	return (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
}

// normPort reduces an MNDP interface value to the local port: "ether1,bridge"
// → "ether1". "" and "unknown" mean no port.
func normPort(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "unknown") {
		return ""
	}
	return s
}

func normMAC(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

func gatewayNodeID(ip string) string { return "gw:" + ip }

func vpnNodeID(deviceID, iface string) string { return "vpn:" + deviceID + ":" + iface }
