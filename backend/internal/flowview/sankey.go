package flowview

import (
	"cmp"
	"math"
	"net/netip"
	"slices"
	"strconv"

	"github.com/mikrotik-nms/backend/internal/flow/svcport"
)

// Measured Sankey node types (the Phase 1 estimated graph uses the others).
const (
	NodeRemote = "remote"
	NodeApp    = "app"
	NodePoint  = "point"
	NodeHost   = "host"
	NodePort   = "port"
	NodeOther  = "other"
)

// Remote groupings of the measured Sankey.
const (
	RemoteHost = "host"
	RemoteApp  = "app"
)

// SankeyNode is one node of the measured Sankey.
type SankeyNode struct {
	ID       string `json:"id"`   // remote:<ip> | app:<proto>/<port> | point:<id> | host:<ip> | port:<dev>:<iface> | other:remote | other:local
	Name     string `json:"name"` // display label
	Type     string `json:"type"` // remote | app | point | host | port | other
	IP       string `json:"ip,omitempty"`
	Class    string `json:"class,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	Iface    string `json:"iface,omitempty"`
}

// SankeyLink is one link; Source/Target index the node list. Value is an
// average bit rate, always >= 1.
type SankeyLink struct {
	Source int   `json:"source"`
	Target int   `json:"target"`
	Value  int64 `json:"value"`
}

// SankeyOptions tunes BuildSankey.
type SankeyOptions struct {
	Dir       Dir
	Remote    string // RemoteHost (default) | RemoteApp
	PointID   int64
	PointName string
	Seconds   float64 // covered seconds of the window; values = bytes*8/Seconds
	MaxRemote int     // default 12
	MaxLocal  int     // default 15
	MaxPorts  int     // default 15
}

// SankeyEndpoint names an endpoint (name "" = show the IP) and returns its
// class.
type SankeyEndpoint func(ip netip.Addr) (name, class string)

// SankeyAttach returns where a local endpoint attaches in the fleet (the
// Phase 1 attachment of its MAC): device id, port, device display name.
type SankeyAttach func(ip netip.Addr) (deviceID, iface, deviceName string, ok bool)

// AppLabel returns the app label ("" when unknown) and protocol name of
// (proto, port) — svcport.Label, the list the collector keys rows by.
func AppLabel(proto uint8, port uint16) (label, protoName string) {
	return svcport.Label(proto, port)
}

// AppName is the display label of an app: "HTTPS (tcp/443)", "tcp/12345",
// or just the protocol name for port-less protocols ("icmp").
func AppName(proto uint8, port uint16) string {
	label, protoName := svcport.Label(proto, port)
	if port == 0 {
		if label != "" {
			return label + " (" + protoName + ")"
		}
		return protoName
	}
	pp := protoName + "/" + strconv.Itoa(int(port))
	if label == "" {
		return pp
	}
	return label + " (" + pp + ")"
}

// sankeyGroup is one node candidate with its byte total.
type sankeyGroup struct {
	key   string
	ip    netip.Addr
	proto uint8
	port  uint16
	bytes int64
}

// topGroups sorts groups by bytes desc (ties: key) and splits off the top n.
func topGroups(m map[string]*sankeyGroup, n int) (top []*sankeyGroup, restBytes int64, restCount int) {
	all := make([]*sankeyGroup, 0, len(m))
	for _, g := range m {
		if g.bytes > 0 {
			all = append(all, g)
		}
	}
	slices.SortFunc(all, func(a, b *sankeyGroup) int {
		if c := cmp.Compare(b.bytes, a.bytes); c != 0 {
			return c
		}
		return cmp.Compare(a.key, b.key)
	})
	if len(all) > n {
		for _, g := range all[n:] {
			restBytes += g.bytes
		}
		restCount = len(all) - n
		all = all[:n]
	}
	return all, restBytes, restCount
}

// BuildSankey lays out the measured flows of one point and direction as a
// layered DAG (contract §9.12). Download: remote|app → point → host → port;
// upload: port → host → point → remote|app (the same nodes, links
// reversed). Remote side: the top MaxRemote remote endpoints (or apps) plus
// other:remote; local side: the top MaxLocal local endpoints plus
// other:local; ports: each shown host's attachment port, top MaxPorts ports
// (other hosts get no port link). Folded ingest-other rows feed both other
// nodes. Values are max(1, round(bytes*8/Seconds)); zero links are omitted.
// Never returns nil slices.
func BuildSankey(rows []DirRow, opt SankeyOptions, endpoint SankeyEndpoint, attach SankeyAttach) ([]SankeyNode, []SankeyLink) {
	nodes, links := []SankeyNode{}, []SankeyLink{}
	if opt.Seconds <= 0 {
		return nodes, links
	}
	if opt.MaxRemote <= 0 {
		opt.MaxRemote = 12
	}
	if opt.MaxLocal <= 0 {
		opt.MaxLocal = 15
	}
	if opt.MaxPorts <= 0 {
		opt.MaxPorts = 15
	}
	byApp := opt.Remote == RemoteApp

	remotes, locals := map[string]*sankeyGroup{}, map[string]*sankeyGroup{}
	var folded, total int64
	for _, r := range rows {
		if r.Bytes <= 0 {
			continue
		}
		total += r.Bytes
		if r.Other {
			folded += r.Bytes
			continue
		}
		var rk string
		var rg sankeyGroup
		if byApp {
			rk = AppKey(r.Proto, r.Port)
			rg = sankeyGroup{key: rk, proto: r.Proto, port: r.Port}
		} else {
			rk = r.Remote.String()
			rg = sankeyGroup{key: rk, ip: r.Remote}
		}
		g := remotes[rk]
		if g == nil {
			g = &rg
			remotes[rk] = g
		}
		g.bytes += r.Bytes
		lk := r.Local.String()
		l := locals[lk]
		if l == nil {
			l = &sankeyGroup{key: lk, ip: r.Local}
			locals[lk] = l
		}
		l.bytes += r.Bytes
	}
	if total <= 0 {
		return nodes, links
	}
	value := func(bytes int64) int64 {
		return max(1, int64(math.Round(float64(bytes)*8/opt.Seconds)))
	}

	topRemote, restRemote, nRemote := topGroups(remotes, opt.MaxRemote)
	topLocal, restLocal, nLocal := topGroups(locals, opt.MaxLocal)
	restRemote += folded
	restLocal += folded

	// Ports: aggregate the shown hosts by attachment port.
	type portAgg struct {
		dev, iface, name string
		bytes            int64
	}
	ports := map[string]*portAgg{}
	hostPort := map[string]string{}
	if attach != nil {
		for _, h := range topLocal {
			dev, iface, devName, ok := attach(h.ip)
			if !ok || dev == "" || iface == "" {
				continue
			}
			id := "port:" + dev + ":" + iface
			p := ports[id]
			if p == nil {
				p = &portAgg{dev: dev, iface: iface, name: devName}
				ports[id] = p
			}
			p.bytes += h.bytes
			hostPort[h.key] = id
		}
	}
	portIDs := make([]string, 0, len(ports))
	for id := range ports {
		portIDs = append(portIDs, id)
	}
	slices.SortFunc(portIDs, func(a, b string) int {
		if c := cmp.Compare(ports[b].bytes, ports[a].bytes); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	if len(portIDs) > opt.MaxPorts {
		portIDs = portIDs[:opt.MaxPorts]
	}
	shownPort := map[string]bool{}
	for _, id := range portIDs {
		shownPort[id] = true
	}

	// Nodes, in flow order for download.
	index := map[string]int{}
	add := func(n SankeyNode) int {
		index[n.ID] = len(nodes)
		nodes = append(nodes, n)
		return index[n.ID]
	}
	endpointNode := func(prefix, typ string, ip netip.Addr) SankeyNode {
		n := SankeyNode{ID: prefix + ip.String(), Name: ip.String(), Type: typ, IP: ip.String()}
		if endpoint != nil {
			name, class := endpoint(ip)
			if name != "" {
				n.Name = name
			}
			n.Class = class
		}
		return n
	}
	type edge struct {
		from, to string
		bytes    int64
	}
	var edges []edge // download orientation
	pointID := "point:" + strconv.FormatInt(opt.PointID, 10)

	var remoteNodes, localNodes, portNodes []SankeyNode
	for _, g := range topRemote {
		var n SankeyNode
		if byApp {
			n = SankeyNode{ID: "app:" + g.key, Name: AppName(g.proto, g.port), Type: NodeApp}
		} else {
			n = endpointNode("remote:", NodeRemote, g.ip)
		}
		remoteNodes = append(remoteNodes, n)
		edges = append(edges, edge{n.ID, pointID, g.bytes})
	}
	if restRemote > 0 {
		label := "Other remote"
		if byApp {
			label = "Other apps"
		}
		if nRemote > 0 {
			label += " (" + strconv.Itoa(nRemote) + ")"
		}
		remoteNodes = append(remoteNodes, SankeyNode{ID: "other:remote", Name: label, Type: NodeOther})
		edges = append(edges, edge{"other:remote", pointID, restRemote})
	}
	for _, h := range topLocal {
		n := endpointNode("host:", NodeHost, h.ip)
		localNodes = append(localNodes, n)
		edges = append(edges, edge{pointID, n.ID, h.bytes})
	}
	if restLocal > 0 {
		label := "Other hosts"
		if nLocal > 0 {
			label += " (" + strconv.Itoa(nLocal) + ")"
		}
		localNodes = append(localNodes, SankeyNode{ID: "other:local", Name: label, Type: NodeOther})
		edges = append(edges, edge{pointID, "other:local", restLocal})
	}
	for _, id := range portIDs {
		p := ports[id]
		name := p.iface
		if p.name != "" {
			name = p.name + " · " + p.iface
		}
		portNodes = append(portNodes, SankeyNode{ID: id, Name: name, Type: NodePort, DeviceID: p.dev, Iface: p.iface})
	}
	for _, h := range topLocal {
		if id := hostPort[h.key]; shownPort[id] {
			edges = append(edges, edge{"host:" + h.ip.String(), id, h.bytes})
		}
	}

	pointNode := SankeyNode{ID: pointID, Name: opt.PointName, Type: NodePoint}
	var ordered []SankeyNode
	if opt.Dir == Upload {
		ordered = slices.Concat(portNodes, localNodes, []SankeyNode{pointNode}, remoteNodes)
	} else {
		ordered = slices.Concat(remoteNodes, []SankeyNode{pointNode}, localNodes, portNodes)
	}
	for _, n := range ordered {
		add(n)
	}
	for _, e := range edges {
		if e.bytes <= 0 {
			continue
		}
		from, to := index[e.from], index[e.to]
		if opt.Dir == Upload {
			from, to = to, from
		}
		links = append(links, SankeyLink{Source: from, Target: to, Value: value(e.bytes)})
	}
	return nodes, links
}
