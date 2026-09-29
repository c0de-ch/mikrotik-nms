package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow"
)

// Lab identities (contract §2).
const (
	labRB   = "c7a3774f-9d5e-489a-b7dd-6a8ebb9fc119" // switch002-rb5009
	labSW   = "0bf4fc44-0dd5-4a7f-9823-41a314e61e4d" // switch001
	macFW   = "00:03:2D:21:FB:4C"                    // firewall003 LAN NIC
	macC01  = "BC:24:11:B3:6B:38"                    // net28-client01 (behind switch001 ether12)
	macPmox = "78:9A:18:00:00:25"                    // ccr2004-pmox05 (RB5009 eth3)
	macSL   = "74:24:9F:00:00:01"                    // Starlink gateway
)

// fakeFlows stands in for the flow collector.
type fakeFlows struct {
	enabled         bool
	status          flow.Status
	flushed, rolled time.Time
	reloads         atomic.Int32
}

func (f *fakeFlows) Enabled() bool             { return f.enabled }
func (f *fakeFlows) Status() flow.Status       { return f.status }
func (f *fakeFlows) FlushedThrough() time.Time { return f.flushed }
func (f *fakeFlows) RolledThrough() time.Time  { return f.rolled }
func (f *fakeFlows) Reload()                   { f.reloads.Add(1) }

// flowFixture is the lab of contract §2/§10 in a real DB: the RB5009 (IPFIX
// exporter with a device) and firewall003 (OPNsense, NetFlow v9, no device),
// switch001 behind both, the address plan, points #1-#3/#6-#8 and 10 minutes
// of flow rows ending at end.
type flowFixture struct {
	db    *sql.DB
	s     *Server
	fake  *fakeFlows
	end   time.Time // FlushedThrough: minute-aligned end of the 1m windows
	rbExp int64     // RB5009 exporter id
	fwExp int64     // firewall003 exporter id
	// Points.
	pWAN, pNet28, pTrunk, pFwLAN, pSwEther1, pWANDerived int64
}

// minutes of seeded flow / counter data.
const flowFixtureMinutes = 10

func newFlowFixture(t *testing.T) *flowFixture {
	t.Helper()
	db := newTestDB(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []struct{ id, name, addr string }{
		{labRB, "switch002-rb5009", "192.168.78.202"}, {labSW, "switch001", "192.168.78.201"}} {
		must(queries.CreateDevice(db, &queries.Device{ID: d.id, Identity: d.name, Address: d.addr, Username: "admin",
			APIPort: 8728, Status: "online"}))
	}
	for dev, specs := range map[string][]string{
		labRB: {"eth1-wan:ether", "eth3:ether", "sfp-sfpplus1:ether", "bridge:bridge", "management:vlan", "net28:vlan", "wg-labnet:wg"},
		labSW: {"ether1:ether", "ether12:ether", "sfp-sfpplus2:ether", "bridge:bridge"},
	} {
		for _, spec := range specs {
			name, typ, _ := strings.Cut(spec, ":")
			must(queries.UpsertInterface(db, &queries.Interface{ID: uuid.NewString(), DeviceID: dev, Name: name, Type: typ,
				MACAddress: ifaceMAC(dev[:4], name), Running: true}))
		}
	}
	must(queries.UpsertLink(db, &queries.Link{ID: uuid.NewString(), DeviceAID: labRB, InterfaceA: "sfp-sfpplus1,bridge",
		DeviceBID: labSW, InterfaceB: "sfp-sfpplus2,bridge", LinkType: "ethernet", DiscoveredBy: "mndp", Status: "up"}))
	must(queries.ReplaceDeviceUplinks(db, labRB, []queries.DeviceUplink{
		{DeviceID: labRB, Kind: "default-route", Interface: "eth1-wan", IfaceType: "ether", GatewayIP: "100.64.0.1"}}))
	must(queries.ReplaceDeviceUplinks(db, labSW, []queries.DeviceUplink{
		{DeviceID: labSW, Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.168.78.81"}}))
	must(queries.ReplaceGatewayHosts(db, []queries.DeviceUplink{
		{DeviceID: labSW, Interface: "ether1", IfaceType: "ether", GatewayIP: "192.168.78.81"}}))
	now := time.Now()
	must(queries.UpsertPortHosts(db, labSW, []queries.PortHost{
		{InterfaceName: "ether1", MACAddress: macFW, VID: 1},
		{InterfaceName: "ether12", MACAddress: macC01, VID: 28},
		{InterfaceName: "sfp-sfpplus2", MACAddress: macPmox, VID: 28},
	}, now))
	must(queries.UpsertPortHosts(db, labRB, []queries.PortHost{
		{InterfaceName: "eth3", MACAddress: macPmox, VID: 28},
		{InterfaceName: "sfp-sfpplus1", MACAddress: macC01, VID: 28},
		{InterfaceName: "sfp-sfpplus1", MACAddress: macFW, VID: 1},
		{InterfaceName: "eth1-wan", MACAddress: macSL, VID: 0},
	}, now))
	for _, m := range []queries.MACLookup{
		{MACAddress: macFW, IPAddress: "192.168.78.81", HostName: "firewall003.lan", Source: "dhcp"},
		{MACAddress: macC01, IPAddress: "192.168.28.33", HostName: "net28-client01", Source: "dhcp"},
		{MACAddress: macPmox, IPAddress: "192.168.28.25", HostName: "ccr2004-pmox05", Source: "arp"},
		{MACAddress: macSL, IPAddress: "100.64.0.1", Source: "arp"},
	} {
		must(queries.UpsertMACLookup(db, &m))
	}
	for _, v := range []queries.BridgeVLAN{
		{DeviceID: labRB, BridgeName: "bridge", VLANIDs: "1", CurrentUntagged: "bridge,sfp-sfpplus1"},
		{DeviceID: labRB, BridgeName: "bridge", VLANIDs: "28", CurrentTagged: "sfp-sfpplus1", CurrentUntagged: "eth3"},
		{DeviceID: labRB, BridgeName: "bridge", VLANIDs: "6,28", CurrentTagged: "bridge"},
		{DeviceID: labRB, BridgeName: "bridge", VLANIDs: "88"},
	} {
		v.ID = uuid.NewString()
		must(queries.UpsertBridgeVLAN(db, &v))
	}
	must(queries.ReplaceFlowPrefixes(db, []queries.FlowPrefix{
		{Prefix: netip.MustParsePrefix("100.64.0.0/10"), Class: "transit", DeviceID: labRB, Iface: "eth1-wan", UpdatedAt: now},
		{Prefix: netip.MustParsePrefix("192.168.78.0/23"), Class: "internal", DeviceID: labRB, Iface: "bridge", UpdatedAt: now},
		{Prefix: netip.MustParsePrefix("192.168.28.0/24"), Class: "internal", DeviceID: labRB, Iface: "net28", UpdatedAt: now},
		{Prefix: netip.MustParsePrefix("172.16.28.0/28"), Class: "vpn", DeviceID: labRB, Iface: "wg-labnet", UpdatedAt: now},
	}))

	f := &flowFixture{db: db}
	var err error
	f.rbExp, err = queries.CreateFlowExporter(db, &queries.FlowExporter{Name: "switch002-rb5009", Address: "192.168.78.202",
		Kind: "routeros", DeviceID: labRB, Enabled: true, Auto: true})
	must(err)
	f.fwExp, err = queries.CreateFlowExporter(db, &queries.FlowExporter{Name: "firewall003", Address: "192.168.78.81",
		Kind: "opnsense", Enabled: true, NATAddresses: []netip.Addr{netip.MustParseAddr("192.168.28.81"),
			netip.MustParseAddr("192.168.111.81")}})
	must(err)
	must(queries.UpsertDeviceFlowIfaces(db, f.rbExp, []queries.FlowIface{
		{IfIndex: 1, Name: "eth1-wan", Type: "ether"}, {IfIndex: 3, Name: "eth3", Type: "ether"},
		{IfIndex: 9, Name: "sfp-sfpplus1", Type: "ether"}, {IfIndex: 10, Name: "bridge", Type: "bridge"},
		{IfIndex: 12, Name: "management", Type: "vlan", VLANID: 6, Parent: "bridge"},
		{IfIndex: 13, Name: "net28", Type: "vlan", VLANID: 28, Parent: "bridge"},
		{IfIndex: 15, Name: "wg-labnet", Type: "wg"},
	}))

	f.end = time.Now().UTC().Truncate(time.Minute)
	must(queries.TouchFlowIfaces(db, f.rbExp, []queries.FlowIfaceSeen{{IfIndex: 1}, {IfIndex: 10}, {IfIndex: 13}}, f.end))
	must(queries.TouchFlowIfaces(db, f.fwExp, []queries.FlowIfaceSeen{
		{IfIndex: 1, Hint: "192.168.78.0/24,192.168.79.0/24"}, {IfIndex: 7}}, f.end))

	native := func(exp int64, name string, ifs []uint32, facing, side, dev, iface string, local bool) int64 {
		t.Helper()
		_, err := queries.EnsureNativeFlowPoint(db, &queries.FlowPoint{ExporterID: exp, Name: name, IfIndexes: ifs,
			Facing: facing, PortSide: side, DeviceID: dev, Iface: iface, LocalInternal: local, Enabled: true})
		must(err)
		var id int64
		must(db.QueryRow(`SELECT id FROM flow_points WHERE name = ?`, name).Scan(&id))
		return id
	}
	derived := func(exp int64, name string, ifs []uint32, facing, side, dev, iface string, local bool, excl []queries.PortRef) int64 {
		t.Helper()
		id, err := queries.CreateFlowPoint(db, &queries.FlowPoint{ExporterID: exp, Name: name, Kind: "derived",
			IfIndexes: ifs, Facing: facing, PortSide: side, DeviceID: dev, Iface: iface, LocalInternal: local,
			ExcludeAttach: excl, Note: "derived note", Enabled: true})
		must(err)
		return id
	}
	f.pWAN = native(f.rbExp, "switch002-rb5009 · eth1-wan", []uint32{1}, "up", "same", labRB, "eth1-wan", false)
	f.pNet28 = native(f.rbExp, "switch002-rb5009 · net28", []uint32{13}, "down", "same", labRB, "net28", false)
	f.pFwLAN = native(f.fwExp, "firewall003 · if#1", []uint32{1}, "down", "", "", "", true)
	f.pTrunk = derived(f.rbExp, "switch001 · sfp-sfpplus2", []uint32{10, 13}, "down", "peer", labSW, "sfp-sfpplus2",
		false, []queries.PortRef{{DeviceID: labRB, Iface: "eth3"}})
	f.pSwEther1 = derived(f.fwExp, "switch001 · ether1", []uint32{1}, "down", "peer", labSW, "ether1", true, nil)
	f.pWANDerived = derived(f.rbExp, "WAN (derived copy)", []uint32{1}, "up", "same", labRB, "eth1-wan", false, nil)

	// Ten minutes of flows per exporter; the per-minute bytes are given
	// below and in the tests' expectations.
	var rows []queries.FlowRow
	var meta []queries.FlowMeta
	a := netip.MustParseAddr
	for i := 1; i <= flowFixtureMinutes; i++ {
		b := f.end.Add(-time.Duration(i) * time.Minute)
		rb := func(in, out uint32, src, dst string, proto uint8, port uint16, bytes int64) {
			r := queries.FlowRow{ExporterID: f.rbExp, Bucket: b, InIf: in, OutIf: out, Proto: proto, Port: port,
				Bytes: bytes, Packets: bytes / 1000, Flows: 1}
			if src != "" {
				r.Src, r.Dst = a(src), a(dst)
			}
			rows = append(rows, r)
		}
		rb(1, 13, "1.1.1.1", "192.168.28.33", 6, 443, 60000)         // download to net28-client01
		rb(13, 1, "192.168.28.33", "1.1.1.1", 6, 443, 6000)          // its upload
		rb(13, 1, "192.168.28.81", "9.9.9.9", 6, 853, 3000)          // firewall003 NATed out WAN
		rb(1, 13, "", "", 0, 0, 500)                                 // folded other (download)
		rb(13, 10, "192.168.28.25", "192.168.78.201", 6, 8728, 1000) // hairpin pmox05 → switch001
		rb(10, 0, "192.168.79.216", "192.168.78.202", 6, 8728, 2000) // NMS polling the router
		rb(1, 13, "2606:4700::1111", "192.168.28.33", 17, 443, 100)  // (odd but harmless) v6 src
		fw := func(in, out uint32, src, dst string, proto uint8, port uint16, bytes int64) {
			rows = append(rows, queries.FlowRow{ExporterID: f.fwExp, Bucket: b, InIf: in, OutIf: out, Src: a(src),
				Dst: a(dst), Proto: proto, Port: port, Bytes: bytes, Packets: bytes / 1000, Flows: 1})
		}
		fw(1, 7, "192.168.78.60", "9.9.9.9", 6, 853, 1480)  // LAN upload
		fw(7, 1, "9.9.9.9", "192.168.78.60", 6, 853, 64000) // LAN download
		fw(7, 1, "9.9.9.9", "192.168.111.81", 6, 853, 5000) // WAN-copy duplicate: local not internal → dropped
		meta = append(meta,
			queries.FlowMeta{ExporterID: f.rbExp, Bucket: b, Datagrams: 10, Records: 7, Bytes: 72600, Sampling: 1},
			queries.FlowMeta{ExporterID: f.fwExp, Bucket: b, Datagrams: 5, Records: 3, Bytes: 70480, Sampling: 1})
	}
	must(queries.InsertFlows1m(db, rows, meta))

	f.fake = flowFakeFor(f.end)
	f.s = &Server{db: db, flows: f.fake}
	return f
}

// flowFakeFor is an enabled collector flushed through end.
func flowFakeFor(end time.Time) *fakeFlows {
	return &fakeFlows{enabled: true, flushed: end, status: flow.Status{Enabled: true, Listen: []string{":2055"},
		ListenErrors: []string{}, Exporters: []flow.ExporterStatus{}, Unknown: []flow.UnknownSender{}}}
}

// seedCounters writes Phase 1 one-minute rows for a port over the fixture's
// minutes (skipping the given minutes-ago values).
func (f *flowFixture) seedCounters(t *testing.T, dev, iface string, rx, tx int64, skip ...int) {
	t.Helper()
	skipped := map[int]bool{}
	for _, s := range skip {
		skipped[s] = true
	}
	var rows []queries.PortStatRow
	for i := 1; i <= flowFixtureMinutes; i++ {
		if skipped[i] {
			continue
		}
		rows = append(rows, queries.PortStatRow{DeviceID: dev, InterfaceName: iface,
			Bucket: f.end.Add(-time.Duration(i) * time.Minute), RxBytes: rx, TxBytes: tx,
			RxBpsAvg: rx * 8 / 60, TxBpsAvg: tx * 8 / 60, Samples: 4})
	}
	if err := queries.InsertPortStats1m(f.db, rows); err != nil {
		t.Fatal(err)
	}
}

// withUser injects an authenticated user, standing in for RequireAuth.
func withUser(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := &auth.ContextUser{ID: "u1", Username: role, Role: role}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auth.UserContextKey, u)))
	})
}

// router serves the flow + traffic routes as an admin.
func (f *flowFixture) router() http.Handler {
	r := chi.NewRouter()
	f.s.mountTrafficRoutes(r)
	f.s.mountFlowRoutes(r)
	return withUser("admin", r)
}

// do issues a request and decodes a JSON reply into out (when non-nil and
// the status is 2xx). It returns the status and the raw body.
func (f *flowFixture) do(t *testing.T, method, path string, body any, out any) (int, string) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		var raw []byte
		switch b := body.(type) {
		case string:
			raw = []byte(b)
		default:
			var err error
			if raw, err = json.Marshal(b); err != nil {
				t.Fatal(err)
			}
		}
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	rec := httptest.NewRecorder()
	f.router().ServeHTTP(rec, httptest.NewRequest(method, path, rd))
	if out != nil && rec.Code >= 200 && rec.Code < 300 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code, rec.Body.String()
}

// get is do(GET) with query params.
func (f *flowFixture) get(t *testing.T, path string, params map[string]string, out any) (int, string) {
	t.Helper()
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return f.do(t, http.MethodGet, path, nil, out)
}
