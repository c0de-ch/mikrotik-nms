package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowview"
)

// The router's own interface addresses (host rows the collector's plan
// refresh writes next to the masked prefixes) classify as self at the
// router's points and keep their plan class elsewhere.
func TestFlowSelfClassFromPlanHostRows(t *testing.T) {
	f := newFlowFixture(t)
	now := time.Now()
	wanIP := netip.MustParseAddr("100.114.241.213")
	if err := queries.ReplaceFlowPrefixes(f.db, []queries.FlowPrefix{
		{Prefix: netip.MustParsePrefix("100.64.0.0/10"), Class: "transit", DeviceID: labRB, Iface: "eth1-wan", UpdatedAt: now},
		{Prefix: netip.PrefixFrom(wanIP, 32), Class: "transit", DeviceID: labRB, Iface: "eth1-wan", UpdatedAt: now},
		{Prefix: netip.MustParsePrefix("192.168.28.0/24"), Class: "internal", DeviceID: labRB, Iface: "net28", UpdatedAt: now},
		{Prefix: netip.MustParsePrefix("192.168.28.202/32"), Class: "internal", DeviceID: labRB, Iface: "net28", UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	env, err := f.s.loadFlowEnv(now)
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.s.newFlowDescriber(env, false)
	if err != nil {
		t.Fatal(err)
	}
	wan, _ := queries.GetFlowPoint(f.db, f.pWAN)
	lan, _ := queries.GetFlowPoint(f.db, f.pFwLAN)
	if c := d.planFor(*wan).Class(wanIP); c != flowview.ClassSelf {
		t.Errorf("RB5009 WAN address at the RB5009 point: %s, want self", c)
	}
	if c := d.planFor(*wan).Class(netip.MustParseAddr("192.168.28.202")); c != flowview.ClassSelf {
		t.Errorf("RB5009 net28 gateway at the RB5009 point: %s, want self", c)
	}
	if c := d.planFor(*lan).Class(wanIP); c != flowview.ClassExternal {
		t.Errorf("RB5009 WAN address at the firewall003 point: %s, want external", c)
	}
}

// A point filtering rows (local_internal) keeps exact coverage totals when
// Phase 1 missed minutes: only the missing minutes are re-read, with
// endpoints, and subtracted.
func TestFlowPortCoverageFilteredPointMissingMinutes(t *testing.T) {
	f := newFlowFixture(t)
	f.seedCounters(t, labSW, "ether1", 70000, 2000, 3, 4, 8)
	var r flowPortResponse
	if code, body := f.get(t, "/flows/port", map[string]string{"device": labSW, "iface": "ether1"}, &r); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	c := r.Coverage
	// Download: the LAN download (64000/min); the WAN-copy duplicate's
	// local is not internal. Upload: 1480/min. Counters: ether1 is a peer
	// port facing down → download = rx.
	if c.FlowDownBytes != 7*64000 || c.FlowUpBytes != 7*1480 || c.CounterDownBytes != 7*70000 || c.CounterUpBytes != 7*2000 {
		t.Fatalf("coverage = %+v", c)
	}
}

func TestMissingRuns(t *testing.T) {
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	var buckets []time.Time
	for i := 0; i < 8; i++ {
		buckets = append(buckets, base.Add(time.Duration(i)*time.Minute))
	}
	keep := map[int64]bool{}
	for _, i := range []int{0, 3, 4, 7} {
		keep[buckets[i].Unix()] = true
	}
	runs := missingRuns(buckets, keep, time.Minute)
	want := [][2]time.Time{{buckets[1], buckets[3]}, {buckets[5], buckets[7]}}
	if len(runs) != len(want) || runs[0] != want[0] || runs[1] != want[1] {
		t.Fatalf("runs = %v, want %v", runs, want)
	}
}

// 7d/30d: an hour covered for 20 minutes counts 1200 s, not 3600 s, and a
// partial hour is left out of the coverage ratio while full hours exist.
func TestFlowHourlyPartialHours(t *testing.T) {
	f := newFlowFixture(t)
	h0 := f.end.Truncate(time.Hour).Add(-3 * time.Hour)
	var counters []queries.PortStatRow
	for h, mins := range []int64{60, 20} {
		b := h0.Add(time.Duration(h) * time.Hour)
		if _, err := f.db.Exec(`INSERT INTO flow_1h VALUES (?, ?, 1, 13, ?, ?, 6, 443, ?, 1, 1)`, f.rbExp, b.Unix(),
			queries.FlowIPBlob(netip.MustParseAddr("1.1.1.1")), queries.FlowIPBlob(netip.MustParseAddr("192.168.28.33")),
			mins*1000); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Exec(`INSERT INTO flow_1h_meta VALUES (?, ?, 1, 1, ?, 1, 0, 0, ?)`, f.rbExp, b.Unix(),
			mins*1000, mins); err != nil {
			t.Fatal(err)
		}
		for m := 0; m < 60; m++ {
			counters = append(counters, queries.PortStatRow{DeviceID: labRB, InterfaceName: "eth1-wan",
				Bucket: b.Add(time.Duration(m) * time.Minute), RxBytes: 1100, TxBytes: 100, Samples: 4})
		}
	}
	if err := queries.InsertPortStats1m(f.db, counters); err != nil {
		t.Fatal(err)
	}
	if err := queries.RollupPortStats1h(f.db, h0, h0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.fake.rolled = h0.Add(2 * time.Hour)
	var top flowTopResponse
	if code, body := f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN), "range": "7d"}, &top); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if top.Seconds != 80*60 || top.TotalBytes != 80*1000 {
		t.Fatalf("7d seconds %d total %d, want %d / %d", top.Seconds, top.TotalBytes, 80*60, 80*1000)
	}
	var port flowPortResponse
	if code, body := f.get(t, "/flows/port", map[string]string{"device": labRB, "iface": "eth1-wan", "range": "7d"}, &port); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if c := port.Coverage; c.FlowDownBytes != 60*1000 || c.CounterDownBytes != 60*1100 {
		t.Fatalf("7d coverage = %+v (want the full hour only)", c)
	}
}

func TestFlowPointTooManyIfIndexes(t *testing.T) {
	f := newFlowFixture(t)
	ifs := make([]int, queries.MaxFlowIfs+1)
	for i := range ifs {
		ifs[i] = i + 1
	}
	body := map[string]any{"exporter_id": f.fwExp, "name": "huge", "kind": "derived", "if_indexes": ifs,
		"facing": "down", "exclude_attach": []any{}}
	if code, got := f.do(t, http.MethodPost, "/flows/points", body, nil); code != 400 || got != errBody("too many if_indexes (max 256)") {
		t.Fatalf("%d %s", code, got)
	}
	body["if_indexes"] = ifs[:queries.MaxFlowIfs]
	if code, got := f.do(t, http.MethodPost, "/flows/points", body, nil); code != 201 {
		t.Fatalf("256 ifIndexes: %d %s", code, got)
	}
}

// A collector bound to one specific address advertises that address.
func TestFlowSetupSpecificListenAddress(t *testing.T) {
	f := newFlowFixture(t)
	f.fake.status.Listen = []string{"127.0.0.1:12055", "10.0.0.5:2055"}
	var s flowSetupResponse
	if code, body := f.get(t, "/flows/setup", nil, &s); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if s.AdvertiseAddress != "10.0.0.5" || s.AdvertiseSource != "auto" || s.Port != 12055 && s.Port != 2055 {
		t.Fatalf("setup: %s (%s) port %d", s.AdvertiseAddress, s.AdvertiseSource, s.Port)
	}
	for _, l := range [][]string{{":2055"}, {"0.0.0.0:2055"}, {"[::]:2055"}, {"127.0.0.1:2055"}} {
		if a, ok := specificListenAddr(l); ok {
			t.Errorf("%v: advertised %s", l, a)
		}
	}
}

// Restoring an older backup after firewall003 was deleted and re-created
// (new id) maps its interfaces and points through the address; rows of
// exporters that do not exist are skipped instead of failing the restore.
func TestFlowRestoreRemapsExporterIDs(t *testing.T) {
	f := newFlowFixture(t)
	rec := httptest.NewRecorder()
	f.s.handleFullBackup(rec, httptest.NewRequest(http.MethodGet, "/admin/backup", nil))
	raw := rec.Body.String()

	db := newTestDB(t)
	// A different exporter holds the old firewall003 id's slot first, then
	// firewall003 is re-created with another id.
	other, err := queries.CreateFlowExporter(db, &queries.FlowExporter{Name: "other", Address: "10.9.9.9", Kind: "other", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = other
	if _, err := queries.CreateFlowExporter(db, &queries.FlowExporter{Name: "rb-new", Address: "192.168.78.202", Kind: "routeros", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	fwNew, err := queries.CreateFlowExporter(db, &queries.FlowExporter{Name: "fw-new", Address: "192.168.78.81", Kind: "opnsense", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	fake := flowFakeFor(time.Now())
	s := &Server{db: db, flows: fake}
	rec = httptest.NewRecorder()
	s.handleFullRestore(rec, httptest.NewRequest(http.MethodPost, "/admin/restore", strings.NewReader(raw)))
	if rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body.String())
	}
	if fake.reloads.Load() == 0 {
		t.Error("restore of flow tables did not notify the collector")
	}
	pts, err := queries.ListFlowPoints(db)
	if err != nil || len(pts) != 6 {
		t.Fatalf("restored points: %d %v", len(pts), err)
	}
	for _, p := range pts {
		if p.ExporterID == other {
			t.Errorf("point %q attached to the unrelated exporter", p.Name)
		}
		if strings.HasPrefix(p.Name, "firewall003") && p.ExporterID != fwNew {
			t.Errorf("point %q on exporter %d, want %d", p.Name, p.ExporterID, fwNew)
		}
	}

	// Per-table import of rows whose exporter does not exist: skipped.
	var ifRows []map[string]any
	var bundle backupBundle
	if err := json.Unmarshal([]byte(raw), &bundle); err != nil {
		t.Fatal(err)
	}
	for _, r := range bundle.Tables["flow_exporter_ifaces"] {
		r["exporter_id"] = 999
		ifRows = append(ifRows, r)
	}
	kept, dropped, err := remapFlowExporterIDs(db, ifRows, nil)
	if err != nil || len(kept) != 0 || dropped != int64(len(ifRows)) {
		t.Fatalf("unknown exporter rows: kept %d dropped %d %v", len(kept), dropped, err)
	}
}
