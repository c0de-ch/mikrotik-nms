package decode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
	"github.com/netsampler/goflow2/v2/decoders/netflow"
)

func mustDecode(t *testing.T, e *Exporter, pkt []byte, now time.Time) Result {
	t.Helper()
	res, err := e.Decode(pkt, now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return res
}

func learnedROS(t *testing.T) *Exporter {
	t.Helper()
	e := NewExporter(addrROS)
	mustDecode(t, e, loadFixture(t, "ros_ipfix_templates"), tExport.Add(tRecvLag))
	return e
}

func TestRouterOSIPFIXFields(t *testing.T) {
	e := learnedROS(t)
	res := mustDecode(t, e, loadFixture(t, "ros_ipfix_data"), tExport.Add(tRecvLag))
	if len(res.Flows) != 6 {
		t.Fatalf("flows = %d, want 6", len(res.Flows))
	}
	r1, r2, r4, r6 := res.Flows[0], res.Flows[1], res.Flows[3], res.Flows[5]
	// sysuptime times = sysInit + uptime, exact to the millisecond.
	if r1.TimeSource != TimeSysUptime || !r1.Start.Equal(tExport.Add(-45*time.Second)) || !r1.End.Equal(tExport.Add(-2*time.Second)) {
		t.Errorf("R1 times = %v..%v (%s)", r1.Start, r1.End, r1.TimeSource)
	}
	if r1.RawBytes != 5_000_000_000 || r1.Bytes != 5_000_000_000 || r1.Packets != 3_500_000 || r1.SamplingRate != 1 {
		t.Errorf("R1 64-bit counters = %d / %d", r1.Bytes, r1.Packets)
	}
	if r1.InIf != 1 || r1.OutIf != 13 || r1.PostNATDst != netip.MustParseAddr("192.168.28.33") || r1.PostNATDstPort != 50514 {
		t.Errorf("R1 ifaces/NAT = %d/%d %v:%d", r1.InIf, r1.OutIf, r1.PostNATDst, r1.PostNATDstPort)
	}
	if r2.PostNATSrc != netip.MustParseAddr("100.114.241.213") || r2.PostNATSrcPort != 61000 || r2.InIf != 13 || r2.OutIf != 1 {
		t.Errorf("R2 = %+v", r2)
	}
	if r4.PostNATDst != netip.IPv4Unspecified() || r4.OutIf != 0 {
		t.Errorf("R4 post-NAT dst = %v, out %d (want 0.0.0.0, local)", r4.PostNATDst, r4.OutIf)
	}
	if r6.SrcAddr != netip.MustParseAddr("2606:4700::1111") || r6.DstAddr != netip.MustParseAddr("2a01:4f8:202:13d1:28::33") ||
		r6.DstPort != 50600 || r6.Bytes != 64_000 {
		t.Errorf("R6 IPv6 = %+v", r6)
	}
	if res.ClockSkew != tRecvLag || !res.ExportTime.Equal(tExport) {
		t.Errorf("skew %v export %v", res.ClockSkew, res.ExportTime)
	}
}

func TestRouterOSIPFIXUptimeEstimate(t *testing.T) {
	e := NewExporter(addrROS)
	res := mustDecode(t, e, loadFixture(t, "ros_ipfix_no160"), tExport.Add(tRecvLag))
	truth := rosFlows(true)
	if len(res.Flows) != len(truth) {
		t.Fatalf("flows = %d", len(res.Flows))
	}
	for i, f := range res.Flows {
		wantStart := time.UnixMilli(int64(tSysInit) + int64(truth[i].StartUp))
		wantEnd := time.UnixMilli(int64(tSysInit) + int64(truth[i].EndUp))
		if f.TimeSource != TimeUptimeEst {
			t.Errorf("flow %d time source %s, want uptime_est", i, f.TimeSource)
		}
		if d := f.Start.Sub(wantStart); d < 0 || d >= time.Second {
			t.Errorf("flow %d start off by %v (want within 1 s)", i, d)
		}
		if d := f.End.Sub(wantEnd); d < 0 || d >= time.Second {
			t.Errorf("flow %d end off by %v (want within 1 s)", i, d)
		}
	}
}

func TestUptimeWrapIPFIX(t *testing.T) {
	// A RouterOS up 60 days: IE 22/21 (unsigned32 ms) wrapped once.
	sysInit := uint64(tExport.Add(-60 * 24 * time.Hour).UnixMilli())
	up := uint64(tExport.UnixMilli()) - sysInit // > 2^32
	f := rosFlows(true)[1]
	f.SysInitMs = sysInit
	f.StartUp, f.EndUp = uint32(up-10_000), uint32(up-1_000)
	e := NewExporter(addrROS)
	res := mustDecode(t, e, pktgen.RouterOSIPFIXMessage(exportSecs, 1, 0, true, f), tExport)
	if got := res.Flows[0]; got.TimeSource != TimeSysUptime || !got.End.Equal(tExport.Add(-time.Second)) ||
		!got.Start.Equal(tExport.Add(-10*time.Second)) {
		t.Fatalf("wrapped uptime times = %v..%v (%s)", got.Start, got.End, got.TimeSource)
	}
}

func TestOPNsenseTemplateAfterData(t *testing.T) {
	e := NewExporter(addrOPN)
	e.SkipV9Seq = true
	res := mustDecode(t, e, loadFixture(t, "opn_v9_data_first"), tExport.Add(tRecvLag))
	if len(res.Flows) != 0 || e.Stats.TemplateMisses != 2 || e.PendingSets() != 2 {
		t.Fatalf("data first: flows %d misses %d pending %d", len(res.Flows), e.Stats.TemplateMisses, e.PendingSets())
	}
	res = mustDecode(t, e, loadFixture(t, "opn_v9_templates"), tExport.Add(5*time.Second+tRecvLag))
	if len(res.Flows) != 3 || e.Stats.Replayed != 2 || e.PendingSets() != 0 || e.Stats.SeqLost != 0 {
		t.Fatalf("replay: flows %d replayed %d pending %d seq_lost %d", len(res.Flows), e.Stats.Replayed, e.PendingSets(), e.Stats.SeqLost)
	}
	o1, o2 := res.Flows[0], res.Flows[1]
	// Times from the data datagram's header, not the template one.
	if !o1.Start.Equal(tExport.Add(-40*time.Second)) || !o1.End.Equal(tExport.Add(-time.Second)) {
		t.Errorf("O1 times = %v..%v", o1.Start, o1.End)
	}
	if o1.Bytes != 1480 || o1.Packets != 12 || o1.InIf != 1 || o1.OutIf != 7 || o2.Bytes != 64_000 || o2.InIf != 7 {
		t.Errorf("O1/O2 = %+v / %+v", o1, o2)
	}
	if len(res.Learned) != 2 || res.Learned[0].ID != 256 || res.Learned[1].ID != 259 {
		t.Errorf("learned = %+v", res.Learned)
	}
	// Without SkipV9Seq the interleaved node counters (10 then 3) are
	// reordering, never loss; a forward jump is.
	e2 := NewExporter(addrOPN)
	mustDecode(t, e2, pktgen.OPNsenseV9Templates(exportSecs, tOPNUp, 1), tExport)
	mustDecode(t, e2, pktgen.OPNsenseV9Templates(exportSecs, tOPNUp, 5), tExport)
	if e2.Stats.SeqLost != 3 {
		t.Errorf("v9 seq_lost = %d, want 3", e2.Stats.SeqLost)
	}
}

func TestV9WrapAndPadding(t *testing.T) {
	e := NewExporter(addrOPN)
	res := mustDecode(t, e, loadFixture(t, "opn_v9_wrap"), tExport.Add(tRecvLag))
	f := res.Flows[0]
	if !f.End.Equal(tExport.Add(-512*time.Millisecond)) || !f.Start.Equal(tExport.Add(-65792*time.Millisecond)) {
		t.Fatalf("wrap times = %v..%v", f.Start, f.End)
	}
	// Trailing padding after the last flowset and count = records (not
	// flowsets): no decode error, all records decoded.
	pkt := append(pktgen.OPNsenseV9TemplatesAndData(exportSecs, 1000, 2, opnFlows(1000)...), 0, 0)
	e = NewExporter(addrOPN)
	res, err := e.Decode(pkt, tExport)
	if err != nil || len(res.Flows) != 3 {
		t.Fatalf("padded v9: err %v flows %d", err, len(res.Flows))
	}
	// A flowset running past the datagram is malformed: earlier sets are kept.
	bad := append(pktgen.OPNsenseV9TemplatesAndData(exportSecs, 2000, 3, opnFlows(2000)[0]), 1, 0, 0, 200, 9, 9)
	res, err = e.Decode(bad, tExport.Add(time.Second))
	if !errors.Is(err, ErrMalformedV9) || len(res.Flows) != 1 {
		t.Fatalf("malformed v9: err %v flows %d", err, len(res.Flows))
	}
}

func TestV5AndSFlow(t *testing.T) {
	e := NewExporter(addrSW)
	res := mustDecode(t, e, loadFixture(t, "v5"), tExport)
	if len(res.Flows) != 2 || res.Flows[0].SamplingRate != 64 || res.Flows[0].Bytes != 1500*64 {
		t.Fatalf("v5 = %+v", res.Flows)
	}
	_, err := e.Decode(loadFixture(t, "v5_truncated"), tExport)
	if !errors.Is(err, ErrBadV5) || !errors.Is(err, ErrRejected) || e.Stats.Rejected != 1 || e.Stats.DecodeErrors != 0 {
		t.Fatalf("truncated v5: err %v stats %+v", err, e.Stats)
	}
	res = mustDecode(t, e, loadFixture(t, "sflow_switch"), tExport)
	if len(res.Flows) != 1 || len(res.Counters) != 1 {
		t.Fatalf("sflow: %d flows %d counters", len(res.Flows), len(res.Counters))
	}
	if f := res.Flows[0]; f.Bytes != 1518*512 || f.Packets != 512 || f.SamplingRate != 512 || f.VLAN != 28 ||
		f.DstPort != 443 || f.InIf != 1 || f.OutIf != 2 || !f.End.Equal(tExport) {
		t.Fatalf("sflow flow = %+v", f)
	}
	// sFlow sequence loss per agent.
	hdr := pktgen.EthIPv4TCPHeader(1, "10.0.0.1", "10.0.0.2", 1, 2, 100)
	mustDecode(t, e, pktgen.SFlowSwitch("192.168.78.201", 5, 1, 1, 1, 2, 100, hdr), tExport)
	if e.Stats.SeqLost != 3 {
		t.Fatalf("sflow seq_lost = %d, want 3 (1 -> 5)", e.Stats.SeqLost)
	}
}

func TestSamplingPrecedence(t *testing.T) {
	// Options before data in the same datagram.
	res := mustDecode(t, NewExporter(addrROS), loadFixture(t, "ipfix_options_sampling"), tExport)
	if f := res.Flows[0]; f.SamplingRate != 100 || f.Bytes != 80_000*100 || f.RawBytes != 80_000 {
		t.Fatalf("305/306 rate = %d bytes %d", f.SamplingRate, f.Bytes)
	}
	res = mustDecode(t, NewExporter(addrOPN), loadFixture(t, "v9_options_sampling"), tExport)
	if f := res.Flows[0]; f.SamplingRate != 10 || f.Packets != 120 {
		t.Fatalf("v9 IE 34 rate = %d packets %d", f.SamplingRate, f.Packets)
	}
	// An announced rate beats the override; the override applies when
	// nothing is announced.
	e := NewExporter(addrROS)
	e.SamplingOverride = 7
	res = mustDecode(t, e, loadFixture(t, "ipfix_options_sampling"), tExport)
	if res.Flows[0].SamplingRate != 100 || e.SamplingRate(10, 0) != 100 {
		t.Fatalf("announced vs override = %d", res.Flows[0].SamplingRate)
	}
	e = learnedROS(t)
	e.SamplingOverride = 7
	res = mustDecode(t, e, loadFixture(t, "ros_ipfix_data"), tExport)
	if res.Flows[0].SamplingRate != 7 || res.Flows[0].Bytes != 5_000_000_000*7 {
		t.Fatalf("override = %d", res.Flows[0].SamplingRate)
	}
	// A per-record IE 34 beats both.
	r := rawRecord{f: Flow{RawBytes: 10, RawPackets: 1}, sampInt: 4}
	r.scale(100)
	if r.f.SamplingRate != 4 || r.f.Bytes != 40 {
		t.Fatalf("per-record 34 = %d", r.f.SamplingRate)
	}
	r = rawRecord{f: Flow{RawBytes: 10, RawPackets: 1}, sampInt: 1, sampSpace: 9}
	r.scale(100)
	if r.f.SamplingRate != 10 {
		t.Fatalf("per-record 305/306 = %d", r.f.SamplingRate)
	}
}

func TestClockSkewAndSanitize(t *testing.T) {
	// 30 s skew: the flow times move into the collector's clock.
	e := learnedROS(t)
	res := mustDecode(t, e, loadFixture(t, "ros_ipfix_data"), tExport.Add(30*time.Second))
	if res.ClockSkew != 30*time.Second || !res.Flows[0].End.Equal(tExport.Add(28*time.Second)) {
		t.Fatalf("skew %v end %v", res.ClockSkew, res.Flows[0].End)
	}
	// 3 s skew: below the threshold, no shift.
	res = mustDecode(t, e, loadFixture(t, "ros_ipfix_data"), tExport.Add(3*time.Second))
	if !res.Flows[0].End.Equal(tExport.Add(-2 * time.Second)) {
		t.Fatalf("small skew shifted: %v", res.Flows[0].End)
	}
	// Garbage sysInit (3 days off): outside [export-2h, export+1m] -> export.
	f := rosFlows(true)[0]
	f.SysInitMs = tSysInit - 3*24*3600*1000
	e = NewExporter(addrROS)
	res = mustDecode(t, e, pktgen.RouterOSIPFIXMessage(exportSecs, 1, 0, true, f), tExport)
	if g := res.Flows[0]; g.TimeSource != TimeExport || !g.Start.Equal(tExport) || !g.End.Equal(tExport) {
		t.Fatalf("sanitize fallback = %v..%v %s", g.Start, g.End, g.TimeSource)
	}
	// End before start swaps.
	x := Flow{Start: tExport, End: tExport.Add(-time.Second), TimeSource: TimeSysUptime}
	sanitizeTimes(&x, tExport)
	if !x.Start.Equal(tExport.Add(-time.Second)) || x.TimeSource != TimeSysUptime {
		t.Fatalf("swap = %+v", x)
	}
}

func TestRebootV9UptimeRegression(t *testing.T) {
	e := NewExporter(addrOPN)
	e.SkipV9Seq = true
	mustDecode(t, e, loadFixture(t, "opn_v9_templates"), tExport)
	if e.Templates.Len() != 2 {
		t.Fatal("templates not learned")
	}
	// Uptime 10 min back, 1 s later: reboot, templates dropped, data pending.
	res := mustDecode(t, e, pktgen.OPNsenseV9Data(exportSecs+1, tOPNUp+5_000-600_000, 4, opnFlows(tOPNUp)[0]), tExport.Add(time.Second))
	if !res.Rebooted || e.Templates.Len() != 0 || len(res.Flows) != 0 || e.PendingSets() != 1 || e.Stats.Reboots != 1 {
		t.Fatalf("reboot: %v templates %d flows %d pending %d", res.Rebooted, e.Templates.Len(), len(res.Flows), e.PendingSets())
	}
	// A reboot datagram that re-announces its templates keeps them.
	e = NewExporter(addrOPN)
	mustDecode(t, e, pktgen.OPNsenseV9Templates(exportSecs, tOPNUp, 1), tExport)
	res = mustDecode(t, e, pktgen.OPNsenseV9TemplatesAndData(exportSecs+1, 1000, 2, opnFlows(1000)[0]), tExport.Add(time.Second))
	if !res.Rebooted || e.Templates.Len() != 2 || len(res.Flows) != 1 || len(res.Learned) != 2 {
		t.Fatalf("reboot + templates: %v %d %d %d", res.Rebooted, e.Templates.Len(), len(res.Flows), len(res.Learned))
	}
	// Normal progress (uptime follows the wall clock, even across the
	// 2^32 ms wrap) is not a reboot.
	e = NewExporter(addrOPN)
	before := uint32(0xFFFFF000)
	mustDecode(t, e, pktgen.OPNsenseV9Templates(exportSecs, before, 1), tExport)
	res = mustDecode(t, e, pktgen.OPNsenseV9Templates(exportSecs+10, before+10_000, 2), tExport.Add(10*time.Second))
	if res.Rebooted {
		t.Fatal("uptime wrap mistaken for a reboot")
	}
}

func TestRebootIPFIX(t *testing.T) {
	e := learnedROS(t)
	mustDecode(t, e, loadFixture(t, "ros_ipfix_data"), tExport)
	// IE 160 moved by 10 s: reboot; the data datagram carried no template.
	flows := rosFlows(true)
	for i := range flows {
		flows[i].SysInitMs += 10_000
	}
	res := mustDecode(t, e, pktgen.RouterOSIPFIXData(exportSecs+60, 7, 0, flows...), tExport.Add(time.Minute))
	if !res.Rebooted || e.Templates.Len() != 0 {
		t.Fatalf("IE 160 change: rebooted %v templates %d", res.Rebooted, e.Templates.Len())
	}
	// A small IE 160 jitter (< 5 s) is not a reboot.
	e = learnedROS(t)
	flows = rosFlows(true)
	flows[0].SysInitMs += 2_000
	if res := mustDecode(t, e, pktgen.RouterOSIPFIXData(exportSecs, 1, 0, flows...), tExport); res.Rebooted {
		t.Fatal("IE 160 jitter mistaken for a reboot")
	}
	// Sequence reset from > 1000 to < 100.
	e = NewExporter(addrROS)
	mustDecode(t, e, pktgen.RouterOSIPFIXTemplates(exportSecs, 5000, 0, true), tExport)
	res = mustDecode(t, e, pktgen.RouterOSIPFIXTemplates(exportSecs, 3, 0, true), tExport)
	if !res.Rebooted || e.Templates.Len() != 2 {
		t.Fatalf("seq reset: rebooted %v templates %d (re-announced ones kept)", res.Rebooted, e.Templates.Len())
	}
}

func TestIPFIXSequenceLoss(t *testing.T) {
	e := learnedROS(t)                                                                       // templates at seq 1, 0 records -> next 1
	mustDecode(t, e, pktgen.RouterOSIPFIXData(exportSecs, 1, 0, rosFlows(true)...), tExport) // 6 records -> next 7
	mustDecode(t, e, pktgen.RouterOSIPFIXData(exportSecs, 10, 0, rosFlows(true)[0]), tExport)
	if e.Stats.SeqLost != 3 {
		t.Fatalf("ipfix seq_lost = %d, want 3", e.Stats.SeqLost)
	}
	// Data before its template: the record count is unknown, skip one check.
	e = NewExporter(addrROS)
	mustDecode(t, e, pktgen.RouterOSIPFIXData(exportSecs, 1, 0, rosFlows(true)...), tExport)
	mustDecode(t, e, pktgen.RouterOSIPFIXTemplates(exportSecs, 7, 0, true), tExport)
	if e.Stats.SeqLost != 0 {
		t.Fatalf("seq_lost after raw sets = %d, want 0", e.Stats.SeqLost)
	}
}

func TestTemplatePersistenceRoundTrip(t *testing.T) {
	a := NewExporter(addrOPN)
	res := mustDecode(t, a, loadFixture(t, "opn_v9_templates"), tExport)
	if len(res.Learned) != 2 {
		t.Fatalf("learned = %d", len(res.Learned))
	}
	// A fresh process loads the persisted blobs and decodes data at once.
	b := NewExporter(addrOPN)
	b.SkipV9Seq = true
	n, errs := b.LoadTemplates(res.Learned)
	if n != 2 || len(errs) != 0 || !b.Templates.PersistedOnly() {
		t.Fatalf("load = %d %v persisted-only %v", n, errs, b.Templates.PersistedOnly())
	}
	out := mustDecode(t, b, loadFixture(t, "opn_v9_data_first"), tExport)
	if len(out.Flows) != 3 || b.Stats.TemplateMisses != 0 || len(out.Learned) != 0 {
		t.Fatalf("decode with persisted templates: %d flows, %d misses", len(out.Flows), b.Stats.TemplateMisses)
	}
	// A template received from the wire replaces the persisted one.
	mustDecode(t, b, loadFixture(t, "opn_v9_templates"), tExport.Add(5*time.Second))
	if b.Templates.PersistedOnly() {
		t.Fatal("received templates must clear persisted-only")
	}
	// Options templates round-trip too (IPFIX and v9).
	for _, fx := range []string{"ipfix_options_sampling", "v9_options_sampling"} {
		src := NewExporter(addrROS)
		r := mustDecode(t, src, loadFixture(t, fx), tExport)
		dst := NewExporter(addrROS)
		if n, errs := dst.LoadTemplates(r.Learned); n != 3 || len(errs) != 0 {
			t.Fatalf("%s: load options = %d %v", fx, n, errs)
		}
		for _, l := range r.Learned {
			got, ok := dst.Templates.Export(l.Key())
			if !ok || got.Kind != l.Kind || !bytes.Equal(got.Fields, l.Fields) {
				t.Fatalf("%s: round trip of %+v = %+v", fx, l, got)
			}
		}
	}
	// Corrupt blobs are skipped with an error, never installed.
	bad := []TemplateRecord{
		{Version: 10, ID: 256, Kind: KindData, Fields: []byte{0, 0, 1}},
		{Version: 10, ID: 257, Kind: KindData, Fields: []byte{0, 0}},                         // no fields
		{Version: 10, ID: 258, Kind: KindData, Fields: []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0}}, // zero width
		{Version: 10, ID: 259, Kind: KindData, Fields: []byte{0, 1, 0, 1, 0, 4, 0, 0, 0, 0}}, // scope on data
		{Version: 7, ID: 260, Kind: KindData, Fields: []byte{0, 0, 0, 1, 0, 4, 0, 0, 0, 0}},  // bad version
	}
	c := NewExporter(addrROS)
	if n, errs := c.LoadTemplates(bad); n != 0 || len(errs) != len(bad) || c.Templates.Len() != 0 {
		t.Fatalf("corrupt blobs: loaded %d errs %d", n, len(errs))
	}
}

func TestGuardsAndCaps(t *testing.T) {
	// Zero-field template = withdrawal; degenerate template rejected.
	for _, name := range []string{"ipfix_zero_field_template", "ipfix_degenerate_template"} {
		done := make(chan error, 1)
		go func() { _, err := NewExporter(addrROS).Decode(loadFixture(t, name), tExport); done <- err }()
		select {
		case err := <-done:
			if name == "ipfix_degenerate_template" && !errors.Is(err, ErrDegenerateTemplate) {
				t.Errorf("%s: err %v", name, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: hang", name)
		}
	}
	st := NewSafeTemplates(64)
	tr := func(id uint16) netflow.TemplateRecord {
		return netflow.TemplateRecord{TemplateId: id, FieldCount: 1, Fields: []netflow.Field{{Type: 1, Length: 4}}}
	}
	for id := uint16(256); id < 256+64; id++ {
		if err := st.AddTemplate(10, 0, id, tr(id)); err != nil {
			t.Fatalf("template %d: %v", id, err)
		}
	}
	if !errors.Is(st.AddTemplate(10, 0, 400, tr(400)), ErrTooManyTemplates) || st.Len() != 64 {
		t.Fatal("> 64 templates not capped")
	}
	if st.AddTemplate(10, 0, 256, tr(256)) != nil {
		t.Fatal("refresh of a known template must pass")
	}
	wide := netflow.TemplateRecord{TemplateId: 999, FieldCount: 129, Fields: make([]netflow.Field, 129)}
	for i := range wide.Fields {
		wide.Fields[i] = netflow.Field{Type: 1, Length: 1}
	}
	if !errors.Is(NewSafeTemplates(64).AddTemplate(10, 0, 999, wide), ErrTemplateTooWide) {
		t.Fatal("> 128 fields not rejected")
	}
	// > 16 observation domains: the 17th domain's data is dropped.
	e := NewExporter(addrROS)
	for obs := uint32(0); obs < MaxDomains; obs++ {
		mustDecode(t, e, pktgen.RouterOSIPFIXTemplates(exportSecs, 1, obs, true), tExport)
	}
	res, err := e.Decode(pktgen.RouterOSIPFIXMessage(exportSecs, 1, 99, true, rosFlows(true)...), tExport)
	if !errors.Is(err, ErrTooManyDomains) || len(res.Flows) != 0 || errors.Is(err, ErrRejected) {
		t.Fatalf("17th domain: err %v flows %d", err, len(res.Flows))
	}
	if _, err := NewExporter(addrROS).Decode(loadFixture(t, "sflow_expanded_huge_count"), tExport); !errors.Is(err, ErrSFlowRecordCount) {
		t.Fatalf("huge sflow count: %v", err)
	}
	if _, err := NewExporter(addrROS).Decode([]byte{0, 10, 0}, tExport); !errors.Is(err, ErrRejected) {
		t.Fatalf("3-byte datagram: %v", err)
	}
	if _, err := NewExporter(addrROS).Decode([]byte{0, 7, 0, 0, 1, 2, 3, 4}, tExport); !errors.Is(err, ErrRejected) {
		t.Fatalf("unknown version: %v", err)
	}
}

func TestPendingLimits(t *testing.T) {
	e := NewExporter(addrOPN)
	e.SkipV9Seq = true
	data := pktgen.OPNsenseV9Data(exportSecs, tOPNUp, 1, opnFlows(tOPNUp)[0])
	for i := 0; i < MaxPendingSets+5; i++ {
		mustDecode(t, e, data, tExport)
	}
	if e.PendingSets() != MaxPendingSets || e.Stats.PendingDropped != 5 || e.Stats.TemplateMisses != MaxPendingSets+5 {
		t.Fatalf("pending %d dropped %d misses %d", e.PendingSets(), e.Stats.PendingDropped, e.Stats.TemplateMisses)
	}
	// TTL: everything older than 2 min is dropped.
	e.ExpirePending(tExport.Add(PendingTTL))
	if e.PendingSets() != 0 || e.Stats.PendingDropped != MaxPendingSets+5 {
		t.Fatalf("after TTL: pending %d dropped %d", e.PendingSets(), e.Stats.PendingDropped)
	}
	// The byte cap: 256 KiB of pending data.
	e = NewExporter(addrROS)
	big := make([]pktgen.IPFIXFlow, 90) // ~6.6 KB per set
	for i := range big {
		big[i] = rosFlows(true)[1]
	}
	pkt := pktgen.RouterOSIPFIXData(exportSecs, 1, 0, big...)
	for i := 0; i < MaxPendingSets; i++ {
		mustDecode(t, e, pkt, tExport)
	}
	if e.pendSize > MaxPendingBytes || e.Stats.PendingDropped == 0 {
		t.Fatalf("byte cap: %d bytes pending, dropped %d", e.pendSize, e.Stats.PendingDropped)
	}
}

func TestPendingNeverAliasesInput(t *testing.T) {
	e := NewExporter(addrOPN)
	e.SkipV9Seq = true
	pkt := loadFixture(t, "opn_v9_data_first")
	mustDecode(t, e, pkt, tExport)
	for i := range pkt {
		pkt[i] = 0xff
	}
	res := mustDecode(t, e, loadFixture(t, "opn_v9_templates"), tExport.Add(5*time.Second))
	if len(res.Flows) != 3 || res.Flows[0].Bytes != 1480 {
		t.Fatalf("replay after the input buffer was reused: %+v", res.Flows)
	}
}

// crasher returns the upstream raw-fuzz crasher (a go test fuzz v1 corpus
// entry) that made unguarded goflow2 v2.2.6 allocate ~19 GB.
func crasher(t testing.TB) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/fuzz/FuzzDecode/092c738507f6cd0b")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Split(string(raw), "\n")[1]
	s, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(line, "[]byte("), ")"))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(s)
}

func TestUpstreamCrasherReturnsQuickly(t *testing.T) {
	b := crasher(t)
	done := make(chan error, 1)
	go func() { _, err := NewExporter(addrROS).Decode(b, time.Now()); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Log("crasher decoded without error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("guarded decode of the upstream crasher did not return within 3 s")
	}
}

func TestSniff(t *testing.T) {
	cases := map[string]Kind{"\x00\x05": KindNFv5, "\x00\x09": KindNFv9, "\x00\x0a": KindIPFIX,
		"\x00\x00\x00\x05": KindSFlow, "\x00\x00\x00\x04": KindUnknown, "\x01": KindUnknown, "\x45\x00": KindUnknown}
	for in, want := range cases {
		if got := Sniff([]byte(in)); got != want {
			t.Errorf("Sniff(%x) = %v, want %v", in, got, want)
		}
	}
	for k, want := range map[Kind]string{KindNFv5: "netflow5", KindNFv9: "netflow9", KindIPFIX: "ipfix", KindSFlow: "sflow5", KindUnknown: ""} {
		if k.String() != want {
			t.Errorf("%d.String() = %q", k, k.String())
		}
	}
	// normalizeV9 never grows the packet and patches count.
	pkt := pktgen.OPNsenseV9Templates(1, 2, 3)
	out, err := normalizeV9(append(pkt, 0, 0, 0))
	if err != nil || len(out) != len(pkt) || binary.BigEndian.Uint16(out[2:4]) != 1 {
		t.Fatalf("normalizeV9: len %d count %d err %v", len(out), binary.BigEndian.Uint16(out[2:4]), err)
	}
}
