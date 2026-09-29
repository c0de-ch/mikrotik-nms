package decode

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/flow/pktgen"
)

// go test ./internal/flow/decode -run TestGolden -update regenerates the
// .hex fixtures and the .golden.json expectations under testdata/.
var update = flag.Bool("update", false, "rewrite testdata fixtures and golden files")

// Fixed fixture times: export 1790000000 = 2026-09-21T13:33:20Z; the
// RouterOS sysInit is 72 h earlier; OPNsense's header SysUptime 500 000 000.
var (
	tExport  = time.Unix(1_790_000_000, 0).UTC()
	tSysInit = uint64(tExport.Add(-72 * time.Hour).UnixMilli())
	tUptime  = uint32(uint64(tExport.UnixMilli()) - tSysInit) // RouterOS uptime at export
	tOPNUp   = uint32(500_000_000)
	tRecvLag = 500 * time.Millisecond // receive = header export time + this

	addrROS = netip.MustParseAddr("192.168.78.202")
	addrOPN = netip.MustParseAddr("192.168.78.81")
	addrSW  = netip.MustParseAddr("192.168.78.201")
)

const exportSecs = uint32(1_790_000_000)

// rosUp is the RouterOS uptime d before the export (d <= 0).
func rosUp(d time.Duration) uint32 { return uint32(int64(tUptime) + d.Milliseconds()) }

// rosFlows are R1–R6 of the contract (§13.1 fixture 2); withSysInit=false
// drops IE 160 (fixture 3).
func rosFlows(withSysInit bool) []pktgen.IPFIXFlow {
	si := tSysInit
	if !withSysInit {
		si = 0
	}
	return []pktgen.IPFIXFlow{
		{ // R1 download, NATed back to the net28 host; 64-bit counters
			Src: "142.250.203.100", Dst: "100.114.241.213", SrcPort: 443, DstPort: 50514, Proto: 6, TCPFlags: 0x18,
			Bytes: 5_000_000_000, Packets: 3_500_000, InIf: 1, OutIf: 13, NextHop: "192.168.28.33",
			StartUp: rosUp(-45 * time.Second), EndUp: rosUp(-2 * time.Second), SysInitMs: si,
			NatSrc: "142.250.203.100", NatDst: "192.168.28.33", NatSrcPort: 443, NatDstPort: 50514,
		},
		{ // R2 upload, masqueraded out eth1-wan
			Src: "192.168.28.33", Dst: "142.250.203.100", SrcPort: 50514, DstPort: 443, Proto: 6, TCPFlags: 0x18,
			Bytes: 80_000, Packets: 100, InIf: 13, OutIf: 1, NextHop: "100.64.0.1",
			StartUp: rosUp(-45 * time.Second), EndUp: rosUp(-2 * time.Second), SysInitMs: si,
			NatSrc: "100.114.241.213", NatDst: "142.250.203.100", NatSrcPort: 61000, NatDstPort: 443,
		},
		{ // R3 firewall003's own NATed traffic
			Src: "192.168.28.81", Dst: "1.1.1.1", SrcPort: 40000, DstPort: 443, Proto: 6,
			Bytes: 10_000, Packets: 20, InIf: 13, OutIf: 1, NextHop: "100.64.0.1",
			StartUp: rosUp(-30 * time.Second), EndUp: rosUp(-1 * time.Second), SysInitMs: si,
			NatSrc: "100.114.241.213", NatDst: "1.1.1.1", NatSrcPort: 40001, NatDstPort: 443,
		},
		{ // R4 the NMS polling the router (local delivery, no NAT: 226 = 0.0.0.0)
			Src: "192.168.79.216", Dst: "192.168.78.202", SrcPort: 51000, DstPort: 8728, Proto: 6,
			Bytes: 5_000, Packets: 40, InIf: 10, OutIf: 0,
			StartUp: rosUp(-59 * time.Second), EndUp: rosUp(-500 * time.Millisecond), SysInitMs: si,
		},
		{ // R5 the router's own IPFIX export (self-export)
			Src: "192.168.78.202", Dst: "192.168.79.216", SrcPort: 40001, DstPort: 2055, Proto: 17,
			Bytes: 3_000, Packets: 3, InIf: 0, OutIf: 10,
			StartUp: rosUp(-60 * time.Second), EndUp: rosUp(0), SysInitMs: si,
		},
		{ // R6 IPv6 download
			Src: "2606:4700::1111", Dst: "2a01:4f8:202:13d1:28::33", SrcPort: 443, DstPort: 50600, Proto: 6,
			Bytes: 64_000, Packets: 50, InIf: 1, OutIf: 13,
			StartUp: rosUp(-20 * time.Second), EndUp: rosUp(-3 * time.Second), SysInitMs: si,
		},
	}
}

// opnFlows are O1–O3 (fixture 4): uptime is the header SysUptime.
func opnFlows(uptime uint32) []pktgen.V9Flow {
	return []pktgen.V9Flow{
		{ // O1 upload from the LAN
			Src: "192.168.78.60", Dst: "9.9.9.9", NextHop: "192.168.111.1", InIf: 1, OutIf: 7, Packets: 12,
			Bytes: 1480, First: uptime - 40_000, Last: uptime - 1_000, SrcPort: 53124, DstPort: 853, TCPFlags: 0x1b,
			Proto: 6, SrcMask: 23, DstMask: 0,
		},
		{ // O2 the matching download (LAN egress)
			Src: "9.9.9.9", Dst: "192.168.78.60", InIf: 7, OutIf: 1, Packets: 50, Bytes: 64_000,
			First: uptime - 40_000, Last: uptime - 1_000, SrcPort: 853, DstPort: 53124, TCPFlags: 0x1b, Proto: 6,
			SrcMask: 0, DstMask: 23,
		},
		{ // O3 IPv6
			Src: "2a01:4f8:202:13d1:78::60", Dst: "2620:fe::fe", InIf: 1, OutIf: 7, Packets: 10, Bytes: 2000,
			First: uptime - 30_000, Last: uptime - 2_000, SrcPort: 40000, DstPort: 853, Proto: 6, SrcMask: 64,
		},
	}
}

type fixture struct {
	name string
	gen  func() []byte
}

// fixtures are the checked-in datagrams (testdata/<name>.hex).
var fixtures = []fixture{
	{"ros_ipfix_templates", func() []byte { return pktgen.RouterOSIPFIXTemplates(exportSecs, 1, 0, true) }},
	{"ros_ipfix_data", func() []byte { return pktgen.RouterOSIPFIXData(exportSecs, 1, 0, rosFlows(true)...) }},
	{"ros_ipfix_no160", func() []byte { return pktgen.RouterOSIPFIXMessage(exportSecs, 1, 0, false, rosFlows(false)...) }},
	{"opn_v9_data_first", func() []byte { return pktgen.OPNsenseV9Data(exportSecs, tOPNUp, 10, opnFlows(tOPNUp)...) }},
	{"opn_v9_templates", func() []byte { return pktgen.OPNsenseV9Templates(exportSecs+5, tOPNUp+5_000, 3) }},
	{"opn_v9_wrap", func() []byte {
		return pktgen.OPNsenseV9TemplatesAndData(exportSecs, 0x00000100, 1, pktgen.V9Flow{
			Src: "192.168.78.60", Dst: "9.9.9.9", InIf: 1, OutIf: 7, Packets: 1, Bytes: 100,
			First: 0xFFFF0000, Last: 0xFFFFFF00, SrcPort: 53124, DstPort: 853, Proto: 6})
	}},
	{"v5", func() []byte { return pktgen.V5(exportSecs, 1_000_000, 2, 2) }},
	{"v5_truncated", func() []byte { return pktgen.V5(exportSecs, 1_000_000, 30, 1) }},
	{"sflow_switch", func() []byte {
		hdr := pktgen.EthIPv4TCPHeader(28, "192.168.28.25", "142.250.203.100", 50514, 443, 1500)
		return pktgen.SFlowSwitch("192.168.78.201", 1, 512, 1, 1, 2, 1518, hdr)
	}},
	{"ipfix_options_sampling", func() []byte {
		return pktgen.IPFIXSampledData(exportSecs, 1, 0, 1, 99, rosFlows(true)[1])
	}},
	{"v9_options_sampling", func() []byte { return pktgen.V9Sampled(exportSecs, tOPNUp, 1, 0, 10, opnFlows(tOPNUp)[0]) }},
	{"ipfix_zero_field_template", pktgen.IPFIXZeroFieldTemplateThenData},
	{"ipfix_degenerate_template", pktgen.DegenerateTemplatePacket},
	{"sflow_expanded_huge_count", pktgen.SFlowExpandedHugeCount},
}

func fixturePath(name string) string { return filepath.Join("testdata", name+".hex") }

// encodeHex renders a datagram as lowercase hex, 32 bytes per line.
func encodeHex(b []byte) []byte {
	var out bytes.Buffer
	for i := 0; i < len(b); i += 32 {
		out.WriteString(hex.EncodeToString(b[i:min(i+32, len(b))]))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// loadFixture reads testdata/<name>.hex (whitespace ignored).
func loadFixture(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatalf("read fixture %s: %v (run go test -run TestGolden -update)", name, err)
	}
	b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return b
}

// recvTime is when a fixture datagram "arrived": its header export time +
// tRecvLag (sFlow has none: tExport + tRecvLag).
func recvTime(pkt []byte) time.Time {
	switch Sniff(pkt) {
	case KindNFv5:
		if len(pkt) >= 12 {
			return time.Unix(int64(binary.BigEndian.Uint32(pkt[8:12])), 0).Add(tRecvLag)
		}
	case KindNFv9:
		if len(pkt) >= 12 {
			return time.Unix(int64(binary.BigEndian.Uint32(pkt[8:12])), 0).Add(tRecvLag)
		}
	case KindIPFIX:
		if len(pkt) >= 8 {
			return time.Unix(int64(binary.BigEndian.Uint32(pkt[4:8])), 0).Add(tRecvLag)
		}
	}
	return tExport.Add(tRecvLag)
}

type scenario struct {
	name     string
	addr     netip.Addr
	override uint32
	skipSeq  bool
	steps    []string
}

var scenarios = []scenario{
	{name: "ros_ipfix", addr: addrROS, steps: []string{"ros_ipfix_templates", "ros_ipfix_data"}},
	{name: "ros_ipfix_no160", addr: addrROS, steps: []string{"ros_ipfix_no160"}},
	{name: "ros_ipfix_override", addr: addrROS, override: 100, steps: []string{"ros_ipfix_templates", "ros_ipfix_data"}},
	{name: "opn_v9_template_after_data", addr: addrOPN, skipSeq: true, steps: []string{"opn_v9_data_first", "opn_v9_templates"}},
	{name: "opn_v9_wrap", addr: addrOPN, skipSeq: true, steps: []string{"opn_v9_wrap"}},
	{name: "v5", addr: addrSW, steps: []string{"v5", "v5_truncated"}},
	{name: "sflow_switch", addr: addrSW, steps: []string{"sflow_switch"}},
	{name: "ipfix_options_sampling", addr: addrROS, steps: []string{"ipfix_options_sampling"}},
	{name: "v9_options_sampling", addr: addrOPN, steps: []string{"v9_options_sampling"}},
	{name: "guards", addr: addrROS, steps: []string{"ipfix_zero_field_template", "ipfix_degenerate_template", "sflow_expanded_huge_count"}},
}

type goldenFlow struct {
	Kind          string `json:"kind"`
	ObsDomain     uint32 `json:"obs_domain"`
	Proto         uint8  `json:"proto"`
	Src           string `json:"src"`
	Dst           string `json:"dst"`
	SrcPort       uint16 `json:"src_port"`
	DstPort       uint16 `json:"dst_port"`
	InIf          uint32 `json:"in_if"`
	OutIf         uint32 `json:"out_if"`
	VLAN          uint16 `json:"vlan"`
	Direction     uint8  `json:"direction"`
	TCPFlags      uint8  `json:"tcp_flags"`
	Bytes         uint64 `json:"bytes"`
	Packets       uint64 `json:"packets"`
	RawBytes      uint64 `json:"raw_bytes"`
	RawPackets    uint64 `json:"raw_packets"`
	SamplingRate  uint32 `json:"sampling_rate"`
	Start         string `json:"start"`
	End           string `json:"end"`
	TimeSource    string `json:"time_source"`
	PostNATSrc    string `json:"post_nat_src"`
	PostNATDst    string `json:"post_nat_dst"`
	PostNATSrcPrt uint16 `json:"post_nat_src_port"`
	PostNATDstPrt uint16 `json:"post_nat_dst_port"`
}

type goldenTemplate struct {
	Version uint16 `json:"version"`
	Obs     uint32 `json:"obs_domain"`
	ID      uint16 `json:"id"`
	Kind    int    `json:"kind"`
	Fields  string `json:"fields"`
}

type goldenStep struct {
	File        string           `json:"file"`
	Kind        string           `json:"kind"`
	Error       string           `json:"error"`
	Rejected    bool             `json:"rejected"`
	Rebooted    bool             `json:"rebooted"`
	ExportTime  string           `json:"export_time"`
	ClockSkewMs int64            `json:"clock_skew_ms"`
	Flows       []goldenFlow     `json:"flows"`
	Counters    int              `json:"counters"`
	Learned     []goldenTemplate `json:"learned"`
	Templates   int              `json:"templates"`
	PendingSets int              `json:"pending_sets"`
	Stats       map[string]any   `json:"stats"`
}

func addrString(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

func timeString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func goldenOf(f Flow) goldenFlow {
	return goldenFlow{Kind: f.Kind.String(), ObsDomain: f.ObsDomain, Proto: f.Proto, Src: addrString(f.SrcAddr),
		Dst: addrString(f.DstAddr), SrcPort: f.SrcPort, DstPort: f.DstPort, InIf: f.InIf, OutIf: f.OutIf, VLAN: f.VLAN,
		Direction: f.Direction, TCPFlags: f.TCPFlags, Bytes: f.Bytes, Packets: f.Packets, RawBytes: f.RawBytes,
		RawPackets: f.RawPackets, SamplingRate: f.SamplingRate, Start: timeString(f.Start), End: timeString(f.End),
		TimeSource: f.TimeSource.String(), PostNATSrc: addrString(f.PostNATSrc), PostNATDst: addrString(f.PostNATDst),
		PostNATSrcPrt: f.PostNATSrcPort, PostNATDstPrt: f.PostNATDstPort}
}

func runScenario(t *testing.T, sc scenario) []goldenStep {
	t.Helper()
	e := NewExporter(sc.addr)
	e.SamplingOverride, e.SkipV9Seq = sc.override, sc.skipSeq
	var out []goldenStep
	for _, name := range sc.steps {
		pkt := loadFixture(t, name)
		var res Result
		var err error
		done := make(chan struct{})
		go func() { defer close(done); res, err = e.Decode(pkt, recvTime(pkt)) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s/%s: decode hung", sc.name, name)
		}
		st := goldenStep{File: name + ".hex", Kind: res.Kind.String(), Rebooted: res.Rebooted,
			ExportTime: timeString(res.ExportTime), ClockSkewMs: res.ClockSkew.Milliseconds(),
			Flows: []goldenFlow{}, Counters: len(res.Counters), Learned: []goldenTemplate{},
			Templates: e.Templates.Len(), PendingSets: e.PendingSets()}
		if err != nil {
			st.Error, st.Rejected = err.Error(), errors.Is(err, ErrRejected)
		}
		if res.ExportTime.IsZero() {
			st.ClockSkewMs = 0
		}
		for _, f := range res.Flows {
			st.Flows = append(st.Flows, goldenOf(f))
		}
		for _, r := range res.Learned {
			st.Learned = append(st.Learned, goldenTemplate{r.Version, r.ObsDomain, r.ID, r.Kind, hex.EncodeToString(r.Fields)})
		}
		s := e.Stats
		st.Stats = map[string]any{"datagrams": s.Datagrams, "flows": s.Flows, "decode_errors": s.DecodeErrors,
			"rejected": s.Rejected, "template_misses": s.TemplateMisses, "replayed": s.Replayed,
			"pending_dropped": s.PendingDropped, "seq_lost": s.SeqLost, "panics": s.Panics, "reboots": s.Reboots}
		out = append(out, st)
	}
	return out
}

// TestGolden checks the checked-in fixtures against their generators and the
// decode of every scenario against its golden JSON.
func TestGolden(t *testing.T) {
	for _, fx := range fixtures {
		want := encodeHex(fx.gen())
		if *update {
			if err := os.WriteFile(fixturePath(fx.name), want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(fixturePath(fx.name))
		if err != nil {
			t.Fatalf("%s: %v (run with -update)", fx.name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("fixture %s.hex differs from its generator (run with -update)", fx.name)
		}
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			got, err := json.MarshalIndent(runScenario(t, sc), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", sc.name+".golden.json")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s: decode differs from golden (run with -update and review the diff):\n%s", sc.name, got)
			}
		})
	}
}
