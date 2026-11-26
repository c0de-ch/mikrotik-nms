package flow

import (
	"context"
	"database/sql"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
	"github.com/mikrotik-nms/backend/internal/flowguard"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

// Documentation addresses for the guard tests.
var (
	routerAddr = netip.MustParseAddr("192.0.2.1")
	switchAddr = netip.MustParseAddr("192.0.2.10")
	apAddr     = netip.MustParseAddr("192.0.2.20")
	weakAddr   = netip.MustParseAddr("192.0.2.30")
	otherAddr  = netip.MustParseAddr("198.51.100.77")
)

// seedRoleDevice adds a device with the stored facts its role rests on: the
// board, the probed class, and (egress) a default route to a public next hop
// or (gateway) one to another router on the LAN.
func seedRoleDevice(t *testing.T, db *sql.DB, id string, addr netip.Addr, board string, class devclass.Class, route string) {
	t.Helper()
	seedDevice(t, db, id, addr.String(), id)
	if err := queries.UpdateDeviceInfo(db, id, "MikroTik", board, "7.23.1 (stable)", "", "arm64"); err != nil {
		t.Fatal(err)
	}
	if class != "" {
		if err := queries.SetDeviceClass(db, id, class, devclass.ResourceFacts{}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var ups []queries.DeviceUplink
	switch route {
	case "egress":
		ups = []queries.DeviceUplink{{DeviceID: id, Kind: "default-route", Interface: "ether1", IfaceType: "ether", GatewayIP: "203.0.113.1"}}
	case "gateway":
		ups = []queries.DeviceUplink{{DeviceID: id, Kind: "default-route", Interface: "bridge", IfaceType: "bridge", GatewayIP: "192.168.1.1"}}
	}
	if err := queries.ReplaceDeviceUplinks(db, id, ups); err != nil {
		t.Fatal(err)
	}
}

// guardPackets are an IPFIX template and data datagram as the given address
// would export them.
func guardPackets(from netip.Addr, seq uint32) (tmpl, data []byte) {
	sysInit := uint64(tLab.Add(-72 * time.Hour).UnixMilli())
	flows := pktgen.LabRouterOS(tLab, sysInit, true, from.String(), "198.51.100.5", 2055)
	return pktgen.RouterOSIPFIXTemplates(uint32(tLab.Unix()), seq, 0, true),
		pktgen.RouterOSIPFIXData(uint32(tLab.Unix()), seq, 0, flows...)
}

func guardCollector(t *testing.T, db *sql.DB) (*Collector, time.Time) {
	t.Helper()
	c := newTestCollector(t, db)
	recv := tLab.Add(lag)
	c.now = func() time.Time { return recv }
	c.reconcile(recv)
	return c, recv
}

func exportFinding(t *testing.T, db *sql.DB, deviceID string) *queries.DeviceFinding {
	t.Helper()
	rows, err := queries.ListDeviceFindings(db, deviceID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		if rows[i].Kind == flowguard.FindingExportOnSwitch {
			return &rows[i]
		}
	}
	return nil
}

// Nothing is accepted automatically by default: a strong router with an
// egress of its own is listed as an unknown sender until an admin adds it.
func TestAutoAcceptIsOffByDefault(t *testing.T) {
	db := testDB(t)
	seedRoleDevice(t, db, "edge", routerAddr, "RB5009UG+S+", devclass.C3, "egress")
	if AutoAcceptClasses(db) != "" || AutoAcceptClasses(nil) != "" {
		t.Fatalf("auto-accept default = %q, want off", AutoAcceptClasses(db))
	}
	c, recv := guardCollector(t, db)
	tmpl, data := guardPackets(routerAddr, 1)
	feed(c, routerAddr, tmpl, recv)
	feed(c, routerAddr, data, recv)
	if exps, _ := queries.ListFlowExporters(db); len(exps) != 0 {
		t.Fatalf("exporter created with auto-accept off: %+v", exps)
	}
	unk := c.Status().Unknown
	if len(unk) != 1 || unk[0].DeviceID == nil || *unk[0].DeviceID != "edge" || unk[0].DeviceRole != string(devclass.RoleRouter) {
		t.Fatalf("unknown senders = %+v", unk)
	}
	if list, _ := queries.ListFlowSenderPolicies(db); len(list) != 0 {
		t.Fatalf("a router was blocked: %+v", list)
	}
	// The key of the previous release does nothing any more.
	if err := queries.SetSetting(db, "flow_auto_accept_devices", "true"); err != nil {
		t.Fatal(err)
	}
	c.reconcile(recv)
	feed(c, routerAddr, tmpl, recv)
	if exps, _ := queries.ListFlowExporters(db); len(exps) != 0 {
		t.Fatalf("the old flow_auto_accept_devices key still accepts: %+v", exps)
	}
}

// flow_auto_accept_classes = router accepts router-role C3 devices with a
// fresh internet or VPN egress — and nothing else.
func TestAutoAcceptRouterClassOnly(t *testing.T) {
	db := testDB(t)
	seedRoleDevice(t, db, "edge", routerAddr, "RB5009UG+S+", devclass.C3, "egress")
	seedRoleDevice(t, db, "inner", netip.MustParseAddr("192.0.2.2"), "RB5009UG+S+", devclass.C3, "gateway") // no egress of its own
	seedRoleDevice(t, db, "small", weakAddr, "RB4011iGS+", devclass.C2, "egress")                           // weak-router
	seedRoleDevice(t, db, "light", netip.MustParseAddr("192.0.2.3"), "RB5009UG+S+", devclass.C3, "egress")
	if err := queries.SetDevicePollProfile(db, "light", devclass.ProfileLight); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetSetting(db, "flow_auto_accept_classes", "router"); err != nil {
		t.Fatal(err)
	}
	c, recv := guardCollector(t, db)
	for _, a := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.30", "192.0.2.3"} {
		addr := netip.MustParseAddr(a)
		tmpl, data := guardPackets(addr, 1)
		feed(c, addr, tmpl, recv)
		feed(c, addr, data, recv)
	}
	exps, _ := queries.ListFlowExporters(db)
	if len(exps) != 1 || exps[0].DeviceID != "edge" || !exps[0].Auto || exps[0].Kind != "routeros" || !exps[0].Enabled {
		t.Fatalf("auto-accepted = %+v, want only the strong router with an egress", exps)
	}
	if st := statusOf(t, c, exps[0].ID); st.Counters["datagrams"] != 2 || st.Counters["flows"] != 5 {
		t.Fatalf("the accepted router's datagrams were not both decoded: %+v", st.Counters)
	}
	if got := len(c.Status().Unknown); got != 3 {
		t.Fatalf("unknown senders = %d, want the three devices that were not accepted", got)
	}
	if list, _ := queries.ListFlowSenderPolicies(db); len(list) != 0 {
		t.Fatalf("a router-class device was blocked: %+v", list)
	}
}

// The first flow datagram from a switch blocks its address: nothing is
// decoded, a tombstone with the reason is stored, and an unconfirmed finding
// is raised — once, however many datagrams follow within the minute.
func TestSwitchSenderIsBlockedAndFlagged(t *testing.T) {
	db := testDB(t)
	seedRoleDevice(t, db, "sw", switchAddr, "CRS326-24G-2S+", devclass.C2, "gateway")
	// Even with auto-accept on.
	if err := queries.SetSetting(db, "flow_auto_accept_classes", "router"); err != nil {
		t.Fatal(err)
	}
	c, recv := guardCollector(t, db)
	tmpl, data := guardPackets(switchAddr, 1)
	feed(c, switchAddr, tmpl, recv)
	feed(c, switchAddr, data, recv.Add(time.Second))
	feed(c, switchAddr, data, recv.Add(2*time.Second))

	if exps, _ := queries.ListFlowExporters(db); len(exps) != 0 {
		t.Fatalf("a switch became an exporter: %+v", exps)
	}
	list, _ := queries.ListFlowSenderPolicies(db)
	if len(list) != 1 || list[0].Address != switchAddr.String() || list[0].Source != queries.FlowBlockAuto ||
		list[0].DeviceID != "sw" || list[0].Name != "sw" || !strings.Contains(list[0].Reason, "switch") {
		t.Fatalf("block list = %+v", list)
	}
	g := c.Status().Global
	if g.Blocked != 3 || g.UnknownDatagrams != 0 || g.Datagrams != 3 {
		t.Fatalf("global = %+v, want 3 datagrams blocked before decode", g)
	}
	if n := countTable(t, db, "flow_templates"); n != 0 {
		t.Fatalf("a blocked sender's template was decoded and stored (%d rows)", n)
	}
	f := exportFinding(t, db, "sw")
	if f == nil || f.Severity != "warn" || f.Count != 1 || !strings.Contains(f.Detail, switchAddr.String()) {
		t.Fatalf("finding = %+v, want one unconfirmed (warn) finding", f)
	}
	chk, _ := queries.GetFlowExportCheck(db, "sw")
	if chk == nil || chk.Address != switchAddr.String() || chk.Result != "" || !chk.CheckedAt.IsZero() {
		t.Fatalf("export check row = %+v", chk)
	}
	// It is shown as blocked with its reason, not as an unknown sender.
	st := c.Status()
	if len(st.Unknown) != 0 || len(st.Blocked) != 1 || st.Blocked[0].Address != switchAddr.String() ||
		st.Blocked[0].Datagrams != 3 || st.Blocked[0].Source != queries.FlowBlockAuto || st.Blocked[0].Reason == "" ||
		st.Blocked[0].DeviceID != "sw" {
		t.Fatalf("status: unknown %+v blocked %+v", st.Unknown, st.Blocked)
	}

	// The dropped datagrams are persisted with the next status write, and a
	// later minute's datagram is one more sighting.
	c.persistStatus()
	if list, _ := queries.ListFlowSenderPolicies(db); list[0].Datagrams != 3 || list[0].LastSeen.IsZero() {
		t.Fatalf("persisted drops = %+v", list[0])
	}
	later := recv.Add(2 * time.Minute)
	c.reconcile(later)
	feed(c, switchAddr, data, later)
	if f := exportFinding(t, db, "sw"); f == nil || f.Count != 2 {
		t.Fatalf("finding after a later datagram = %+v, want count 2", f)
	}
	if got := c.Status().Blocked[0].Datagrams; got != 4 {
		t.Fatalf("blocked datagrams = %d, want 4 (3 persisted + 1 live)", got)
	}
}

// An access point behind a router and a C1 device are blocked like a switch;
// a device whose role the facts do not settle is neither accepted nor
// blocked, and a non-device sender is an unknown sender as before.
func TestAccessPointAndWeakSendersAreBlocked(t *testing.T) {
	db := testDB(t)
	seedRoleDevice(t, db, "ap", apAddr, "cAP ax", devclass.C3, "gateway")
	seedRoleDevice(t, db, "weak", weakAddr, "hEX S", devclass.C1, "egress") // a C1 router: never, whatever its role
	seedRoleDevice(t, db, "inner", netip.MustParseAddr("192.0.2.2"), "RB5009UG+S+", devclass.C3, "gateway")
	c, recv := guardCollector(t, db)
	for _, a := range []netip.Addr{apAddr, weakAddr, netip.MustParseAddr("192.0.2.2"), otherAddr} {
		tmpl, _ := guardPackets(a, 1)
		feed(c, a, tmpl, recv)
	}
	list, _ := queries.ListFlowSenderPolicies(db)
	blocked := map[string]string{}
	for _, p := range list {
		blocked[p.DeviceID] = p.Reason
	}
	if len(list) != 2 || !strings.Contains(blocked["ap"], "access point") || !strings.Contains(blocked["weak"], "C1") {
		t.Fatalf("block list = %+v", list)
	}
	for _, id := range []string{"ap", "weak"} {
		if f := exportFinding(t, db, id); f == nil || f.Severity != "warn" {
			t.Errorf("%s: finding = %+v", id, f)
		}
	}
	if f := exportFinding(t, db, "inner"); f != nil {
		t.Errorf("a device of unknown role was flagged: %+v", f)
	}
	unk := map[string]bool{}
	for _, u := range c.Status().Unknown {
		unk[u.Address] = true
	}
	if len(unk) != 2 || !unk["192.0.2.2"] || !unk[otherAddr.String()] {
		t.Fatalf("unknown senders = %v", unk)
	}
}

// A source address alone proves nothing: a datagram that is not a flow
// export, sent with a switch's address, neither blocks nor flags it.
func TestNonFlowDatagramFromSwitchAddressIsIgnored(t *testing.T) {
	db := testDB(t)
	seedRoleDevice(t, db, "sw", switchAddr, "CRS326-24G-2S+", devclass.C2, "gateway")
	c, recv := guardCollector(t, db)
	feed(c, switchAddr, []byte("not a flow datagram at all"), recv)
	feed(c, switchAddr, []byte{0, 1}, recv)
	if list, _ := queries.ListFlowSenderPolicies(db); len(list) != 0 {
		t.Fatalf("blocked on garbage: %+v", list)
	}
	if f := exportFinding(t, db, "sw"); f != nil {
		t.Fatalf("flagged on garbage: %+v", f)
	}
}

// Deleting an exporter leaves a tombstone: its datagrams are dropped before
// decode, it is not accepted again by itself — not even with auto-accept on
// for a router — and only an explicit re-add brings it back.
func TestDeletedExporterStaysBlocked(t *testing.T) {
	db := testDB(t)
	seedRoleDevice(t, db, "edge", routerAddr, "RB5009UG+S+", devclass.C3, "egress")
	if err := queries.SetSetting(db, "flow_auto_accept_classes", "router"); err != nil {
		t.Fatal(err)
	}
	c, recv := guardCollector(t, db)
	tmpl, data := guardPackets(routerAddr, 1)
	feed(c, routerAddr, tmpl, recv)
	feed(c, routerAddr, data, recv)
	exps, _ := queries.ListFlowExporters(db)
	if len(exps) != 1 {
		t.Fatalf("exporters = %+v", exps)
	}
	if _, err := queries.DeleteFlowExporterBlocking(db, exps[0].ID, "exporter deleted by admin", "admin", recv); err != nil {
		t.Fatal(err)
	}
	c.reconcile(recv)

	before := c.Status().Global
	feed(c, routerAddr, tmpl, recv.Add(time.Second))
	feed(c, routerAddr, data, recv.Add(time.Second))
	after := c.Status().Global
	if after.Blocked-before.Blocked != 2 {
		t.Fatalf("blocked datagrams = %d, want 2", after.Blocked-before.Blocked)
	}
	if exps, _ := queries.ListFlowExporters(db); len(exps) != 0 {
		t.Fatalf("a deleted exporter was accepted again: %+v", exps)
	}
	st := c.Status()
	if len(st.Unknown) != 0 || len(st.Blocked) != 1 || st.Blocked[0].Source != queries.FlowBlockDeleted ||
		st.Blocked[0].Reason != "exporter deleted by admin" || st.Blocked[0].Datagrams != 2 {
		t.Fatalf("status after delete: unknown %+v blocked %+v", st.Unknown, st.Blocked)
	}
	// A router's tombstone is a block, not an alarm.
	if f := exportFinding(t, db, "edge"); f != nil {
		t.Fatalf("a blocked router was flagged: %+v", f)
	}

	// The explicit re-add.
	id, err := queries.CreateFlowExporterChecked(db, &queries.FlowExporter{Name: "edge", Address: routerAddr.String(),
		Kind: "routeros", DeviceID: "edge", Enabled: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	c.reconcile(recv)
	feed(c, routerAddr, tmpl, recv.Add(2*time.Second))
	feed(c, routerAddr, data, recv.Add(2*time.Second))
	if es := statusOf(t, c, id); es.Counters["datagrams"] != 2 || es.Counters["flows"] != 5 {
		t.Fatalf("after the re-add: %+v", es.Counters)
	}
	if got := c.Status().Blocked; len(got) != 0 {
		t.Fatalf("still listed as blocked: %+v", got)
	}
}

// An exporter that was accepted before — here on a device that turns out to
// be a switch — keeps working: its datagrams are decoded as before. It is
// flagged, not blocked.
func TestExistingExporterOnSwitchKeepsWorking(t *testing.T) {
	db := testDB(t)
	seedRoleDevice(t, db, "sw", switchAddr, "CRS326-24G-2S+", devclass.C2, "gateway")
	id := seedExporter(t, db, queries.FlowExporter{Name: "sw", Address: switchAddr.String(), Kind: "routeros",
		DeviceID: "sw", Auto: true})
	c, recv := guardCollector(t, db)
	tmpl, data := guardPackets(switchAddr, 1)
	feed(c, switchAddr, tmpl, recv)
	feed(c, switchAddr, data, recv)
	es := statusOf(t, c, id)
	if es.Counters["datagrams"] != 2 || es.Counters["flows"] != 5 || es.State != StateOK {
		t.Fatalf("an existing exporter stopped working: %+v %s", es.Counters, es.State)
	}
	if es.DeviceRole != string(devclass.RoleSwitch) || es.ExportWarning == "" || es.DeviceRoleReason == "" {
		t.Fatalf("status does not flag the exporter: role %q warning %q", es.DeviceRole, es.ExportWarning)
	}
	if list, _ := queries.ListFlowSenderPolicies(db); len(list) != 0 {
		t.Fatalf("an accepted exporter was blocked: %+v", list)
	}
	if exps, _ := queries.ListFlowExporters(db); len(exps) != 1 || !exps[0].Enabled {
		t.Fatalf("exporters = %+v", exps)
	}
	f := exportFinding(t, db, "sw")
	if f == nil || f.Severity != "warn" || f.Count != 1 {
		t.Fatalf("finding = %+v, want one unconfirmed finding for two datagrams in the same minute", f)
	}
	// A router's exporter is not flagged at all.
	seedRoleDevice(t, db, "edge", routerAddr, "RB5009UG+S+", devclass.C3, "egress")
	rid := seedExporter(t, db, queries.FlowExporter{Name: "edge", Address: routerAddr.String(), Kind: "routeros", DeviceID: "edge"})
	c.reconcile(recv)
	rt, rd := guardPackets(routerAddr, 1)
	feed(c, routerAddr, rt, recv)
	feed(c, routerAddr, rd, recv)
	if es := statusOf(t, c, rid); es.ExportWarning != "" || es.DeviceRole != string(devclass.RoleRouter) {
		t.Fatalf("router exporter flagged: %+v", es)
	}
	if f := exportFinding(t, db, "edge"); f != nil {
		t.Fatalf("router flagged: %+v", f)
	}
}

// exportCheckFixture is a switch that sent a flow datagram, with a pooled
// session to a fake device that records every command.
type exportCheckFixture struct {
	db   *sql.DB
	c    *Collector
	fake *routerostest.Device
	recv time.Time
}

func newExportCheckFixture(t *testing.T, enabled string, targets ...map[string]string) *exportCheckFixture {
	t.Helper()
	routeros.SetDialPolicy(nil)
	t.Cleanup(func() { routeros.SetDialPolicy(nil) })
	db := testDB(t)
	seedRoleDevice(t, db, "sw", switchAddr, "CRS326-24G-2S+", devclass.C2, "gateway")
	pool := routeros.NewPool(false)
	fake := routerostest.NewDevice(t)
	fake.Dial(t, pool, "sw")
	fake.Reply("/ip/traffic-flow/print", map[string]string{"enabled": enabled, "interfaces": "all"})
	fake.Reply("/ip/traffic-flow/target/print", targets...)
	c := New(db, pool, nil, Config{Listen: []string{":2055"}})
	c.syncWrites = true
	recv := tLab.Add(lag)
	c.now = func() time.Time { return recv }
	c.reconcile(recv)
	tmpl, _ := guardPackets(switchAddr, 1)
	feed(c, switchAddr, tmpl, recv)
	return &exportCheckFixture{db: db, c: c, fake: fake, recv: recv}
}

var exportTarget = map[string]string{"dst-address": "198.51.100.5", "port": "2055", "version": "ipfix", "disabled": "false"}

// The datagram alone leaves the finding unconfirmed. One read of the
// device's traffic-flow settings — two proplisted prints on the pooled
// session — confirms it; the read is not repeated within the hour.
func TestExportCheckConfirmsOncePerHour(t *testing.T) {
	f := newExportCheckFixture(t, "true", exportTarget)
	if got := f.fake.Commands(); len(got) != 0 {
		t.Fatalf("the datagram itself reached the device: %q", got)
	}
	if fd := exportFinding(t, f.db, "sw"); fd == nil || fd.Severity != "warn" {
		t.Fatalf("before the read: %+v", fd)
	}
	// The sighting asked for the confirmation read.
	select {
	case id := <-f.c.exportCheckReq:
		if id != "sw" {
			t.Fatalf("check requested for %q", id)
		}
	default:
		t.Fatal("no confirmation read was requested")
	}

	if !f.c.checkExport("sw", f.recv) {
		t.Fatal("the read did not run")
	}
	wantCmds := []string{"/ip/traffic-flow/print", "/ip/traffic-flow/target/print"}
	if got := f.fake.Commands(); !reflect.DeepEqual(got, wantCmds) {
		t.Fatalf("commands = %q, want %q", got, wantCmds)
	}
	for i, args := range f.fake.Args() {
		if len(args) != 1 || !strings.HasPrefix(args[0], "=.proplist=") {
			t.Errorf("command %d is not a proplisted print: %q", i, args)
		}
	}
	chk, _ := queries.GetFlowExportCheck(f.db, "sw")
	if chk == nil || chk.Result != queries.FlowExportExporting || !chk.CheckedAt.Equal(f.recv.Truncate(time.Second)) ||
		!strings.Contains(chk.Detail, "198.51.100.5:2055") {
		t.Fatalf("check row = %+v", chk)
	}
	if fd := exportFinding(t, f.db, "sw"); fd == nil || fd.Severity != "critical" {
		t.Fatalf("after a confirming read: %+v", fd)
	}

	// Within the hour: no further command, whatever asks.
	for _, d := range []time.Duration{time.Second, 30 * time.Minute, 59 * time.Minute} {
		if f.c.checkExport("sw", f.recv.Add(d)) {
			t.Fatalf("read again after %s", d)
		}
	}
	if got := f.fake.Commands(); len(got) != 2 {
		t.Fatalf("commands within the hour = %q", got)
	}
	// A later sighting keeps the confirmed severity.
	later := f.recv.Add(10 * time.Minute)
	f.c.reconcile(later)
	tmpl, _ := guardPackets(switchAddr, 2)
	feed(f.c, switchAddr, tmpl, later)
	if fd := exportFinding(t, f.db, "sw"); fd == nil || fd.Severity != "critical" || fd.Count < 2 {
		t.Fatalf("a later sighting dropped the confirmation: %+v", fd)
	}
	// After the hour the device is asked again — and has stopped exporting.
	f.fake.Reply("/ip/traffic-flow/print", map[string]string{"enabled": "false"})
	f.fake.Reply("/ip/traffic-flow/target/print")
	if !f.c.checkExport("sw", f.recv.Add(61*time.Minute)) {
		t.Fatal("no re-check after the hour")
	}
	if got := f.fake.Commands(); len(got) != 4 {
		t.Fatalf("commands after the hour = %q", got)
	}
	chk, _ = queries.GetFlowExportCheck(f.db, "sw")
	if chk.Result != queries.FlowExportNotExporting {
		t.Fatalf("check row after the device stopped = %+v", chk)
	}
	if fd := exportFinding(t, f.db, "sw"); fd == nil || fd.Severity != "warn" {
		t.Fatalf("finding after the device stopped: %+v", fd)
	}
}

// A device that says it does not export leaves the finding unconfirmed: a
// forged source address must never become critical.
func TestExportCheckDoesNotConfirmASpoofedSource(t *testing.T) {
	for name, f := range map[string]*exportCheckFixture{
		"traffic-flow disabled":      newExportCheckFixture(t, "false"),
		"enabled without a target":   newExportCheckFixture(t, "true"),
		"enabled, target disabled":   newExportCheckFixture(t, "true", map[string]string{"dst-address": "198.51.100.5", "port": "2055", "disabled": "true"}),
		"disabled, target left over": newExportCheckFixture(t, "false", exportTarget),
	} {
		if !f.c.checkExport("sw", f.recv) {
			t.Fatalf("%s: the read did not run", name)
		}
		chk, _ := queries.GetFlowExportCheck(f.db, "sw")
		if chk == nil || chk.Result != queries.FlowExportNotExporting {
			t.Errorf("%s: check row = %+v", name, chk)
		}
		if fd := exportFinding(t, f.db, "sw"); fd == nil || fd.Severity != "warn" {
			t.Errorf("%s: finding = %+v, want it to stay a warning", name, fd)
		}
	}
}

// The read never runs on a paused device, never while polling is off, never
// without a sighting, and a failed read is not a confirmation.
func TestExportCheckGuards(t *testing.T) {
	t.Run("paused", func(t *testing.T) {
		f := newExportCheckFixture(t, "true", exportTarget)
		if err := queries.PauseDevicePolling(f.db, "sw", f.recv.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if f.c.checkExport("sw", f.recv) || len(f.fake.Commands()) != 0 {
			t.Fatalf("a paused device was read: %q", f.fake.Commands())
		}
		// The pause does not use up the hour: once it ends, the read runs.
		if !f.c.checkExport("sw", f.recv.Add(2*time.Hour)) {
			t.Fatal("no read after the pause ended")
		}
	})
	t.Run("polling off", func(t *testing.T) {
		f := newExportCheckFixture(t, "true", exportTarget)
		routeros.SetDialPolicy(&routeros.DialPolicy{Enabled: false})
		if f.c.checkExport("sw", f.recv) || len(f.fake.Commands()) != 0 {
			t.Fatalf("a device was read with polling off: %q", f.fake.Commands())
		}
	})
	t.Run("no sighting", func(t *testing.T) {
		f := newExportCheckFixture(t, "true", exportTarget)
		if err := queries.DeleteFlowExportCheck(f.db, "sw"); err != nil {
			t.Fatal(err)
		}
		if f.c.checkExport("sw", f.recv) || len(f.fake.Commands()) != 0 {
			t.Fatalf("read without a sighting: %q", f.fake.Commands())
		}
	})
	t.Run("no pooled session", func(t *testing.T) {
		f := newExportCheckFixture(t, "true", exportTarget)
		f.c.pool = routeros.NewPool(false)
		if f.c.checkExport("sw", f.recv) {
			t.Fatal("read without a session")
		}
		chk, _ := queries.GetFlowExportCheck(f.db, "sw")
		if chk == nil || !chk.CheckedAt.IsZero() {
			t.Fatalf("a read that could not start used up the hour: %+v", chk)
		}
	})
	t.Run("failed read", func(t *testing.T) {
		f := newExportCheckFixture(t, "true", exportTarget)
		f.fake.SetTrap("/ip/traffic-flow/print", "not enough permissions (9)")
		if !f.c.checkExport("sw", f.recv) {
			t.Fatal("the read did not run")
		}
		chk, _ := queries.GetFlowExportCheck(f.db, "sw")
		if chk == nil || chk.Result != queries.FlowExportCheckFailed || chk.CheckedAt.IsZero() {
			t.Fatalf("check row = %+v", chk)
		}
		if fd := exportFinding(t, f.db, "sw"); fd == nil || fd.Severity != "warn" {
			t.Fatalf("a failed read confirmed the export: %+v", fd)
		}
		// A failed read counts against the hour too: the device is not asked
		// again a minute later.
		if f.c.checkExport("sw", f.recv.Add(time.Minute)) {
			t.Fatal("re-read right after a failure")
		}
	})
}

// The G6 seam: the accept hook runs once when the collector starts accepting
// an exporter — auto-accepted or added by an admin — and not for the
// exporters it finds at startup.
func TestAcceptHook(t *testing.T) {
	db := testDB(t)
	seedRoleDevice(t, db, "edge", routerAddr, "RB5009UG+S+", devclass.C3, "egress")
	seedExporter(t, db, queries.FlowExporter{Name: "old", Address: otherAddr.String(), Kind: "other"})
	if err := queries.SetSetting(db, "flow_auto_accept_classes", "router"); err != nil {
		t.Fatal(err)
	}
	c := newTestCollector(t, db)
	var accepted []string
	c.UseAcceptHook(func(e queries.FlowExporter, _ time.Time) { accepted = append(accepted, e.Name) })
	recv := tLab.Add(lag)
	c.now = func() time.Time { return recv }
	c.reconcile(recv)
	if len(accepted) != 0 {
		t.Fatalf("hook ran for exporters present at startup: %v", accepted)
	}
	tmpl, data := guardPackets(routerAddr, 1)
	feed(c, routerAddr, tmpl, recv)
	feed(c, routerAddr, data, recv)
	if !reflect.DeepEqual(accepted, []string{"edge"}) {
		t.Fatalf("after an auto-accept: %v", accepted)
	}
	// An admin adds one: the next reconcile (Reload) sees it.
	if _, err := queries.CreateFlowExporter(db, &queries.FlowExporter{Name: "firewall", Address: "198.51.100.9", Kind: "opnsense", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	c.reconcile(recv)
	c.reconcile(recv)
	if !reflect.DeepEqual(accepted, []string{"edge", "firewall"}) {
		t.Fatalf("after an admin add: %v", accepted)
	}
}

// The whole path with its real goroutines (and the race detector): a socket
// reader sees flow datagrams from a switch's address, the worker blocks it
// and records the sighting, the maintenance loop is asked for the read (and
// has no session to run it on), and the drops are persisted at shutdown.
func TestGuardThroughRunningCollector(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := testDB(t)
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("no loopback UDP: %v", err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	loop := netip.MustParseAddr("127.0.0.1")
	seedRoleDevice(t, db, "sw", loop, "CRS326-24G-2S+", devclass.C2, "gateway")
	c := New(db, routeros.NewPool(false), nil, Config{Listen: []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()

	src, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	deadline := time.Now().Add(10 * time.Second)
	for c.Status().StartedAt == nil {
		if time.Now().After(deadline) {
			t.Fatal("collector never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tmpl, _ := guardPackets(loop, 1)
	// Re-sent until the worker has blocked the sender (a datagram written
	// before the socket is bound is lost on loopback).
	for len(c.Status().Blocked) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the switch was never blocked: %+v", c.Status())
		}
		_, _ = src.Write(tmpl)
		time.Sleep(20 * time.Millisecond)
	}
	sent := c.Status().Global.Datagrams
	_, _ = src.Write(tmpl)
	for c.Status().Global.Datagrams <= sent {
		if time.Now().After(deadline) {
			t.Fatal("the datagram after the block never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := c.Status()
	if st.Global.Blocked != st.Global.Datagrams || len(st.Exporters) != 0 || st.Blocked[0].Address != "127.0.0.1" {
		t.Fatalf("status = global %+v exporters %+v blocked %+v", st.Global, st.Exporters, st.Blocked)
	}
	cancel()
	if !c.Wait(8 * time.Second) {
		t.Fatal("Wait: Run did not finish after cancel")
	}
	<-done
	list, _ := queries.ListFlowSenderPolicies(db)
	if len(list) != 1 || list[0].Source != queries.FlowBlockAuto || list[0].Datagrams != st.Global.Blocked || list[0].LastSeen.IsZero() {
		t.Fatalf("persisted block list = %+v, want %d dropped datagrams", list, st.Global.Blocked)
	}
	if f := exportFinding(t, db, "sw"); f == nil || f.Severity != "warn" {
		t.Fatalf("finding = %+v", f)
	}
	if chk, _ := queries.GetFlowExportCheck(db, "sw"); chk == nil || chk.CollectorPort != port || !chk.CheckedAt.IsZero() {
		t.Fatalf("check row = %+v (no session: the read must not have run)", chk)
	}
}
