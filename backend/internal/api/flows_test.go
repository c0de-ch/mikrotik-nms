package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow"
)

func errBody(msg string) string { return `{"error":"` + msg + `"}` + "\n" }

func TestFlowExporterCRUD(t *testing.T) {
	f := newFlowFixture(t)
	reloads := func() int32 { return f.fake.reloads.Load() }

	valid := map[string]any{"name": " probe01 ", "address": "::ffff:10.0.0.9", "kind": "other", "device_id": nil,
		"enabled": true, "sampling_override": 100, "nat_addresses": []string{"10.0.0.10", "10.0.0.10", "2001:db8::1"}}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range valid {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	for _, c := range []struct {
		body any
		msg  string
	}{
		{"not json", "invalid request body"},
		{with("name", "  "), "name is required"},
		{with("name", strings.Repeat("x", 65)), "name is too long (max 64 characters)"},
		{with("address", "bogus"), "invalid address"},
		{with("address", "fe80::1%eth0"), "invalid address"},
		{with("kind", "cisco"), "invalid kind"},
		{with("kind", nil), "invalid kind"},
		{with("device_id", "nope"), "unknown device"},
		{with("sampling_override", 70000), "invalid sampling_override"},
		{with("sampling_override", -1), "invalid sampling_override"},
		{with("nat_addresses", []string{"10.0.0.1", "x"}), "invalid nat address"},
	} {
		if code, body := f.do(t, http.MethodPost, "/flows/exporters", c.body, nil); code != 400 || body != errBody(c.msg) {
			t.Errorf("%v: %d %s", c.body, code, body)
		}
	}
	if reloads() != 0 {
		t.Fatalf("failed writes reloaded the collector %d times", reloads())
	}

	var created flow.ExporterStatus
	code, body := f.do(t, http.MethodPost, "/flows/exporters", valid, &created)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	if created.ID == 0 || created.Name != "probe01" || created.Address != "10.0.0.9" || created.Kind != "other" ||
		created.DeviceID != nil || !created.Enabled || created.Auto || created.SamplingOverride != 100 ||
		!reflect.DeepEqual(created.NATAddresses, []string{"10.0.0.10", "2001:db8::1"}) || created.State != flow.StateNever ||
		created.TemplateState != "n/a" || len(created.Counters) != len(flow.ExporterCounterKeys) || created.LastSeen != nil {
		t.Fatalf("created = %+v", created)
	}
	if reloads() != 1 {
		t.Fatalf("reloads after create = %d", reloads())
	}
	if code, body := f.do(t, http.MethodPost, "/flows/exporters", with("address", "10.0.0.9"), nil); code != 409 ||
		body != errBody("exporter address already exists") {
		t.Fatalf("duplicate: %d %s", code, body)
	}
	// POST defaults: enabled true, override 0.
	var def flow.ExporterStatus
	f.do(t, http.MethodPost, "/flows/exporters", map[string]any{"name": "d", "address": "10.0.0.11", "kind": "routeros",
		"device_id": labSW}, &def)
	if !def.Enabled || def.SamplingOverride != 0 || def.DeviceID == nil || *def.DeviceID != labSW || len(def.NATAddresses) != 0 {
		t.Fatalf("defaults = %+v", def)
	}

	path := "/flows/exporters/" + itoa(created.ID)
	put := map[string]any{"name": "probe01b", "address": "10.0.0.9", "kind": "opnsense", "enabled": false, "sampling_override": 0}
	var updated flow.ExporterStatus
	if code, body := f.do(t, http.MethodPut, path, put, &updated); code != 200 {
		t.Fatalf("update: %d %s", code, body)
	}
	// Omitted nat_addresses / device_id are kept; state follows enabled.
	if updated.Name != "probe01b" || updated.Kind != "opnsense" || updated.Enabled || updated.State != flow.StateDisabled ||
		len(updated.NATAddresses) != 2 {
		t.Fatalf("updated = %+v", updated)
	}
	put["nat_addresses"] = nil
	put["device_id"] = labRB
	f.do(t, http.MethodPut, path, put, &updated)
	if len(updated.NATAddresses) != 0 || updated.DeviceID == nil || *updated.DeviceID != labRB {
		t.Fatalf("nat cleared / device set = %+v", updated)
	}
	put["device_id"] = nil
	f.do(t, http.MethodPut, path, put, &updated)
	if updated.DeviceID != nil {
		t.Fatalf("device_id null did not clear: %v", *updated.DeviceID)
	}
	for _, c := range []struct {
		path string
		body any
		code int
		msg  string
	}{
		{"/flows/exporters/99999", put, 404, "exporter not found"},
		{"/flows/exporters/abc", put, 400, "invalid exporter id"},
		{path, map[string]any{"name": "x", "address": "192.168.78.81", "kind": "other", "enabled": true, "sampling_override": 0}, 409, "exporter address already exists"},
		{path, map[string]any{"name": "x", "address": "10.0.0.9", "kind": "other", "sampling_override": 0}, 400, "enabled is required"},
		{path, map[string]any{"name": "x", "address": "10.0.0.9", "kind": "other", "enabled": true}, 400, "sampling_override is required"},
	} {
		if code, body := f.do(t, http.MethodPut, c.path, c.body, nil); code != c.code || body != errBody(c.msg) {
			t.Errorf("PUT %s %v: %d %s", c.path, c.body, code, body)
		}
	}
	before := reloads()

	// Interfaces.
	var ifs struct {
		ExporterID int64           `json:"exporter_id"`
		Interfaces []flowIfaceJSON `json:"interfaces"`
	}
	if code, _ := f.get(t, "/flows/exporters/"+itoa(f.fwExp)+"/interfaces", nil, &ifs); code != 200 || ifs.ExporterID != f.fwExp ||
		len(ifs.Interfaces) != 2 || ifs.Interfaces[0].Hint != "192.168.78.0/24,192.168.79.0/24" || ifs.Interfaces[0].Source != "learned" ||
		ifs.Interfaces[0].LastSeen == nil {
		t.Fatalf("ifaces = %d %+v", code, ifs)
	}
	var iface flowIfaceJSON
	ifPath := "/flows/exporters/" + itoa(f.fwExp) + "/interfaces/1"
	if code, body := f.do(t, http.MethodPut, ifPath, map[string]string{"name": " LAN ", "role": "lan"}, &iface); code != 200 ||
		iface.Name != "LAN" || iface.Role != "lan" || iface.Source != "manual" || iface.IfIndex != 1 || iface.Hint == "" {
		t.Fatalf("iface put: %d %s", code, body)
	}
	for _, c := range []struct {
		path string
		body any
		code int
		msg  string
	}{
		{ifPath, map[string]string{"name": "x", "role": "core"}, 400, "invalid role"},
		{"/flows/exporters/" + itoa(f.fwExp) + "/interfaces/0", map[string]string{}, 400, "invalid ifIndex"},
		{"/flows/exporters/" + itoa(f.fwExp) + "/interfaces/4294967296", map[string]string{}, 400, "invalid ifIndex"},
		{"/flows/exporters/99999/interfaces/1", map[string]string{"name": "x"}, 404, "exporter not found"},
	} {
		if code, body := f.do(t, http.MethodPut, c.path, c.body, nil); code != c.code || body != errBody(c.msg) {
			t.Errorf("PUT %s: %d %s", c.path, code, body)
		}
	}
	if code, body := f.get(t, "/flows/exporters/99999/interfaces", nil, nil); code != 404 || body != errBody("exporter not found") {
		t.Errorf("ifaces of missing exporter: %d %s", code, body)
	}
	if reloads() != before+1 {
		t.Fatalf("iface write reloads = %d, want %d", reloads(), before+1)
	}

	// Delete: points of the exporter cascade.
	if code, _ := f.do(t, http.MethodDelete, "/flows/exporters/"+itoa(f.fwExp), nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if p, _ := queries.GetFlowPoint(f.db, f.pSwEther1); p != nil {
		t.Fatalf("derived point survived its exporter")
	}
	if code, body := f.do(t, http.MethodDelete, "/flows/exporters/"+itoa(f.fwExp), nil, nil); code != 404 || body != errBody("exporter not found") {
		t.Fatalf("delete again: %d %s", code, body)
	}
	if reloads() != before+2 {
		t.Fatalf("reloads after delete = %d", reloads())
	}
}

func TestFlowPointCRUD(t *testing.T) {
	f := newFlowFixture(t)
	valid := map[string]any{"exporter_id": f.fwExp, "name": "switch001 · ether12", "kind": "derived", "if_indexes": []int{7, 7},
		"facing": "down", "port_side": "peer", "device_id": labSW, "iface": "ether12", "exclude_attach": []any{},
		"note": "hand-made"}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range valid {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		body any
		msg  string
	}{
		{with("exporter_id", 999), "unknown exporter"},
		{with("name", ""), "name is required"},
		{with("kind", "virtual"), "invalid kind"},
		{with("if_indexes", []int{}), "if_indexes must be non-empty"},
		{with("if_indexes", []int{1, 0}), "if_indexes must be non-empty"},
		{with("if_indexes", []int64{4294967296}), "if_indexes must be non-empty"},
		{with("facing", "sideways"), "invalid facing"},
		{with("port_side", "far"), "invalid port_side"},
		{with("device_id", "nope"), "unknown device"},
		{with("device_id", nil), "unknown device"},
		{with("iface", " "), "iface is required"},
		{with("exclude_attach", []map[string]string{{"device_id": labRB}}), "invalid exclude_attach"},
		{with("exclude_attach", []map[string]string{{"device_id": "nope", "iface": "x"}}), "unknown device in exclude_attach"},
	} {
		if code, body := f.do(t, http.MethodPost, "/flows/points", c.body, nil); code != 400 || body != errBody(c.msg) {
			t.Errorf("%v: %d %s", c.body, code, body)
		}
	}
	var v flowPointView
	code, body := f.do(t, http.MethodPost, "/flows/points", valid, &v)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	if v.Kind != "derived" || v.ExporterName != "firewall003" || v.ExporterKind != "opnsense" || !reflect.DeepEqual(v.IfIndexes, []uint32{7}) ||
		!reflect.DeepEqual(v.IfNames, []string{"if#7"}) || v.DeviceID == nil || *v.DeviceID != labSW || v.DeviceName != "switch001" ||
		!v.LocalInternal || !v.Enabled || v.Auto || !v.CoverageCapable || v.Note != "hand-made" || v.ExcludeAttach == nil ||
		v.SamplingRate != 1 || v.LastData == nil {
		t.Fatalf("view = %+v", v)
	}
	if f.fake.reloads.Load() != 1 {
		t.Fatalf("reloads = %d", f.fake.reloads.Load())
	}
	if code, body := f.do(t, http.MethodPost, "/flows/points", with("name", "dup"), nil); code != 409 ||
		body != errBody("a view for this port already exists") {
		t.Fatalf("duplicate: %d %s", code, body)
	}
	// A native duplicate of (exporter, if_indexes) conflicts too.
	if code, _ := f.do(t, http.MethodPost, "/flows/points", with("kind", "native", "if_indexes", []int{1}, "port_side", ""), nil); code != 409 {
		t.Fatalf("native duplicate: %d", code)
	}
	// port_side "" clears device/iface; local_internal defaults by exporter kind.
	var bare flowPointView
	f.do(t, http.MethodPost, "/flows/points", map[string]any{"exporter_id": f.rbExp, "name": "all routed", "kind": "derived",
		"if_indexes": []int{13, 10}, "facing": "down", "port_side": "", "device_id": labSW, "iface": "ether1"}, &bare)
	if bare.DeviceID != nil || bare.Iface != "" || bare.CoverageCapable || bare.LocalInternal ||
		!reflect.DeepEqual(bare.IfNames, []string{"bridge", "net28"}) {
		t.Fatalf("bare = %+v", bare)
	}

	// PUT takes an auto native point over; omitted enabled keeps it.
	var up flowPointView
	put := map[string]any{"exporter_id": f.rbExp, "name": "Starlink", "kind": "native", "if_indexes": []int{1}, "facing": "up",
		"port_side": "same", "device_id": labRB, "iface": "eth1-wan"}
	if code, body := f.do(t, http.MethodPut, "/flows/points/"+itoa(f.pWAN), put, &up); code != 200 {
		t.Fatalf("put: %d %s", code, body)
	}
	if up.Name != "Starlink" || up.Auto || !up.Enabled || up.ID != f.pWAN {
		t.Fatalf("put view = %+v", up)
	}
	put["enabled"] = false
	f.do(t, http.MethodPut, "/flows/points/"+itoa(f.pWAN), put, &up)
	if up.Enabled {
		t.Fatal("enabled=false ignored")
	}
	if code, body := f.do(t, http.MethodPut, "/flows/points/99999", put, nil); code != 404 || body != errBody("point not found") {
		t.Fatalf("put missing: %d %s", code, body)
	}
	if code, body := f.do(t, http.MethodPut, "/flows/points/"+itoa(f.pWAN), with("facing", "x"), nil); code != 400 || body != errBody("invalid facing") {
		t.Fatalf("put invalid: %d %s", code, body)
	}

	// List: exporter name, derived before native, name; disabled included.
	var list struct {
		Points []flowPointView `json:"points"`
	}
	f.get(t, "/flows/points", nil, &list)
	var names []string
	for _, p := range list.Points {
		names = append(names, p.ExporterName+"/"+p.Kind+"/"+p.Name)
	}
	want := []string{
		"firewall003/derived/switch001 · ether1", "firewall003/derived/switch001 · ether12", "firewall003/native/firewall003 · if#1",
		"switch002-rb5009/derived/WAN (derived copy)", "switch002-rb5009/derived/all routed",
		"switch002-rb5009/derived/switch001 · sfp-sfpplus2", "switch002-rb5009/native/Starlink",
		"switch002-rb5009/native/switch002-rb5009 · net28",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("list order =\n%v\nwant\n%v", names, want)
	}
	for _, p := range list.Points {
		if p.ID == f.pTrunk && (!reflect.DeepEqual(p.ExcludeAttach, []queries.PortRef{{DeviceID: labRB, Iface: "eth3"}}) ||
			!reflect.DeepEqual(p.IfNames, []string{"bridge", "net28"})) {
			t.Errorf("trunk view = %+v", p)
		}
	}

	if code, _ := f.do(t, http.MethodDelete, "/flows/points/"+itoa(v.ID), nil, nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code, body := f.do(t, http.MethodDelete, "/flows/points/"+itoa(v.ID), nil, nil); code != 404 || body != errBody("point not found") {
		t.Fatalf("delete again: %d %s", code, body)
	}
	if code, _ := f.do(t, http.MethodDelete, "/flows/points/x", nil, nil); code != 400 {
		t.Fatalf("delete bad id: %d", code)
	}
	// create, bare create, 2 puts, delete.
	if n := f.fake.reloads.Load(); n != 5 {
		t.Fatalf("reloads = %d, want 5", n)
	}
}

// Admin writes go through RequireRole: viewers get 403, reads stay open.
func TestFlowRoutesAuth(t *testing.T) {
	f := newFlowFixture(t)
	const secret = "flow-test-secret-flow-test-secret"
	h := NewRouter(f.db, nil, &config.Config{JWTSecret: secret, AllowedOrigins: []string{"http://x"}}, nil, nil, nil, nil)
	token := func(role string) string {
		pair, err := auth.GenerateTokenPair(secret, "u-"+role, role, role, 0)
		if err != nil {
			t.Fatal(err)
		}
		return pair.AccessToken
	}
	call := func(method, path, tok string) int {
		req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(`{}`))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	viewer, admin := token("viewer"), token("admin")
	writes := [][2]string{
		{http.MethodPost, "/flows/exporters"},
		{http.MethodPut, "/flows/exporters/" + itoa(f.fwExp)},
		{http.MethodDelete, "/flows/exporters/" + itoa(f.fwExp)},
		{http.MethodPut, "/flows/exporters/" + itoa(f.fwExp) + "/interfaces/1"},
		{http.MethodPost, "/flows/points"},
		{http.MethodPut, "/flows/points/" + itoa(f.pWAN)},
		{http.MethodDelete, "/flows/points/" + itoa(f.pWAN)},
	}
	for _, wr := range writes {
		if code := call(wr[0], wr[1], viewer); code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", wr[0], wr[1], code)
		}
		if code := call(wr[0], wr[1], ""); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", wr[0], wr[1], code)
		}
	}
	// An admin passes the role check (400 = body validation ran).
	if code := call(http.MethodPost, "/flows/points", admin); code != http.StatusBadRequest {
		t.Errorf("admin POST /flows/points = %d, want 400", code)
	}
	for _, path := range []string{"/flows/status", "/flows/setup", "/flows/points", "/flows/points/suggestions",
		"/flows/exporters/" + itoa(f.fwExp) + "/interfaces", "/flows/top?point=" + itoa(f.pWAN),
		"/flows/coverage?point=" + itoa(f.pWAN), "/flows/port?device=" + labRB + "&iface=eth1-wan",
		"/traffic/sankey?source=flows&point=" + itoa(f.pWAN)} {
		if code := call(http.MethodGet, path, viewer); code != http.StatusOK {
			t.Errorf("viewer GET %s = %d, want 200", path, code)
		}
	}
	if code := call(http.MethodGet, "/flows/status", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d", code)
	}
}

func TestFlowStatusMerge(t *testing.T) {
	f := newFlowFixture(t)
	f.s.flows = nil
	var st flowStatusResponse
	if code, body := f.get(t, "/flows/status", nil, &st); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if st.Enabled || st.Listen == nil || st.ListenErrors == nil || st.Unknown == nil || st.StartedAt != nil ||
		st.SuggestedExporters == nil || len(st.SuggestedExporters) != 0 || len(st.Exporters) != 2 ||
		st.Exporters[0].Name != "firewall003" || st.Exporters[1].Name != "switch002-rb5009" ||
		st.Exporters[0].State != flow.StateNever || st.Exporters[1].DeviceID == nil {
		t.Fatalf("nil collector status = %+v", st)
	}
	if err := queries.SetSetting(f.db, "opnsense2_url", "https://192.168.78.99:1443/api"); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetSetting(f.db, "opnsense_url", "https://firewall003.lan:1443"); err != nil { // not an IP literal
		t.Fatal(err)
	}
	st = flowStatusResponse{}
	f.get(t, "/flows/status", nil, &st)
	if want := []flowSuggestedExporter{{Address: "192.168.78.99", Name: "192.168.78.99", Kind: "opnsense",
		Reason: "configured as OPNsense (opnsense2_url)"}}; !reflect.DeepEqual(st.SuggestedExporters, want) {
		t.Fatalf("suggested = %+v", st.SuggestedExporters)
	}

	// A live collector: its entries win for known ids, config comes from
	// the DB, deleted exporters vanish, missing rows are added.
	now := time.Now().UTC()
	swID := labSW
	f.fake.status = flow.Status{Enabled: true, Listen: []string{":2055"}, ListenErrors: []string{}, StartedAt: &now,
		Exporters: []flow.ExporterStatus{
			{ID: f.fwExp, Name: "stale name", Enabled: true, State: flow.StateOK, Templates: 2, TemplateState: "ok", LastSeen: &now,
				Protocol: "netflow9", NATAddresses: []string{}},
			{ID: 4242, Name: "deleted", Enabled: true},
		},
		Unknown: []flow.UnknownSender{
			{Address: "192.168.78.99", Protocol: "netflow9", Datagrams: 3},
			{Address: "192.168.78.201", Protocol: "ipfix", DeviceID: &swID, DeviceName: "switch001"},
			{Address: "10.9.9.9", Protocol: "sflow5"},
			{Address: "192.168.78.81", Protocol: "netflow9"}, // already an exporter
		}}
	f.s.flows = f.fake
	if err := queries.UpsertMACLookup(f.db, &queries.MACLookup{MACAddress: "AA:00:00:00:00:99", IPAddress: "10.9.9.9",
		HostName: "probe.lab.example", Source: "dhcp"}); err != nil {
		t.Fatal(err)
	}
	st = flowStatusResponse{}
	f.get(t, "/flows/status", nil, &st)
	if len(st.Exporters) != 2 || st.Exporters[0].ID != f.fwExp || st.Exporters[0].Name != "firewall003" ||
		st.Exporters[0].Templates != 2 || st.Exporters[0].State != flow.StateOK ||
		!reflect.DeepEqual(st.Exporters[0].NATAddresses, []string{"192.168.28.81", "192.168.111.81"}) ||
		st.Exporters[1].ID != f.rbExp || st.Exporters[1].TemplateState != "n/a" {
		t.Fatalf("merged exporters = %+v", st.Exporters)
	}
	want := []flowSuggestedExporter{
		{Address: "192.168.78.99", Name: "192.168.78.99", Kind: "opnsense", Reason: "sending NetFlow v9 (unknown sender)"},
		{Address: "192.168.78.201", Name: "switch001", Kind: "routeros", Reason: "sending IPFIX (unknown sender)"},
		{Address: "10.9.9.9", Name: "probe", Kind: "other", Reason: "sending sFlow (unknown sender)"},
	}
	if !reflect.DeepEqual(st.SuggestedExporters, want) {
		t.Fatalf("suggested =\n%+v\nwant\n%+v", st.SuggestedExporters, want)
	}

	// Disabling in the DB wins over a collector that has not reloaded yet.
	if _, err := f.db.Exec(`UPDATE flow_exporters SET enabled = 0 WHERE id = ?`, f.fwExp); err != nil {
		t.Fatal(err)
	}
	st = flowStatusResponse{}
	f.get(t, "/flows/status", nil, &st)
	if st.Exporters[0].Enabled || st.Exporters[0].State != flow.StateDisabled {
		t.Fatalf("disabled overlay = %+v", st.Exporters[0])
	}
}

func TestFlowSetup(t *testing.T) {
	f := newFlowFixture(t)
	if err := queries.SetSetting(f.db, "flow_advertise_address", "192.168.79.216"); err != nil {
		t.Fatal(err)
	}
	var s flowSetupResponse
	if code, body := f.get(t, "/flows/setup", nil, &s); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	want := flowSetupResponse{Enabled: true, Port: 2055, AdvertiseAddress: "192.168.79.216", AdvertiseSource: "setting",
		RouterOS: flowSetupRouterOS{
			Apply: []string{"/ip traffic-flow target add dst-address=192.168.79.216 port=2055 version=ipfix v9-template-timeout=1m",
				"/ip traffic-flow set enabled=yes interfaces=all active-flow-timeout=1m inactive-flow-timeout=15s"},
			Rollback: []string{"/ip traffic-flow set enabled=no active-flow-timeout=30m",
				"/ip traffic-flow target remove [find dst-address=192.168.79.216 port=2055]"}},
		OPNsense: flowSetupOPNsense{Destination: "192.168.79.216:2055", Version: "v9", ActiveTimeout: 60, InactiveTimeout: 15,
			Steps: []string{"Reporting → NetFlow → Capture (enable advanced mode)", "Listening interfaces: LAN",
				"WAN interfaces: leave empty", "Version: v9", "Destinations: add 192.168.79.216:2055",
				"Active Timeout 60, Inactive Timeout 15", "Apply"}}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("setup =\n%+v\nwant\n%+v", s, want)
	}
	s = flowSetupResponse{}
	f.get(t, "/flows/setup", map[string]string{"device": labRB}, &s)
	if s.RouterOS.DeviceID == nil || *s.RouterOS.DeviceID != labRB ||
		s.RouterOS.Apply[0] != "/ip traffic-flow target add dst-address=192.168.79.216 port=2055 src-address=192.168.78.202 version=ipfix v9-template-timeout=1m" {
		t.Fatalf("with device: %+v", s.RouterOS)
	}
	if code, body := f.get(t, "/flows/setup", map[string]string{"device": "nope"}, nil); code != 404 || body != errBody("device not found") {
		t.Fatalf("unknown device: %d %s", code, body)
	}
	// Listen port from the collector.
	f.fake.status.Listen = []string{"bogus", "0.0.0.0:9995"}
	s = flowSetupResponse{}
	f.get(t, "/flows/setup", nil, &s)
	if s.Port != 9995 || s.OPNsense.Destination != "192.168.79.216:9995" {
		t.Fatalf("port = %d dest %s", s.Port, s.OPNsense.Destination)
	}

	// auto: the local address toward the first exporter; unknown without
	// any target.
	db := newTestDB(t)
	if _, err := queries.CreateFlowExporter(db, &queries.FlowExporter{Name: "loop", Address: "127.0.0.2", Kind: "other", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	g := &flowFixture{db: db, s: &Server{db: db}}
	s = flowSetupResponse{}
	g.get(t, "/flows/setup", nil, &s)
	if a, err := netip.ParseAddr(s.AdvertiseAddress); s.Enabled || s.Port != 2055 || s.AdvertiseSource != "auto" || err != nil || !a.IsLoopback() {
		t.Fatalf("auto: %+v", s)
	}
	h := &flowFixture{db: newTestDB(t)}
	h.s = &Server{db: h.db}
	s = flowSetupResponse{}
	h.get(t, "/flows/setup", nil, &s)
	if s.AdvertiseSource != "unknown" || s.AdvertiseAddress != "" || s.OPNsense.Destination != "<NMS-IP>:2055" ||
		!strings.Contains(s.RouterOS.Apply[0], "dst-address=<NMS-IP> port=2055") {
		t.Fatalf("unknown: %+v", s)
	}
}

func TestFlowPointSuggestionsAPI(t *testing.T) {
	f := newFlowFixture(t)
	var resp struct {
		Suggestions []flowSuggestionJSON `json:"suggestions"`
	}
	if code, body := f.get(t, "/flows/points/suggestions", nil, &resp); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if len(resp.Suggestions) != 3 {
		t.Fatalf("suggestions = %+v", resp.Suggestions)
	}
	g := resp.Suggestions[0]
	if g.Rule != "gateway-host" || !g.Exists || g.Point == nil || g.Point.ExporterID != f.fwExp ||
		!reflect.DeepEqual(g.Point.IfIndexes, []uint32{1}) || g.Point.DeviceID == nil || *g.Point.DeviceID != labSW ||
		g.Point.Iface != "ether1" || g.Point.PortSide != "peer" || !g.Point.LocalInternal || g.Point.Name != "switch001 · ether1" ||
		!strings.HasPrefix(g.Point.Note, "Derived from firewall003 NetFlow on its LAN interface") {
		t.Fatalf("rule G = %+v %+v", g, g.Point)
	}
	excl := []queries.PortRef{{DeviceID: labRB, Iface: "eth3"}}
	for i, c := range []struct {
		name, dev, iface, side string
		exists                 bool
	}{
		{"switch001 · sfp-sfpplus2", labSW, "sfp-sfpplus2", "peer", true},
		{"switch002-rb5009 · sfp-sfpplus1", labRB, "sfp-sfpplus1", "same", false},
	} {
		s := resp.Suggestions[1+i]
		if s.Rule != "trunk" || s.Exists != c.exists || s.Point == nil || s.Point.Name != c.name || *s.Point.DeviceID != c.dev ||
			s.Point.Iface != c.iface || s.Point.PortSide != c.side || !reflect.DeepEqual(s.Point.IfIndexes, []uint32{10, 13}) ||
			!reflect.DeepEqual(s.Point.ExcludeAttach, excl) || s.Point.LocalInternal || !s.Point.Enabled || s.Point.Kind != "derived" {
			t.Fatalf("rule T #%d = %+v %+v", i, s, s.Point)
		}
	}
	// A suggestion's point is a valid POST body.
	var v flowPointView
	if code, body := f.do(t, http.MethodPost, "/flows/points", resp.Suggestions[2].Point, &v); code != 201 || v.Kind != "derived" {
		t.Fatalf("POST suggestion: %d %s", code, body)
	}
}

func TestPurgeHistoryFlows(t *testing.T) {
	f := newFlowFixture(t)
	count := func(table string) int {
		var n int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	old := f.end.AddDate(0, 0, -3)
	if err := queries.InsertFlows1m(f.db, []queries.FlowRow{{ExporterID: f.rbExp, Bucket: old, InIf: 1, OutIf: 13, Bytes: 1}},
		[]queries.FlowMeta{{ExporterID: f.rbExp, Bucket: old, Datagrams: 1, Sampling: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := queries.RollupFlows1h(f.db, f.rbExp, old.Truncate(time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	purge := func(body string) (int, purgeResponse) {
		rec := httptest.NewRecorder()
		f.s.handlePurgeHistory(rec, httptest.NewRequest(http.MethodPost, "/admin/purge-history", strings.NewReader(body)))
		var resp purgeResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp
	}
	h1 := count("flow_1h") + count("flow_1h_meta")
	if code, resp := purge(`{"flows": true, "older_than_days": 1}`); code != 200 || resp.Deleted["flow_1m"] != 2 ||
		resp.Deleted["flow_1h"] != int64(h1) || h1 < 2 || count("flow_1m") != 70+30 || count("flow_1h") != 0 {
		t.Fatalf("older_than_days=1: %d %v 1m=%d 1h=%d", code, resp.Deleted, count("flow_1m"), count("flow_1h"))
	}
	if code, resp := purge(`{"flows": true}`); code != 200 || resp.Deleted["flow_1m"] != 120 || count("flow_1m") != 0 ||
		count("flow_1m_meta") != 0 || count("flow_exporters") != 2 || count("flow_points") != 6 {
		t.Fatalf("full purge: %d %v", code, resp.Deleted)
	}
	if code, _ := purge(`{"flows": false}`); code != 400 {
		t.Fatalf("no targets: %d", code)
	}
}

// The flow configuration tables are part of the backup, after their
// parents, and restore into an empty database.
func TestFlowBackupRoundTrip(t *testing.T) {
	n := len(exportableTables)
	if n < 3 || !slices.Equal(exportableTables[n-3:], []string{"flow_exporters", "flow_exporter_ifaces", "flow_points"}) {
		t.Fatalf("exportable tables end with %v", exportableTables[max(n-3, 0):])
	}
	for _, fact := range []string{"flow_1m", "flow_1h", "flow_1m_meta", "flow_1h_meta", "flow_templates", "flow_prefixes"} {
		if tableAllowed(fact) {
			t.Errorf("%s must not be exportable", fact)
		}
	}
	f := newFlowFixture(t)
	rec := httptest.NewRecorder()
	f.s.handleFullBackup(rec, httptest.NewRequest(http.MethodGet, "/admin/backup", nil))
	if rec.Code != 200 {
		t.Fatalf("backup: %d", rec.Code)
	}
	var bundle backupBundle
	if err := json.Unmarshal(rec.Body.Bytes(), &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Tables["flow_exporters"]) != 2 || len(bundle.Tables["flow_points"]) != 6 || len(bundle.Tables["flow_exporter_ifaces"]) != 9 {
		t.Fatalf("bundle flow rows: %d %d %d", len(bundle.Tables["flow_exporters"]), len(bundle.Tables["flow_points"]),
			len(bundle.Tables["flow_exporter_ifaces"]))
	}
	db := newTestDB(t)
	s := &Server{db: db}
	raw := rec.Body.String()
	rec = httptest.NewRecorder()
	s.handleFullRestore(rec, httptest.NewRequest(http.MethodPost, "/admin/restore", strings.NewReader(raw)))
	if rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	pts, err := queries.ListFlowPoints(db)
	if err != nil || len(pts) != 6 {
		t.Fatalf("restored points: %d %v", len(pts), err)
	}
	e, err := queries.GetFlowExporter(db, f.fwExp)
	if err != nil || e == nil || len(e.NATAddresses) != 2 {
		t.Fatalf("restored exporter: %+v %v", e, err)
	}
}

func TestFlowAggCacheBounded(t *testing.T) {
	var c flowAggCache
	now := time.Now()
	rows := make([]queries.FlowAgg, flowCacheMaxRows/4)
	for i := 0; i < 12; i++ {
		c.put(flowAggKey{exporter: int64(i)}, rows, time.Minute, now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(c.m) != 4 || c.rows != flowCacheMaxRows {
		t.Fatalf("cache size %d entries, %d rows", len(c.m), c.rows)
	}
	if _, ok := c.get(flowAggKey{exporter: 0}, now); ok {
		t.Fatal("oldest entry not evicted")
	}
	if _, ok := c.get(flowAggKey{exporter: 11}, now); !ok {
		t.Fatal("newest entry missing")
	}
	c.put(flowAggKey{exporter: 99}, make([]queries.FlowAgg, flowCacheMaxRows+1), time.Minute, now)
	if _, ok := c.get(flowAggKey{exporter: 99}, now); ok || len(c.m) != 4 {
		t.Fatal("an over-budget result was cached")
	}
	if _, ok := c.get(flowAggKey{exporter: 11}, now.Add(2*time.Minute)); ok {
		t.Fatal("expired entry served")
	}
	if _, ok := c.m[flowAggKey{exporter: 11}]; ok || c.rows != 3*flowCacheMaxRows/4 {
		t.Fatalf("expired entry not released on get (%d rows held)", c.rows)
	}
	c.clear()
	if _, ok := c.get(flowAggKey{exporter: 10}, now); ok || c.rows != 0 {
		t.Fatal("clear kept entries")
	}
}

func TestFlowAggCacheSingleFlight(t *testing.T) {
	var c flowAggCache
	var calls atomic.Int32
	release := make(chan struct{})
	query := func() ([]queries.FlowAgg, error) {
		calls.Add(1)
		<-release
		return []queries.FlowAgg{{Bytes: 42}}, nil
	}
	k := flowAggKey{exporter: 1}
	var wg sync.WaitGroup
	results := make([][]queries.FlowAgg, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = c.load(k, time.Minute, query)
		}()
	}
	for {
		c.mu.Lock()
		n := 0
		if call := c.inflight[k]; call != nil {
			n = 1
		}
		c.mu.Unlock()
		if n == 1 && calls.Load() == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the followers join
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("query ran %d times", calls.Load())
	}
	for i, r := range results {
		if len(r) != 1 || r[0].Bytes != 42 {
			t.Fatalf("caller %d got %v", i, r)
		}
	}
	if _, ok := c.get(k, time.Now()); !ok {
		t.Fatal("result not cached")
	}

	// A clear during the query keeps its (possibly stale) rows out.
	k2 := flowAggKey{exporter: 2}
	_, _ = c.load(k2, time.Minute, func() ([]queries.FlowAgg, error) {
		c.clear()
		return []queries.FlowAgg{{Bytes: 1}}, nil
	})
	if _, ok := c.get(k2, time.Now()); ok {
		t.Fatal("rows of a query overtaken by clear were cached")
	}
}
