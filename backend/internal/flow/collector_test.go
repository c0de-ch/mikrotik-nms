package flow

import (
	"context"
	"database/sql"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
)

var (
	tLab    = time.Date(2026, 9, 28, 12, 30, 20, 0, time.UTC) // export time of the lab datagrams
	rosAddr = netip.MustParseAddr("192.168.78.202")
	opnAddr = netip.MustParseAddr("192.168.78.81")
	lag     = 500 * time.Millisecond
)

func newTestCollector(t *testing.T, db *sql.DB) *Collector {
	t.Helper()
	c := New(db, nil, nil, Config{Listen: []string{":2055"}})
	c.syncWrites = true
	return c
}

func seedDevice(t *testing.T, db *sql.DB, id, addr, identity string) {
	t.Helper()
	if err := queries.CreateDevice(db, &queries.Device{ID: id, Address: addr, Identity: identity, Username: "u",
		APIPort: 8728, Status: "online"}); err != nil {
		t.Fatalf("device %s: %v", id, err)
	}
}

func seedExporter(t *testing.T, db *sql.DB, e queries.FlowExporter) int64 {
	t.Helper()
	e.Enabled = true
	id, err := queries.CreateFlowExporter(db, &e)
	if err != nil {
		t.Fatalf("exporter %s: %v", e.Name, err)
	}
	return id
}

// feed passes one datagram through the reader path and the worker.
func feed(c *Collector, addr netip.Addr, pkt []byte, at time.Time) {
	c.handlePacket(pkt, addr, at)
	c.drainQueue()
}

func rosPackets(export time.Time, seq uint32) (tmpl, data []byte) {
	sysInit := uint64(export.Add(-72 * time.Hour).UnixMilli())
	flows := pktgen.LabRouterOS(export, sysInit, true, rosAddr.String(), "192.168.79.216", 2055)
	return pktgen.RouterOSIPFIXTemplates(uint32(export.Unix()), seq, 0, true),
		pktgen.RouterOSIPFIXData(uint32(export.Unix()), seq, 0, flows...)
}

func aggAll(t *testing.T, db *sql.DB, exp int64) []queries.FlowAgg {
	t.Helper()
	ifs := make([]uint32, 0, 64)
	for i := uint32(1); i <= 64; i++ {
		ifs = append(ifs, i)
	}
	rows, err := queries.AggregateFlows(db, queries.Flows1m, exp, ifs, tLab.Add(-time.Hour), tLab.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func statusOf(t *testing.T, c *Collector, id int64) ExporterStatus {
	t.Helper()
	c.rebuildStatus(c.now())
	for _, e := range c.Status().Exporters {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("exporter %d not in status", id)
	return ExporterStatus{}
}

func TestCollectorRouterOSNATAndSelfExport(t *testing.T) {
	db := testDB(t)
	seedDevice(t, db, "rb", rosAddr.String(), "switch002-rb5009")
	id := seedExporter(t, db, queries.FlowExporter{Name: "switch002-rb5009", Address: rosAddr.String(), Kind: "routeros", DeviceID: "rb"})
	c := newTestCollector(t, db)
	recv := tLab.Add(lag)
	c.now = func() time.Time { return recv }
	c.reconcile(recv)

	tmpl, data := rosPackets(tLab, 1)
	feed(c, rosAddr, tmpl, recv)
	feed(c, rosAddr, data, recv)
	c.flushDue(recv.Add(4 * time.Minute))

	rows := aggAll(t, db, id)
	var r1Bytes int64
	for _, r := range rows {
		if r.Dst == netip.MustParseAddr("100.114.241.213") {
			t.Errorf("CGNAT address stored as dst: %+v (NAT inside view)", r)
		}
		if r.Proto == 17 && r.Src == rosAddr {
			t.Errorf("self-export row stored: %+v", r)
		}
		if r.InIf == 1 && r.OutIf == 13 && r.Src == netip.MustParseAddr("142.250.203.100") &&
			r.Dst == netip.MustParseAddr("192.168.28.33") && r.Port == 443 {
			r1Bytes += r.Bytes
		}
		if r.InIf == 10 && r.Dst != rosAddr {
			t.Errorf("R4 (226 = 0.0.0.0) must keep dst: %+v", r)
		}
	}
	if r1Bytes != 5_000_000_000 {
		t.Fatalf("R1 bytes = %d, want 5e9 (NAT dst 192.168.28.33, exact across minutes)", r1Bytes)
	}
	sum, err := queries.FlowMetaSummary(db, queries.Flows1m, id, tLab.Add(-time.Hour), tLab.Add(time.Hour))
	if err != nil || sum.Buckets != 3 || sum.Bytes != 5_000_159_000 || sum.Records != 5 || sum.Datagrams != 2 {
		t.Fatalf("meta = %+v, %v", sum, err)
	}
	if got := c.FlushedThrough(); !got.Equal(time.Date(2026, 9, 28, 12, 32, 0, 0, time.UTC)) {
		t.Fatalf("flushed through = %v", got)
	}
	st := statusOf(t, c, id)
	if st.Counters["self_export_dropped"] != 1 || st.Counters["nat_dst_records"] != 1 || st.Counters["datagrams"] != 2 ||
		st.Counters["flows"] != 5 || st.Protocol != "ipfix" || st.State != StateOK || st.Templates != 2 ||
		st.TemplateState != TemplateOK || st.TimeSource["sysuptime"] != 6 {
		t.Fatalf("status = %+v", st)
	}
	if n := countTable(t, db, "flow_templates"); n != 2 {
		t.Fatalf("templates persisted = %d, want 2", n)
	}

	// Interfaces seen and native points (names unknown yet: if#N, down).
	ifs, _ := queries.ListFlowIfaces(db, id)
	var idxs []uint32
	for _, f := range ifs {
		idxs = append(idxs, f.IfIndex)
		if f.Source != "learned" || f.LastSeen.IsZero() {
			t.Errorf("iface row %+v", f)
		}
	}
	if !slices.Equal(idxs, []uint32{1, 10, 13}) {
		t.Fatalf("ifaces touched = %v, want [1 10 13] (0 never a row)", idxs)
	}
	points, _ := queries.ListFlowPoints(db)
	if len(points) != 3 || points[0].Name != "switch002-rb5009 · if#1" || points[0].PortSide != "same" ||
		points[0].DeviceID != "rb" || points[0].Facing != "down" || points[0].LocalInternal {
		t.Fatalf("native points = %+v", points)
	}

	// ifsync names them and the uplink poller knows eth1-wan is the WAN:
	// the reconcile renames and turns eth1-wan up.
	if err := queries.UpsertDeviceFlowIfaces(db, id, []queries.FlowIface{
		{IfIndex: 1, Name: "eth1-wan", Type: "ether"}, {IfIndex: 10, Name: "bridge", Type: "bridge"},
		{IfIndex: 13, Name: "net28", Type: "vlan", VLANID: 28, Parent: "bridge"}}); err != nil {
		t.Fatal(err)
	}
	if err := queries.ReplaceDeviceUplinks(db, "rb", []queries.DeviceUplink{
		{DeviceID: "rb", Kind: "default-route", Interface: "eth1-wan", IfaceType: "ether", GatewayIP: "100.64.0.1"}}); err != nil {
		t.Fatal(err)
	}
	reconcileNativePoints(db)
	points, _ = queries.ListFlowPoints(db)
	facing := map[string]string{}
	for _, p := range points {
		facing[p.Name] = p.Facing + "/" + p.Iface
	}
	want := map[string]string{"switch002-rb5009 · eth1-wan": "up/eth1-wan", "switch002-rb5009 · bridge": "down/bridge",
		"switch002-rb5009 · net28": "down/net28"}
	for name, f := range want {
		if facing[name] != f {
			t.Errorf("point %q = %q, want %q (all: %v)", name, facing[name], f, facing)
		}
	}

	// The facing does not flip back when the uplink rows go stale (device
	// not polled) or the WAN default route disappears (link down).
	wanFacing := func() string {
		points, _ := queries.ListFlowPoints(db)
		for _, p := range points {
			if p.Iface == "eth1-wan" {
				return p.Facing
			}
		}
		return "?"
	}
	if _, err := db.Exec(`UPDATE device_uplinks SET last_seen = '2000-01-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}
	reconcileNativePoints(db)
	if f := wanFacing(); f != "up" {
		t.Fatalf("stale uplinks flipped eth1-wan to %s", f)
	}
	if err := queries.ReplaceDeviceUplinks(db, "rb", nil); err != nil {
		t.Fatal(err)
	}
	reconcileNativePoints(db)
	if f := wanFacing(); f != "up" {
		t.Fatalf("a vanished default route flipped eth1-wan to %s", f)
	}
}

func TestCollectorOPNsenseReplayDupAndHints(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "firewall003", Address: opnAddr.String(), Kind: "opnsense",
		NATAddresses: []netip.Addr{netip.MustParseAddr("192.168.28.81")}})
	c := newTestCollector(t, db)
	recv := tLab.Add(lag)
	c.now = func() time.Time { return recv }
	c.reconcile(recv)

	up := uint32(500_000_000)
	flows := pktgen.LabOPNsense(up, opnAddr.String(), "192.168.79.216", 2055)
	unix := uint32(tLab.Unix())
	// Data first (interleaved node counters: seq 10, then 3), then an exact
	// duplicate of O1 (captured on a second interface), then templates.
	feed(c, opnAddr, pktgen.OPNsenseV9Data(unix, up, 10, flows...), recv)
	if st := statusOf(t, c, id); st.TemplateState != TemplateWaiting || st.Counters["template_misses"] != 2 {
		t.Fatalf("before templates: %+v", st)
	}
	feed(c, opnAddr, pktgen.OPNsenseV9Templates(unix+5, up+5000, 3), recv.Add(5*time.Second))
	feed(c, opnAddr, pktgen.OPNsenseV9Data(unix+6, up+6000, 4, flows[0]), recv.Add(6*time.Second))
	c.flushDue(recv.Add(5 * time.Minute))

	st := statusOf(t, c, id)
	if st.Counters["replayed"] != 2 || st.Counters["dup_dropped"] != 1 || st.Counters["self_export_dropped"] != 1 ||
		st.Counters["seq_lost"] != 0 || st.Counters["flows"] != 3 || st.TemplateState != TemplateOK || st.Protocol != "netflow9" {
		t.Fatalf("status = %+v", st)
	}
	rows := aggAll(t, db, id)
	var up1, down1 int64
	for _, r := range rows {
		if r.InIf == 1 && r.OutIf == 7 && r.Src.Is4() {
			up1 += r.Bytes
		}
		if r.InIf == 7 && r.OutIf == 1 {
			down1 += r.Bytes
		}
	}
	if up1 != 1480 || down1 != 64_000 {
		t.Fatalf("O1/O2 bytes = %d / %d (duplicate must not double O1)", up1, down1)
	}
	ifs, _ := queries.ListFlowIfaces(db, id)
	hints := map[uint32]string{}
	for _, f := range ifs {
		hints[f.IfIndex] = f.Hint
	}
	// bytes descending: O3 (2000 B) before O1 (1480 B)
	if hints[1] != "2a01:4f8:202:13d1::/64,192.168.78.0/24" || hints[7] != "9.9.9.0/24" {
		t.Fatalf("hints = %v", hints)
	}
	points, _ := queries.ListFlowPoints(db)
	byIdx := map[uint32]queries.FlowPoint{}
	for _, p := range points {
		byIdx[p.IfIndexes[0]] = p
	}
	lan, wan := byIdx[1], byIdx[7]
	if lan.Facing != "down" || !lan.LocalInternal || lan.PortSide != "" || lan.DeviceID != "" || lan.Name != "firewall003 · if#1" {
		t.Fatalf("LAN point = %+v", lan)
	}
	if wan.Facing != "up" || !wan.LocalInternal {
		t.Fatalf("WAN point = %+v", wan)
	}
	// An admin names and roles the LAN: the auto point follows.
	_ = queries.SetFlowIfaceManual(db, id, 1, "LAN", "lan")
	_ = queries.SetFlowIfaceManual(db, id, 7, "", "lan")
	reconcileNativePoints(db)
	points, _ = queries.ListFlowPoints(db)
	names := map[string]string{}
	for _, p := range points {
		names[p.Name] = p.Facing
	}
	if names["firewall003 · LAN"] != "down" || names["firewall003 · if#7"] != "down" {
		t.Fatalf("after naming: %v", names)
	}
}

func TestCollectorTemplatePersistenceAcrossRestart(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "firewall003", Address: opnAddr.String(), Kind: "opnsense"})
	recv := tLab.Add(lag)
	up := uint32(500_000_000)
	unix := uint32(tLab.Unix())

	c1 := newTestCollector(t, db)
	c1.now = func() time.Time { return recv }
	c1.reconcile(recv)
	feed(c1, opnAddr, pktgen.OPNsenseV9Templates(unix, up, 1), recv)
	// Re-learned within a minute: no second upsert (throttle), still 2 rows.
	feed(c1, opnAddr, pktgen.OPNsenseV9Templates(unix+10, up+10_000, 2), recv.Add(10*time.Second))
	if n := countTable(t, db, "flow_templates"); n != 2 {
		t.Fatalf("persisted templates = %d", n)
	}

	// Restart: a new process decodes data at once from persisted templates.
	c2 := newTestCollector(t, db)
	c2.now = func() time.Time { return recv.Add(time.Minute) }
	c2.reconcile(recv.Add(time.Minute))
	if st := statusOf(t, c2, id); st.TemplateState != TemplatePersisted || st.Templates != 2 {
		t.Fatalf("after restart: %+v", st)
	}
	flows := pktgen.LabOPNsense(up+60_000, opnAddr.String(), "192.168.79.216", 2055)
	feed(c2, opnAddr, pktgen.OPNsenseV9Data(unix+60, up+60_000, 7, flows...), recv.Add(time.Minute))
	st := statusOf(t, c2, id)
	if st.Counters["flows"] != 3 || st.Counters["template_misses"] != 0 {
		t.Fatalf("data after restart: %+v", st)
	}

	// A reboot (uptime back by 10 min) deletes the persisted templates.
	feed(c2, opnAddr, pktgen.OPNsenseV9Data(unix+61, up+61_000-600_000, 8, flows[0]), recv.Add(61*time.Second))
	if n := countTable(t, db, "flow_templates"); n != 0 {
		t.Fatalf("templates after reboot = %d, want 0", n)
	}
}

func TestCollectorMetaAliveAndAdditiveRestart(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "rb", Address: rosAddr.String(), Kind: "routeros"})
	recv := tLab.Add(lag) // 12:30:20.5
	c := newTestCollector(t, db)
	c.now = func() time.Time { return recv }
	c.reconcile(recv)
	tmpl, data := rosPackets(tLab, 1)
	feed(c, rosAddr, tmpl, recv) // templates only: alive 12:28..12:30, no records
	c.flushDue(recv.Add(10 * time.Minute))
	buckets, _ := queries.FlowMetaBuckets(db, queries.Flows1m, id, tLab.Add(-time.Hour), tLab.Add(time.Hour))
	if len(buckets) != 3 || buckets[0].Minute() != 28 || buckets[2].Minute() != 30 {
		t.Fatalf("alive meta buckets = %v (dead minutes must have none)", buckets)
	}
	if n := countTable(t, db, "flow_1m"); n != 0 {
		t.Fatalf("rows without records = %d", n)
	}

	// Shutdown mid-minute, restart, more data for the same minute: additive.
	recv2 := tLab.Add(20 * time.Minute) // 12:50:20
	c1 := newTestCollector(t, db)
	c1.now = func() time.Time { return recv2 }
	c1.reconcile(recv2)
	_, data = rosPackets(recv2.Add(-lag), 2)
	feed(c1, rosAddr, tmpl, recv2)
	feed(c1, rosAddr, data, recv2)
	c1.flushAll(recv2)
	c2 := newTestCollector(t, db)
	c2.now = func() time.Time { return recv2.Add(10 * time.Second) }
	c2.reconcile(recv2)
	feed(c2, rosAddr, data, recv2.Add(10*time.Second)) // persisted templates
	c2.flushDue(recv2.Add(5 * time.Minute))
	sum, _ := queries.FlowMetaSummary(db, queries.Flows1m, id, recv2.Add(-5*time.Minute), recv2.Add(5*time.Minute))
	if sum.Datagrams != 3 || sum.Bytes != 2*5_000_159_000 {
		t.Fatalf("after restart: %+v (double flush must accumulate)", sum)
	}
}

func TestCollectorFlushTiming(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "rb", Address: rosAddr.String(), Kind: "routeros"})
	recv := tLab.Add(lag)
	c := newTestCollector(t, db)
	c.reconcile(recv)
	tmpl, data := rosPackets(tLab, 1)
	feed(c, rosAddr, tmpl, recv)
	feed(c, rosAddr, data, recv)
	m := time.Date(2026, 9, 28, 12, 30, 0, 0, time.UTC)
	c.flushDue(m.Add(179 * time.Second))
	if b, ok, _ := queries.LatestFlowBucket(db, queries.Flows1m, id); ok && !b.Before(m) {
		t.Fatalf("minute 12:30 flushed before M+180 s")
	}
	c.flushDue(m.Add(180 * time.Second))
	if b, ok, _ := queries.LatestFlowBucket(db, queries.Flows1m, id); !ok || !b.Equal(m) {
		t.Fatalf("minute 12:30 not flushed at M+180 s: %v %v", b, ok)
	}
}

func TestCollectorAutoAcceptOnce(t *testing.T) {
	db := testDB(t)
	seedDevice(t, db, "rb", rosAddr.String(), "switch002-rb5009")
	c := newTestCollector(t, db)
	recv := tLab.Add(lag)
	c.now = func() time.Time { return recv }
	c.reconcile(recv)
	tmpl, data := rosPackets(tLab, 1)
	feed(c, rosAddr, tmpl, recv)
	feed(c, rosAddr, data, recv)
	exps, _ := queries.ListFlowExporters(db)
	if len(exps) != 1 || !exps[0].Auto || exps[0].Kind != "routeros" || exps[0].DeviceID != "rb" ||
		exps[0].Name != "switch002-rb5009" || !exps[0].Enabled {
		t.Fatalf("auto-accepted exporters = %+v", exps)
	}
	if st := statusOf(t, c, exps[0].ID); st.Counters["datagrams"] != 2 || st.Counters["flows"] != 5 {
		t.Fatalf("both datagrams decoded: %+v", st.Counters)
	}

	// Auto-accept off: a device's datagrams are an unknown sender with its id.
	db2 := testDB(t)
	seedDevice(t, db2, "rb", rosAddr.String(), "switch002-rb5009")
	_ = queries.SetSetting(db2, "flow_auto_accept_devices", "false")
	c2 := newTestCollector(t, db2)
	c2.reconcile(recv)
	feed(c2, rosAddr, tmpl, recv)
	if exps, _ := queries.ListFlowExporters(db2); len(exps) != 0 {
		t.Fatalf("exporter created with auto-accept off: %+v", exps)
	}
	unk := c2.Status().Unknown
	if len(unk) != 1 || unk[0].DeviceID == nil || *unk[0].DeviceID != "rb" || unk[0].DeviceName != "switch002-rb5009" ||
		unk[0].Protocol != "ipfix" {
		t.Fatalf("unknown senders = %+v", unk)
	}
}

func TestCollectorRollupLookback(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "rb", Address: rosAddr.String(), Kind: "routeros"})
	end := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	var rows []queries.FlowRow
	var meta []queries.FlowMeta
	for h := 1; h <= 5; h++ {
		b := end.Add(-time.Duration(h) * time.Hour)
		rows = append(rows, queries.FlowRow{ExporterID: id, Bucket: b, InIf: 1, OutIf: 2, Src: rosAddr, Dst: opnAddr, Bytes: 100})
		meta = append(meta, queries.FlowMeta{ExporterID: id, Bucket: b, Datagrams: 1})
	}
	if err := queries.InsertFlows1m(db, rows, meta); err != nil {
		t.Fatal(err)
	}
	c := newTestCollector(t, db)
	c.rollup(end, 5)
	var hours []int
	rs, _ := db.Query(`SELECT bucket FROM flow_1h_meta ORDER BY bucket`)
	for rs.Next() {
		var b int64
		_ = rs.Scan(&b)
		hours = append(hours, int(end.Sub(time.Unix(b, 0))/time.Hour))
	}
	rs.Close()
	if !slices.Equal(hours, []int{3, 2, 1}) || !c.RolledThrough().Equal(end) {
		t.Fatalf("rolled hours back = %v (look-back 3 h), rolled through %v", hours, c.RolledThrough())
	}
	// A flush crossing the hour boundary re-rolls the two previous hours.
	c2 := newTestCollector(t, db)
	c2.reconcile(end)
	c2.nextFlush = end.Add(-time.Minute).Unix()
	c2.flushDue(end.Add(2 * time.Minute))
	if !c2.RolledThrough().Equal(end) || !c2.FlushedThrough().Equal(end) {
		t.Fatalf("hour crossing: rolled %v flushed %v", c2.RolledThrough(), c2.FlushedThrough())
	}
}

func TestCollectorDisabledExporterAndStatusShape(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "fw", Address: opnAddr.String(), Kind: "opnsense"})
	e, _ := queries.GetFlowExporter(db, id)
	e.Enabled = false
	_ = queries.UpdateFlowExporterConfig(db, e)
	c := newTestCollector(t, db)
	recv := tLab.Add(lag)
	c.reconcile(recv)
	c.handlePacket(pktgen.OPNsenseV9Templates(uint32(tLab.Unix()), 1, 1), opnAddr, recv)
	if len(c.queue) != 0 || c.global.rejected.Load() != 1 {
		t.Fatalf("disabled exporter: queued %d rejected %d", len(c.queue), c.global.rejected.Load())
	}
	s := c.Status()
	if !s.Enabled || len(s.Exporters) != 1 || s.Exporters[0].State != StateDisabled || s.Listen[0] != ":2055" ||
		s.ListenErrors == nil || s.Unknown == nil || len(s.Exporters[0].Counters) != len(ExporterCounterKeys) {
		t.Fatalf("status = %+v", s)
	}
	// Status is a deep copy.
	s.Exporters[0].Counters["datagrams"] = 99
	if c.Status().Exporters[0].Counters["datagrams"] == 99 {
		t.Fatal("Status leaked internal maps")
	}
}

// A listed exporter that sends a datagram failing the prechecks (here a v5
// header declaring 30 records with 1 present) is counted as rejected, and
// the rejection shows up as the exporter's last_error.
func TestCollectorRejectedDatagramSetsLastError(t *testing.T) {
	db := testDB(t)
	id := seedExporter(t, db, queries.FlowExporter{Name: "fw", Address: opnAddr.String(), Kind: "other"})
	c := newTestCollector(t, db)
	recv := tLab.Add(lag)
	c.reconcile(recv)
	feed(c, opnAddr, pktgen.V5(uint32(tLab.Unix()), 1_000_000, 30, 1), recv)
	s := statusOf(t, c, id)
	if c.global.rejected.Load() != 1 || s.Counters["decode_errors"] != 0 || s.Counters["datagrams"] != 1 {
		t.Fatalf("rejected %d, counters %v", c.global.rejected.Load(), s.Counters)
	}
	if !strings.Contains(s.LastError, "v5") || s.LastErrorAt == nil || !s.LastErrorAt.Equal(recv) {
		t.Fatalf("last_error %q at %v", s.LastError, s.LastErrorAt)
	}
}

func TestCollectorRunSocketAndGracefulShutdown(t *testing.T) {
	db := testDB(t)
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("no loopback UDP: %v", err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()
	id := seedExporter(t, db, queries.FlowExporter{Name: "fw", Address: "127.0.0.1", Kind: "opnsense"})
	c := New(db, nil, nil, Config{Listen: []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()

	src, err := net.DialUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	now := time.Now()
	up := uint32(900_000_000)
	unix := uint32(now.Unix())
	deadline := time.Now().Add(5 * time.Second)
	for c.Status().Global.Datagrams == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("collector never received a datagram")
		}
		_, _ = src.Write(pktgen.OPNsenseV9Templates(unix, up, 1))
		time.Sleep(20 * time.Millisecond)
	}
	flows := pktgen.LabOPNsense(up, "127.0.0.1", "127.0.0.1", uint16(port))
	_, _ = src.Write(pktgen.OPNsenseV9Data(unix, up, 2, flows...))
	for c.Status().Global.Datagrams < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if !c.Wait(8 * time.Second) {
		t.Fatal("Wait: Run did not finish after cancel")
	}
	<-done
	// The open minutes were flushed, the status and templates persisted.
	sum, _ := queries.FlowMetaSummary(db, queries.Flows1m, id, now.Add(-10*time.Minute), now.Add(10*time.Minute))
	if sum.Records != 3 || sum.Buckets == 0 {
		t.Fatalf("shutdown flush meta = %+v", sum)
	}
	e, _ := queries.GetFlowExporter(db, id)
	if e.Datagrams < 2 || e.Flows != 3 || e.Protocol != "netflow9" || e.LastSeen.IsZero() {
		t.Fatalf("persisted status = %+v", e)
	}
	if n := countTable(t, db, "flow_templates"); n != 2 {
		t.Fatalf("templates = %d", n)
	}
	if st := c.Status(); st.StartedAt == nil || st.RcvBufBytes <= 0 || len(st.ListenErrors) != 0 ||
		st.Global.SelfExportDropped != 1 {
		t.Fatalf("status after run = %+v", st)
	}
}

func TestCollectorListenErrorKeepsEnabled(t *testing.T) {
	c := New(testDB(t), nil, nil, Config{Listen: []string{"203.0.113.1:2055"}}) // not a local address
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(c.Status().ListenErrors) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	s := c.Status()
	if !s.Enabled || len(s.ListenErrors) != 1 || !strings.HasPrefix(s.ListenErrors[0], "203.0.113.1:2055: ") {
		t.Fatalf("status = %+v", s)
	}
}

func countTable(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
