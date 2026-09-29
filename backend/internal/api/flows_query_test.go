package api

import (
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowview"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestFlowTop(t *testing.T) {
	f := newFlowFixture(t)
	var top flowTopResponse
	code, body := f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN)}, &top)
	if code != http.StatusOK {
		t.Fatalf("status %d %s", code, body)
	}
	// Window end = FlushedThrough; coverage from the first meta bucket.
	if !top.To.Equal(f.end) || !top.From.Equal(f.end.Add(-time.Hour)) || top.CoverageFrom == nil ||
		!top.CoverageFrom.Equal(f.end.Add(-10*time.Minute)) || top.Seconds != 600 || top.Resolution != "1m" {
		t.Fatalf("window = %v..%v cov %v secs %d res %s", top.From, top.To, top.CoverageFrom, top.Seconds, top.Resolution)
	}
	// eth1-wan (up): download = in_if 1 → 60000 + 100 (v6) + 500 (other) per minute.
	if top.TotalBytes != 606000 || top.TotalBps != 8080 || top.SamplingRate != 1 || top.Dir != "download" || top.Group != "pair" {
		t.Fatalf("totals = %d bytes %d bps sampling %d dir %s group %s", top.TotalBytes, top.TotalBps, top.SamplingRate, top.Dir, top.Group)
	}
	if len(top.Rows) != 2 || top.Rows[0].Key != "1.1.1.1|192.168.28.33" || top.Rows[0].Bytes != 600000 ||
		top.Rows[0].Share != 0.9901 || top.Rows[0].AvgBps != 8000 || top.Rows[0].Rank != 1 ||
		top.Rows[1].Key != "2606:4700::1111|192.168.28.33" || top.Rows[1].Rank != 2 {
		t.Fatalf("rows = %+v", top.Rows)
	}
	if top.Other.Bytes != 5000 || top.Other.AvgBps != 67 || top.Other.Share != 0.0083 {
		t.Fatalf("other = %+v", top.Other)
	}
	src, dst := top.Rows[0].Src, top.Rows[0].Dst
	if src == nil || src.IP != "1.1.1.1" || src.Class != "external" || src.Name != "" || src.Attached != nil || top.Rows[0].App != nil {
		t.Fatalf("src endpoint = %+v app %+v", src, top.Rows[0].App)
	}
	if dst == nil || dst.Class != "internal" || dst.Name != "net28-client01" || dst.MAC != macC01 || dst.DeviceID != nil ||
		dst.Attached == nil || *dst.Attached != (flowAttached{DeviceID: labSW, DeviceName: "switch001", Iface: "ether12"}) {
		t.Fatalf("dst endpoint = %+v attached %+v", dst, dst.Attached)
	}
	if top.Point.ID != f.pWAN || top.Point.ExporterName != "switch002-rb5009" || !reflect.DeepEqual(top.Point.IfNames, []string{"eth1-wan"}) ||
		top.Point.DeviceName != "switch002-rb5009" || !top.Point.CoverageCapable || top.Point.LastData == nil || !top.Point.LastData.Equal(f.end) {
		t.Fatalf("point view = %+v", top.Point)
	}

	// Field presence and keys per grouping.
	for _, c := range []struct {
		group, dir, key string
		src, dst, app   bool
	}{
		{"src", "download", "1.1.1.1", true, false, false},
		{"dst", "download", "192.168.28.33", false, true, false},
		{"conv", "download", "1.1.1.1|192.168.28.33|6/443", true, true, true},
		{"app", "download", "6/443", false, false, true},
		{"src", "upload", "192.168.28.33", true, false, false},
	} {
		var r flowTopResponse
		if code, body := f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN), "group": c.group, "dir": c.dir}, &r); code != 200 {
			t.Fatalf("%s: %d %s", c.group, code, body)
		}
		row := r.Rows[0]
		if row.Key != c.key || (row.Src != nil) != c.src || (row.Dst != nil) != c.dst || (row.App != nil) != c.app {
			t.Errorf("%s/%s: first row %q src %v dst %v app %v", c.group, c.dir, row.Key, row.Src != nil, row.Dst != nil, row.App != nil)
		}
	}
	var conv flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN), "group": "conv"}, &conv)
	if a := conv.Rows[0].App; *a != (flowApp{Proto: 6, ProtoName: "tcp", Port: 443, Label: "HTTPS"}) {
		t.Errorf("app = %+v", a)
	}
	if a := conv.Rows[1].App; a.Label != "QUIC" || a.ProtoName != "udp" {
		t.Errorf("quic app = %+v", a)
	}

	// Upload of the WAN: firewall003's NATed address is labelled.
	var up flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN), "dir": "upload", "group": "src"}, &up)
	if up.TotalBytes != 90000 || len(up.Rows) != 2 || up.Rows[1].Src.IP != "192.168.28.81" || up.Rows[1].Src.NATOf == nil ||
		*up.Rows[1].Src.NATOf != "firewall003" || up.Rows[1].Src.Class != "internal" {
		t.Fatalf("upload src rows = %+v", up.Rows)
	}

	// limit folds the remainder into other.
	var lim flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN), "limit": "1"}, &lim)
	if len(lim.Rows) != 1 || lim.Other.Bytes != 6000 || lim.TotalBytes != 606000 {
		t.Fatalf("limit=1: %d rows other %d", len(lim.Rows), lim.Other.Bytes)
	}

	// The router's own address is "self" at its points; the trunk view
	// drops the rows whose local end sits on RB5009 eth3 (exclude_attach).
	var trunk flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pTrunk), "dir": "upload", "group": "conv", "limit": "200"}, &trunk)
	if trunk.TotalBytes != 110000 {
		t.Fatalf("trunk upload total = %d, want 110000", trunk.TotalBytes)
	}
	for _, r := range trunk.Rows {
		if r.Src.IP == "192.168.28.25" {
			t.Errorf("excluded host present: %+v", r.Src)
		}
		if r.Dst.IP == "192.168.78.202" && (r.Dst.Class != "self" || r.Dst.Name != "switch002-rb5009" || r.Dst.DeviceID == nil) {
			t.Errorf("router endpoint = %+v", r.Dst)
		}
	}

	// firewall003 LAN (local_internal): the WAN-copy row to its own NAT
	// address is dropped.
	var lan flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pFwLAN)}, &lan)
	if lan.TotalBytes != 640000 || len(lan.Rows) != 1 || lan.Rows[0].Dst.IP != "192.168.78.60" {
		t.Fatalf("fw LAN download = %d %+v", lan.TotalBytes, lan.Rows)
	}

	// live = the last 5 minutes.
	var live flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN), "range": "live"}, &live)
	if !live.From.Equal(f.end.Add(-5*time.Minute)) || live.Seconds != 300 || live.TotalBytes != 303000 || live.Range != "live" {
		t.Fatalf("live = %v secs %d total %d", live.From, live.Seconds, live.TotalBytes)
	}

	// The window follows FlushedThrough, and falls back to the newest meta
	// bucket without a collector.
	f.fake.flushed = f.end.Add(-5 * time.Minute)
	f.s.flowCache.clear()
	var lag flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN)}, &lag)
	if !lag.To.Equal(f.end.Add(-5*time.Minute)) || lag.Seconds != 300 || lag.TotalBytes != 303000 {
		t.Fatalf("lagging flush: to %v secs %d total %d", lag.To, lag.Seconds, lag.TotalBytes)
	}
	f.s.flows = nil
	var off flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN)}, &off)
	if !off.To.Equal(f.end) || off.TotalBytes != 606000 {
		t.Fatalf("collector off: to %v total %d", off.To, off.TotalBytes)
	}

	// No hourly rows yet: empty, coverage_from null.
	var week flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN), "range": "7d"}, &week)
	if week.CoverageFrom != nil || week.Seconds != 0 || len(week.Rows) != 0 || week.TotalBytes != 0 || week.Resolution != "1h" || week.Rows == nil {
		t.Fatalf("7d = %+v", week)
	}

	for params, want := range map[string]string{
		"":                           `{"error":"point is required"}`,
		"point=abc":                  `{"error":"invalid point"}`,
		"point=1&range=2h":           `{"error":"invalid range"}`,
		"point=1&dir=sideways":       `{"error":"invalid dir"}`,
		"point=1&group=host":         `{"error":"invalid group"}`,
		"point=99999":                `{"error":"point not found"}`,
		"point=99999&limit=notanint": `{"error":"point not found"}`,
	} {
		code, body := f.do(t, http.MethodGet, "/flows/top?"+params, nil, nil)
		wantCode := http.StatusBadRequest
		if want == `{"error":"point not found"}` {
			wantCode = http.StatusNotFound
		}
		if code != wantCode || body != want+"\n" {
			t.Errorf("%q: %d %s", params, code, body)
		}
	}
}

func TestFlowPort(t *testing.T) {
	f := newFlowFixture(t)
	f.seedCounters(t, labRB, "eth1-wan", 64000, 9500)
	f.seedCounters(t, labRB, "net28", 10000, 62000)
	f.seedCounters(t, labSW, "ether1", 70000, 2000)
	f.seedCounters(t, labSW, "sfp-sfpplus2", 70000, 22000)

	port := func(dev, iface string, extra map[string]string) flowPortResponse {
		t.Helper()
		p := map[string]string{"device": dev, "iface": iface}
		for k, v := range extra {
			p[k] = v
		}
		var r flowPortResponse
		if code, body := f.get(t, "/flows/port", p, &r); code != http.StatusOK {
			t.Fatalf("%s/%s: %d %s", dev, iface, code, body)
		}
		return r
	}
	ratio := func(p *float64) string {
		if p == nil {
			return "null"
		}
		return fmt.Sprint(*p)
	}

	// Native preferred over derived; the derived copy is an alternative.
	r := port(labRB, "eth1-wan", nil)
	if r.Reason != "ok" || r.Point == nil || r.Point.ID != f.pWAN || len(r.Alternatives) != 1 || r.Alternatives[0].ID != f.pWANDerived {
		t.Fatalf("eth1-wan choice: %+v alts %+v", r.Point, r.Alternatives)
	}
	if r.Download.TotalBytes != 606000 || r.Upload.TotalBytes != 90000 || len(r.Download.Rows) != 2 ||
		r.Download.Rows[0].Key != "1.1.1.1|192.168.28.33|6/443" || r.Download.Rows[0].App == nil || r.Seconds != 600 {
		t.Fatalf("eth1-wan rows: down %d up %d rows %+v", r.Download.TotalBytes, r.Upload.TotalBytes, r.Download.Rows)
	}
	// (up, same): download = rx.
	if c := r.Coverage; c.FlowDownBytes != 606000 || c.CounterDownBytes != 640000 || c.CounterUpBytes != 95000 ||
		ratio(c.Download) != "0.9469" || ratio(c.Upload) != "0.9474" {
		t.Fatalf("eth1-wan coverage = %+v down %s up %s", c, ratio(c.Download), ratio(c.Upload))
	}
	// point= picks an alternative; a point of another port is rejected.
	if r := port(labRB, "eth1-wan", map[string]string{"point": itoa(f.pWANDerived)}); r.Point.ID != f.pWANDerived ||
		len(r.Alternatives) != 1 || r.Alternatives[0].ID != f.pWAN {
		t.Fatalf("point override: %+v", r.Point)
	}
	if code, body := f.get(t, "/flows/port", map[string]string{"device": labRB, "iface": "eth1-wan", "point": itoa(f.pTrunk)}, nil); code != 400 ||
		body != `{"error":"point does not cover this port"}`+"\n" {
		t.Fatalf("foreign point: %d %s", code, body)
	}
	// limit clamps to 1..50.
	if r := port(labRB, "eth1-wan", map[string]string{"limit": "1"}); len(r.Download.Rows) != 1 || r.Download.Other.Bytes != 6000 {
		t.Fatalf("limit=1: %+v", r.Download)
	}

	// (down, same): download = tx.
	r = port(labRB, "net28", nil)
	if r.Point.ID != f.pNet28 || r.Coverage.CounterDownBytes != 620000 || r.Coverage.FlowDownBytes != 606000 ||
		ratio(r.Coverage.Download) != "0.9774" || ratio(r.Coverage.Upload) != "1" {
		t.Fatalf("net28 coverage = %+v", r.Coverage)
	}
	// (down, peer) for the gateway-host view: download = rx; local_internal
	// drops the WAN-copy row.
	r = port(labSW, "ether1", nil)
	if r.Point.ID != f.pSwEther1 || r.Point.Kind != "derived" || r.Coverage.CounterDownBytes != 700000 ||
		r.Coverage.FlowDownBytes != 640000 || ratio(r.Coverage.Download) != "0.9143" || ratio(r.Coverage.Upload) != "0.74" {
		t.Fatalf("switch001 ether1 coverage = %+v", r.Coverage)
	}
	// (down, peer) trunk: download = rx.
	r = port(labSW, "sfp-sfpplus2", nil)
	if r.Point.ID != f.pTrunk || r.Coverage.CounterDownBytes != 700000 || r.Coverage.FlowDownBytes != 616000 ||
		r.Coverage.FlowUpBytes != 110000 || r.Coverage.CounterUpBytes != 220000 {
		t.Fatalf("trunk coverage = %+v", r.Coverage)
	}

	// No point on the port.
	r = port(labSW, "ether12", nil)
	if r.Reason != "no_point" || r.Point != nil || r.Alternatives == nil || len(r.Alternatives) != 0 || r.From != nil ||
		r.Download.Rows == nil || r.Coverage.Download != nil || r.Resolution != "1m" {
		t.Fatalf("no point: %+v", r)
	}
	f.fake.enabled = false
	if r := port(labSW, "ether12", nil); r.Reason != "collector_disabled" {
		t.Fatalf("disabled: %s", r.Reason)
	}
	f.s.flows = nil
	if r := port(labSW, "ether12", nil); r.Reason != "collector_disabled" {
		t.Fatalf("nil collector: %s", r.Reason)
	}
	// History stays readable while the collector is off.
	if r := port(labRB, "eth1-wan", nil); r.Reason != "ok" || r.Download.TotalBytes != 606000 {
		t.Fatalf("collector off, existing point: %s %d", r.Reason, r.Download.TotalBytes)
	}

	for _, c := range []struct {
		params map[string]string
		code   int
		body   string
	}{
		{map[string]string{"device": labRB}, 400, "device and iface are required"},
		{map[string]string{"device": "nope", "iface": "x"}, 404, "device not found"},
		{map[string]string{"device": labRB, "iface": "x", "range": "1y"}, 400, "invalid range"},
		{map[string]string{"device": labRB, "iface": "x", "point": "zz"}, 400, "invalid point"},
	} {
		code, body := f.get(t, "/flows/port", c.params, nil)
		if code != c.code || body != `{"error":"`+c.body+`"}`+"\n" {
			t.Errorf("%v: %d %s", c.params, code, body)
		}
	}
}

// Coverage counts only the buckets both the exporter and the Phase 1
// collector covered.
func TestFlowPortCoverageIntersection(t *testing.T) {
	f := newFlowFixture(t)
	f.seedCounters(t, labRB, "eth1-wan", 64000, 9500, 2, 5) // Phase 1 missed two minutes
	var r flowPortResponse
	f.get(t, "/flows/port", map[string]string{"device": labRB, "iface": "eth1-wan"}, &r)
	c := r.Coverage
	if c.FlowDownBytes != 8*60600 || c.CounterDownBytes != 8*64000 || c.FlowUpBytes != 8*9000 || c.CounterUpBytes != 8*9500 {
		t.Fatalf("intersection coverage = %+v", c)
	}
	// Rows still cover the whole window.
	if r.Download.TotalBytes != 606000 {
		t.Fatalf("rows total = %d", r.Download.TotalBytes)
	}
	// No counters at all: null ratios.
	g := newFlowFixture(t)
	var r2 flowPortResponse
	g.get(t, "/flows/port", map[string]string{"device": labRB, "iface": "eth1-wan"}, &r2)
	if r2.Coverage.Download != nil || r2.Coverage.Upload != nil || r2.Coverage.FlowDownBytes != 0 {
		t.Fatalf("no counters: %+v", r2.Coverage)
	}
}

func TestFlowCoverageSeries(t *testing.T) {
	f := newFlowFixture(t)
	f.seedCounters(t, labRB, "eth1-wan", 64000, 9500, 6)
	// The RB5009 was silent 3 minutes ago: no meta, no rows.
	gap := f.end.Add(-3 * time.Minute).Unix()
	for _, table := range []string{"flow_1m", "flow_1m_meta"} {
		if _, err := f.db.Exec(`DELETE FROM `+table+` WHERE exporter_id = ? AND bucket = ?`, f.rbExp, gap); err != nil {
			t.Fatal(err)
		}
	}
	var r flowCoverageResponse
	if code, body := f.get(t, "/flows/coverage", map[string]string{"point": itoa(f.pWAN)}, &r); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if r.StepSeconds != 60 || len(r.Points) != 10 || !r.Points[0].TS.Equal(f.end.Add(-10*time.Minute)) ||
		r.CoverageFrom == nil || !r.To.Equal(f.end) {
		t.Fatalf("series shape: step %d, %d points, first %v", r.StepSeconds, len(r.Points), r.Points[0].TS)
	}
	val := func(p *int64) string {
		if p == nil {
			return "null"
		}
		return itoa(*p)
	}
	for _, pt := range r.Points {
		ago := int(f.end.Sub(pt.TS) / time.Minute)
		got := val(pt.FlowDownBps) + "/" + val(pt.FlowUpBps) + " " + val(pt.CounterDownBps) + "/" + val(pt.CounterUpBps)
		want := "8080/1200 8533/1267"
		switch ago {
		case 3:
			want = "null/null 8533/1267"
		case 6:
			want = "8080/1200 null/null"
		}
		if got != want {
			t.Errorf("%d min ago: %s, want %s", ago, got, want)
		}
	}
	// Ratio over the 8 minutes both covered.
	if r.Ratio.Download == nil || *r.Ratio.Download != 0.9469 || r.Ratio.Upload == nil || *r.Ratio.Upload != 0.9474 {
		t.Fatalf("ratio = %v %v", r.Ratio.Download, r.Ratio.Upload)
	}

	// No managed port: counters and ratio null.
	var fw flowCoverageResponse
	f.get(t, "/flows/coverage", map[string]string{"point": itoa(f.pFwLAN), "range": "15m"}, &fw)
	if len(fw.Points) != 10 || fw.Ratio.Download != nil || fw.Point.CoverageCapable {
		t.Fatalf("fw coverage: %d points ratio %v", len(fw.Points), fw.Ratio.Download)
	}
	for _, pt := range fw.Points {
		if pt.CounterDownBps != nil || pt.FlowDownBps == nil || *pt.FlowDownBps != 8533 {
			t.Fatalf("fw point %+v", pt)
		}
	}
	// 24h uses 5-minute steps; the partial first step is not diluted.
	var day flowCoverageResponse
	f.get(t, "/flows/coverage", map[string]string{"point": itoa(f.pFwLAN), "range": "24h"}, &day)
	if day.StepSeconds != 300 || len(day.Points) < 2 {
		t.Fatalf("24h: step %d points %d", day.StepSeconds, len(day.Points))
	}
	for _, pt := range day.Points {
		if pt.FlowDownBps == nil || *pt.FlowDownBps != 8533 {
			t.Fatalf("24h point %v = %s", pt.TS, val(pt.FlowDownBps))
		}
	}
	// No data: empty series.
	var week flowCoverageResponse
	f.get(t, "/flows/coverage", map[string]string{"point": itoa(f.pWAN), "range": "7d"}, &week)
	if week.Points == nil || len(week.Points) != 0 || week.StepSeconds != 3600 || week.CoverageFrom != nil {
		t.Fatalf("7d: %+v", week)
	}
	if code, _ := f.get(t, "/flows/coverage", nil, nil); code != 400 {
		t.Fatalf("missing point: %d", code)
	}
	if code, _ := f.get(t, "/flows/coverage", map[string]string{"point": "4242"}, nil); code != 404 {
		t.Fatalf("unknown point: %d", code)
	}
}

// checkFlowSankey asserts the measured-Sankey invariants: valid indices, no
// self links, values > 0, unique ids, acyclic, and the layer order of the
// direction.
func checkFlowSankey(t *testing.T, sk flowSankeyResponse) {
	t.Helper()
	ids := map[string]bool{}
	for _, n := range sk.Nodes {
		if ids[n.ID] {
			t.Fatalf("duplicate node %s", n.ID)
		}
		ids[n.ID] = true
	}
	layer := map[string]int{flowview.NodeRemote: 0, flowview.NodeApp: 0, flowview.NodePoint: 1, flowview.NodeHost: 2, flowview.NodePort: 3}
	lay := func(n flowview.SankeyNode) int {
		if n.ID == "other:remote" {
			return 0
		}
		if n.ID == "other:local" {
			return 2
		}
		return layer[n.Type]
	}
	for _, l := range sk.Links {
		if l.Source < 0 || l.Source >= len(sk.Nodes) || l.Target < 0 || l.Target >= len(sk.Nodes) || l.Source == l.Target {
			t.Fatalf("bad link %+v", l)
		}
		if l.Value <= 0 {
			t.Fatalf("non-positive link %+v", l)
		}
		from, to := lay(sk.Nodes[l.Source]), lay(sk.Nodes[l.Target])
		if sk.Direction == "upload" {
			from, to = to, from
		}
		if to != from+1 { // strictly layered → acyclic
			t.Fatalf("%s link %s → %s skips/reverses layers", sk.Direction, sk.Nodes[l.Source].ID, sk.Nodes[l.Target].ID)
		}
	}
}

func flowSankeyEdges(sk flowSankeyResponse, reverse bool) map[string]int64 {
	out := map[string]int64{}
	for _, l := range sk.Links {
		a, b := sk.Nodes[l.Source].ID, sk.Nodes[l.Target].ID
		if reverse {
			a, b = b, a
		}
		out[a+">"+b] = l.Value
	}
	return out
}

func TestFlowSankey(t *testing.T) {
	f := newFlowFixture(t)
	f.seedCounters(t, labRB, "eth1-wan", 64000, 9500)
	var sk flowSankeyResponse
	code, body := f.get(t, "/traffic/sankey", map[string]string{"source": "flows", "point": itoa(f.pWAN)}, &sk)
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	checkFlowSankey(t, sk)
	if sk.Source != "flows" || sk.Estimated || sk.Direction != "download" || sk.Range != "1h" || !sk.To.Equal(f.end) ||
		sk.SamplingRate != 1 || sk.TotalBps != 8080 || sk.Coverage == nil || *sk.Coverage != 0.9469 || sk.Point.ID != f.pWAN {
		t.Fatalf("header = %+v cov %v", sk, sk.Coverage)
	}
	pt := "point:" + itoa(f.pWAN)
	want := map[string]int64{
		"remote:1.1.1.1>" + pt:                          8000,
		"remote:2606:4700::1111>" + pt:                  13,
		"other:remote>" + pt:                            67,
		pt + ">host:192.168.28.33":                      8013,
		pt + ">other:local":                             67,
		"host:192.168.28.33>port:" + labSW + ":ether12": 8013,
	}
	if got := flowSankeyEdges(sk, false); !reflect.DeepEqual(got, want) {
		t.Fatalf("download edges = %v\nwant %v", got, want)
	}
	for _, n := range sk.Nodes {
		switch n.ID {
		case "host:192.168.28.33":
			if n.Name != "net28-client01" || n.Class != "internal" || n.IP != "192.168.28.33" {
				t.Errorf("host node %+v", n)
			}
		case "port:" + labSW + ":ether12":
			if n.Name != "switch001 · ether12" || n.DeviceID != labSW || n.Iface != "ether12" || n.Type != "port" {
				t.Errorf("port node %+v", n)
			}
		case "remote:1.1.1.1":
			if n.Class != "external" || n.Type != "remote" {
				t.Errorf("remote node %+v", n)
			}
		}
	}

	// Upload: port → host → point → remote.
	var up flowSankeyResponse
	f.get(t, "/traffic/sankey", map[string]string{"source": "flows", "point": itoa(f.pWAN), "dir": "upload"}, &up)
	checkFlowSankey(t, up)
	if got := flowSankeyEdges(up, true); got["remote:9.9.9.9>"+pt] != 400 || got[pt+">host:192.168.28.81"] != 400 ||
		got["host:192.168.28.33>port:"+labSW+":ether12"] != 800 || len(got) != 5 {
		t.Fatalf("upload edges (reversed) = %v", got)
	}
	if up.Coverage == nil || *up.Coverage != 0.9474 {
		t.Fatalf("upload coverage %v", up.Coverage)
	}

	// Apps as the remote layer; trunk view (no port attachment for the
	// router-local rows) and a point without a managed port.
	var apps flowSankeyResponse
	f.get(t, "/traffic/sankey", map[string]string{"source": "flows", "point": itoa(f.pWAN), "remote": "app"}, &apps)
	checkFlowSankey(t, apps)
	if got := flowSankeyEdges(apps, false); got["app:6/443>"+pt] != 8000 || got["app:17/443>"+pt] != 13 {
		t.Fatalf("app edges = %v", got)
	}
	for _, p := range []int64{f.pTrunk, f.pFwLAN, f.pNet28} {
		for _, dir := range []string{"download", "upload"} {
			var s flowSankeyResponse
			if code, body := f.get(t, "/traffic/sankey", map[string]string{"source": "flows", "point": itoa(p), "dir": dir}, &s); code != 200 {
				t.Fatalf("point %d %s: %d %s", p, dir, code, body)
			}
			checkFlowSankey(t, s)
			if len(s.Nodes) == 0 {
				t.Fatalf("point %d %s: empty", p, dir)
			}
		}
	}
	var fw flowSankeyResponse
	f.get(t, "/traffic/sankey", map[string]string{"source": "flows", "point": itoa(f.pFwLAN)}, &fw)
	if fw.Coverage != nil {
		t.Fatalf("no managed port but coverage %v", *fw.Coverage)
	}

	// Empty window.
	var week flowSankeyResponse
	f.get(t, "/traffic/sankey", map[string]string{"source": "flows", "point": itoa(f.pWAN), "range": "7d"}, &week)
	if week.Nodes == nil || week.Links == nil || len(week.Nodes) != 0 || len(week.Links) != 0 {
		t.Fatalf("7d: %+v", week)
	}

	for params, want := range map[string]string{
		"source=bogus":                    "invalid source",
		"source=flows":                    "point is required",
		"source=flows&point=x":            "invalid point",
		"source=flows&point=1&dir=x":      "invalid dir",
		"source=flows&point=1&remote=x":   "invalid remote",
		"source=flows&point=1&range=1y":   "invalid range",
		"source=flows&point=9999":         "point not found",
		"source=flows&point=9999&range=":  "point not found",
		"source=flows&point=9999&dir=upl": "invalid dir",
	} {
		code, body := f.do(t, http.MethodGet, "/traffic/sankey?"+params, nil, nil)
		wantCode := 400
		if want == "point not found" {
			wantCode = 404
		}
		if code != wantCode || body != `{"error":"`+want+`"}`+"\n" {
			t.Errorf("%s: %d %s", params, code, body)
		}
	}
}

// The Phase 1 estimated Sankey is unchanged apart from "source":"counters".
func TestTrafficSankeySourceCounters(t *testing.T) {
	f := newTrafficFixture(t)
	for _, src := range []string{"", "counters"} {
		var sk trafficSankeyResponse
		p := map[string]string{"range": "live"}
		if src != "" {
			p["source"] = src
		}
		if code := f.get(t, "/traffic/sankey", p, &sk); code != http.StatusOK {
			t.Fatalf("source=%q: %d", src, code)
		}
		checkSankeyShape(t, sk)
		if sk.Source != "counters" || !sk.Estimated || len(sk.Links) == 0 {
			t.Fatalf("source=%q: %+v", src, sk)
		}
	}
}

// Hourly ranges: hour buckets are often partial (the collector started
// mid-hour, the newest hour is still running, or a downtime). Each hour
// counts its covered minutes (flow_1h_meta.minutes), so averages are not
// diluted by the uncovered part of an hour.
func TestFlowTopHourlyPartialFirstHour(t *testing.T) {
	f := newFlowFixture(t)
	first := f.end.Add(-flowFixtureMinutes * time.Minute)
	last := f.end.Add(-time.Minute).Truncate(time.Hour)
	for h := first.Truncate(time.Hour); !h.After(last); h = h.Add(time.Hour) {
		if err := queries.RollupFlows1h(f.db, f.rbExp, h, 100); err != nil {
			t.Fatal(err)
		}
	}
	f.fake.rolled = last.Add(time.Hour)
	var week flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN), "range": "7d"}, &week)
	wantSecs := int64(flowFixtureMinutes * 60)
	if week.Resolution != "1h" || week.CoverageFrom == nil || !week.CoverageFrom.Equal(first) || week.Seconds != wantSecs {
		t.Fatalf("7d: coverage_from %v seconds %d, want %v / %d", week.CoverageFrom, week.Seconds, first, wantSecs)
	}
	var hour flowTopResponse
	f.get(t, "/flows/top", map[string]string{"point": itoa(f.pWAN), "range": "1h"}, &hour)
	if week.TotalBytes != hour.TotalBytes || week.TotalBps != int64(math.Round(float64(week.TotalBytes*8)/float64(wantSecs))) {
		t.Fatalf("7d total %d bps %d vs 1h total %d", week.TotalBytes, week.TotalBps, hour.TotalBytes)
	}
}
