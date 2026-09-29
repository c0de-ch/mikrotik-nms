package queries

import (
	"bytes"
	"database/sql"
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// flowMinute returns 2026-09-28 hh:mm:00 UTC.
func flowMinute(hh, mm int) time.Time {
	return time.Date(2026, 9, 28, hh, mm, 0, 0, time.UTC)
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

func newFlowExporter(t *testing.T, db *sql.DB, name, addr string) int64 {
	t.Helper()
	id, err := CreateFlowExporter(db, &FlowExporter{Name: name, Address: addr, Kind: "other", Enabled: true})
	if err != nil {
		t.Fatalf("create exporter %s: %v", name, err)
	}
	return id
}

func TestFlowIPBlobRoundTrip(t *testing.T) {
	cases := []struct {
		in      netip.Addr
		wantLen int
		want    netip.Addr
	}{
		{mustAddr("192.168.28.33"), 4, mustAddr("192.168.28.33")},
		{mustAddr("2a01:4f8:202:13d1:28::33"), 16, mustAddr("2a01:4f8:202:13d1:28::33")},
		{mustAddr("::ffff:10.1.2.3"), 4, mustAddr("10.1.2.3")},
		{netip.Addr{}, 0, netip.Addr{}},
	}
	for _, c := range cases {
		b := FlowIPBlob(c.in)
		if b == nil {
			t.Fatalf("FlowIPBlob(%v) = nil, want non-nil", c.in)
		}
		if len(b) != c.wantLen {
			t.Errorf("FlowIPBlob(%v) len = %d, want %d", c.in, len(b), c.wantLen)
		}
		if got := FlowBlobIP(b); got != c.want {
			t.Errorf("FlowBlobIP(FlowIPBlob(%v)) = %v, want %v", c.in, got, c.want)
		}
	}
	if FlowBlobIP([]byte{1, 2, 3}).IsValid() {
		t.Error("3-byte blob must be invalid")
	}
}

func TestFlowExporterCRUDAndAutoincrement(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "dev1")

	e := &FlowExporter{Name: "firewall003", Address: "192.168.78.81", Kind: "opnsense", Enabled: true,
		NATAddresses: []netip.Addr{mustAddr("192.168.28.81"), mustAddr("192.168.111.81")}}
	id1, err := CreateFlowExporter(db, e)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := CreateFlowExporter(db, &FlowExporter{Name: "dup", Address: "192.168.78.81", Kind: "other"}); !errors.Is(err, ErrFlowExporterExists) {
		t.Fatalf("duplicate address err = %v, want ErrFlowExporterExists", err)
	}
	id2, err := CreateFlowExporter(db, &FlowExporter{Name: "rb5009", Address: "192.168.78.202", Kind: "routeros",
		DeviceID: "dev1", Enabled: true, Auto: true})
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}

	got, err := GetFlowExporter(db, id1)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Kind != "opnsense" || got.DeviceID != "" || !got.Enabled || got.Auto || len(got.NATAddresses) != 2 ||
		got.NATAddresses[1] != mustAddr("192.168.111.81") || got.CreatedAt.IsZero() || !got.LastSeen.IsZero() ||
		got.SamplingRate != 1 {
		t.Fatalf("round trip: %+v", got)
	}
	if missing, err := GetFlowExporter(db, 999); missing != nil || err != nil {
		t.Fatalf("missing exporter = %v, %v; want nil, nil", missing, err)
	}

	list, err := ListFlowExporters(db)
	if err != nil || len(list) != 2 || list[0].Name != "firewall003" || list[1].DeviceID != "dev1" || !list[1].Auto {
		t.Fatalf("list = %+v, %v", list, err)
	}

	// Config update never touches auto or health.
	got.Name, got.Enabled, got.SamplingOverride, got.NATAddresses = "fw3", false, 100, nil
	if err := UpdateFlowExporterConfig(db, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got2, _ := GetFlowExporter(db, id1)
	if got2.Name != "fw3" || got2.Enabled || got2.SamplingOverride != 100 || len(got2.NATAddresses) != 0 {
		t.Fatalf("after update: %+v", got2)
	}
	got2.Address = "192.168.78.202"
	if err := UpdateFlowExporterConfig(db, got2); !errors.Is(err, ErrFlowExporterExists) {
		t.Fatalf("update to duplicate address err = %v", err)
	}
	if err := UpdateFlowExporterConfig(db, &FlowExporter{ID: 999, Name: "x", Address: "10.0.0.1", Kind: "other"}); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("update missing err = %v", err)
	}

	// Health delta: counters add, others replace per the rules.
	at := flowMinute(12, 0)
	if err := AddFlowExporterStatus(db, id2, FlowExporterStatusDelta{LastSeen: at, Protocol: "ipfix", SamplingRate: 1,
		Datagrams: 10, Flows: 20, Bytes: 3000, DecodeErrors: 1, TemplateMisses: 2, SeqLost: 3, ClockSkewMs: 7,
		LastError: "boom", LastErrorAt: at}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if err := AddFlowExporterStatus(db, id2, FlowExporterStatusDelta{Datagrams: 1, Flows: 1, Bytes: 1, ClockSkewMs: -2,
		LastError: "ignored"}); err != nil {
		t.Fatalf("status 2: %v", err)
	}
	h, _ := GetFlowExporter(db, id2)
	if !h.LastSeen.Equal(at) || h.Protocol != "ipfix" || h.Datagrams != 11 || h.Flows != 21 || h.Bytes != 3001 ||
		h.DecodeErrors != 1 || h.TemplateMisses != 2 || h.SeqLost != 3 || h.ClockSkewMs != -2 ||
		h.LastError != "boom" || !h.LastErrorAt.Equal(at) {
		t.Fatalf("health: %+v", h)
	}

	// AUTOINCREMENT: deleting the highest id never lets it be reused.
	if err := DeleteFlowExporter(db, id2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := DeleteFlowExporter(db, id2); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
	id3 := newFlowExporter(t, db, "new", "192.168.78.203")
	if id3 <= id2 {
		t.Fatalf("new id %d reuses deleted max id %d", id3, id2)
	}
}

func TestFlowIfacesManualSurvivesDeviceSync(t *testing.T) {
	db := testDB(t)
	exp := newFlowExporter(t, db, "rb", "10.0.0.1")

	if err := UpsertDeviceFlowIfaces(db, exp, []FlowIface{
		{IfIndex: 1, Name: "eth1-wan", Type: "ether"},
		{IfIndex: 13, Name: "net28", Type: "vlan", VLANID: 28, Parent: "bridge"},
		{IfIndex: 0, Name: "local"}, // never a row
	}); err != nil {
		t.Fatalf("device upsert: %v", err)
	}
	if err := SetFlowIfaceManual(db, exp, 13, "VLAN 28", "lan"); err != nil {
		t.Fatalf("manual: %v", err)
	}
	// A later sync renames/retypes: the manual name and role survive, type
	// / vlan / parent follow the device.
	if err := UpsertDeviceFlowIfaces(db, exp, []FlowIface{
		{IfIndex: 1, Name: "ether1-wan", Type: "ether"},
		{IfIndex: 13, Name: "net28-renamed", Type: "vlan", VLANID: 29, Parent: "bridge2"},
	}); err != nil {
		t.Fatalf("device upsert 2: %v", err)
	}
	ifs, err := ListFlowIfaces(db, exp)
	if err != nil || len(ifs) != 2 {
		t.Fatalf("list = %+v, %v", ifs, err)
	}
	if ifs[0].Name != "ether1-wan" || ifs[0].Source != "device" {
		t.Errorf("device row = %+v", ifs[0])
	}
	if ifs[1].Name != "VLAN 28" || ifs[1].Role != "lan" || ifs[1].Source != "manual" || ifs[1].VLANID != 29 ||
		ifs[1].Parent != "bridge2" {
		t.Errorf("manual row = %+v", ifs[1])
	}

	// Revert: learned, cleared; the next sync may rename it.
	if err := SetFlowIfaceManual(db, exp, 13, "", ""); err != nil {
		t.Fatalf("revert: %v", err)
	}
	_ = UpsertDeviceFlowIfaces(db, exp, []FlowIface{{IfIndex: 13, Name: "net28", Type: "vlan"}})
	ifs, _ = ListFlowIfaces(db, exp)
	if ifs[1].Name != "net28" || ifs[1].Source != "device" || ifs[1].Role != "" {
		t.Errorf("after revert + sync = %+v", ifs[1])
	}
}

func TestTouchFlowIfacesHintRules(t *testing.T) {
	db := testDB(t)
	exp := newFlowExporter(t, db, "fw", "10.0.0.2")
	_ = UpsertDeviceFlowIfaces(db, exp, []FlowIface{{IfIndex: 5, Name: "synced"}})

	t1, t2 := flowMinute(10, 0), flowMinute(10, 5)
	if err := TouchFlowIfaces(db, exp, []FlowIfaceSeen{{IfIndex: 1, Hint: "192.168.78.0/24"}, {IfIndex: 5}, {IfIndex: 0}}, t1); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := TouchFlowIfaces(db, exp, []FlowIfaceSeen{{IfIndex: 1}, {IfIndex: 7, Hint: "10.0.0.0/24"}}, t2); err != nil {
		t.Fatalf("touch 2: %v", err)
	}
	// An older touch never lowers last_seen.
	if err := TouchFlowIfaces(db, exp, []FlowIfaceSeen{{IfIndex: 1, Hint: "192.168.79.0/24"}}, t1); err != nil {
		t.Fatalf("touch 3: %v", err)
	}
	ifs, _ := ListFlowIfaces(db, exp)
	byIdx := map[uint32]FlowIface{}
	for _, f := range ifs {
		byIdx[f.IfIndex] = f
	}
	if len(ifs) != 3 {
		t.Fatalf("rows = %+v (ifIndex 0 must not be a row)", ifs)
	}
	f1 := byIdx[1]
	if f1.Source != "learned" || f1.Name != "" || !f1.FirstSeen.Equal(t1) || !f1.LastSeen.Equal(t2) ||
		f1.Hint != "192.168.79.0/24" {
		t.Errorf("learned row = %+v (empty hint must keep, non-empty replace)", f1)
	}
	f5 := byIdx[5]
	if f5.Source != "device" || f5.Name != "synced" || !f5.FirstSeen.Equal(t1) || !f5.LastSeen.Equal(t1) || f5.Hint != "" {
		t.Errorf("device row after touch = %+v", f5)
	}
	if f7 := byIdx[7]; !f7.FirstSeen.Equal(t2) || f7.Hint != "10.0.0.0/24" {
		t.Errorf("new row = %+v", f7)
	}

	n, err := DeleteOldLearnedFlowIfaces(db, flowMinute(10, 3))
	if err != nil || n != 0 {
		t.Fatalf("delete old learned = %d, %v (row 1 was seen at 10:05)", n, err)
	}
	n, _ = DeleteOldLearnedFlowIfaces(db, flowMinute(10, 6))
	if n != 2 {
		t.Fatalf("deleted %d learned rows, want 2 (device row kept)", n)
	}
}

func TestFlowPointsUniqueAndAuto(t *testing.T) {
	db := testDB(t)
	seedPortStatsDevice(t, db, "sw1")
	exp := newFlowExporter(t, db, "fw", "10.0.0.3")

	native := &FlowPoint{ExporterID: exp, Name: "fw · if#1", IfIndexes: []uint32{1}, Facing: "down", Enabled: true}
	created, err := EnsureNativeFlowPoint(db, native)
	if err != nil || !created {
		t.Fatalf("ensure native = %v, %v", created, err)
	}
	created, err = EnsureNativeFlowPoint(db, &FlowPoint{ExporterID: exp, Name: "other name", IfIndexes: []uint32{1}, Facing: "up"})
	if err != nil || created {
		t.Fatalf("second ensure = %v, %v; want no-op", created, err)
	}
	if _, err := CreateFlowPoint(db, &FlowPoint{ExporterID: exp, Name: "dup", Kind: "native", IfIndexes: []uint32{1}, Facing: "down"}); !errors.Is(err, ErrFlowPointExists) {
		t.Fatalf("native dup err = %v", err)
	}

	derived := &FlowPoint{ExporterID: exp, Name: "sw1 · ether1", Kind: "derived", IfIndexes: []uint32{13, 10, 10, 0},
		Facing: "down", PortSide: "peer", DeviceID: "sw1", Iface: "ether1", LocalInternal: true,
		ExcludeAttach: []PortRef{{DeviceID: "sw1", Iface: "eth3"}}, Note: "n", Enabled: true}
	did, err := CreateFlowPoint(db, derived)
	if err != nil {
		t.Fatalf("create derived: %v", err)
	}
	if _, err := CreateFlowPoint(db, &FlowPoint{ExporterID: exp, Name: "again", Kind: "derived", IfIndexes: []uint32{1},
		Facing: "down", PortSide: "peer", DeviceID: "sw1", Iface: "ether1"}); !errors.Is(err, ErrFlowPointExists) {
		t.Fatalf("derived dup err = %v", err)
	}
	if _, err := CreateFlowPoint(db, &FlowPoint{ExporterID: exp, Name: "empty", Kind: "derived", IfIndexes: []uint32{0}}); err == nil {
		t.Fatal("point without a non-zero ifIndex must fail")
	}

	p, err := GetFlowPoint(db, did)
	if err != nil || p == nil {
		t.Fatalf("get: %v, %v", p, err)
	}
	if !slices.Equal(p.IfIndexes, []uint32{10, 13}) || !p.LocalInternal || len(p.ExcludeAttach) != 1 ||
		p.ExcludeAttach[0].Iface != "eth3" || p.Auto || p.Kind != "derived" || p.DeviceID != "sw1" {
		t.Fatalf("derived round trip = %+v", p)
	}

	points, _ := ListFlowPoints(db)
	if len(points) != 2 || points[0].Kind != "derived" || points[1].Kind != "native" {
		t.Fatalf("order (derived before native) = %+v", points)
	}
	nat := points[1]
	if nat.ExcludeAttach == nil || len(nat.ExcludeAttach) != 0 || !nat.Auto {
		t.Fatalf("native point = %+v", nat)
	}

	// UpdateAutoFlowPoint rewrites auto points only.
	nat.Name, nat.Facing = "fw · LAN", "up"
	_ = UpdateAutoFlowPoint(db, &nat)
	if got, _ := GetFlowPoint(db, nat.ID); got.Name != "fw · LAN" || got.Facing != "up" {
		t.Fatalf("auto update = %+v", got)
	}
	// An admin edit takes the point over (auto=0); later auto updates no-op.
	nat.Name = "admin name"
	if err := UpdateFlowPoint(db, &nat); err != nil {
		t.Fatalf("update: %v", err)
	}
	nat.Name = "auto name"
	_ = UpdateAutoFlowPoint(db, &nat)
	if got, _ := GetFlowPoint(db, nat.ID); got.Name != "admin name" || got.Auto {
		t.Fatalf("auto update on auto=0 point changed it: %+v", got)
	}
	if err := UpdateFlowPoint(db, &FlowPoint{ID: 999, ExporterID: exp, Name: "x", Kind: "derived", IfIndexes: []uint32{1}}); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("update missing err = %v", err)
	}
	if err := DeleteFlowPoint(db, did); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := DeleteFlowPoint(db, did); !errors.Is(err, ErrFlowNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
}

func TestDeleteStaleAutoFlowPoints(t *testing.T) {
	db := testDB(t)
	exp := newFlowExporter(t, db, "rb", "10.0.0.4")
	for _, idx := range []uint32{1, 2, 3} {
		if _, err := EnsureNativeFlowPoint(db, &FlowPoint{ExporterID: exp, Name: "p", IfIndexes: []uint32{idx}, Facing: "down", Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	// Point 3 was taken over by an admin: never swept.
	points, _ := ListFlowPoints(db)
	p3 := points[2]
	_ = UpdateFlowPoint(db, &p3)
	// Created long ago (the sweep ignores points younger than the cutoff).
	if _, err := db.Exec(`UPDATE flow_points SET created_at = ?`, flowMinute(0, 0).Add(-30*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	cutoff := flowMinute(12, 0)
	_ = TouchFlowIfaces(db, exp, []FlowIfaceSeen{{IfIndex: 1}}, cutoff.Add(time.Minute)) // fresh
	_ = TouchFlowIfaces(db, exp, []FlowIfaceSeen{{IfIndex: 2}, {IfIndex: 3}}, cutoff.Add(-time.Hour))

	n, err := DeleteStaleAutoFlowPoints(db, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("stale sweep = %d, %v; want 1", n, err)
	}
	points, _ = ListFlowPoints(db)
	var left []uint32
	for _, p := range points {
		left = append(left, p.IfIndexes[0])
	}
	if !slices.Equal(left, []uint32{1, 3}) {
		t.Fatalf("left = %v, want [1 3]", left)
	}
}

func TestInsertFlows1mAdditive(t *testing.T) {
	db := testDB(t)
	exp := newFlowExporter(t, db, "rb", "10.0.0.5")
	m := flowMinute(12, 0)
	rows := []FlowRow{
		{ExporterID: exp, Bucket: m.Add(15 * time.Second), InIf: 1, OutIf: 13, Src: mustAddr("1.1.1.1"),
			Dst: mustAddr("192.168.28.33"), Proto: 6, Port: 443, Bytes: 1000, Packets: 10, Flows: 1},
		{ExporterID: exp, Bucket: m, InIf: 1, OutIf: 13, Bytes: 50, Packets: 1, Flows: 2}, // other
	}
	meta := []FlowMeta{{ExporterID: exp, Bucket: m, Datagrams: 3, Records: 4, Bytes: 1050, Sampling: 1, Overflow: 0, Late: 1}}
	if err := InsertFlows1m(db, rows, meta); err != nil {
		t.Fatalf("insert: %v", err)
	}
	meta[0].Sampling = 100
	meta2 := []FlowMeta{{ExporterID: exp, Bucket: m, Datagrams: 1, Records: 1, Bytes: 10, Sampling: 100, Overflow: 2}}
	if err := InsertFlows1m(db, rows, meta2); err != nil {
		t.Fatalf("insert twice: %v", err)
	}
	if n := countRows(t, db, "flow_1m"); n != 2 {
		t.Fatalf("rows = %d, want 2 (same keys upsert)", n)
	}
	var b, p, f int64
	var bucket int64
	if err := db.QueryRow(`SELECT bucket, bytes, packets, flows FROM flow_1m WHERE length(src) = 4`).Scan(&bucket, &b, &p, &f); err != nil {
		t.Fatal(err)
	}
	if bucket != m.Unix() || b != 2000 || p != 20 || f != 2 {
		t.Fatalf("summed row = bucket %d bytes %d packets %d flows %d", bucket, b, p, f)
	}
	var otherSrc []byte
	if err := db.QueryRow(`SELECT src FROM flow_1m WHERE length(src) = 0`).Scan(&otherSrc); err != nil {
		t.Fatalf("other row stored with x'': %v", err)
	}
	sum, err := FlowMetaSummary(db, Flows1m, exp, m, m.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Buckets != 1 || sum.Datagrams != 4 || sum.Records != 5 || sum.Bytes != 1060 || sum.MaxSampling != 100 ||
		sum.Overflow != 2 || sum.Late != 1 {
		t.Fatalf("meta summary = %+v", sum)
	}
	latest, ok, err := LatestFlowBucket(db, Flows1m, 0)
	if err != nil || !ok || !latest.Equal(m) {
		t.Fatalf("latest = %v %v %v", latest, ok, err)
	}
	if _, ok, _ := LatestFlowBucket(db, Flows1h, exp); ok {
		t.Fatal("empty 1h meta must report ok=false")
	}
}

func TestAggregateFlowsOrFilterAndStep(t *testing.T) {
	db := testDB(t)
	exp := newFlowExporter(t, db, "rb", "10.0.0.6")
	other := newFlowExporter(t, db, "rb2", "10.0.0.7")
	a, b := mustAddr("192.168.28.33"), mustAddr("1.1.1.1")
	var rows []FlowRow
	for mm := 0; mm < 10; mm++ {
		rows = append(rows,
			FlowRow{ExporterID: exp, Bucket: flowMinute(12, mm), InIf: 13, OutIf: 1, Src: a, Dst: b, Proto: 6, Port: 443, Bytes: 100, Packets: 1, Flows: 1},
			FlowRow{ExporterID: exp, Bucket: flowMinute(12, mm), InIf: 1, OutIf: 13, Src: b, Dst: a, Proto: 6, Port: 443, Bytes: 1000, Packets: 2, Flows: 1},
			FlowRow{ExporterID: exp, Bucket: flowMinute(12, mm), InIf: 10, OutIf: 0, Src: a, Dst: b, Proto: 17, Port: 53, Bytes: 7, Packets: 1, Flows: 1},
			FlowRow{ExporterID: other, Bucket: flowMinute(12, mm), InIf: 13, OutIf: 1, Src: a, Dst: b, Proto: 6, Port: 443, Bytes: 99999, Packets: 1, Flows: 1},
		)
	}
	if err := InsertFlows1m(db, rows, nil); err != nil {
		t.Fatal(err)
	}
	from, to := flowMinute(12, 0), flowMinute(12, 10)

	// OR filter: ifIndex 13 matches rows with in_if 13 and rows with out_if 13.
	got, err := AggregateFlows(db, Flows1m, exp, []uint32{13}, from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Bytes != 10000 || got[0].InIf != 1 || got[1].Bytes != 1000 || got[1].InIf != 13 ||
		!got[0].Bucket.Equal(from) || got[0].Src != b || got[0].Dst != a {
		t.Fatalf("OR filter step 0 = %+v", got)
	}
	if got, _ := AggregateFlows(db, Flows1m, exp, nil, from, to, 0); len(got) != 0 {
		t.Fatalf("no ifs must return no rows, got %d", len(got))
	}

	// Step grouping: 5-minute groups starting at from.
	got, err = AggregateFlows(db, Flows1m, exp, []uint32{10}, flowMinute(12, 1), to, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[0].Bucket.Equal(flowMinute(12, 1)) || got[0].Bytes != 35 ||
		!got[1].Bucket.Equal(flowMinute(12, 6)) || got[1].Bytes != 28 {
		t.Fatalf("step grouping = %+v", got)
	}
	if _, err := AggregateFlows(db, Flows1m, exp, []uint32{10}, from, to, 90*time.Second); err == nil {
		t.Fatal("step not a multiple of the native bucket must fail")
	}
	if _, err := AggregateFlows(db, FlowTable("bogus"), exp, []uint32{10}, from, to, 0); err == nil {
		t.Fatal("unknown table must fail")
	}
}

func TestRollupFlows1hTopNOtherExactAndIdempotent(t *testing.T) {
	db := testDB(t)
	exp := newFlowExporter(t, db, "fw", "10.0.0.8")
	hour := flowMinute(11, 0)
	var rows []FlowRow
	var meta []FlowMeta
	pairTotal := map[flowPair]int64{}
	add := func(r FlowRow) {
		rows = append(rows, r)
		pairTotal[flowPair{r.InIf, r.OutIf}] += r.Bytes
	}
	for mm := 0; mm < 60; mm += 10 {
		m := hour.Add(time.Duration(mm) * time.Minute)
		for h := 1; h <= 5; h++ {
			// 5 hosts on pair (1,7) with distinct volumes, 2 on pair (7,1).
			add(FlowRow{ExporterID: exp, Bucket: m, InIf: 1, OutIf: 7, Src: netip.AddrFrom4([4]byte{192, 168, 78, byte(h)}),
				Dst: mustAddr("9.9.9.9"), Proto: 6, Port: 853, Bytes: int64(h * 100), Packets: int64(h), Flows: 1})
		}
		add(FlowRow{ExporterID: exp, Bucket: m, InIf: 7, OutIf: 1, Src: mustAddr("9.9.9.9"), Dst: mustAddr("192.168.78.1"), Proto: 6, Port: 853, Bytes: 50, Packets: 1, Flows: 1})
		add(FlowRow{ExporterID: exp, Bucket: m, InIf: 7, OutIf: 1, Src: mustAddr("2620:fe::fe"), Dst: mustAddr("2a01::1"), Proto: 6, Port: 853, Bytes: 40, Packets: 1, Flows: 1})
		// An existing folded other row of pair (1,7) must fold into the new one.
		add(FlowRow{ExporterID: exp, Bucket: m, InIf: 1, OutIf: 7, Bytes: 3, Packets: 1, Flows: 4})
		meta = append(meta, FlowMeta{ExporterID: exp, Bucket: m, Datagrams: 2, Records: 8, Bytes: 1, Sampling: int64(mm + 1), Late: 1})
	}
	// A row of the next hour must be ignored.
	rows = append(rows, FlowRow{ExporterID: exp, Bucket: hour.Add(time.Hour), InIf: 1, OutIf: 7, Src: mustAddr("1.2.3.4"), Dst: mustAddr("5.6.7.8"), Bytes: 1 << 40})
	if err := InsertFlows1m(db, rows, meta); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		type row struct {
			inIf, outIf int64
			src         []byte
			bytes       int64
			flows       int64
		}
		rs, err := db.Query(`SELECT in_if, out_if, src, bytes, flows FROM flow_1h WHERE exporter_id = ? AND bucket = ?`, exp, hour.Unix())
		if err != nil {
			t.Fatal(err)
		}
		var got []row
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.inIf, &r.outIf, &r.src, &r.bytes, &r.flows); err != nil {
				t.Fatal(err)
			}
			got = append(got, r)
		}
		rs.Close()
		sums := map[flowPair]int64{}
		var otherRows, otherFlows int64
		var topSrc [][]byte
		for _, r := range got {
			sums[flowPair{uint32(r.inIf), uint32(r.outIf)}] += r.bytes
			if len(r.src) == 0 {
				otherRows++
				if r.inIf == 1 {
					otherFlows = r.flows
				}
			} else if r.inIf == 1 {
				topSrc = append(topSrc, r.src)
			}
		}
		for p, want := range pairTotal {
			if sums[p] != want {
				t.Errorf("pair %v total = %d, want %d (exact)", p, sums[p], want)
			}
		}
		// top 2 of pair (1,7) = hosts .5 and .4; hosts .1-.3 + the old other fold.
		if len(topSrc) != 2 || otherRows != 1 {
			t.Fatalf("rows = %+v (want 2 top rows + 1 other for pair 1/7, pair 7/1 fits)", got)
		}
		for _, s := range topSrc {
			if !bytes.Equal(s, []byte{192, 168, 78, 5}) && !bytes.Equal(s, []byte{192, 168, 78, 4}) {
				t.Errorf("unexpected top src %v", s)
			}
		}
		if otherFlows != 6*(3+4) {
			t.Errorf("other flows = %d, want %d", otherFlows, 6*7)
		}
		var mins, dg, sampling, late int64
		if err := db.QueryRow(`SELECT minutes, datagrams, sampling, late FROM flow_1h_meta WHERE exporter_id = ? AND bucket = ?`,
			exp, hour.Unix()).Scan(&mins, &dg, &sampling, &late); err != nil {
			t.Fatal(err)
		}
		if mins != 6 || dg != 12 || sampling != 51 || late != 6 {
			t.Errorf("1h meta = minutes %d datagrams %d sampling %d late %d", mins, dg, sampling, late)
		}
	}
	if err := RollupFlows1h(db, exp, hour.Add(17*time.Minute), 2); err != nil {
		t.Fatal(err)
	}
	check()
	if err := RollupFlows1h(db, exp, hour, 2); err != nil {
		t.Fatal(err)
	}
	check() // idempotent
	if n := countRows(t, db, "flow_1h_meta"); n != 1 {
		t.Fatalf("1h meta rows = %d, want 1", n)
	}
}

func TestDeleteOldAndOrphanFlowsChunked(t *testing.T) {
	db := testDB(t)
	flowDeleteChunk, flowDeletePause = 3, 0
	t.Cleanup(func() { flowDeleteChunk, flowDeletePause = 5000, 5*time.Millisecond })

	e1 := newFlowExporter(t, db, "a", "10.0.0.9")
	e2 := newFlowExporter(t, db, "b", "10.0.0.10")
	var rows []FlowRow
	var meta []FlowMeta
	for _, e := range []int64{e1, e2} {
		for mm := 0; mm < 10; mm++ {
			m := flowMinute(12, mm)
			rows = append(rows,
				FlowRow{ExporterID: e, Bucket: m, InIf: 1, OutIf: 2, Src: mustAddr("10.1.1.1"), Dst: mustAddr("10.2.2.2"), Bytes: 1},
				FlowRow{ExporterID: e, Bucket: m, InIf: 1, OutIf: 2, Bytes: 1})
			meta = append(meta, FlowMeta{ExporterID: e, Bucket: m, Datagrams: 1})
		}
	}
	if err := InsertFlows1m(db, rows, meta); err != nil {
		t.Fatal(err)
	}
	_ = RollupFlows1h(db, e1, flowMinute(12, 0), 10)
	_ = RollupFlows1h(db, e2, flowMinute(12, 0), 10)

	// Minutes 12:00..12:06 of both exporters: 7 * (2 rows + 1 meta) * 2.
	n, err := DeleteOldFlows(db, Flows1m, flowMinute(12, 7))
	if err != nil || n != 42 {
		t.Fatalf("delete old = %d, %v; want 42", n, err)
	}
	if c := countRows(t, db, "flow_1m"); c != 12 {
		t.Fatalf("flow_1m left = %d, want 12", c)
	}
	if c := countRows(t, db, "flow_1m_meta"); c != 6 {
		t.Fatalf("flow_1m_meta left = %d, want 6", c)
	}
	if c := countRows(t, db, "flow_1h"); c != 4 {
		t.Fatalf("flow_1h untouched = %d, want 4", c)
	}

	// Deleting an exporter leaves orphans; the orphan purge removes them in
	// chunks from all four tables and keeps the live exporter's rows.
	if err := DeleteFlowExporter(db, e2); err != nil {
		t.Fatal(err)
	}
	n, err = DeleteOrphanFlows(db)
	if err != nil || n != 6+3+2+1 {
		t.Fatalf("orphans = %d, %v; want 12", n, err)
	}
	for table, want := range map[string]int{"flow_1m": 6, "flow_1m_meta": 3, "flow_1h": 2, "flow_1h_meta": 1} {
		if c := countRows(t, db, table); c != want {
			t.Errorf("%s = %d, want %d", table, c, want)
		}
	}
	n, err = DeleteOldFlows(db, Flows1h, flowMinute(13, 0))
	if err != nil || n != 3 {
		t.Fatalf("delete old 1h = %d, %v; want 3", n, err)
	}
}

func TestFlowTemplatesAndPrefixes(t *testing.T) {
	db := testDB(t)
	exp := newFlowExporter(t, db, "fw", "10.0.0.11")
	t0 := flowMinute(9, 0)
	blob := []byte{0, 0, 0, 8, 0, 4, 0, 0, 0, 0}
	for _, tpl := range []FlowTemplate{
		{ExporterID: exp, Version: 9, ObsDomain: 0, TemplateID: 256, Kind: 0, Fields: blob, UpdatedAt: t0},
		{ExporterID: exp, Version: 9, ObsDomain: 0, TemplateID: 259, Kind: 0, Fields: blob, UpdatedAt: t0.Add(-48 * time.Hour)},
	} {
		if err := UpsertFlowTemplate(db, tpl); err != nil {
			t.Fatal(err)
		}
	}
	_ = UpsertFlowTemplate(db, FlowTemplate{ExporterID: exp, Version: 9, TemplateID: 256, Kind: 1, Fields: []byte{1}, UpdatedAt: t0.Add(time.Minute)})
	got, err := LoadFlowTemplates(db, exp, t0.Add(-24*time.Hour), 0)
	if err != nil || len(got) != 1 || got[0].TemplateID != 256 || got[0].Kind != 1 || !bytes.Equal(got[0].Fields, []byte{1}) ||
		!got[0].UpdatedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("load = %+v, %v", got, err)
	}
	if n, err := DeleteOldFlowTemplates(db, t0.Add(-24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("delete old templates = %d, %v", n, err)
	}
	if err := DeleteFlowTemplates(db, exp); err != nil || countRows(t, db, "flow_templates") != 0 {
		t.Fatalf("delete templates: %v", err)
	}
	// Churn is bounded: trim keeps the newest, load returns the newest first.
	for i := 0; i < 10; i++ {
		_ = UpsertFlowTemplate(db, FlowTemplate{ExporterID: exp, Version: 10, TemplateID: uint16(300 + i), Fields: blob,
			UpdatedAt: t0.Add(time.Duration(i) * time.Second)})
	}
	if n, err := TrimFlowTemplates(db, exp, 4); err != nil || n != 6 || countRows(t, db, "flow_templates") != 4 {
		t.Fatalf("trim = %d, %v", n, err)
	}
	got, err = LoadFlowTemplates(db, exp, t0.Add(-time.Hour), 2)
	if err != nil || len(got) != 2 || got[0].TemplateID != 309 || got[1].TemplateID != 308 {
		t.Fatalf("load newest 2 = %+v, %v", got, err)
	}
	if err := DeleteFlowTemplateKeys(db, exp, []FlowTemplateKey{{Version: 10, TemplateID: 309}, {Version: 9, TemplateID: 309}}); err != nil ||
		countRows(t, db, "flow_templates") != 3 {
		t.Fatalf("delete keys: %v", err)
	}
	_ = DeleteFlowTemplates(db, exp)

	ps := []FlowPrefix{
		{Prefix: netip.MustParsePrefix("192.168.78.202/23"), Class: "internal", DeviceID: "d", Iface: "bridge", UpdatedAt: t0},
		{Prefix: netip.MustParsePrefix("100.64.0.0/10"), Class: "transit", DeviceID: "d", Iface: "eth1-wan", UpdatedAt: t0},
	}
	if err := ReplaceFlowPrefixes(db, ps); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFlowPrefixes(db, ps[:1]); err != nil {
		t.Fatal(err)
	}
	list, err := ListFlowPrefixes(db)
	if err != nil || len(list) != 1 || list[0].Prefix.String() != "192.168.78.0/23" || list[0].Class != "internal" ||
		!list[0].UpdatedAt.Equal(t0) {
		t.Fatalf("prefixes = %+v, %v", list, err)
	}
}

func TestFlowExporterDeleteCascades(t *testing.T) {
	db := testDB(t)
	exp := newFlowExporter(t, db, "fw", "10.0.0.12")
	_ = TouchFlowIfaces(db, exp, []FlowIfaceSeen{{IfIndex: 1}}, flowMinute(1, 0))
	_, _ = EnsureNativeFlowPoint(db, &FlowPoint{ExporterID: exp, Name: "p", IfIndexes: []uint32{1}, Facing: "down"})
	_ = UpsertFlowTemplate(db, FlowTemplate{ExporterID: exp, Version: 10, TemplateID: 256, Fields: []byte{0, 0}, UpdatedAt: flowMinute(1, 0)})
	if err := DeleteFlowExporter(db, exp); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"flow_exporter_ifaces", "flow_points", "flow_templates"} {
		if c := countRows(t, db, table); c != 0 {
			t.Errorf("%s rows after exporter delete = %d, want 0 (cascade)", table, c)
		}
	}
}
