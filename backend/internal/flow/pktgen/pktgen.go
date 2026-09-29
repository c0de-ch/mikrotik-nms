// Package pktgen builds synthetic NetFlow v5 / v9 / IPFIX / sFlow v5
// datagrams shaped like what RouterOS traffic-flow (IPFIX), OPNsense
// ng_netflow (NetFlow v9) and a switch sFlow agent emit, plus the hostile
// datagrams that crash unguarded goflow2 v2.2.6. It is pure (no goflow2):
// the decoder's golden fixtures, fuzz seeds and the flowreplay tool use it.
package pktgen

import (
	"encoding/binary"
	"net/netip"
)

type buf struct{ b []byte }

func (w *buf) u8(v uint8)   { w.b = append(w.b, v) }
func (w *buf) u16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *buf) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *buf) u64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *buf) raw(p []byte) { w.b = append(w.b, p...) }

// ip4 appends an IPv4 address ("" = 0.0.0.0).
func (w *buf) ip4(s string) {
	if s == "" {
		w.u32(0)
		return
	}
	a := netip.MustParseAddr(s).Unmap().As4()
	w.raw(a[:])
}

// ip16 appends an IPv6 address ("" = ::).
func (w *buf) ip16(s string) {
	if s == "" {
		w.raw(make([]byte, 16))
		return
	}
	a := netip.MustParseAddr(s).As16()
	w.raw(a[:])
}

func (w *buf) pad4(start int) {
	for (len(w.b)-start)%4 != 0 {
		w.u8(0)
	}
}

// isV6 reports whether s is an IPv6 (not v4-mapped) address.
func isV6(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is6() && !a.Is4In6()
}

// Field is an (IE id, length) template entry.
type Field struct{ ID, Len uint16 }

// ---- IPFIX (RouterOS traffic-flow) ----

// RouterOS IPFIX template ids: 256 carries IPv4 flows, 257 IPv6 flows.
const (
	RouterOSTemplateV4 = 256
	RouterOSTemplateV6 = 257
	// OptionsTemplateSampling is the options template id IPFIXTemplates and
	// IPFIXSampledData use for the 305/306 sampling announcement.
	OptionsTemplateSampling = 258
)

// RouterOSIPFIXFieldsV4 returns the IPv4 template (256): 8,12,7,11,4,5,6,
// 1(8),2(8),10,14,15,22,21,[160(8)],225,226,227,228,61,80(6),81(6),60.
func RouterOSIPFIXFieldsV4(withSysInit bool) []Field {
	f := []Field{{8, 4}, {12, 4}, {7, 2}, {11, 2}, {4, 1}, {5, 1}, {6, 1}, {1, 8}, {2, 8}, {10, 4}, {14, 4}, {15, 4},
		{22, 4}, {21, 4}}
	if withSysInit {
		f = append(f, Field{160, 8})
	}
	return append(f, Field{225, 4}, Field{226, 4}, Field{227, 2}, Field{228, 2}, Field{61, 1}, Field{80, 6},
		Field{81, 6}, Field{60, 1})
}

// RouterOSIPFIXFieldsV6 returns the IPv6 template (257): 27,28,7,11,4,6,
// 1(8),2(8),10,14,22,21,[160(8)],31,60.
func RouterOSIPFIXFieldsV6(withSysInit bool) []Field {
	f := []Field{{27, 16}, {28, 16}, {7, 2}, {11, 2}, {4, 1}, {6, 1}, {1, 8}, {2, 8}, {10, 4}, {14, 4}, {22, 4}, {21, 4}}
	if withSysInit {
		f = append(f, Field{160, 8})
	}
	return append(f, Field{31, 4}, Field{60, 1})
}

// IPFIXFlow is one RouterOS IPFIX data record. Src/Dst select the template:
// IPv6 addresses go to 257, the rest to 256. SysInitMs == 0 encodes the
// record WITHOUT IE 160 (it must then match templates sent with
// withSysInit=false).
type IPFIXFlow struct {
	Src, Dst               string
	SrcPort, DstPort       uint16
	Proto, TOS, TCPFlags   uint8
	Bytes, Packets         uint64
	InIf, OutIf            uint32
	NextHop                string // IPv4 only
	StartUp, EndUp         uint32 // ms since sysInit (flowStart/EndSysUpTime)
	SysInitMs              uint64 // systemInitTimeMilliseconds; 0 = omit IE 160
	NatSrc, NatDst         string // IPv4 only (225 / 226)
	NatSrcPort, NatDstPort uint16 // 227 / 228
	Direction              uint8  // 61
	FlowLabel              uint32 // IPv6 only (31)
}

func (f IPFIXFlow) encodeV4(w *buf) {
	w.ip4(f.Src)
	w.ip4(f.Dst)
	w.u16(f.SrcPort)
	w.u16(f.DstPort)
	w.u8(f.Proto)
	w.u8(f.TOS)
	w.u8(f.TCPFlags)
	w.u64(f.Bytes)
	w.u64(f.Packets)
	w.u32(f.InIf)
	w.u32(f.OutIf)
	w.ip4(f.NextHop)
	w.u32(f.StartUp)
	w.u32(f.EndUp)
	if f.SysInitMs != 0 {
		w.u64(f.SysInitMs)
	}
	w.ip4(f.NatSrc)
	w.ip4(f.NatDst)
	w.u16(f.NatSrcPort)
	w.u16(f.NatDstPort)
	w.u8(f.Direction)
	w.raw([]byte{0x48, 0xa9, 0x8a, 0xbc, 0x72, 0xa5}) // destinationMacAddress
	w.raw([]byte{0x48, 0xa9, 0x8a, 0xbc, 0x72, 0xa6}) // postSourceMacAddress
	w.u8(4)
}

func (f IPFIXFlow) encodeV6(w *buf) {
	w.ip16(f.Src)
	w.ip16(f.Dst)
	w.u16(f.SrcPort)
	w.u16(f.DstPort)
	w.u8(f.Proto)
	w.u8(f.TCPFlags)
	w.u64(f.Bytes)
	w.u64(f.Packets)
	w.u32(f.InIf)
	w.u32(f.OutIf)
	w.u32(f.StartUp)
	w.u32(f.EndUp)
	if f.SysInitMs != 0 {
		w.u64(f.SysInitMs)
	}
	w.u32(f.FlowLabel)
	w.u8(6)
}

func ipfixHeader(w *buf, exportTime, seq, obs uint32) {
	w.u16(10)
	w.u16(0) // length, patched by ipfixDone
	w.u32(exportTime)
	w.u32(seq)
	w.u32(obs)
}

func ipfixDone(w *buf) []byte {
	binary.BigEndian.PutUint16(w.b[2:4], uint16(len(w.b)))
	return w.b
}

func templateRecord(w *buf, tmplID uint16, fields []Field) {
	w.u16(tmplID)
	w.u16(uint16(len(fields)))
	for _, f := range fields {
		w.u16(f.ID)
		w.u16(f.Len)
	}
}

// setStart opens a (template / data) set; setEnd patches its length.
func setStart(w *buf, id uint16) int {
	start := len(w.b)
	w.u16(id)
	w.u16(0)
	return start
}

func setEnd(w *buf, start int) {
	binary.BigEndian.PutUint16(w.b[start+2:], uint16(len(w.b)-start))
}

func routerOSTemplateSet(w *buf, withSysInit bool) {
	s := setStart(w, 2)
	templateRecord(w, RouterOSTemplateV4, RouterOSIPFIXFieldsV4(withSysInit))
	templateRecord(w, RouterOSTemplateV6, RouterOSIPFIXFieldsV6(withSysInit))
	setEnd(w, s)
}

func routerOSDataSets(w *buf, flows []IPFIXFlow) {
	var v4, v6 []IPFIXFlow
	for _, f := range flows {
		if isV6(f.Src) || isV6(f.Dst) {
			v6 = append(v6, f)
		} else {
			v4 = append(v4, f)
		}
	}
	if len(v4) > 0 {
		s := setStart(w, RouterOSTemplateV4)
		for _, f := range v4 {
			f.encodeV4(w)
		}
		setEnd(w, s)
	}
	if len(v6) > 0 {
		s := setStart(w, RouterOSTemplateV6)
		for _, f := range v6 {
			f.encodeV6(w)
		}
		setEnd(w, s)
	}
}

// RouterOSIPFIXTemplates returns an IPFIX message with the RouterOS data
// templates 256 (IPv4) and 257 (IPv6) in one template set, with or without
// IE 160. RouterOS sends no options template (it does not sample here).
func RouterOSIPFIXTemplates(export, seq, obs uint32, withSysInit bool) []byte {
	w := &buf{}
	ipfixHeader(w, export, seq, obs)
	routerOSTemplateSet(w, withSysInit)
	return ipfixDone(w)
}

// RouterOSIPFIXData returns an IPFIX message with data sets 256 / 257 for
// the given flows (IPv4 / IPv6 by address) and no template.
func RouterOSIPFIXData(export, seq, obs uint32, flows ...IPFIXFlow) []byte {
	w := &buf{}
	ipfixHeader(w, export, seq, obs)
	routerOSDataSets(w, flows)
	return ipfixDone(w)
}

func samplingOptions(w *buf, obs, interval, space uint32) {
	// options template set (id 3): templateId, fieldCount, scopeFieldCount, fields
	s := setStart(w, 3)
	w.u16(OptionsTemplateSampling)
	w.u16(3) // total fields
	w.u16(1) // scope fields
	w.u16(149)
	w.u16(4) // observationDomainId
	w.u16(305)
	w.u16(4) // samplingPacketInterval
	w.u16(306)
	w.u16(4) // samplingPacketSpace
	setEnd(w, s)
	s = setStart(w, OptionsTemplateSampling)
	w.u32(obs)
	w.u32(interval)
	w.u32(space)
	setEnd(w, s)
}

// IPFIXTemplates returns an IPFIX message with the RouterOS data templates
// (with IE 160), an options template (258: scope observationDomainId,
// options samplingPacketInterval 305 + samplingPacketSpace 306) and the
// options data record announcing 1-in-((interval+space)/interval).
func IPFIXTemplates(export, seq, obs, sampInterval, sampSpace uint32) []byte {
	w := &buf{}
	ipfixHeader(w, export, seq, obs)
	routerOSTemplateSet(w, true)
	samplingOptions(w, obs, sampInterval, sampSpace)
	return ipfixDone(w)
}

// IPFIXSampledData is IPFIXTemplates followed by data sets for flows in the
// SAME message: the options data precedes the data, so a collector that
// processes options first applies the rate to these records.
func IPFIXSampledData(export, seq, obs, sampInterval, sampSpace uint32, flows ...IPFIXFlow) []byte {
	w := &buf{}
	ipfixHeader(w, export, seq, obs)
	routerOSTemplateSet(w, true)
	samplingOptions(w, obs, sampInterval, sampSpace)
	routerOSDataSets(w, flows)
	return ipfixDone(w)
}

// IPFIXZeroFieldTemplateThenData is the hostile/buggy packet: a template
// record with fieldCount=0 for id 300 followed by a non-empty data set for
// 300. goflow2 v2.2.6 stores the empty template (it does not treat it as an
// RFC 7011 withdrawal) and DecodeDataSet then loops forever
// (for payload.Len() >= 0), appending records until OOM.
func IPFIXZeroFieldTemplateThenData() []byte {
	w := &buf{}
	ipfixHeader(w, 1_790_000_000, 1, 0)
	s := setStart(w, 2)
	templateRecord(w, 300, nil)
	setEnd(w, s)
	s = setStart(w, 300)
	w.u32(0xdeadbeef)
	setEnd(w, s)
	return ipfixDone(w)
}

// ---- NetFlow v9 (OPNsense ng_netflow) ----

// ng_netflow template ids.
const (
	OPNsenseTemplateV4 = 256
	OPNsenseTemplateV6 = 259
)

// OPNsenseV9FieldsV4 is the ng_netflow IPv4 template (256, 57-byte records):
// SRC_ADDR, DST_ADDR, NEXT_HOP, INPUT_SNMP(2), OUTPUT_SNMP(2), IN_PKTS,
// IN_BYTES, OUT_PKTS, OUT_BYTES, FIRST_SWITCHED, LAST_SWITCHED, L4_SRC_PORT,
// L4_DST_PORT, TCP_FLAGS, PROTOCOL, TOS, SRC_AS, DST_AS, SRC_MASK, DST_MASK.
var OPNsenseV9FieldsV4 = []Field{{8, 4}, {12, 4}, {15, 4}, {10, 2}, {14, 2}, {2, 4}, {1, 4}, {24, 4}, {23, 4},
	{22, 4}, {21, 4}, {7, 2}, {11, 2}, {6, 1}, {4, 1}, {5, 1}, {16, 4}, {17, 4}, {9, 1}, {13, 1}}

// OPNsenseV9FieldsV6 is the ng_netflow IPv6 template (259, 93-byte records):
// the IPv4 layout with IPV6_SRC_ADDR, IPV6_DST_ADDR, IPV6_NEXT_HOP and the
// IPv6 masks.
var OPNsenseV9FieldsV6 = []Field{{27, 16}, {28, 16}, {62, 16}, {10, 2}, {14, 2}, {2, 4}, {1, 4}, {24, 4}, {23, 4},
	{22, 4}, {21, 4}, {7, 2}, {11, 2}, {6, 1}, {4, 1}, {5, 1}, {16, 4}, {17, 4}, {29, 1}, {30, 1}}

// V9Flow is one ng_netflow data record. OUT_PKTS / OUT_BYTES and the AS
// numbers are always 0, as ng_netflow sends them.
type V9Flow struct {
	Src, Dst, NextHop string // IPv6 Src/Dst select template 259
	InIf, OutIf       uint16
	Packets, Bytes    uint32 // IN_PKTS / IN_BYTES
	First, Last       uint32 // FIRST/LAST_SWITCHED, ms of sysUptime
	SrcPort, DstPort  uint16
	TCPFlags, Proto   uint8
	TOS               uint8
	SrcMask, DstMask  uint8
}

func (f V9Flow) encode(w *buf, v6 bool) {
	if v6 {
		w.ip16(f.Src)
		w.ip16(f.Dst)
		w.ip16(f.NextHop)
	} else {
		w.ip4(f.Src)
		w.ip4(f.Dst)
		w.ip4(f.NextHop)
	}
	w.u16(f.InIf)
	w.u16(f.OutIf)
	w.u32(f.Packets)
	w.u32(f.Bytes)
	w.u32(0) // OUT_PKTS
	w.u32(0) // OUT_BYTES
	w.u32(f.First)
	w.u32(f.Last)
	w.u16(f.SrcPort)
	w.u16(f.DstPort)
	w.u8(f.TCPFlags)
	w.u8(f.Proto)
	w.u8(f.TOS)
	w.u32(0) // SRC_AS
	w.u32(0) // DST_AS
	w.u8(f.SrcMask)
	w.u8(f.DstMask)
}

func v9Header(w *buf, count uint16, uptime, unix, seq, sourceID uint32) {
	w.u16(9)
	w.u16(count)
	w.u32(uptime)
	w.u32(unix)
	w.u32(seq)
	w.u32(sourceID)
}

// OPNsenseV9Templates returns a NetFlow v9 packet with the ng_netflow
// template flowset (256 IPv4, 259 IPv6); source_id 0 as on every node.
func OPNsenseV9Templates(unix, uptime, seq uint32) []byte {
	w := &buf{}
	v9Header(w, 2, uptime, unix, seq, 0)
	s := setStart(w, 0)
	templateRecord(w, OPNsenseTemplateV4, OPNsenseV9FieldsV4)
	templateRecord(w, OPNsenseTemplateV6, OPNsenseV9FieldsV6)
	setEnd(w, s)
	return w.b
}

func v9DataSets(w *buf, flows []V9Flow) {
	var v4, v6 []V9Flow
	for _, f := range flows {
		if isV6(f.Src) || isV6(f.Dst) {
			v6 = append(v6, f)
		} else {
			v4 = append(v4, f)
		}
	}
	for _, g := range []struct {
		id    uint16
		flows []V9Flow
		v6    bool
	}{{OPNsenseTemplateV4, v4, false}, {OPNsenseTemplateV6, v6, true}} {
		if len(g.flows) == 0 {
			continue
		}
		s := setStart(w, g.id)
		for _, f := range g.flows {
			f.encode(w, g.v6)
		}
		w.pad4(s) // flowsets are padded to a 4-byte boundary
		setEnd(w, s)
	}
}

// OPNsenseV9Data returns a NetFlow v9 packet with data flowsets 256 / 259
// (IPv4 / IPv6 by address, padded to 4 bytes) and no template; count = the
// number of records.
func OPNsenseV9Data(unix, uptime, seq uint32, flows ...V9Flow) []byte {
	w := &buf{}
	v9Header(w, uint16(len(flows)), uptime, unix, seq, 0)
	v9DataSets(w, flows)
	return w.b
}

// V9Sampled returns a NetFlow v9 packet with the ng_netflow IPv4 template,
// an options template (257: scope System, SAMPLING_INTERVAL 34 +
// SAMPLING_ALGORITHM 35), its options data announcing 1-in-rate and data
// records for flows — options before data in one datagram.
func V9Sampled(unix, uptime, seq, sourceID, rate uint32, flows ...V9Flow) []byte {
	w := &buf{}
	v9Header(w, uint16(3+len(flows)), uptime, unix, seq, sourceID)
	s := setStart(w, 0)
	templateRecord(w, OPNsenseTemplateV4, OPNsenseV9FieldsV4)
	templateRecord(w, OPNsenseTemplateV6, OPNsenseV9FieldsV6)
	setEnd(w, s)
	// v9 options template: flowset id 1: templateId, scopeLen(bytes), optionLen(bytes)
	s = setStart(w, 1)
	w.u16(257)
	w.u16(4) // scope length: 1 field * 4 bytes
	w.u16(8) // option length: 2 fields * 4 bytes
	w.u16(1) // scope type 1 = System
	w.u16(4)
	w.u16(34) // SAMPLING_INTERVAL
	w.u16(4)
	w.u16(35) // SAMPLING_ALGORITHM
	w.u16(1)
	w.pad4(s)
	setEnd(w, s)
	s = setStart(w, 257)
	w.u32(0)    // scope System = 0
	w.u32(rate) // 1-in-rate
	w.u8(1)     // deterministic
	w.pad4(s)
	setEnd(w, s)
	v9DataSets(w, flows)
	return w.b
}

// ---- NetFlow v5 ----

// V5 builds a NetFlow v5 packet with n records (declaredCount may exceed n to
// simulate truncation). Sampling mode 01, interval 64.
func V5(unixSecs, sysUptimeMs uint32, declaredCount uint16, n int) []byte {
	w := &buf{}
	w.u16(5)
	w.u16(declaredCount)
	w.u32(sysUptimeMs)
	w.u32(unixSecs)
	w.u32(0)
	w.u32(1)
	w.u8(0)
	w.u8(0)
	w.u16(0x4000 | 64) // sampling mode 01 (packet interval) + interval 64
	for i := 0; i < n; i++ {
		w.ip4("192.168.78.70")
		w.ip4("192.168.79.216")
		w.ip4("0.0.0.0")
		w.u16(7)
		w.u16(9)
		w.u32(10)
		w.u32(1500)
		w.u32(sysUptimeMs - 30_000)
		w.u32(sysUptimeMs - 500)
		w.u16(40000 + uint16(i))
		w.u16(8080)
		w.u8(0)
		w.u8(0x18)
		w.u8(6)
		w.u8(0)
		w.u16(0)
		w.u16(0)
		w.u8(24)
		w.u8(24)
		w.u16(0)
	}
	return w.b
}

// ---- sFlow v5 ----

// EthIPv4TCPHeader returns a 64-byte sampled frame header: Ethernet +
// 802.1Q VLAN vid + IPv4 + TCP.
func EthIPv4TCPHeader(vid uint16, src, dst string, sport, dport uint16, ipTotalLen uint16) []byte {
	w := &buf{}
	w.raw([]byte{0x48, 0xa9, 0x8a, 0xbc, 0x72, 0xa5}) // dst mac
	w.raw([]byte{0x00, 0x03, 0x2d, 0x21, 0xfb, 0x4c}) // src mac
	w.u16(0x8100)
	w.u16(vid & 0x0fff)
	w.u16(0x0800)
	// IPv4 header (20 bytes)
	w.u8(0x45)
	w.u8(0)
	w.u16(ipTotalLen)
	w.u16(0x1234)
	w.u16(0x4000) // DF
	w.u8(63)
	w.u8(6)
	w.u16(0)
	w.ip4(src)
	w.ip4(dst)
	// TCP (first 20 bytes)
	w.u16(sport)
	w.u16(dport)
	w.u32(1)
	w.u32(0)
	w.u8(0x50)
	w.u8(0x10)
	w.u16(0xffff)
	w.u16(0)
	w.u16(0)
	for len(w.b) < 64 {
		w.u8(0)
	}
	return w.b
}

// SFlowSwitch builds an sFlow v5 datagram as a switch agent would send:
// one flow sample (format 1) from data source ifIndex srcIf with a raw
// packet header record, one counter sample (format 2) with generic
// interface counters.
func SFlowSwitch(agent string, seq uint32, rate uint32, srcIf, inIf, outIf uint32, frameLen uint32, hdr []byte) []byte {
	w := &buf{}
	w.u32(5)
	w.u32(1)
	w.ip4(agent)
	w.u32(0)      // sub-agent
	w.u32(seq)    // datagram sequence
	w.u32(123456) // uptime ms
	w.u32(2)      // samples

	// flow sample
	sample := &buf{}
	sample.u32(seq)           // sample sequence
	sample.u32(0<<24 | srcIf) // source id: type 0 (ifIndex) | index
	sample.u32(rate)
	sample.u32(rate * seq) // sample pool
	sample.u32(0)          // drops
	sample.u32(inIf)
	sample.u32(outIf)
	sample.u32(1) // records
	rec := &buf{}
	rec.u32(1) // header_protocol ethernet
	rec.u32(frameLen)
	rec.u32(4) // stripped FCS
	rec.u32(uint32(len(hdr)))
	rec.raw(hdr)
	for len(rec.b)%4 != 0 {
		rec.u8(0)
	}
	sample.u32(1) // record data_format 1 = sampled header
	sample.u32(uint32(len(rec.b)))
	sample.raw(rec.b)
	w.u32(1) // sample format 1 = flow sample
	w.u32(uint32(len(sample.b)))
	w.raw(sample.b)

	// counter sample with generic if counters
	cs := &buf{}
	cs.u32(seq)
	cs.u32(srcIf)
	cs.u32(1) // records
	ctr := &buf{}
	ctr.u32(srcIf)
	ctr.u32(6)
	ctr.u64(10_000_000_000)
	ctr.u32(1)
	ctr.u32(3)
	ctr.u64(987654321) // ifInOctets
	for i := 0; i < 6; i++ {
		ctr.u32(uint32(i))
	}
	ctr.u64(123456789) // ifOutOctets
	for i := 0; i < 6; i++ {
		ctr.u32(uint32(i))
	}
	ctr.u32(0) // promiscuous
	cs.u32(1)  // counter data_format 1 = generic interface
	cs.u32(uint32(len(ctr.b)))
	cs.raw(ctr.b)
	w.u32(2)
	w.u32(uint32(len(cs.b)))
	w.raw(cs.b)
	return w.b
}

// SFlowExpandedHugeCount is the hostile packet: an expanded flow sample
// (format 3) announcing 0xFFFFFFFF records. goflow2 v2.2.6 caps the record
// count for formats 1, 2, 4, 5 at 1000 but not for 3, and does
// make([]FlowRecord, count) up front: a ~100 GB allocation from a 76-byte
// datagram.
func SFlowExpandedHugeCount() []byte {
	w := &buf{}
	w.u32(5)
	w.u32(1)
	w.ip4("192.168.78.201")
	w.u32(0)
	w.u32(1)
	w.u32(1)
	w.u32(1) // one sample
	s := &buf{}
	s.u32(1)          // seq
	s.u32(0)          // source id type
	s.u32(1)          // source id index
	s.u32(512)        // rate
	s.u32(0)          // pool
	s.u32(0)          // drops
	s.u32(0)          // in fmt
	s.u32(1)          // in val
	s.u32(0)          // out fmt
	s.u32(2)          // out val
	s.u32(0xFFFFFFFF) // records count
	w.u32(3)
	w.u32(uint32(len(s.b)))
	w.raw(s.b)
	return w.b
}

// RouterOSIPFIXMessage returns ONE IPFIX message carrying the RouterOS
// template set (with or without IE 160) followed by data sets for flows.
func RouterOSIPFIXMessage(export, seq, obs uint32, withSysInit bool, flows ...IPFIXFlow) []byte {
	w := &buf{}
	ipfixHeader(w, export, seq, obs)
	routerOSTemplateSet(w, withSysInit)
	routerOSDataSets(w, flows)
	return ipfixDone(w)
}

// OPNsenseV9TemplatesAndData returns ONE NetFlow v9 packet with the
// ng_netflow template flowset followed by data flowsets for flows.
func OPNsenseV9TemplatesAndData(unix, uptime, seq uint32, flows ...V9Flow) []byte {
	w := &buf{}
	v9Header(w, uint16(2+len(flows)), uptime, unix, seq, 0)
	s := setStart(w, 0)
	templateRecord(w, OPNsenseTemplateV4, OPNsenseV9FieldsV4)
	templateRecord(w, OPNsenseTemplateV6, OPNsenseV9FieldsV6)
	setEnd(w, s)
	v9DataSets(w, flows)
	return w.b
}

// DegenerateTemplatePacket is an IPFIX message whose template 300 has one
// fixed-length-0 field (not a withdrawal) followed by a data set for 300:
// goflow2 would loop forever decoding it.
func DegenerateTemplatePacket() []byte {
	w := &buf{}
	ipfixHeader(w, 1_790_000_000, 1, 0)
	s := setStart(w, 2)
	templateRecord(w, 300, []Field{{1, 0}})
	setEnd(w, s)
	s = setStart(w, 300)
	w.u32(0xdeadbeef)
	setEnd(w, s)
	return ipfixDone(w)
}

// ZeroLengthFieldsTemplate is a template of 127 zero-length fields plus
// last: with 1-byte last, goflow2 v2.2.6 turns every data byte into a
// 128-field record (~6 KB each) unless the guard refuses it.
func ZeroLengthFieldsTemplate(last Field) []Field {
	fs := make([]Field, 0, 128)
	for i := 0; i < 127; i++ {
		fs = append(fs, Field{uint16(100 + i), 0})
	}
	return append(fs, last)
}

// IPFIXTemplate is an IPFIX message (observation domain obs) holding one
// template set with template id and fields.
func IPFIXTemplate(export, seq, obs uint32, id uint16, fields []Field) []byte {
	w := &buf{}
	ipfixHeader(w, export, seq, obs)
	s := setStart(w, 2)
	templateRecord(w, id, fields)
	setEnd(w, s)
	return ipfixDone(w)
}

// IPFIXDataSet is an IPFIX message (observation domain obs) holding one data
// set for template id with payload as its records.
func IPFIXDataSet(export, seq, obs uint32, id uint16, payload []byte) []byte {
	w := &buf{}
	ipfixHeader(w, export, seq, obs)
	s := setStart(w, id)
	w.raw(payload)
	setEnd(w, s)
	return ipfixDone(w)
}

// IPFIXTemplateThenData is IPFIXTemplate and IPFIXDataSet in one message.
func IPFIXTemplateThenData(export, seq, obs uint32, id uint16, fields []Field, payload []byte) []byte {
	w := &buf{}
	ipfixHeader(w, export, seq, obs)
	s := setStart(w, 2)
	templateRecord(w, id, fields)
	setEnd(w, s)
	s = setStart(w, id)
	w.raw(payload)
	setEnd(w, s)
	return ipfixDone(w)
}

// IPFIXHugeFieldCount is a 25-byte IPFIX message whose template record
// announces 0xec30 fields: goflow2 v2.2.6 allocates make([]Field, 0xec30)
// (~725 KB) before it notices the record is truncated.
func IPFIXHugeFieldCount() []byte {
	w := &buf{}
	ipfixHeader(w, 1_790_000_000, 1, 0)
	s := setStart(w, 2)
	w.raw([]byte{1, 0, 0xec, 0x30, 0, 8, 0, 4, 0})
	setEnd(w, s)
	return ipfixDone(w)
}
