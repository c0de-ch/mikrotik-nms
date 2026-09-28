package api

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/macvendor"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// Fleet-wide port analytics served from the port-stats collector (live
// snapshot) and the port_stats_1m / port_stats_1h / port_hosts tables. Port
// endpoints take device + iface as query params. Direction: rx = bits entering
// the device through the port, tx = bits leaving it.

const (
	topDefaultLimit = 20
	topMaxLimit     = 200
	// behindMaxClients caps the client list of /traffic/ports/behind (a core
	// downlink can carry the whole LAN's MACs).
	behindMaxClients = 500
	// maxVLANRange is the widest a-b range expanded for a port's VLAN list;
	// wider ranges (e.g. a trunk allowing 2-4094) are left out.
	maxVLANRange = 64
)

// isFalseParam reports a boolean query param explicitly switched off.
func isFalseParam(v string) bool { return v == "0" || strings.EqualFold(v, "false") }

// physicalRole reports whether a role survives the physical=1 filter (drops
// virtual interfaces and VPN tunnels; every physical role — peer links, bond
// members and idle-labelled ports included — stays, so a port carrying
// traffic is never hidden).
func physicalRole(role string) bool {
	return role != topology.RoleVirtual && role != topology.RoleVPN
}

// ---------- /traffic/ports/latest ----------

type latestPortRow struct {
	poller.PortRate
	DeviceName string `json:"device_name"`
	Role       string `json:"role"`
}

type latestPortsResponse struct {
	TS              *time.Time      `json:"ts"`
	IntervalSeconds int             `json:"interval_seconds"`
	Ready           bool            `json:"ready"`
	Ports           []latestPortRow `json:"ports"`
}

// handleTrafficPortsLatest returns the collector's snapshot enriched with the
// device name and port role.
func (s *Server) handleTrafficPortsLatest(w http.ResponseWriter, r *http.Request) {
	snap := s.portSnapshot()
	resp := latestPortsResponse{
		TS:              snap.TS,
		IntervalSeconds: snap.IntervalSeconds,
		Ready:           snap.Ready,
		Ports:           make([]latestPortRow, 0, len(snap.Ports)),
	}
	if len(snap.Ports) > 0 {
		t, err := s.trafficTopology()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load topology")
			return
		}
		for _, p := range snap.Ports {
			resp.Ports = append(resp.Ports, latestPortRow{PortRate: p, DeviceName: t.name(p.DeviceID),
				Role: t.graph.Role(p.DeviceID, p.Iface).Role})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- /traffic/ports/roles ----------

type portRoleRow struct {
	topology.PortRoleInfo
	DeviceName string `json:"device_name"`
	Behind     string `json:"behind"`
}

type portRolesResponse struct {
	Anchored bool                      `json:"anchored"`
	Devices  []topology.DeviceTreeInfo `json:"devices"`
	Roles    []portRoleRow             `json:"roles"`
}

// handleTrafficPortRoles returns the inferred device tree and every known
// port's role with its "behind" summary.
func (s *Server) handleTrafficPortRoles(w http.ResponseWriter, r *http.Request) {
	t, err := s.trafficTopology()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load topology")
		return
	}
	roles := t.graph.Roles()
	rows := make([]portRoleRow, 0, len(roles))
	for _, ri := range roles {
		rows = append(rows, portRoleRow{PortRoleInfo: ri, DeviceName: t.name(ri.DeviceID), Behind: t.behind(ri)})
	}
	writeJSON(w, http.StatusOK, portRolesResponse{Anchored: t.graph.Anchored(), Devices: t.graph.Devices(), Roles: rows})
}

// behind is the one-line "what is behind this port" summary shown next to a
// port: the upstream for wan/uplink, the child device for a downlink, the
// device and port across a peer link, the client (or client count) for an
// edge port. A bond member names its bond, then the bond's summary.
func (t *trafficTopo) behind(ri topology.PortRoleInfo) string {
	if ri.Master != "" {
		bond := t.graph.Role(ri.DeviceID, ri.Master)
		if b := t.behind(bond); b != "" {
			return "member of " + ri.Master + " · " + b
		}
		return "member of " + ri.Master
	}
	switch ri.Role {
	case topology.RoleWAN:
		if ri.UpstreamNode == "internet" {
			return "Internet"
		}
		label := ri.NeighborName
		if label == "" {
			label = ri.GatewayIP
		}
		if label == "" {
			return ""
		}
		return "via " + label
	case topology.RoleUplink:
		if ri.NeighborName == "" {
			return ""
		}
		return "↑ " + ri.NeighborName
	case topology.RoleDownlink:
		if ri.NeighborCount > 1 {
			return fmt.Sprintf("%s +%d", ri.NeighborName, ri.NeighborCount-1)
		}
		return ri.NeighborName
	case topology.RolePeer:
		if ri.NeighborName == "" {
			return ""
		}
		return strings.TrimSpace("↔ " + ri.NeighborName + " " + ri.NeighborIface)
	case topology.RoleAccess, topology.RoleWireless:
		n := ri.ClientCount
		if n == 0 {
			return ""
		}
		if macs := t.graph.AttachedMACs(ri.DeviceID, ri.Iface); n == 1 && len(macs) == 1 {
			return t.clientName(macs[0])
		}
		if ri.Role == topology.RoleWireless {
			return fmt.Sprintf("%d Wi-Fi clients", n)
		}
		return fmt.Sprintf("%d clients", n)
	case topology.RoleVPN:
		return "VPN tunnel"
	}
	return ""
}

// clientName is a client's best display name: host name, DNS name, IP,
// vendor, then the MAC itself.
func (t *trafficTopo) clientName(mac string) string {
	if m := t.macs[mac]; m != nil {
		for _, v := range []string{m.HostName, m.DNSName, m.IPAddress} {
			if v != "" {
				return v
			}
		}
	}
	if vendor, _ := macvendor.Describe(mac); vendor != "" {
		return vendor
	}
	return mac
}

// ---------- /traffic/ports/top ----------

type topPortRow struct {
	Rank             int                   `json:"rank"`
	DeviceID         string                `json:"device_id"`
	DeviceName       string                `json:"device_name"`
	Iface            string                `json:"iface"`
	Type             string                `json:"type"`
	Role             string                `json:"role"`
	Behind           string                `json:"behind"`
	NeighborDeviceID string                `json:"neighbor_device_id,omitempty"`
	ClientCount      int                   `json:"client_count"`
	Running          bool                  `json:"running"`
	RxAvg            int64                 `json:"rx_avg"`
	TxAvg            int64                 `json:"tx_avg"`
	RxMax            int64                 `json:"rx_max"`
	TxMax            int64                 `json:"tx_max"`
	RxBytes          int64                 `json:"rx_bytes"`
	TxBytes          int64                 `json:"tx_bytes"`
	Value            int64                 `json:"value"` // sort key: bps for avg/max, bytes for bytes
	Sparkline        []queries.SeriesPoint `json:"sparkline"`
}

type topPortsResponse struct {
	Range        string       `json:"range"`
	Metric       string       `json:"metric"`
	Dir          string       `json:"dir"`
	Resolution   string       `json:"resolution"`
	From         *time.Time   `json:"from"`
	To           *time.Time   `json:"to"`
	CoverageFrom *time.Time   `json:"coverage_from"`
	Rows         []topPortRow `json:"rows"`
}

// portTotals is one port's figures over the requested range.
type portTotals struct {
	rxAvg, txAvg     int64
	rxMax, txMax     int64
	rxBytes, txBytes int64
}

// topValue is the ranking key for metric × dir.
func topValue(metric, dir string, v portTotals) int64 {
	switch metric {
	case "max":
		switch dir {
		case "rx":
			return v.rxMax
		case "tx":
			return v.txMax
		}
		return max(v.rxMax, v.txMax)
	case "bytes":
		switch dir {
		case "rx":
			return v.rxBytes
		case "tx":
			return v.txBytes
		}
		return v.rxBytes + v.txBytes
	}
	switch dir {
	case "rx":
		return v.rxAvg
	case "tx":
		return v.txAvg
	}
	return v.rxAvg + v.txAvg
}

// handleTrafficPortsTop ranks ports by traffic over a range. Query: range
// (default 1h), metric = avg|max|bytes, dir = total|rx|tx, limit (1..200,
// default 20), physical (default on), device (optional: rank every known
// interface of that device, idle ones as zeros), sparkline (default on).
func (s *Server) handleTrafficPortsTop(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rng, ok := parseTrafficRange(q.Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid range")
		return
	}
	metric := q.Get("metric")
	if metric == "" {
		metric = "avg"
	}
	if metric != "avg" && metric != "max" && metric != "bytes" {
		writeError(w, http.StatusBadRequest, "invalid metric")
		return
	}
	dir := q.Get("dir")
	if dir == "" {
		dir = "total"
	}
	if dir != "total" && dir != "rx" && dir != "tx" {
		writeError(w, http.StatusBadRequest, "invalid dir")
		return
	}
	limit := topDefaultLimit
	if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		limit = min(max(n, 1), topMaxLimit)
	}
	physical := !isFalseParam(q.Get("physical"))
	withSparkline := !isFalseParam(q.Get("sparkline"))
	deviceID := q.Get("device")

	t, err := s.trafficTopology()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load topology")
		return
	}
	snap := s.portSnapshot()
	live := indexSnapshot(snap)
	now := time.Now()

	resp := topPortsResponse{Range: rng.key, Metric: metric, Dir: dir, Resolution: rng.resolution, Rows: []topPortRow{}}
	totals := make(map[ifaceKey]portTotals)
	sparkRange := rng
	var win rangeWindow

	if rng.key == rangeLive {
		// Live rates have no volume: bytes ranks like avg.
		if metric == "bytes" {
			metric, resp.Metric = "avg", "avg"
		}
		for _, p := range snap.Ports {
			if deviceID != "" && p.DeviceID != deviceID {
				continue
			}
			if deviceID == "" && p.RxBps+p.TxBps <= 0 {
				continue
			}
			totals[ifaceKey{p.DeviceID, p.Iface}] = portTotals{rxAvg: p.RxBps, txAvg: p.TxBps, rxMax: p.RxBps, txMax: p.TxBps}
		}
		sparkRange = liveSparkRange
	} else {
		if win, err = s.resolveWindow(rng, now); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read port stats")
			return
		}
		resp.From, resp.To, resp.CoverageFrom = timePtr(win.start), timePtr(win.end), win.coverageFrom()
		if win.covered {
			// A device's ranking scans only its known ports: with the full
			// primary key (device, iface, bucket) the range stays narrow,
			// where device_id alone would read the device's whole retention.
			var ifaces []string
			if deviceID != "" {
				ifaces = t.devicePorts(deviceID, snap)
			}
			aggs, err := queries.AggregatePortStats(s.db, rng.table, win.rowsFrom(), win.end, deviceID, ifaces)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "failed to read port stats")
				return
			}
			secs := win.seconds()
			for _, a := range aggs {
				totals[ifaceKey{a.DeviceID, a.InterfaceName}] = portTotals{
					rxAvg: bpsOver(a.RxBytes, secs), txAvg: bpsOver(a.TxBytes, secs),
					rxMax: a.RxBpsMax, txMax: a.TxBpsMax,
					rxBytes: a.RxBytes, txBytes: a.TxBytes,
				}
			}
		}
	}

	// A device's ranked list is complete: every interface it is known to
	// have, idle ones as zeros.
	if deviceID != "" {
		for _, iface := range t.ports[deviceID] {
			if k := (ifaceKey{deviceID, iface}); !hasKey(totals, k) {
				totals[k] = portTotals{}
			}
		}
		for _, p := range snap.Ports {
			if k := (ifaceKey{p.DeviceID, p.Iface}); p.DeviceID == deviceID && !hasKey(totals, k) {
				totals[k] = portTotals{}
			}
		}
	}

	for k, v := range totals {
		ri := t.graph.Role(k.dev, k.iface)
		if physical && !physicalRole(ri.Role) {
			continue
		}
		typ := t.types[k]
		if p, ok := live[k]; ok && p.Type != "" {
			typ = p.Type
		}
		resp.Rows = append(resp.Rows, topPortRow{
			DeviceID: k.dev, DeviceName: t.name(k.dev), Iface: k.iface, Type: typ,
			Role: ri.Role, Behind: t.behind(ri), NeighborDeviceID: ri.NeighborDeviceID, ClientCount: ri.ClientCount,
			Running: live[k].Running,
			RxAvg:   v.rxAvg, TxAvg: v.txAvg, RxMax: v.rxMax, TxMax: v.txMax, RxBytes: v.rxBytes, TxBytes: v.txBytes,
			Value:     topValue(metric, dir, v),
			Sparkline: []queries.SeriesPoint{},
		})
	}
	sort.Slice(resp.Rows, func(i, j int) bool {
		a, b := resp.Rows[i], resp.Rows[j]
		if a.Value != b.Value {
			return a.Value > b.Value
		}
		if a.DeviceName != b.DeviceName {
			return a.DeviceName < b.DeviceName
		}
		return a.Iface < b.Iface
	})
	if len(resp.Rows) > limit {
		resp.Rows = resp.Rows[:limit]
	}
	for i := range resp.Rows {
		resp.Rows[i].Rank = i + 1
	}

	if withSparkline && len(resp.Rows) > 0 {
		sw := win
		if rng.key == rangeLive {
			if sw, err = s.resolveWindow(sparkRange, now); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to read port stats")
				return
			}
		}
		if sw.covered {
			from := sw.pointsFrom(sparkRange.sparkStep)
			for i := range resp.Rows {
				row := &resp.Rows[i]
				series, err := queries.GetPortStatsSeries(s.db, sparkRange.table, row.DeviceID, row.Iface, from, sw.end)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "failed to read port stats")
					return
				}
				row.Sparkline = queries.DownsampleSeriesCovered(series, from, sw.end, sparkRange.sparkStep, sw.cov)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func hasKey[K comparable, V any](m map[K]V, k K) bool {
	_, ok := m[k]
	return ok
}

// ---------- /traffic/ports/history ----------

type portHistoryResponse struct {
	DeviceID     string                `json:"device_id"`
	Iface        string                `json:"iface"`
	Range        string                `json:"range"`
	Resolution   string                `json:"resolution"` // 1s | 1m | 1h
	StepSeconds  int                   `json:"step_seconds"`
	From         time.Time             `json:"from"`
	To           time.Time             `json:"to"`
	CoverageFrom *time.Time            `json:"coverage_from"`
	Points       []queries.SeriesPoint `json:"points"`
	Stats        queries.SeriesStats   `json:"stats"`
}

// deviceExists distinguishes an unknown device (false, nil) from a DB error.
func (s *Server) deviceExists(id string) (bool, error) {
	if _, err := queries.GetDevice(s.db, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// portParams reads the required device + iface query params, writing the
// 400 / 404 / 500 itself when they are unusable.
func (s *Server) portParams(w http.ResponseWriter, r *http.Request) (deviceID, iface string, ok bool) {
	q := r.URL.Query()
	deviceID, iface = q.Get("device"), q.Get("iface")
	if deviceID == "" || iface == "" {
		writeError(w, http.StatusBadRequest, "device and iface are required")
		return "", "", false
	}
	exists, err := s.deviceExists(deviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get device")
		return "", "", false
	}
	if !exists {
		writeError(w, http.StatusNotFound, "device not found")
		return "", "", false
	}
	return deviceID, iface, true
}

// handleTrafficPortHistory returns one port's chart series and summary stats.
// Query: device, iface (required), range (default 1h). live = the last 5
// minutes of the 1 s traffic_samples stream (only recorded while someone
// watches the port); other ranges = port_stats_1m / port_stats_1h, points
// zero-filled from the start of coverage.
func (s *Server) handleTrafficPortHistory(w http.ResponseWriter, r *http.Request) {
	rng, ok := parseTrafficRange(r.URL.Query().Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid range")
		return
	}
	deviceID, iface, ok := s.portParams(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	resp := portHistoryResponse{DeviceID: deviceID, Iface: iface, Range: rng.key, Points: []queries.SeriesPoint{}}

	if rng.key == rangeLive {
		resp.Resolution, resp.StepSeconds = "1s", 1
		resp.From, resp.To = now.Add(-5*time.Minute), now
		samples, err := queries.GetTrafficSamples(s.db, deviceID, iface, resp.From, resp.To, 1000)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to get traffic samples")
			return
		}
		rows := make([]queries.PortStatRow, 0, len(samples))
		for i := len(samples) - 1; i >= 0; i-- { // newest-first → ascending
			sm := samples[i]
			ts := sm.CollectedAt.UTC()
			resp.Points = append(resp.Points, queries.SeriesPoint{TS: ts, RxBps: sm.RxBitsPerSec, TxBps: sm.TxBitsPerSec})
			rows = append(rows, queries.PortStatRow{Bucket: ts,
				RxBpsAvg: sm.RxBitsPerSec, TxBpsAvg: sm.TxBitsPerSec,
				RxBpsMax: sm.RxBitsPerSec, TxBpsMax: sm.TxBitsPerSec,
				RxBytes: sm.RxBitsPerSec / 8, TxBytes: sm.TxBitsPerSec / 8})
		}
		if n := len(rows); n > 0 {
			resp.Stats = queries.ComputeSeriesStats(rows, rows[0].Bucket, rows[n-1].Bucket.Add(time.Second), time.Second)
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	win, err := s.resolveWindow(rng, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read port stats")
		return
	}
	resp.Resolution, resp.StepSeconds = rng.resolution, int(rng.step/time.Second)
	resp.From, resp.To, resp.CoverageFrom = win.start, win.end, win.coverageFrom()
	if win.covered {
		from := win.pointsFrom(rng.step)
		rows, err := queries.GetPortStatsSeries(s.db, rng.table, deviceID, iface, from, win.end)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read port stats")
			return
		}
		resp.Points = queries.DownsampleSeriesCovered(rows, from, win.end, rng.step, win.cov)
		resp.Stats = queries.ComputeSeriesStatsCovered(rows, win.rowsFrom(), win.end, rng.native, win.cov)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- /traffic/ports/behind ----------

type behindNeighbor struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Iface    string `json:"iface"`
}

type behindUplink struct {
	NodeID    string `json:"node_id"` // internet | gw:<ip> | vpn:<dev>:<iface>
	Label     string `json:"label"`
	GatewayIP string `json:"gateway_ip"`
}

type behindClient struct {
	MAC                string    `json:"mac"`
	IP                 string    `json:"ip"`
	HostName           string    `json:"host_name"`
	Vendor             string    `json:"vendor"`
	VID                int       `json:"vid"`
	Wireless           bool      `json:"wireless"`
	AP                 string    `json:"ap"`
	SSID               string    `json:"ssid"`
	Signal             string    `json:"signal"`
	AttachedDeviceID   string    `json:"attached_device_id"`
	AttachedIface      string    `json:"attached_iface"`
	AttachedDeviceName string    `json:"attached_device_name"`
	LastSeen           time.Time `json:"last_seen"`

	attachedHere bool
}

type behindVLAN struct {
	VID    int    `json:"vid"`
	Name   string `json:"name"`
	Tagged bool   `json:"tagged"`
}

type portBehindResponse struct {
	DeviceID    string          `json:"device_id"`
	Iface       string          `json:"iface"`
	Role        string          `json:"role"`
	Neighbor    *behindNeighbor `json:"neighbor"`
	Uplink      *behindUplink   `json:"uplink"`
	ClientCount int             `json:"client_count"`
	MACCount    int             `json:"mac_count"`
	Clients     []behindClient  `json:"clients"`
	VLANs       []behindVLAN    `json:"vlans"`
}

// handleTrafficPortBehind lists what sits behind one port: the neighbour
// device (uplink/downlink/peer), the upstream node (wan/vpn, or an uplink to a
// synthetic node), every non-managed MAC the port's FDB learned (on a downlink
// or peer link that is everything beyond it), and the VLANs the port carries.
// Query: device, iface (required).
func (s *Server) handleTrafficPortBehind(w http.ResponseWriter, r *http.Request) {
	deviceID, iface, ok := s.portParams(w, r)
	if !ok {
		return
	}
	t, err := s.trafficTopology()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load topology")
		return
	}
	ri := t.graph.Role(deviceID, iface)
	resp := portBehindResponse{DeviceID: deviceID, Iface: iface, Role: ri.Role,
		Clients: []behindClient{}, VLANs: []behindVLAN{}}

	switch ri.Role {
	case topology.RoleUplink, topology.RoleDownlink, topology.RolePeer:
		if ri.NeighborDeviceID != "" {
			resp.Neighbor = &behindNeighbor{DeviceID: ri.NeighborDeviceID, Name: ri.NeighborName, Iface: ri.NeighborIface}
		}
	}
	switch ri.Role {
	case topology.RoleWAN, topology.RoleVPN, topology.RoleUplink:
		if ri.UpstreamNode != "" {
			label := ri.NeighborName
			if ri.Role == topology.RoleVPN && label == "" {
				label = ri.Iface
			}
			resp.Uplink = &behindUplink{NodeID: ri.UpstreamNode, Label: label, GatewayIP: ri.GatewayIP}
		}
	}

	hosts := t.hosts[ifaceKey{deviceID, iface}]
	resp.MACCount = len(hosts) // one row per (port, MAC)
	for _, h := range hosts {
		if t.managed[h.MACAddress] {
			continue
		}
		resp.Clients = append(resp.Clients, t.describeClient(h, deviceID, iface))
	}
	sort.SliceStable(resp.Clients, func(i, j int) bool { return lessClient(resp.Clients[i], resp.Clients[j]) })
	resp.ClientCount = len(resp.Clients)
	if len(resp.Clients) > behindMaxClients {
		resp.Clients = resp.Clients[:behindMaxClients]
	}

	if resp.VLANs, err = portVLANs(s.db, deviceID, iface); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list vlans")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// describeClient enriches one FDB entry from mac_lookup, the OUI vendor table
// and the role graph's attachment point.
func (t *trafficTopo) describeClient(h queries.PortHost, deviceID, iface string) behindClient {
	c := behindClient{MAC: h.MACAddress, VID: h.VID, LastSeen: h.LastSeen}
	if m := t.macs[h.MACAddress]; m != nil {
		c.IP, c.HostName = m.IPAddress, m.HostName
		if c.HostName == "" {
			c.HostName = m.DNSName
		}
		c.AP, c.SSID, c.Signal = m.AP, m.SSID, m.Signal
	}
	c.Vendor, _ = macvendor.Describe(h.MACAddress)
	if dev, port, ok := t.graph.AttachmentOf(h.MACAddress); ok {
		c.AttachedDeviceID, c.AttachedIface, c.AttachedDeviceName = dev, port, t.name(dev)
		c.attachedHere = dev == deviceID && port == iface
		c.Wireless = t.graph.Role(dev, port).Role == topology.RoleWireless
	}
	c.Wireless = c.Wireless || c.SSID != ""
	return c
}

// lessClient orders clients attached at this very port first, then named
// before unnamed (by host name), then by IP (numeric), then MAC.
func lessClient(a, b behindClient) bool {
	if a.attachedHere != b.attachedHere {
		return a.attachedHere
	}
	if (a.HostName == "") != (b.HostName == "") {
		return a.HostName != ""
	}
	if ah, bh := strings.ToLower(a.HostName), strings.ToLower(b.HostName); ah != bh {
		return ah < bh
	}
	if a.IP != b.IP {
		if a.IP == "" || b.IP == "" {
			return a.IP != ""
		}
		ai, aerr := netip.ParseAddr(a.IP)
		bi, berr := netip.ParseAddr(b.IP)
		if aerr == nil && berr == nil {
			return ai.Less(bi)
		}
		return a.IP < b.IP
	}
	return a.MAC < b.MAC
}

// portVLANs lists the VLANs whose bridge VLAN-table entry on deviceID
// currently carries iface (current-tagged / current-untagged), labelled from
// vlan_labels. Untagged wins when a VID lists the port both ways.
func portVLANs(db *sql.DB, deviceID, iface string) ([]behindVLAN, error) {
	rows, err := queries.ListBridgeVLANs(db)
	if err != nil {
		return nil, err
	}
	labels, err := queries.ListVLANLabels(db)
	if err != nil {
		return nil, err
	}
	names := make(map[int]string, len(labels))
	for _, l := range labels {
		names[l.VLANID] = l.Name
	}

	byVID := make(map[int]behindVLAN)
	for _, v := range rows {
		if v.DeviceID != deviceID {
			continue
		}
		tagged, untagged := listHas(v.CurrentTagged, iface), listHas(v.CurrentUntagged, iface)
		if !tagged && !untagged {
			continue
		}
		for _, vid := range expandVLANIDs(v.VLANIDs) {
			e, seen := byVID[vid]
			if !seen {
				e = behindVLAN{VID: vid, Name: names[vid], Tagged: !untagged}
			} else if untagged {
				e.Tagged = false
			}
			byVID[vid] = e
		}
	}
	out := make([]behindVLAN, 0, len(byVID))
	for _, e := range byVID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VID < out[j].VID })
	return out, nil
}

// listHas reports whether a RouterOS comma list contains name.
func listHas(list, name string) bool {
	for _, item := range strings.Split(list, ",") {
		if strings.TrimSpace(item) == name {
			return true
		}
	}
	return false
}

// expandVLANIDs expands a RouterOS vlan-ids value ("10,20-22") into VIDs.
// Ranges wider than maxVLANRange and anything outside 1..4094 are skipped.
func expandVLANIDs(s string) []int {
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
