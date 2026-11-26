package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/flow"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

// guardFixture is a small fleet with every role, on documentation addresses.
type guardFixture struct {
	*flowFixture
	ids []string
}

// Devices of the guard fixture: id, address, board, probed class, RouterOS
// version and the default route that decides the role.
var guardDevices = []struct {
	id, addr, board string
	class           devclass.Class
	version, route  string
}{
	{"edge", "192.0.2.1", "RB5009UG+S+", devclass.C3, "7.23.1 (stable)", "egress"},       // router
	{"small", "192.0.2.2", "RB4011iGS+", devclass.C2, "7.25 (stable)", "egress"},         // weak-router
	{"tiny", "192.0.2.3", "hEX S", devclass.C1, "7.23.1 (stable)", "egress"},             // weak-router on C1: never
	{"sw", "192.0.2.10", "CRS326-24G-2S+", devclass.C2, "7.23.1 (stable)", "gateway"},    // switch
	{"sw725", "192.0.2.11", "CRS328-24P-4S+", devclass.C2, "7.25.1 (stable)", "gateway"}, // switch on fixed RouterOS
	{"ap", "192.0.2.20", "cAP ax", devclass.C3, "7.23.1 (stable)", "gateway"},            // ap-cpe
	{"inner", "192.0.2.30", "RB5009UG+S+", devclass.C3, "7.23.1 (stable)", "gateway"},    // unknown: no egress of its own
	{"new", "192.0.2.40", "", "", "", ""},                                                // unknown: nothing read yet
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	db := newTestDB(t)
	g := &guardFixture{flowFixture: &flowFixture{db: db}}
	for _, d := range guardDevices {
		if err := queries.CreateDevice(db, &queries.Device{ID: d.id, Identity: d.id, Address: d.addr, Username: "nms",
			APIPort: 8728, Status: "online"}); err != nil {
			t.Fatal(err)
		}
		g.ids = append(g.ids, d.id)
		if d.board != "" {
			if err := queries.UpdateDeviceInfo(db, d.id, "MikroTik", d.board, d.version, "", "arm"); err != nil {
				t.Fatal(err)
			}
		}
		if d.class != "" {
			if err := queries.SetDeviceClass(db, d.id, d.class, devclass.ResourceFacts{}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		var ups []queries.DeviceUplink
		switch d.route {
		case "egress":
			ups = []queries.DeviceUplink{
				{DeviceID: d.id, Kind: "default-route", Interface: "ether1", IfaceType: "ether", GatewayIP: "203.0.113.1"},
				{DeviceID: d.id, Kind: "vpn", Interface: "wg0", IfaceType: "wg"}}
		case "gateway":
			ups = []queries.DeviceUplink{
				{DeviceID: d.id, Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.168.1.1"}}
		}
		if err := queries.ReplaceDeviceUplinks(db, d.id, ups); err != nil {
			t.Fatal(err)
		}
	}
	// The address plan knows the edge router's routed interfaces.
	now := time.Now()
	if err := queries.ReplaceFlowPrefixes(db, []queries.FlowPrefix{
		{Prefix: netip.MustParsePrefix("203.0.113.0/24"), Class: "transit", DeviceID: "edge", Iface: "ether1", UpdatedAt: now},
		{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Class: "internal", DeviceID: "edge", Iface: "bridge", UpdatedAt: now},
		{Prefix: netip.MustParsePrefix("198.51.100.0/25"), Class: "internal", DeviceID: "edge", Iface: "vlan20", UpdatedAt: now},
		{Prefix: netip.MustParsePrefix("198.51.100.1/32"), Class: "internal", DeviceID: "edge", Iface: "vlan20", UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetSetting(db, "flow_advertise_address", "198.51.100.5"); err != nil {
		t.Fatal(err)
	}
	g.fake = flowFakeFor(time.Now().UTC().Truncate(time.Minute))
	g.s = &Server{db: db, flows: g.fake}
	return g
}

func (g *guardFixture) setup(t *testing.T, device string) flowSetupResponse {
	t.Helper()
	var s flowSetupResponse
	params := map[string]string{}
	if device != "" {
		params["device"] = device
	}
	if code, body := g.get(t, "/flows/setup", params, &s); code != http.StatusOK {
		t.Fatalf("setup %q: %d %s", device, code, body)
	}
	return s
}

// allCommands is every command a setup response would have an operator run.
func allCommands(s flowSetupResponse) []string {
	out := append(slices.Clone(s.RouterOS.Check), s.RouterOS.Apply...)
	for _, step := range s.RouterOS.Rollback.Steps {
		out = append(out, step.Commands...)
	}
	return out
}

// G1: every device carries its role and the reason, in the list and alone.
func TestDevicesExposeRoleAndReason(t *testing.T) {
	g := newGuardFixture(t)
	r := chi.NewRouter()
	r.Get("/devices", g.s.handleListDevices)
	r.Get("/devices/{id}", g.s.handleGetDevice)
	var list []struct {
		ID                 string `json:"id"`
		Role               string `json:"role"`
		RoleReason         string `json:"role_reason"`
		FlowExportEligible bool   `json:"flow_export_eligible"`
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/devices", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /devices = %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		role     string
		eligible bool
	}{
		"edge": {"router", true}, "small": {"weak-router", true}, "tiny": {"weak-router", false},
		"sw": {"switch", false}, "sw725": {"switch", false}, "ap": {"ap-cpe", false},
		"inner": {"unknown", false}, "new": {"unknown", false},
	}
	if len(list) != len(want) {
		t.Fatalf("devices = %+v", list)
	}
	for _, d := range list {
		w := want[d.ID]
		if d.Role != w.role || d.FlowExportEligible != w.eligible || d.RoleReason == "" {
			t.Errorf("%s: role %q eligible %v reason %q, want %q %v", d.ID, d.Role, d.FlowExportEligible, d.RoleReason, w.role, w.eligible)
		}
	}
	var one struct {
		Role       string `json:"role"`
		RoleReason string `json:"role_reason"`
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/devices/sw", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil || one.Role != "switch" || !strings.Contains(one.RoleReason, "CRS326-24G-2S+") {
		t.Fatalf("GET /devices/sw: %+v %v", one, err)
	}
}

// G2: the commands that enable export appear only for an eligible router —
// with a warning for a weak one — and everything else gets the read-only
// check and the rollback only.
func TestFlowSetupIsGatedByRoleAndClass(t *testing.T) {
	g := newGuardFixture(t)

	// A strong router: the apply commands, no warning, its routed interfaces.
	s := g.setup(t, "edge")
	if s.Class != "C3" || s.Role != "router" || !s.Eligible || len(s.Warnings) != 0 || s.RoleReason == "" {
		t.Fatalf("edge: class %q role %q eligible %v warnings %v", s.Class, s.Role, s.Eligible, s.Warnings)
	}
	wantApply := []string{
		"/ip traffic-flow target add dst-address=198.51.100.5 port=2055 src-address=192.0.2.1 version=ipfix v9-template-timeout=1m",
		"/ip traffic-flow set enabled=yes interfaces=ether1,wg0,bridge,vlan20 active-flow-timeout=1m inactive-flow-timeout=15s",
	}
	if !slices.Equal(s.RouterOS.Apply, wantApply) || s.RouterOS.InterfacesNote == "" {
		t.Fatalf("edge apply = %q\nwant         %q", s.RouterOS.Apply, wantApply)
	}
	if len(s.Advice) != 1 || !strings.Contains(s.Advice[0], "Do not enable Tools → Graphing on switches") ||
		!strings.Contains(s.Advice[0], "1 min for 2 d, 1 h for 365 d") {
		t.Fatalf("advice = %q", s.Advice)
	}

	// A weak router (C2): the same commands, with a warning.
	s = g.setup(t, "small")
	if s.Class != "C2" || s.Role != "weak-router" || !s.Eligible || len(s.RouterOS.Apply) != 2 ||
		len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "small device") {
		t.Fatalf("small: %+v", s)
	}
	// It has no address-plan rows: only the egress interfaces are known.
	if !strings.Contains(s.RouterOS.Apply[1], "interfaces=ether1,wg0 ") {
		t.Fatalf("small apply = %q", s.RouterOS.Apply)
	}

	// Everything else: no apply block, the check, the rollback, the reason.
	for id, wantRole := range map[string]string{"tiny": "weak-router", "sw": "switch", "sw725": "switch", "ap": "ap-cpe",
		"inner": "unknown", "new": "unknown"} {
		s := g.setup(t, id)
		if s.Eligible || len(s.RouterOS.Apply) != 0 || s.RouterOS.Apply == nil || s.RouterOS.InterfacesNote != "" {
			t.Errorf("%s: eligible %v apply %q", id, s.Eligible, s.RouterOS.Apply)
		}
		if s.Role != wantRole || len(s.Warnings) == 0 || s.RouterOS.DeviceID == nil || *s.RouterOS.DeviceID != id {
			t.Errorf("%s: role %q warnings %v", id, s.Role, s.Warnings)
		}
		if !slices.Equal(s.RouterOS.Check, []string{"/ip traffic-flow print", "/ip traffic-flow target print"}) ||
			len(s.RouterOS.Rollback.Steps) != 5 {
			t.Errorf("%s: check %q rollback %+v", id, s.RouterOS.Check, s.RouterOS.Rollback)
		}
	}
	if s := g.setup(t, "tiny"); !strings.Contains(s.Warnings[0], "C1") || s.Class != "C1" {
		t.Errorf("tiny: %+v", s.Warnings)
	}

	// Never interfaces=all, never a [find] wider than the one target.
	for _, id := range append(slices.Clone(g.ids), "") {
		for _, c := range allCommands(g.setup(t, id)) {
			if strings.Contains(c, "interfaces=all") || strings.Contains(c, "[find]") {
				t.Errorf("%q: %q", id, c)
			}
			if strings.Contains(c, "[find") && !strings.Contains(c, "[find dst-address=198.51.100.5 port=2055]") {
				t.Errorf("%q: broad find in %q", id, c)
			}
		}
	}
}

// The rollback follows the device's stored RouterOS version: before 7.25 the
// risky step warns and downloads first; from 7.25 neither. Its order never
// changes.
func TestFlowSetupRollbackIsVersionAware(t *testing.T) {
	g := newGuardFixture(t)
	old := g.setup(t, "sw").RouterOS.Rollback
	if old.Fixed || !strings.Contains(old.Note, "7.23.1") {
		t.Fatalf("7.23.1 rollback = %+v", old)
	}
	lines := strings.Join(old.Lines(), "\n")
	for _, want := range []string{"WARNING", "/system package update download", "maintenance window"} {
		if !strings.Contains(lines, want) {
			t.Errorf("pre-7.25 rollback lacks %q:\n%s", want, lines)
		}
	}
	fixed := g.setup(t, "sw725").RouterOS.Rollback
	lines = strings.Join(fixed.Lines(), "\n")
	if !fixed.Fixed || strings.Contains(lines, "WARNING") || strings.Contains(lines, "package update download") {
		t.Fatalf("7.25.1 rollback:\n%s", lines)
	}
	for name, rb := range map[string][]string{"pre-7.25": old.Lines(), "7.25": fixed.Lines()} {
		text := strings.Join(rb, "\n")
		profile, disable, off, remove := strings.Index(text, "/tool profile duration=30"), strings.Index(text, "target disable"),
			strings.Index(text, "enabled=no"), strings.Index(text, "target remove")
		if profile < 0 || profile > disable || disable > off || off > remove {
			t.Errorf("%s rollback order (profile %d, disable %d, enabled=no %d, remove %d):\n%s", name, profile, disable, off, remove, text)
		}
	}
	// A device whose version is not known yet gets the careful text.
	if rb := g.setup(t, "new").RouterOS.Rollback; rb.Fixed || !strings.Contains(rb.Note, "unknown") {
		t.Errorf("unknown version: %+v", rb)
	}

	// A device that was seen exporting told the NMS its target: the
	// rollback removes that one.
	if err := queries.SetFlowExportCheckResult(g.db, "sw", time.Now(), queries.FlowExportExporting, "enabled",
		[]netip.AddrPort{netip.MustParseAddrPort("198.51.100.5:9995")}); err != nil {
		t.Fatal(err)
	}
	if text := strings.Join(g.setup(t, "sw").RouterOS.Rollback.Lines(), "\n"); !strings.Contains(text, "[find dst-address=198.51.100.5 port=9995]") {
		t.Errorf("rollback does not name the confirmed target:\n%s", text)
	}
}

// The collector address is the admin-approved setting — and one that is a
// managed device's own address is neither used nor accepted when saved.
func TestAdvertiseAddressIsAdminApprovedOnly(t *testing.T) {
	g := newGuardFixture(t)
	// The stored value became a device's address (the device was added
	// later): the hints refuse it and say why.
	if err := queries.SetSetting(g.db, "flow_advertise_address", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	s := g.setup(t, "edge")
	if s.AdvertiseAddress != "" || s.AdvertiseSource != "unknown" ||
		!strings.Contains(strings.Join(s.Warnings, "\n"), "address of the managed device sw") {
		t.Fatalf("a device's address was advertised: %+v", s)
	}
	if !strings.Contains(s.RouterOS.Apply[0], "dst-address=<NMS-IP> ") {
		t.Fatalf("apply = %q", s.RouterOS.Apply)
	}

	h := chi.NewRouter()
	h.Put("/settings", g.s.handleUpdateSettings)
	put := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		withUser("admin", h).ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/settings", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	stored := func(key string) string {
		v, _ := queries.GetSetting(g.db, key)
		return v
	}
	// Saving a device's address is refused — and nothing else in the same
	// request is written.
	code, body := put(`{"flow_advertise_address":"192.0.2.20","flow_top_n":"77"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "managed device") {
		t.Fatalf("device address: %d %s", code, body)
	}
	if stored("flow_advertise_address") != "192.0.2.10" || stored("flow_top_n") == "77" {
		t.Fatalf("a refused save wrote something: %q %q", stored("flow_advertise_address"), stored("flow_top_n"))
	}
	if code, body := put(`{"flow_advertise_address":"nms.example"}`); code != http.StatusBadRequest || !strings.Contains(body, "IP address") {
		t.Fatalf("not an address: %d %s", code, body)
	}
	if code, body := put(`{"flow_advertise_address":" 198.51.100.5 "}`); code != http.StatusOK || stored("flow_advertise_address") != "198.51.100.5" {
		t.Fatalf("valid address: %d %s (stored %q)", code, body, stored("flow_advertise_address"))
	}
	if code, _ := put(`{"flow_advertise_address":""}`); code != http.StatusOK || stored("flow_advertise_address") != "" {
		t.Fatalf("clearing the address: %d (stored %q)", code, stored("flow_advertise_address"))
	}

	// Auto-accept: off or "router", nothing else; the old key is not written.
	if code, body := put(`{"flow_auto_accept_classes":"all"}`); code != http.StatusBadRequest {
		t.Fatalf("flow_auto_accept_classes=all: %d %s", code, body)
	}
	if code, _ := put(`{"flow_auto_accept_classes":"Router"}`); code != http.StatusOK || stored("flow_auto_accept_classes") != "router" {
		t.Fatalf("flow_auto_accept_classes=Router: %d (stored %q)", code, stored("flow_auto_accept_classes"))
	}
	if code, _ := put(`{"flow_auto_accept_devices":"true"}`); code != http.StatusOK || stored("flow_auto_accept_devices") != "false" {
		t.Fatalf("the old auto-accept key was written: %d (stored %q)", code, stored("flow_auto_accept_devices"))
	}
}

// G3 through the API: deleting an exporter blocks its address; the status
// shows it as blocked with the reason instead of offering it; adding it
// again needs the explicit flag.
func TestDeletedExporterIsBlockedUntilAnAdminSaysOtherwise(t *testing.T) {
	g := newGuardFixture(t)
	var created flow.ExporterStatus
	body := map[string]any{"name": "firewall", "address": "198.51.100.9", "kind": "opnsense"}
	if code, raw := g.do(t, http.MethodPost, "/flows/exporters", body, &created); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, raw)
	}
	if code, _ := g.do(t, http.MethodDelete, "/flows/exporters/"+itoa(created.ID), nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	// The collector still lists it among the unknown senders it saw: the
	// API shows it as blocked and suggests nothing for it.
	g.fake.status.Unknown = []flow.UnknownSender{{Address: "198.51.100.9", Protocol: "netflow9", Datagrams: 4}}
	var st flowStatusResponse
	if code, raw := g.get(t, "/flows/status", nil, &st); code != http.StatusOK {
		t.Fatalf("status: %d %s", code, raw)
	}
	if len(st.Blocked) != 1 || st.Blocked[0].Address != "198.51.100.9" || st.Blocked[0].Source != queries.FlowBlockDeleted ||
		st.Blocked[0].Name != "firewall" || !strings.HasPrefix(st.Blocked[0].Reason, "exporter deleted by admin on ") ||
		st.Blocked[0].CreatedBy != "admin" || st.Blocked[0].CreatedAt == nil {
		t.Fatalf("blocked senders = %+v", st.Blocked)
	}
	if len(st.SuggestedExporters) != 0 {
		t.Fatalf("a blocked sender is suggested: %+v", st.SuggestedExporters)
	}

	// Adding it again: refused with the reason, unless the admin says so.
	code, raw := g.do(t, http.MethodPost, "/flows/exporters", body, nil)
	var refusal struct{ Error, Code string }
	_ = json.Unmarshal([]byte(raw), &refusal)
	if code != http.StatusConflict || refusal.Code != "sender_blocked" || !strings.Contains(refusal.Error, "exporter deleted by admin") {
		t.Fatalf("re-add: %d %s", code, raw)
	}
	// Moving another exporter onto the blocked address is refused too.
	var other flow.ExporterStatus
	g.do(t, http.MethodPost, "/flows/exporters", map[string]any{"name": "probe", "address": "198.51.100.10", "kind": "other"}, &other)
	if code, raw := g.do(t, http.MethodPut, "/flows/exporters/"+itoa(other.ID), map[string]any{"name": "probe",
		"address": "198.51.100.9", "kind": "other", "enabled": true, "sampling_override": 0}, nil); code != http.StatusConflict ||
		!strings.Contains(raw, "sender_blocked") {
		t.Fatalf("move onto a blocked address: %d %s", code, raw)
	}
	body["unblock"] = true
	if code, raw := g.do(t, http.MethodPost, "/flows/exporters", body, &created); code != http.StatusCreated {
		t.Fatalf("re-add with unblock: %d %s", code, raw)
	}
	if list, _ := queries.ListFlowSenderPolicies(g.db); len(list) != 0 {
		t.Fatalf("block list after the re-add: %+v", list)
	}

	// Unblocking without adding.
	g.do(t, http.MethodDelete, "/flows/exporters/"+itoa(created.ID), nil, nil)
	before := g.fake.reloads.Load()
	if code, raw := g.do(t, http.MethodDelete, "/flows/blocked?address=198.51.100.9", nil, nil); code != http.StatusNoContent {
		t.Fatalf("unblock: %d %s", code, raw)
	}
	if g.fake.reloads.Load() != before+1 {
		t.Error("unblocking did not reload the collector")
	}
	if code, raw := g.do(t, http.MethodDelete, "/flows/blocked?address=198.51.100.9", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unblock twice: %d %s", code, raw)
	}
	if code, _ := g.do(t, http.MethodDelete, "/flows/blocked?address=nope", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("unblock junk: %d", code)
	}
	// The actions are in the audit log.
	entries, _ := queries.ListAuditLog(g.db, "", 20)
	actions := []string{}
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	for _, want := range []string{"flow_exporter_deleted", "flow_exporter_added_blocked", "flow_sender_unblocked"} {
		if !slices.Contains(actions, want) {
			t.Errorf("audit log lacks %s: %v", want, actions)
		}
	}
}

// A managed switch is not one click away from being an exporter: it is not
// suggested, and adding its address needs the same explicit flag. The status
// carries each sender's role, and flags an exporter that sits on a device
// which must not export.
func TestFlowStatusCarriesRolesAndGuards(t *testing.T) {
	g := newGuardFixture(t)
	swID, edgeID, innerID := "sw", "edge", "inner"
	g.fake.status.Unknown = []flow.UnknownSender{
		{Address: "192.0.2.10", Protocol: "ipfix", DeviceID: &swID, DeviceName: "sw"},
		{Address: "192.0.2.1", Protocol: "ipfix", DeviceID: &edgeID, DeviceName: "edge"},
		{Address: "192.0.2.30", Protocol: "ipfix", DeviceID: &innerID, DeviceName: "inner"},
	}
	// An exporter accepted earlier on the access point.
	apExp, err := queries.CreateFlowExporter(g.db, &queries.FlowExporter{Name: "ap", Address: "192.0.2.20", Kind: "routeros",
		DeviceID: "ap", Enabled: true, Auto: true})
	if err != nil {
		t.Fatal(err)
	}
	var st flowStatusResponse
	if code, raw := g.get(t, "/flows/status", nil, &st); code != http.StatusOK {
		t.Fatalf("status: %d %s", code, raw)
	}
	if len(st.Exporters) != 1 || st.Exporters[0].ID != apExp || st.Exporters[0].DeviceRole != "ap-cpe" ||
		!strings.Contains(st.Exporters[0].ExportWarning, "access point") || !st.Exporters[0].Enabled {
		t.Fatalf("exporter on the access point = %+v", st.Exporters)
	}
	roles := map[string]string{}
	for _, u := range st.Unknown {
		roles[u.Address] = u.DeviceRole
		if u.DeviceRoleReason == "" {
			t.Errorf("unknown sender %s has no role reason", u.Address)
		}
	}
	if roles["192.0.2.10"] != "switch" || roles["192.0.2.1"] != "router" || roles["192.0.2.30"] != "unknown" {
		t.Fatalf("unknown sender roles = %v", roles)
	}
	suggested := []string{}
	for _, sg := range st.SuggestedExporters {
		suggested = append(suggested, sg.Address)
	}
	slices.Sort(suggested)
	if !slices.Equal(suggested, []string{"192.0.2.1", "192.0.2.30"}) {
		t.Fatalf("suggested = %v, want the router and the device of unknown role, not the switch", suggested)
	}
	if st.AutoAcceptClasses != "" {
		t.Fatalf("auto_accept_classes = %q", st.AutoAcceptClasses)
	}

	// Adding the switch by hand: refused with its reason, accepted with the flag.
	body := map[string]any{"name": "sw", "address": "192.0.2.10", "kind": "routeros", "device_id": "sw"}
	if code, raw := g.do(t, http.MethodPost, "/flows/exporters", body, nil); code != http.StatusConflict ||
		!strings.Contains(raw, "sender_blocked") || !strings.Contains(raw, "switch") {
		t.Fatalf("adding a switch: %d %s", code, raw)
	}
	body["unblock"] = true
	if code, raw := g.do(t, http.MethodPost, "/flows/exporters", body, nil); code != http.StatusCreated {
		t.Fatalf("adding a switch with unblock: %d %s", code, raw)
	}
	// A router needs no flag.
	if code, raw := g.do(t, http.MethodPost, "/flows/exporters", map[string]any{"name": "edge", "address": "192.0.2.1",
		"kind": "routeros", "device_id": "edge"}, nil); code != http.StatusCreated {
		t.Fatalf("adding a router: %d %s", code, raw)
	}

	// The collector's own block list entries keep their live counters.
	if _, err := queries.BlockFlowSender(g.db, &queries.FlowSenderPolicy{Address: "192.0.2.11", Reason: "a switch",
		Source: queries.FlowBlockAuto, Name: "sw725", DeviceID: "sw725", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	seen := time.Now().UTC().Truncate(time.Second)
	g.fake.status.Blocked = []flow.BlockedSender{{Address: "192.0.2.11", Datagrams: 42, LastSeen: &seen}}
	st = flowStatusResponse{}
	g.get(t, "/flows/status", nil, &st)
	if len(st.Blocked) != 1 || st.Blocked[0].Datagrams != 42 || st.Blocked[0].LastSeen == nil || st.Blocked[0].DeviceRole != "switch" ||
		st.Blocked[0].DeviceName != "sw725" || st.Blocked[0].Reason != "a switch" {
		t.Fatalf("blocked with live counters = %+v", st.Blocked)
	}
}

// G8: rule T is offered only for router-role exporters, and for a port an
// admin's derived view outranks the native point of an exporter that is not
// a router.
func TestDerivedViewOutranksNativePointOfNonRouter(t *testing.T) {
	f := newFlowFixture(t)
	trunkSuggestions := func() int {
		var resp struct {
			Suggestions []flowSuggestionJSON `json:"suggestions"`
		}
		if code, body := f.get(t, "/flows/points/suggestions", nil, &resp); code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
		n := 0
		for _, s := range resp.Suggestions {
			if s.Rule == "trunk" {
				n++
			}
		}
		return n
	}
	chosen := func() (int64, []int64) {
		var r flowPortResponse
		if code, body := f.get(t, "/flows/port", map[string]string{"device": labRB, "iface": "eth1-wan"}, &r); code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
		var alts []int64
		for _, a := range r.Alternatives {
			alts = append(alts, a.ID)
		}
		return r.Point.ID, alts
	}
	// The RB5009 is a router: its trunk views are offered and its native
	// point is the port's first choice.
	if n := trunkSuggestions(); n != 2 {
		t.Fatalf("router exporter: %d trunk suggestions, want 2", n)
	}
	if id, alts := chosen(); id != f.pWAN || !slices.Equal(alts, []int64{f.pWANDerived}) {
		t.Fatalf("router exporter: point %d alternatives %v", id, alts)
	}
	// The same device without an egress of its own is not clearly a router:
	// no trunk views, and the admin's derived view is the first choice.
	if err := queries.ReplaceDeviceUplinks(f.db, labRB, nil); err != nil {
		t.Fatal(err)
	}
	f.s.invalidateTrafficTopo()
	if n := trunkSuggestions(); n != 0 {
		t.Fatalf("non-router exporter: %d trunk suggestions, want 0", n)
	}
	if id, alts := chosen(); id != f.pWANDerived || !slices.Equal(alts, []int64{f.pWAN}) {
		t.Fatalf("non-router exporter: point %d alternatives %v, want the derived view first", id, alts)
	}
}

// The device budget: the setup hints, the status, the suggestions and the
// device list read stored facts only. Every device has a live pooled session
// to a tripwire that records any command.
func TestFlowGuardReadPathsNeverTouchDevices(t *testing.T) {
	routeros.SetDialPolicy(nil)
	g := newGuardFixture(t)
	pool := routeros.NewPool(false)
	fake := routerostest.NewDevice(t)
	for _, id := range g.ids {
		fake.Dial(t, pool, id)
	}
	g.s.pool = pool
	g.s.cfg = &config.Config{Pollers: config.PollersLocal}
	// A blocked sender, a sighting awaiting its confirmation read and an
	// exporter on a switch: nothing of it makes a read path reach a device.
	if _, err := queries.BlockFlowSender(g.db, &queries.FlowSenderPolicy{Address: "192.0.2.10", Reason: "a switch",
		Source: queries.FlowBlockAuto, DeviceID: "sw", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := queries.NoteFlowExportSighting(g.db, "sw", "192.0.2.10", 2055, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateFlowExporter(g.db, &queries.FlowExporter{Name: "sw725", Address: "192.0.2.11", Kind: "routeros",
		DeviceID: "sw725", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertInterface(g.db, &queries.Interface{ID: uuid.NewString(), DeviceID: "edge", Name: "ether1", Type: "ether"}); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	g.s.mountFlowRoutes(r)
	g.s.mountTrafficRoutes(r)
	r.Get("/devices", g.s.handleListDevices)
	r.Get("/devices/{id}", g.s.handleGetDevice)
	paths := []string{"/flows/setup", "/flows/status", "/flows/points", "/flows/points/suggestions", "/devices"}
	for _, id := range g.ids {
		paths = append(paths, "/flows/setup?device="+id, "/devices/"+id)
	}
	for _, role := range []string{"admin", "viewer"} {
		h := withUser(role, r)
		for _, p := range paths {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("%s GET %s = %d %s", role, p, rec.Code, rec.Body)
			}
		}
	}
	if cmds := fake.Commands(); len(cmds) != 0 {
		t.Fatalf("flow guard read paths sent device commands: %v", cmds)
	}
	if n := fake.Conns(); n != len(g.ids) {
		t.Fatalf("connections = %d, want only the %d test dials", n, len(g.ids))
	}
}

// The block list is part of the backup: a restore must not turn a blocked
// sender back into one that is a click away from accepted.
func TestBlockListIsBackedUp(t *testing.T) {
	if !tableAllowed("flow_sender_policy") {
		t.Fatal("flow_sender_policy is not exportable")
	}
	src, dst := newTestDB(t), newTestDB(t)
	if _, err := queries.BlockFlowSender(src, &queries.FlowSenderPolicy{Address: "198.51.100.9", Reason: "exporter deleted",
		Source: queries.FlowBlockDeleted, Name: "firewall", Kind: "opnsense", CreatedBy: "admin", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rows, err := dumpTable(src, "flow_sender_policy")
	if err != nil || len(rows) != 1 {
		t.Fatalf("dump: %d rows, %v", len(rows), err)
	}
	if ins, _, err := importTableRows(dst, "flow_sender_policy", rows); err != nil || ins != 1 {
		t.Fatalf("import: %d, %v", ins, err)
	}
	if p, _ := queries.GetFlowSenderPolicy(dst, "198.51.100.9"); p == nil || p.Reason != "exporter deleted" || p.Name != "firewall" {
		t.Fatalf("restored row = %+v", p)
	}
}
