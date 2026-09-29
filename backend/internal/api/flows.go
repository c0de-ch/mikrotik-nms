package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow"
	"github.com/mikrotik-nms/backend/internal/flowview"
)

// Flow export (NetFlow / IPFIX / sFlow) endpoints: collector status and setup
// hints, exporter / interface / observation-point configuration (admin
// writes), derived-view suggestions, and the measured views (/flows/top,
// /flows/port, /flows/coverage, /traffic/sankey?source=flows). Reads are
// open to every logged-in user (like /clients); every admin write reloads
// the collector and clears the aggregated-rows cache.

// mountFlowRoutes registers the /flows/* endpoints (inside RequireAuth).
func (s *Server) mountFlowRoutes(r chi.Router) {
	r.Get("/flows/status", s.handleFlowStatus)
	r.Get("/flows/setup", s.handleFlowSetup)
	r.Get("/flows/points", s.handleListFlowPoints)
	r.Get("/flows/points/suggestions", s.handleFlowPointSuggestions)
	r.Get("/flows/exporters/{id}/interfaces", s.handleListFlowIfaces)
	r.Get("/flows/top", s.handleFlowTop)
	r.Get("/flows/port", s.handleFlowPort)
	r.Get("/flows/coverage", s.handleFlowCoverage)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole("admin"))
		r.Post("/flows/exporters", s.handleCreateFlowExporter)
		r.Put("/flows/exporters/{id}", s.handleUpdateFlowExporter)
		r.Delete("/flows/exporters/{id}", s.handleDeleteFlowExporter)
		r.Put("/flows/exporters/{id}/interfaces/{ifIndex}", s.handleUpdateFlowIface)
		r.Post("/flows/points", s.handleCreateFlowPoint)
		r.Put("/flows/points/{id}", s.handleUpdateFlowPoint)
		r.Delete("/flows/points/{id}", s.handleDeleteFlowPoint)
	})
}

// optional is a JSON field that records whether it was present in the body
// (a null counts as present, with the zero Value).
type optional[T any] struct {
	Set   bool
	Value T
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	return json.Unmarshal(b, &o.Value)
}

// idParam parses a positive integer URL param.
func idParam(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	return id, err == nil && id > 0
}

// ---------- /flows/status ----------

type flowSuggestedExporter struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
}

type flowStatusResponse struct {
	flow.Status
	SuggestedExporters []flowSuggestedExporter `json:"suggested_exporters"`
}

// flowProtocolLabels are the human names of the wire protocols.
var flowProtocolLabels = map[string]string{
	"netflow5": "NetFlow v5",
	"netflow9": "NetFlow v9",
	"ipfix":    "IPFIX",
	"sflow5":   "sFlow",
}

// opnsenseURLKey matches the OPNsense source URL settings (opnsense_url,
// opnsense2_url, …).
var opnsenseURLKey = regexp.MustCompile(`^opnsense\d*_url$`)

// handleFlowStatus returns the collector status (every DB exporter present,
// see mergedFlowStatus) plus exporters worth adding: unknown senders and the
// hosts of the OPNsense source settings that are IP literals, minus
// addresses that already are exporters.
func (s *Server) handleFlowStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	st, rows, err := s.mergedFlowStatus(now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list exporters")
		return
	}
	sugg, err := s.suggestedExporters(st, rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build suggestions")
		return
	}
	writeJSON(w, http.StatusOK, flowStatusResponse{Status: st, SuggestedExporters: sugg})
}

// suggestedExporters implements the suggested_exporters list of
// /flows/status (contract §9.2).
func (s *Server) suggestedExporters(st flow.Status, rows []queries.FlowExporter) ([]flowSuggestedExporter, error) {
	out := []flowSuggestedExporter{}
	taken := map[netip.Addr]bool{}
	for _, e := range rows {
		if a, ok := parseIP(e.Address); ok {
			taken[a] = true
		}
	}
	devices, err := queries.ListDevices(s.db)
	if err != nil {
		return nil, err
	}
	devByIP := map[netip.Addr]queries.Device{}
	for _, d := range devices {
		if a, ok := parseIP(d.Address); ok {
			devByIP[a] = d
		}
	}
	settings, err := queries.GetAllSettings(s.db)
	if err != nil {
		return nil, err
	}
	type opnHost struct {
		key  string
		addr netip.Addr
	}
	var opn []opnHost
	opnByIP := map[netip.Addr]string{}
	keys := make([]string, 0, len(settings))
	for k := range settings {
		if opnsenseURLKey.MatchString(k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		u, err := url.Parse(strings.TrimSpace(settings[k]))
		if err != nil || u.Hostname() == "" {
			continue
		}
		if a, ok := parseIP(u.Hostname()); ok {
			if _, dup := opnByIP[a]; !dup {
				opnByIP[a] = k
				opn = append(opn, opnHost{k, a})
			}
		}
	}
	shortHost := func(a netip.Addr) string {
		name, _, _ := strings.Cut(queries.HostnameForIP(s.db, a.String()), ".")
		return name
	}
	for _, u := range st.Unknown {
		a, ok := parseIP(u.Address)
		if !ok || taken[a] {
			continue
		}
		taken[a] = true
		sg := flowSuggestedExporter{Address: a.String(), Kind: "other"}
		dev, isDev := devByIP[a]
		switch {
		case u.DeviceID != nil || isDev:
			sg.Kind = "routeros"
		case u.Protocol == "netflow9" && opnByIP[a] != "":
			sg.Kind = "opnsense"
		}
		switch {
		case u.DeviceName != "":
			sg.Name = u.DeviceName
		case isDev:
			sg.Name = deviceDisplayName(dev)
		default:
			sg.Name = shortHost(a)
		}
		if sg.Name == "" {
			sg.Name = a.String()
		}
		proto := flowProtocolLabels[u.Protocol]
		if proto == "" {
			proto = "flow data"
		}
		sg.Reason = "sending " + proto + " (unknown sender)"
		out = append(out, sg)
	}
	for _, h := range opn {
		if taken[h.addr] {
			continue
		}
		taken[h.addr] = true
		name := shortHost(h.addr)
		if name == "" {
			name = h.addr.String()
		}
		out = append(out, flowSuggestedExporter{Address: h.addr.String(), Name: name, Kind: "opnsense",
			Reason: "configured as OPNsense (" + h.key + ")"})
	}
	return out, nil
}

// ---------- /flows/setup ----------

type flowSetupRouterOS struct {
	DeviceID *string  `json:"device_id"`
	Apply    []string `json:"apply"`
	Rollback []string `json:"rollback"`
}

type flowSetupOPNsense struct {
	Destination     string   `json:"destination"`
	Version         string   `json:"version"`
	ActiveTimeout   int      `json:"active_timeout"`
	InactiveTimeout int      `json:"inactive_timeout"`
	Steps           []string `json:"steps"`
}

type flowSetupResponse struct {
	Enabled          bool              `json:"enabled"`
	Port             int               `json:"port"`
	AdvertiseAddress string            `json:"advertise_address"`
	AdvertiseSource  string            `json:"advertise_source"` // setting | auto | unknown
	RouterOS         flowSetupRouterOS `json:"routeros"`
	OPNsense         flowSetupOPNsense `json:"opnsense"`
}

// defaultFlowPort is the port the setup hints use while the collector is off.
const defaultFlowPort = 2055

// specificListenAddr returns the host of the first listen entry bound to a
// specific unicast address (not a wildcard, not loopback).
func specificListenAddr(listen []string) (netip.Addr, bool) {
	for _, l := range listen {
		host, _, err := net.SplitHostPort(strings.TrimSpace(l))
		if err != nil || host == "" {
			continue
		}
		a, ok := parseIP(host)
		if !ok || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() {
			continue
		}
		return a, true
	}
	return netip.Addr{}, false
}

// localAddrToward returns the local address the kernel would use to reach
// target:port (a connected UDP socket; nothing is sent).
func localAddrToward(target netip.Addr, port int) (netip.Addr, bool) {
	conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(target, uint16(port))))
	if err != nil {
		return netip.Addr{}, false
	}
	defer conn.Close()
	la, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	a, ok := netip.AddrFromSlice(la.IP)
	if !ok || a.IsUnspecified() {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// handleFlowSetup returns copy-pasteable exporter configuration: RouterOS
// traffic-flow commands (+ rollback) and OPNsense NetFlow steps, targeting
// the collector's advertised address. Query device (optional) adds the
// device's address as src-address and is the preferred route probe.
func (s *Server) handleFlowSetup(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("device")
	var dev *queries.Device
	if deviceID != "" {
		exists, err := s.deviceExists(deviceID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to get device")
			return
		}
		if !exists {
			writeError(w, http.StatusNotFound, "device not found")
			return
		}
		if dev, err = queries.GetDevice(s.db, deviceID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to get device")
			return
		}
	}
	resp := flowSetupResponse{Port: defaultFlowPort, AdvertiseSource: "unknown"}
	if s.flows != nil && s.flows.Enabled() {
		resp.Enabled = true
		if p := flow.ListenPort(s.flows.Status().Listen); p > 0 {
			resp.Port = p
		}
	}

	var adv netip.Addr
	if v, _ := queries.GetSetting(s.db, "flow_advertise_address"); strings.TrimSpace(v) != "" {
		if a, ok := parseIP(v); ok {
			adv, resp.AdvertiseSource = a, "setting"
		}
	}
	if !adv.IsValid() && resp.Enabled {
		// A collector bound to one specific address only listens there: a
		// routed source address toward a device could be another NIC's.
		if a, ok := specificListenAddr(s.flows.Status().Listen); ok {
			adv, resp.AdvertiseSource = a, "auto"
		}
	}
	if !adv.IsValid() {
		var targets []netip.Addr
		if dev != nil {
			if a, ok := parseIP(dev.Address); ok {
				targets = append(targets, a)
			}
		}
		if exps, err := queries.ListFlowExporters(s.db); err == nil {
			for _, e := range exps {
				if a, ok := parseIP(e.Address); ok {
					targets = append(targets, a)
				}
			}
		}
		if devs, err := queries.ListDevices(s.db); err == nil {
			for _, d := range devs {
				if a, ok := parseIP(d.Address); ok {
					targets = append(targets, a)
				}
			}
		}
		for _, t := range targets {
			if a, ok := localAddrToward(t, resp.Port); ok {
				adv, resp.AdvertiseSource = a, "auto"
				break
			}
		}
	}
	host := "<NMS-IP>"
	if adv.IsValid() {
		host = adv.String()
		resp.AdvertiseAddress = host
	}
	port := strconv.Itoa(resp.Port)

	target := "/ip traffic-flow target add dst-address=" + host + " port=" + port
	if dev != nil {
		resp.RouterOS.DeviceID = &dev.ID
		if a, ok := parseIP(dev.Address); ok {
			target += " src-address=" + a.String()
		}
	}
	target += " version=ipfix v9-template-timeout=1m"
	resp.RouterOS.Apply = []string{target,
		"/ip traffic-flow set enabled=yes interfaces=all active-flow-timeout=1m inactive-flow-timeout=15s"}
	resp.RouterOS.Rollback = []string{"/ip traffic-flow set enabled=no active-flow-timeout=30m",
		"/ip traffic-flow target remove [find dst-address=" + host + " port=" + port + "]"}

	dest := host + ":" + port
	if adv.Is6() {
		dest = net.JoinHostPort(host, port)
	}
	resp.OPNsense = flowSetupOPNsense{Destination: dest, Version: "v9", ActiveTimeout: 60, InactiveTimeout: 15,
		Steps: []string{"Reporting → NetFlow → Capture (enable advanced mode)", "Listening interfaces: LAN",
			"WAN interfaces: leave empty", "Version: v9", "Destinations: add " + dest,
			"Active Timeout 60, Inactive Timeout 15", "Apply"}}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- exporters (admin) ----------

// flowExporterBody is the POST / PUT body of /flows/exporters. On PUT an
// omitted device_id or nat_addresses keeps the stored value; null clears it.
type flowExporterBody struct {
	Name             string             `json:"name"`
	Address          string             `json:"address"`
	Kind             string             `json:"kind"`
	DeviceID         optional[*string]  `json:"device_id"`
	Enabled          optional[bool]     `json:"enabled"`
	SamplingOverride optional[int]      `json:"sampling_override"`
	NATAddresses     optional[[]string] `json:"nat_addresses"`
}

// maxFlowNameLen bounds exporter / interface / point names.
const maxFlowNameLen = 64

// validName trims a name and checks 1..max runes.
func validName(s string, maxLen int) (string, string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "name is required"
	}
	if utf8.RuneCountInString(s) > maxLen {
		return "", fmt.Sprintf("name is too long (max %d characters)", maxLen)
	}
	return s, ""
}

// applyExporterBody validates b onto e (existing values are kept for fields
// PUT may omit). It returns a 400 message, or "".
func (s *Server) applyExporterBody(b flowExporterBody, e *queries.FlowExporter, create bool) (string, error) {
	name, msg := validName(b.Name, maxFlowNameLen)
	if msg != "" {
		return msg, nil
	}
	e.Name = name
	a, ok := parseIP(b.Address)
	if !ok {
		return "invalid address", nil
	}
	e.Address = a.String()
	switch b.Kind {
	case "routeros", "opnsense", "other":
		e.Kind = b.Kind
	default:
		return "invalid kind", nil
	}
	if b.DeviceID.Set || create {
		e.DeviceID = ""
		if b.DeviceID.Value != nil && strings.TrimSpace(*b.DeviceID.Value) != "" {
			id := strings.TrimSpace(*b.DeviceID.Value)
			exists, err := s.deviceExists(id)
			if err != nil {
				return "", err
			}
			if !exists {
				return "unknown device", nil
			}
			e.DeviceID = id
		}
	}
	switch {
	case b.Enabled.Set:
		e.Enabled = b.Enabled.Value
	case create:
		e.Enabled = true
	default:
		return "enabled is required", nil
	}
	switch {
	case b.SamplingOverride.Set:
		if b.SamplingOverride.Value < 0 || b.SamplingOverride.Value > 65535 {
			return "invalid sampling_override", nil
		}
		e.SamplingOverride = b.SamplingOverride.Value
	case create:
		e.SamplingOverride = 0
	default:
		return "sampling_override is required", nil
	}
	if b.NATAddresses.Set || create {
		nat := make([]netip.Addr, 0, len(b.NATAddresses.Value))
		for _, v := range b.NATAddresses.Value {
			a, ok := parseIP(v)
			if !ok {
				return "invalid nat address", nil
			}
			if !slices.Contains(nat, a) {
				nat = append(nat, a)
			}
		}
		e.NATAddresses = nat
	}
	return "", nil
}

// writeExporter answers a create / update with the stored row's status.
func (s *Server) writeExporter(w http.ResponseWriter, status int, id int64) {
	e, err := queries.GetFlowExporter(s.db, id)
	if err != nil || e == nil {
		writeError(w, http.StatusInternalServerError, "failed to read exporter")
		return
	}
	writeJSON(w, status, flow.ExporterStatusFromRow(*e, time.Now()))
}

// handleCreateFlowExporter adds an exporter (admin). 201 ExporterStatus.
func (s *Server) handleCreateFlowExporter(w http.ResponseWriter, r *http.Request) {
	var b flowExporterBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	e := queries.FlowExporter{}
	msg, err := s.applyExporterBody(b, &e, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to validate exporter")
		return
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	id, err := queries.CreateFlowExporter(s.db, &e)
	if errors.Is(err, queries.ErrFlowExporterExists) {
		writeError(w, http.StatusConflict, "exporter address already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create exporter")
		return
	}
	s.flowChanged()
	s.writeExporter(w, http.StatusCreated, id)
}

// handleUpdateFlowExporter replaces an exporter's configuration (admin).
func (s *Server) handleUpdateFlowExporter(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid exporter id")
		return
	}
	var b flowExporterBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cur, err := queries.GetFlowExporter(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get exporter")
		return
	}
	if cur == nil {
		writeError(w, http.StatusNotFound, "exporter not found")
		return
	}
	msg, err := s.applyExporterBody(b, cur, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to validate exporter")
		return
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	switch err := queries.UpdateFlowExporterConfig(s.db, cur); {
	case errors.Is(err, queries.ErrFlowExporterExists):
		writeError(w, http.StatusConflict, "exporter address already exists")
		return
	case errors.Is(err, queries.ErrFlowNotFound):
		writeError(w, http.StatusNotFound, "exporter not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to update exporter")
		return
	}
	s.flowChanged()
	s.writeExporter(w, http.StatusOK, id)
}

// handleDeleteFlowExporter deletes an exporter's configuration (its
// interfaces, points and templates cascade; its flow history is purged by
// the next retention sweep). 204.
func (s *Server) handleDeleteFlowExporter(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid exporter id")
		return
	}
	switch err := queries.DeleteFlowExporter(s.db, id); {
	case errors.Is(err, queries.ErrFlowNotFound):
		writeError(w, http.StatusNotFound, "exporter not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to delete exporter")
		return
	}
	s.flowChanged()
	w.WriteHeader(http.StatusNoContent)
}

// ---------- exporter interfaces ----------

type flowIfaceJSON struct {
	IfIndex   uint32     `json:"if_index"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	VLANID    int        `json:"vlan_id"`
	Parent    string     `json:"parent"`
	Role      string     `json:"role"`
	Source    string     `json:"source"`
	Hint      string     `json:"hint"`
	FirstSeen *time.Time `json:"first_seen"`
	LastSeen  *time.Time `json:"last_seen"`
}

func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return timePtr(t)
}

func flowIfaceView(f queries.FlowIface) flowIfaceJSON {
	return flowIfaceJSON{IfIndex: f.IfIndex, Name: f.Name, Type: f.Type, VLANID: f.VLANID, Parent: f.Parent,
		Role: f.Role, Source: f.Source, Hint: f.Hint, FirstSeen: optTime(f.FirstSeen), LastSeen: optTime(f.LastSeen)}
}

// exporterParam loads the {id} exporter, writing 400 / 404 / 500 itself.
func (s *Server) exporterParam(w http.ResponseWriter, r *http.Request) (*queries.FlowExporter, bool) {
	id, ok := idParam(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid exporter id")
		return nil, false
	}
	e, err := queries.GetFlowExporter(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get exporter")
		return nil, false
	}
	if e == nil {
		writeError(w, http.StatusNotFound, "exporter not found")
		return nil, false
	}
	return e, true
}

// handleListFlowIfaces lists an exporter's ifIndex → name rows.
func (s *Server) handleListFlowIfaces(w http.ResponseWriter, r *http.Request) {
	e, ok := s.exporterParam(w, r)
	if !ok {
		return
	}
	ifs, err := queries.ListFlowIfaces(s.db, e.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list interfaces")
		return
	}
	out := make([]flowIfaceJSON, 0, len(ifs))
	for _, f := range ifs {
		out = append(out, flowIfaceView(f))
	}
	writeJSON(w, http.StatusOK, map[string]any{"exporter_id": e.ID, "interfaces": out})
}

// handleUpdateFlowIface names an exporter interface and sets its role
// (admin); an empty name and role hand it back to the device sync.
func (s *Server) handleUpdateFlowIface(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.ParseUint(chi.URLParam(r, "ifIndex"), 10, 32)
	if err != nil || idx == 0 {
		writeError(w, http.StatusBadRequest, "invalid ifIndex")
		return
	}
	var b struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch b.Role {
	case "", "lan", "wan", "vpn", "other":
	default:
		writeError(w, http.StatusBadRequest, "invalid role")
		return
	}
	name := strings.TrimSpace(b.Name)
	if utf8.RuneCountInString(name) > maxFlowNameLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("name is too long (max %d characters)", maxFlowNameLen))
		return
	}
	e, ok := s.exporterParam(w, r)
	if !ok {
		return
	}
	if err := queries.SetFlowIfaceManual(s.db, e.ID, uint32(idx), name, b.Role); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update interface")
		return
	}
	s.flowChanged()
	ifs, err := queries.ListFlowIfaces(s.db, e.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list interfaces")
		return
	}
	for _, f := range ifs {
		if f.IfIndex == uint32(idx) {
			writeJSON(w, http.StatusOK, flowIfaceView(f))
			return
		}
	}
	writeJSON(w, http.StatusOK, flowIfaceView(queries.FlowIface{ExporterID: e.ID, IfIndex: uint32(idx),
		Name: name, Role: b.Role, Source: "learned"}))
}

// ---------- points ----------

// flowPointBody is the POST / PUT body of /flows/points and the `point` of a
// suggestion.
type flowPointBody struct {
	ExporterID    int64             `json:"exporter_id"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	IfIndexes     []int64           `json:"if_indexes"`
	Facing        string            `json:"facing"`
	PortSide      string            `json:"port_side"`
	DeviceID      *string           `json:"device_id"`
	Iface         string            `json:"iface"`
	LocalInternal optional[bool]    `json:"local_internal"`
	ExcludeAttach []queries.PortRef `json:"exclude_attach"`
	Note          string            `json:"note"`
	Enabled       optional[bool]    `json:"enabled"`
}

// flowPointBodyJSON renders a point as a POST body (suggestions).
type flowPointBodyJSON struct {
	ExporterID    int64             `json:"exporter_id"`
	Name          string            `json:"name"`
	Kind          string            `json:"kind"`
	IfIndexes     []uint32          `json:"if_indexes"`
	Facing        string            `json:"facing"`
	PortSide      string            `json:"port_side"`
	DeviceID      *string           `json:"device_id"`
	Iface         string            `json:"iface"`
	LocalInternal bool              `json:"local_internal"`
	ExcludeAttach []queries.PortRef `json:"exclude_attach"`
	Note          string            `json:"note"`
	Enabled       bool              `json:"enabled"`
}

func pointBodyJSON(p queries.FlowPoint) *flowPointBodyJSON {
	excl := p.ExcludeAttach
	if excl == nil {
		excl = []queries.PortRef{}
	}
	return &flowPointBodyJSON{ExporterID: p.ExporterID, Name: p.Name, Kind: p.Kind, IfIndexes: p.IfIndexes,
		Facing: p.Facing, PortSide: p.PortSide, DeviceID: strPtr(p.DeviceID), Iface: p.Iface,
		LocalInternal: p.LocalInternal, ExcludeAttach: excl, Note: p.Note, Enabled: p.Enabled}
}

// maxFlowNoteLen bounds a point's note.
const maxFlowNoteLen = 1000

// applyPointBody validates b onto p (create: defaults for omitted optional
// fields; update: omitted optional fields keep p's values). It returns a 400
// message, or "".
func (s *Server) applyPointBody(b flowPointBody, p *queries.FlowPoint, create bool) (string, error) {
	e, err := queries.GetFlowExporter(s.db, b.ExporterID)
	if err != nil {
		return "", err
	}
	if e == nil {
		return "unknown exporter", nil
	}
	p.ExporterID = e.ID
	name, msg := validName(b.Name, 2*maxFlowNameLen)
	if msg != "" {
		return msg, nil
	}
	p.Name = name
	switch b.Kind {
	case "native", "derived":
		p.Kind = b.Kind
	default:
		return "invalid kind", nil
	}
	if len(b.IfIndexes) == 0 {
		return "if_indexes must be non-empty", nil
	}
	ifs := make([]uint32, 0, len(b.IfIndexes))
	for _, v := range b.IfIndexes {
		if v <= 0 || v > 4294967295 {
			return "if_indexes must be non-empty", nil
		}
		ifs = append(ifs, uint32(v))
	}
	slices.Sort(ifs)
	p.IfIndexes = slices.Compact(ifs)
	if len(p.IfIndexes) > queries.MaxFlowIfs {
		return fmt.Sprintf("too many if_indexes (max %d)", queries.MaxFlowIfs), nil
	}
	switch b.Facing {
	case flowview.FacingUp, flowview.FacingDown:
		p.Facing = b.Facing
	default:
		return "invalid facing", nil
	}
	p.PortSide, p.DeviceID, p.Iface = "", "", ""
	switch b.PortSide {
	case "":
	case "same", "peer":
		dev := ""
		if b.DeviceID != nil {
			dev = strings.TrimSpace(*b.DeviceID)
		}
		if dev == "" {
			return "unknown device", nil
		}
		exists, err := s.deviceExists(dev)
		if err != nil {
			return "", err
		}
		if !exists {
			return "unknown device", nil
		}
		iface := strings.TrimSpace(b.Iface)
		if iface == "" {
			return "iface is required", nil
		}
		p.PortSide, p.DeviceID, p.Iface = b.PortSide, dev, iface
	default:
		return "invalid port_side", nil
	}
	excl := make([]queries.PortRef, 0, len(b.ExcludeAttach))
	for _, ref := range b.ExcludeAttach {
		ref.DeviceID, ref.Iface = strings.TrimSpace(ref.DeviceID), strings.TrimSpace(ref.Iface)
		if ref.DeviceID == "" || ref.Iface == "" {
			return "invalid exclude_attach", nil
		}
		exists, err := s.deviceExists(ref.DeviceID)
		if err != nil {
			return "", err
		}
		if !exists {
			return "unknown device in exclude_attach", nil
		}
		if !slices.Contains(excl, ref) {
			excl = append(excl, ref)
		}
	}
	p.ExcludeAttach = excl
	note := strings.TrimSpace(b.Note)
	if utf8.RuneCountInString(note) > maxFlowNoteLen {
		return fmt.Sprintf("note is too long (max %d characters)", maxFlowNoteLen), nil
	}
	p.Note = note
	switch {
	case b.LocalInternal.Set:
		p.LocalInternal = b.LocalInternal.Value
	case create:
		p.LocalInternal = e.Kind == "opnsense"
	}
	switch {
	case b.Enabled.Set:
		p.Enabled = b.Enabled.Value
	case create:
		p.Enabled = true
	}
	return "", nil
}

// writePoint answers a point create / update with its FlowPointView.
func (s *Server) writePoint(w http.ResponseWriter, status int, id int64) {
	p, err := queries.GetFlowPoint(s.db, id)
	if err != nil || p == nil {
		writeError(w, http.StatusInternalServerError, "failed to read point")
		return
	}
	env, err := s.loadFlowEnv(time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read point")
		return
	}
	writeJSON(w, status, env.pointView(*p))
}

// handleListFlowPoints lists every point (disabled included) ordered by
// exporter name, derived before native, name.
func (s *Server) handleListFlowPoints(w http.ResponseWriter, r *http.Request) {
	env, err := s.loadFlowEnv(time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load flow configuration")
		return
	}
	points, err := queries.ListFlowPoints(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list points")
		return
	}
	views := make([]flowPointView, 0, len(points))
	for _, p := range points {
		views = append(views, env.pointView(p))
	}
	slices.SortStableFunc(views, func(a, b flowPointView) int {
		return cmp.Or(cmp.Compare(a.ExporterName, b.ExporterName), cmp.Compare(a.ExporterID, b.ExporterID),
			cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	writeJSON(w, http.StatusOK, map[string]any{"points": views})
}

// handleCreateFlowPoint adds a point (admin). 201 FlowPointView.
func (s *Server) handleCreateFlowPoint(w http.ResponseWriter, r *http.Request) {
	var b flowPointBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var p queries.FlowPoint
	msg, err := s.applyPointBody(b, &p, true)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to validate point")
		return
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	id, err := queries.CreateFlowPoint(s.db, &p)
	if errors.Is(err, queries.ErrFlowPointExists) {
		writeError(w, http.StatusConflict, "a view for this port already exists")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create point")
		return
	}
	s.flowChanged()
	s.writePoint(w, http.StatusCreated, id)
}

// handleUpdateFlowPoint replaces a point's configuration (admin); the point
// is no longer auto-maintained afterwards.
func (s *Server) handleUpdateFlowPoint(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid point id")
		return
	}
	var b flowPointBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cur, err := queries.GetFlowPoint(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get point")
		return
	}
	if cur == nil {
		writeError(w, http.StatusNotFound, "point not found")
		return
	}
	msg, err := s.applyPointBody(b, cur, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to validate point")
		return
	}
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	switch err := queries.UpdateFlowPoint(s.db, cur); {
	case errors.Is(err, queries.ErrFlowPointExists):
		writeError(w, http.StatusConflict, "a view for this port already exists")
		return
	case errors.Is(err, queries.ErrFlowNotFound):
		writeError(w, http.StatusNotFound, "point not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to update point")
		return
	}
	s.flowChanged()
	s.writePoint(w, http.StatusOK, id)
}

// handleDeleteFlowPoint deletes a point (admin). 204.
func (s *Server) handleDeleteFlowPoint(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r, "id")
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid point id")
		return
	}
	switch err := queries.DeleteFlowPoint(s.db, id); {
	case errors.Is(err, queries.ErrFlowNotFound):
		writeError(w, http.StatusNotFound, "point not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "failed to delete point")
		return
	}
	s.flowChanged()
	w.WriteHeader(http.StatusNoContent)
}

// ---------- suggestions ----------

type flowSuggestionJSON struct {
	Rule   string             `json:"rule"`
	Reason string             `json:"reason"`
	Exists bool               `json:"exists"`
	Point  *flowPointBodyJSON `json:"point"`
}

// handleFlowPointSuggestions returns the derived views worth adding (rule G
// gateway-host peers, rule T router trunks; contract §6.5).
func (s *Server) handleFlowPointSuggestions(w http.ResponseWriter, r *http.Request) {
	fail := func() { writeError(w, http.StatusInternalServerError, "failed to build suggestions") }
	t, err := s.trafficTopology()
	if err != nil {
		fail()
		return
	}
	in := flowview.SuggestInput{MACByIP: map[netip.Addr]string{}, IfaceTypes: map[string]string{},
		Adjacent: adjacencies(t.graph)}
	if in.Exporters, err = queries.ListFlowExporters(s.db); err != nil {
		fail()
		return
	}
	if in.Ifaces, err = queries.ListFlowIfaces(s.db, 0); err != nil {
		fail()
		return
	}
	if in.Points, err = queries.ListFlowPoints(s.db); err != nil {
		fail()
		return
	}
	if in.Uplinks, err = queries.ListDeviceUplinks(s.db, 10*time.Minute); err != nil {
		fail()
		return
	}
	if in.VLANs, err = queries.ListBridgeVLANs(s.db); err != nil {
		fail()
		return
	}
	if in.Devices, err = queries.ListDevices(s.db); err != nil {
		fail()
		return
	}
	for _, hs := range t.hosts {
		in.Hosts = append(in.Hosts, hs...)
	}
	for _, m := range t.macs {
		if a, ok := parseIP(m.IPAddress); ok {
			if cur, dup := in.MACByIP[a]; !dup || strings.ToUpper(m.MACAddress) < cur {
				in.MACByIP[a] = strings.ToUpper(m.MACAddress)
			}
		}
	}
	for k, typ := range t.types {
		in.IfaceTypes[k.dev+"\x00"+k.iface] = typ
	}
	out := []flowSuggestionJSON{}
	for _, sg := range flowview.Suggest(in) {
		j := flowSuggestionJSON{Rule: sg.Rule, Reason: sg.Reason, Exists: sg.Exists}
		if sg.Point != nil {
			j.Point = pointBodyJSON(*sg.Point)
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": out})
}
