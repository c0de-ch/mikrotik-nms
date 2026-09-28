package api

import (
	"database/sql"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// portStatsSource is the part of *poller.PortStatsCollector the traffic
// handlers read; tests substitute a fake.
type portStatsSource interface {
	Snapshot() poller.PortSnapshot
	FlushedThrough() time.Time
	RolledThrough() time.Time
}

// portSnapshot returns the collector's latest fleet snapshot, or the empty
// pre-first-cycle snapshot when no collector is wired.
func (s *Server) portSnapshot() poller.PortSnapshot {
	if s.portStats == nil {
		return (*poller.PortStatsCollector)(nil).Snapshot()
	}
	return s.portStats.Snapshot()
}

// windowEnd is the exclusive end of the newest final bucket of table: the
// collector's flush (1m) or rollup (1h) watermark, falling back to the start
// of the current minute / hour before the collector has run.
func (s *Server) windowEnd(table queries.PortStatsTable, now time.Time) time.Time {
	var end time.Time
	if s.portStats != nil {
		if table == queries.PortStats1h {
			end = s.portStats.RolledThrough()
		} else {
			end = s.portStats.FlushedThrough()
		}
	}
	if !end.IsZero() {
		return end.UTC()
	}
	if table == queries.PortStats1h {
		return now.UTC().Truncate(time.Hour)
	}
	return now.UTC().Truncate(time.Minute)
}

// ---------- ranges ----------

const rangeLive = "live"

// trafficRange is one selectable time range: which table backs it and how its
// history / sparkline points are spaced. Every step divides the window and is
// a multiple of the table's native bucket length.
type trafficRange struct {
	key        string
	window     time.Duration
	table      queries.PortStatsTable
	native     time.Duration // bucket length of table
	step       time.Duration // history point spacing
	sparkStep  time.Duration // top-talkers sparkline spacing (~30 points)
	resolution string        // "1m" | "1h" | "live"
}

var trafficRanges = map[string]trafficRange{
	"15m": {key: "15m", window: 15 * time.Minute, table: queries.PortStats1m, native: time.Minute,
		step: time.Minute, sparkStep: time.Minute, resolution: "1m"},
	"1h": {key: "1h", window: time.Hour, table: queries.PortStats1m, native: time.Minute,
		step: time.Minute, sparkStep: 2 * time.Minute, resolution: "1m"},
	"6h": {key: "6h", window: 6 * time.Hour, table: queries.PortStats1m, native: time.Minute,
		step: time.Minute, sparkStep: 12 * time.Minute, resolution: "1m"},
	"24h": {key: "24h", window: 24 * time.Hour, table: queries.PortStats1m, native: time.Minute,
		step: 5 * time.Minute, sparkStep: 48 * time.Minute, resolution: "1m"},
	"7d": {key: "7d", window: 7 * 24 * time.Hour, table: queries.PortStats1h, native: time.Hour,
		step: time.Hour, sparkStep: 6 * time.Hour, resolution: "1h"},
	"30d": {key: "30d", window: 30 * 24 * time.Hour, table: queries.PortStats1h, native: time.Hour,
		step: time.Hour, sparkStep: 24 * time.Hour, resolution: "1h"},
}

// liveSparkRange backs the range=live top-talker sparklines: the last 30
// minutes of 1-minute buckets (the live rates themselves come from the
// snapshot).
var liveSparkRange = trafficRange{key: rangeLive, window: 30 * time.Minute, table: queries.PortStats1m,
	native: time.Minute, step: time.Minute, sparkStep: time.Minute, resolution: "1m"}

// parseTrafficRange validates the range query param (default 1h).
func parseTrafficRange(v string) (trafficRange, bool) {
	switch v {
	case "":
		return trafficRanges["1h"], true
	case rangeLive:
		return trafficRange{key: rangeLive, resolution: rangeLive}, true
	}
	r, ok := trafficRanges[v]
	return r, ok
}

// rangeWindow is an end-anchored window [start, end) resolved against the
// stored buckets.
type rangeWindow struct {
	start, end time.Time
	native     time.Duration
	covered    bool      // the table has any row (fleet-wide) in [start, end)
	effStart   time.Time // since when the collector covered the window: averages and stats run from here
	// cov marks the native buckets the collector was running for, so a
	// restart or outage inside the window reads as a gap (left out of
	// averages), not as the network going silent.
	cov *queries.Coverage
}

// resolveWindow anchors r's window at the newest final bucket of its table and
// finds since when, and in which buckets, the collector has been writing
// inside it. On the hourly table a first hour the collector only covered in
// part (it started inside it) starts at its first covered minute, so day-one
// averages are not diluted by the empty rest of that hour.
func (s *Server) resolveWindow(r trafficRange, now time.Time) (rangeWindow, error) {
	end := s.windowEnd(r.table, now)
	w := rangeWindow{start: end.Add(-r.window), end: end, native: r.native}
	w.effStart = w.start
	cov, ok, err := queries.PortStatsCoverageStart(s.db, r.table, w.start, w.end)
	if err != nil || !ok {
		return w, err
	}
	w.covered = true
	if cov.After(w.start) {
		w.effStart = cov
		if r.table == queries.PortStats1h {
			first, ok, err := queries.FirstCoveredMinute(s.db, cov)
			if err != nil {
				return w, err
			}
			if ok && first.After(cov) {
				w.effStart = first
			}
		}
	}
	present, err := queries.PortStatsPresentBuckets(s.db, r.table, w.start, w.end)
	if err != nil {
		return w, err
	}
	w.cov = queries.NewCoverage(r.native, present, w.effStart)
	return w, nil
}

// pointsFrom is where a series with the given step begins: the step-aligned
// bucket holding effStart, so buckets before coverage are omitted rather than
// drawn as zeros.
func (w rangeWindow) pointsFrom(step time.Duration) time.Time {
	return w.start.Add(w.effStart.Sub(w.start) / step * step)
}

// rowsFrom is the first native bucket holding covered data (effStart itself
// on the 1-minute table; the hour holding it on the hourly one).
func (w rangeWindow) rowsFrom() time.Time {
	if w.native <= 0 {
		return w.effStart
	}
	return w.pointsFrom(w.native)
}

// seconds is how long the collector covered the window, the divisor for
// averages: the effective window minus buckets it was not running for.
func (w rangeWindow) seconds() float64 {
	if !w.covered {
		return 0
	}
	if w.cov == nil {
		return w.end.Sub(w.effStart).Seconds()
	}
	return w.cov.SecondsIn(w.rowsFrom(), w.end)
}

// coverageFrom is the JSON coverage_from value (null without data).
func (w rangeWindow) coverageFrom() *time.Time {
	if !w.covered {
		return nil
	}
	return timePtr(w.effStart)
}

func timePtr(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}

// bpsOver converts a byte total into an average bit rate over secs.
func bpsOver(bytes int64, secs float64) int64 {
	if secs <= 0 {
		return 0
	}
	return int64(math.Round(float64(bytes) * 8 / secs))
}

// ---------- role graph loader ----------

// trafficTopoTTL bounds how stale the cached role graph may get. Links and
// uplinks change once per topology cycle (60 s), the FDB every
// port_hosts_interval, so 30 s is plenty.
const trafficTopoTTL = 30 * time.Second

// ifaceKey identifies one interface of one device.
type ifaceKey struct{ dev, iface string }

// trafficTopo is the role graph plus the lookups the traffic endpoints join
// against, built from one read of the DB and the port snapshot. Immutable once
// built, so it is shared by concurrent requests.
type trafficTopo struct {
	graph   *topology.RoleGraph
	builtAt time.Time
	names   map[string]string               // device id → identity, or address
	types   map[ifaceKey]string             // interface type (interfaces table ∪ snapshot)
	ports   map[string][]string             // device id → every known iface (table ∪ snapshot ∪ role graph), sorted
	known   map[ifaceKey]bool               // set form of ports
	managed map[string]bool                 // managed-device interface MACs, uppercase
	hosts   map[ifaceKey][]queries.PortHost // fresh bridge FDB rows per port, by MAC
	macs    map[string]*queries.MACLookup   // mac_lookup by uppercase MAC
}

// devicePorts is every port the device is known to have: the role graph and
// interfaces table (t.ports) plus the live snapshot, sorted.
func (t *trafficTopo) devicePorts(deviceID string, snap poller.PortSnapshot) []string {
	seen := make(map[string]bool)
	var out []string
	for _, p := range t.ports[deviceID] {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range snap.Ports {
		if p.DeviceID == deviceID && !seen[p.Iface] {
			seen[p.Iface] = true
			out = append(out, p.Iface)
		}
	}
	sort.Strings(out)
	return out
}

// name is a device's display name, falling back to its id.
func (t *trafficTopo) name(deviceID string) string {
	if n := t.names[deviceID]; n != "" {
		return n
	}
	return deviceID
}

// deviceDisplayName is the identity, or the address when identity is empty.
func deviceDisplayName(d queries.Device) string {
	if d.Identity != "" {
		return d.Identity
	}
	return d.Address
}

// trafficTopology returns the cached role graph, rebuilding it when older than
// trafficTopoTTL. Concurrent callers wait for one rebuild instead of each
// running their own.
func (s *Server) trafficTopology() (*trafficTopo, error) {
	s.topoMu.Lock()
	defer s.topoMu.Unlock()
	now := time.Now()
	if s.topo != nil && now.Sub(s.topo.builtAt) < trafficTopoTTL {
		return s.topo, nil
	}
	t, err := loadTrafficTopo(s.db, s.portSnapshot(), now)
	if err != nil {
		return nil, err
	}
	s.topo = t
	return t, nil
}

// invalidateTrafficTopo drops the cached role graph so the next traffic request
// rebuilds it: device create/update/delete and table imports change the
// inputs, and a device added seconds ago must not 404 on /traffic/path for
// the rest of the TTL.
func (s *Server) invalidateTrafficTopo() {
	s.topoMu.Lock()
	s.topo = nil
	s.topoMu.Unlock()
}

// loadTrafficTopo builds the role-graph input from devices, links, fresh
// uplinks, interfaces (running/disabled taken from the live snapshot, plus
// snapshot ports the table does not know yet), fresh FDB rows, interface
// stacking (bond members, VLAN/PPPoE carriers) and gateway labels from the
// client cache.
//
// The snapshot is a full /interface/print of every device it holds, so for
// those devices it is the port list: interfaces-table rows it lacks were
// removed or renamed on the device (the table is never pruned) and would
// surface as ghost ports. Table rows only stand in for devices the snapshot
// does not have (offline). Blank-named rows are always dropped.
func loadTrafficTopo(db *sql.DB, snap poller.PortSnapshot, now time.Time) (*trafficTopo, error) {
	devices, err := queries.ListDevices(db)
	if err != nil {
		return nil, err
	}
	links, err := queries.ListLinks(db)
	if err != nil {
		return nil, err
	}
	uplinks, err := queries.ListDeviceUplinks(db, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	hosts, err := queries.ListPortHosts(db, now.Add(-max(3*poller.PortHostsInterval(db), 15*time.Minute)), "")
	if err != nil {
		return nil, err
	}
	macs, err := queries.GetAllMACLookups(db)
	if err != nil {
		return nil, err
	}
	relations, err := queries.ListInterfaceRelations(db)
	if err != nil {
		return nil, err
	}

	t := &trafficTopo{
		builtAt: now,
		names:   make(map[string]string, len(devices)),
		types:   make(map[ifaceKey]string),
		ports:   make(map[string][]string),
		known:   make(map[ifaceKey]bool),
		managed: make(map[string]bool),
		hosts:   make(map[ifaceKey][]queries.PortHost),
		macs:    make(map[string]*queries.MACLookup, len(macs)),
	}
	in := topology.PathInput{GatewayLabels: make(map[string]string)}

	live := indexSnapshot(snap)
	inSnapshot := make(map[string]bool)
	for _, p := range snap.Ports {
		inSnapshot[p.DeviceID] = true
	}
	for _, d := range devices {
		name := deviceDisplayName(d)
		t.names[d.ID] = name
		in.Devices = append(in.Devices, topology.PathDevice{ID: d.ID, Name: name, Status: d.Status, Address: d.Address})

		ifaces, err := queries.ListInterfacesByDevice(db, d.ID)
		if err != nil {
			return nil, err
		}
		for _, i := range ifaces {
			// A stale row's MAC still belongs to the device (managed, never
			// a client), even when the row itself is dropped below.
			if mac := strings.ToUpper(strings.TrimSpace(i.MACAddress)); mac != "" {
				t.managed[mac] = true
			}
			k := ifaceKey{d.ID, i.Name}
			p, isLive := live[k]
			if strings.TrimSpace(i.Name) == "" || (inSnapshot[d.ID] && !isLive) {
				continue
			}
			pi := topology.PathIface{DeviceID: d.ID, Name: i.Name, Type: i.Type, MAC: i.MACAddress,
				Running: i.Running, Disabled: i.Disabled}
			if isLive {
				pi.Running, pi.Disabled = p.Running, p.Disabled
				if pi.Type == "" {
					pi.Type = p.Type
				}
			}
			t.types[k] = pi.Type
			in.Interfaces = append(in.Interfaces, pi)
		}
	}
	for _, r := range relations {
		in.Relations = append(in.Relations, topology.PathRelation{DeviceID: r.DeviceID, Iface: r.InterfaceName,
			Parent: r.Parent, Kind: r.Kind})
	}
	for _, p := range snap.Ports {
		k := ifaceKey{p.DeviceID, p.Iface}
		if _, ok := t.names[p.DeviceID]; !ok {
			continue // device deleted since the collector polled it
		}
		if _, ok := t.types[k]; ok {
			continue
		}
		t.types[k] = p.Type
		in.Interfaces = append(in.Interfaces, topology.PathIface{DeviceID: p.DeviceID, Name: p.Iface, Type: p.Type,
			Running: p.Running, Disabled: p.Disabled})
	}

	for _, l := range links {
		in.Links = append(in.Links, topology.PathLink{DeviceA: l.DeviceAID, IfaceA: l.InterfaceA,
			DeviceB: l.DeviceBID, IfaceB: l.InterfaceB, Status: l.Status})
	}
	labelled := make(map[string]bool)
	for _, u := range uplinks {
		in.Uplinks = append(in.Uplinks, topology.PathUplink{DeviceID: u.DeviceID, Kind: u.Kind,
			Interface: u.Interface, IfaceType: u.IfaceType, GatewayIP: u.GatewayIP})
		if ip := u.GatewayIP; ip != "" && !labelled[ip] {
			labelled[ip] = true
			// Short host name ("opnsense" from "opnsense.lan"); the graph
			// falls back to the IP when the client cache has no name.
			if label, _, _ := strings.Cut(queries.HostnameForIP(db, ip), "."); label != "" {
				in.GatewayLabels[ip] = label
			}
		}
	}
	for _, h := range hosts {
		h.MACAddress = strings.ToUpper(h.MACAddress)
		k := ifaceKey{h.DeviceID, h.InterfaceName}
		t.hosts[k] = append(t.hosts[k], h)
		in.Hosts = append(in.Hosts, topology.PathHost{DeviceID: h.DeviceID, Iface: h.InterfaceName,
			MAC: h.MACAddress, VID: h.VID})
	}
	for mac, m := range macs {
		t.macs[strings.ToUpper(mac)] = m
	}

	t.graph = topology.BuildRoleGraph(in)

	for k := range t.types {
		t.known[k] = true
	}
	for _, r := range t.graph.Roles() {
		t.known[ifaceKey{r.DeviceID, r.Iface}] = true
	}
	for k := range t.known {
		t.ports[k.dev] = append(t.ports[k.dev], k.iface)
	}
	for _, ps := range t.ports {
		sort.Strings(ps)
	}
	return t, nil
}

// ---------- rates ----------

// indexSnapshot keys the snapshot's ports by (device, iface).
func indexSnapshot(snap poller.PortSnapshot) map[ifaceKey]poller.PortRate {
	out := make(map[ifaceKey]poller.PortRate, len(snap.Ports))
	for _, p := range snap.Ports {
		out[ifaceKey{p.DeviceID, p.Iface}] = p
	}
	return out
}

// liveRates is a RateFunc over the live snapshot: measured when the port is in it.
func liveRates(live map[ifaceKey]poller.PortRate) topology.RateFunc {
	return func(deviceID, iface string) (int64, int64, bool) {
		p, ok := live[ifaceKey{deviceID, iface}]
		return p.RxBps, p.TxBps, ok
	}
}

// windowRates is a RateFunc of range averages (bytes*8 / effective-window
// seconds). Known ports without rows are measured zeros (idle minutes are
// not stored) — unless their device has no rows at all in the window, i.e.
// the collector never reached it (offline): those ports are unmeasured, so
// the Sankey falls back to the other end of the link, as it does live.
func (t *trafficTopo) windowRates(aggs []queries.PortAggregate, secs float64) topology.RateFunc {
	avg := make(map[ifaceKey][2]int64, len(aggs))
	polled := make(map[string]bool)
	for _, a := range aggs {
		avg[ifaceKey{a.DeviceID, a.InterfaceName}] = [2]int64{bpsOver(a.RxBytes, secs), bpsOver(a.TxBytes, secs)}
		polled[a.DeviceID] = true
	}
	return func(deviceID, iface string) (int64, int64, bool) {
		k := ifaceKey{deviceID, iface}
		if v, ok := avg[k]; ok {
			return v[0], v[1], true
		}
		return 0, 0, polled[deviceID] && t.known[k]
	}
}

// ---------- /traffic/path ----------

type trafficPathResponse struct {
	DeviceID string         `json:"device_id"`
	Iface    string         `json:"iface"`
	Role     string         `json:"role"` // "" when no iface was given
	Anchored bool           `json:"anchored"`
	Hops     []topology.Hop `json:"hops"`
}

// handleTrafficPath returns the Internet → device/port path with the live rate
// of every segment. Query: device (required), iface (optional; without it
// there are no sink hops).
func (s *Server) handleTrafficPath(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	deviceID, iface := q.Get("device"), q.Get("iface")
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "device is required")
		return
	}
	t, err := s.trafficTopology()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load topology")
		return
	}
	rates := liveRates(indexSnapshot(s.portSnapshot()))
	hops, err := t.graph.Path(deviceID, iface, rates)
	if errors.Is(err, topology.ErrUnknownDevice) {
		// The cached graph may predate the device (added by another path,
		// e.g. a restore): rebuild once before answering 404.
		if exists, derr := s.deviceExists(deviceID); derr == nil && exists {
			s.invalidateTrafficTopo()
			if t, err = s.trafficTopology(); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to load topology")
				return
			}
			hops, err = t.graph.Path(deviceID, iface, rates)
		}
	}
	if errors.Is(err, topology.ErrUnknownDevice) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build path")
		return
	}
	resp := trafficPathResponse{DeviceID: deviceID, Iface: iface, Anchored: t.graph.Anchored(), Hops: hops}
	if resp.Hops == nil {
		resp.Hops = []topology.Hop{}
	}
	if iface != "" {
		resp.Role = t.graph.Role(deviceID, iface).Role
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- /traffic/sankey ----------

type trafficSankeyResponse struct {
	topology.Sankey
	Range string     `json:"range"`
	From  *time.Time `json:"from"` // null for live
	To    *time.Time `json:"to"`
}

// handleTrafficSankey returns the estimated source → sink flow forest for one
// direction. Query: range (default 1h), dir = download|upload (default
// download), device (optional subtree root). live uses the snapshot rates,
// other ranges the average over the effective window.
func (s *Server) handleTrafficSankey(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rng, ok := parseTrafficRange(q.Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid range")
		return
	}
	dir := q.Get("dir")
	if dir == "" {
		dir = "download"
	}
	if dir != "download" && dir != "upload" {
		writeError(w, http.StatusBadRequest, "invalid dir")
		return
	}
	t, err := s.trafficTopology()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load topology")
		return
	}

	resp := trafficSankeyResponse{Range: rng.key}
	var rates topology.RateFunc
	if rng.key == rangeLive {
		rates = liveRates(indexSnapshot(s.portSnapshot()))
	} else {
		win, err := s.resolveWindow(rng, time.Now())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read port stats")
			return
		}
		resp.From, resp.To = timePtr(win.start), timePtr(win.end)
		var aggs []queries.PortAggregate
		if win.covered {
			if aggs, err = queries.AggregatePortStats(s.db, rng.table, win.rowsFrom(), win.end, "", nil); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to read port stats")
				return
			}
		}
		rates = t.windowRates(aggs, win.seconds())
	}
	resp.Sankey = t.graph.Sankey(rates, topology.SankeyOptions{Direction: dir, RootDeviceID: q.Get("device")})
	writeJSON(w, http.StatusOK, resp)
}
