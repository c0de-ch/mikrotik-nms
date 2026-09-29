package decode

import (
	"encoding/binary"
	"math"
	"net/netip"
	"time"

	"github.com/netsampler/goflow2/v2/decoders/netflow"
	"github.com/netsampler/goflow2/v2/decoders/netflowlegacy"
	"github.com/netsampler/goflow2/v2/decoders/sflow"
)

// Kind is the wire protocol a Flow came from.
type Kind uint8

// Kinds.
const (
	KindUnknown Kind = iota
	KindNFv5
	KindNFv9
	KindIPFIX
	KindSFlow
)

// String returns the protocol name used in the exporter status: netflow5,
// netflow9, ipfix, sflow5 ("" for unknown).
func (k Kind) String() string {
	switch k {
	case KindNFv5:
		return "netflow5"
	case KindNFv9:
		return "netflow9"
	case KindIPFIX:
		return "ipfix"
	case KindSFlow:
		return "sflow5"
	}
	return ""
}

// Sniff returns the protocol of a datagram from its first bytes: 00 05
// netflow5, 00 09 netflow9, 00 0a ipfix, 00 00 00 05 sflow5; KindUnknown
// otherwise.
func Sniff(b []byte) Kind {
	if len(b) < 2 {
		return KindUnknown
	}
	switch binary.BigEndian.Uint16(b) {
	case 5:
		return KindNFv5
	case 9:
		return KindNFv9
	case 10:
		return KindIPFIX
	case 0:
		if len(b) >= 4 && binary.BigEndian.Uint16(b[2:4]) == 5 {
			return KindSFlow
		}
	}
	return KindUnknown
}

// TimeSource says how a flow's start/end were derived.
type TimeSource uint8

// Time sources.
const (
	TimeExport    TimeSource = iota // fallback: the export (or, for sFlow, receive) time
	TimeSysUptime                   // v5/v9 header SysUptime, or IPFIX IE 160 + IE 22/21
	TimeAbsolute                    // IPFIX absolute IEs 150–153
	TimeUptimeEst                   // IPFIX IE 22/21 with an estimated sysInit (no IE 160)
	numTimeSources
)

// String returns the status key: export, sysuptime, absolute, uptime_est.
func (t TimeSource) String() string {
	switch t {
	case TimeSysUptime:
		return "sysuptime"
	case TimeAbsolute:
		return "absolute"
	case TimeUptimeEst:
		return "uptime_est"
	}
	return "export"
}

// Flow is the normalised record handed to the aggregator. Every value was
// copied out of the datagram: it is safe to reuse the read buffer once
// Decode returns.
type Flow struct {
	Kind      Kind
	ObsDomain uint32 // v9 source id / IPFIX observation domain / sFlow data source (type<<24|index)

	Proto            uint8
	SrcAddr, DstAddr netip.Addr
	SrcPort, DstPort uint16
	InIf, OutIf      uint32 // ifIndex as the exporter numbers them; 0 = unknown / local
	VLAN             uint16
	Direction        uint8 // IPFIX flowDirection: 0 ingress, 1 egress, 255 unknown
	TCPFlags         uint8

	Bytes, Packets       uint64 // ESTIMATED totals: raw counters x SamplingRate
	RawBytes, RawPackets uint64 // as exported
	SamplingRate         uint32 // 1 = unsampled

	Start, End time.Time
	TimeSource TimeSource
	// RawStart / RawEnd are the exporter-relative FIRST/LAST_SWITCHED (v5/v9)
	// or flowStart/EndSysUpTime (IPFIX) values as exported, 0 when absent.
	// Every ng_netflow node of one OPNsense shares this uptime, so identical
	// copies of a flow carry identical raw times even when their converted
	// Start/End differ by a second (header unix/uptime phase).
	RawStart, RawEnd uint64

	PostNATSrc, PostNATDst         netip.Addr
	PostNATSrcPort, PostNATDstPort uint16
}

func beUint(b []byte) (uint64, bool) {
	if len(b) == 0 || len(b) > 8 {
		return 0, false
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v, true
}

func addrOf(b []byte) netip.Addr {
	switch len(b) {
	case 4:
		return netip.AddrFrom4([4]byte(b))
	case 16:
		return netip.AddrFrom16([16]byte(b)).Unmap()
	}
	return netip.Addr{}
}

// recordCtx carries per-datagram header facts needed to interpret a record.
type recordCtx struct {
	kind        Kind
	obs         uint32
	exportTime  time.Time // header export time (1 s resolution for v9/IPFIX)
	received    time.Time // when the datagram arrived
	sysUptimeMs uint32    // v9 header only
	sampling    uint32    // from options data / override, 0 = unknown
}

// rawRecord is one v9/IPFIX data record with its fields parsed but its times
// and sampling not yet applied (they depend on the whole datagram).
type rawRecord struct {
	f                  Flow
	upStart, upEnd     uint64
	hasUp              bool
	sysInitMs          uint64
	absStart, absEnd   time.Time
	sampInt, sampSpace uint64
}

// parseDataRecord extracts one v9/IPFIX data record. Unknown and enterprise
// (PEN) fields are ignored. ok is false for a record that is not a flow.
func parseDataRecord(vals []netflow.DataField, c recordCtx) (rawRecord, bool) {
	r := rawRecord{f: Flow{Kind: c.kind, ObsDomain: c.obs, Direction: 255}}
	f := &r.f
	var (
		hasStart, hasEnd  bool
		outBytes, outPkts uint64
	)
	for _, v := range vals {
		if v.PenProvided {
			continue
		}
		b, ok := v.Value.([]byte)
		if !ok {
			continue
		}
		n, isNum := beUint(b)
		switch v.Type {
		case 1: // octetDeltaCount / IN_BYTES
			f.RawBytes = n
		case 2: // packetDeltaCount / IN_PKTS
			f.RawPackets = n
		case 23: // postOctetDeltaCount / OUT_BYTES
			outBytes = n
		case 24:
			outPkts = n
		case 4:
			f.Proto = uint8(n)
		case 6:
			f.TCPFlags = uint8(n)
		case 7:
			f.SrcPort = uint16(n)
		case 11:
			f.DstPort = uint16(n)
		case 8, 27:
			f.SrcAddr = addrOf(b)
		case 12, 28:
			f.DstAddr = addrOf(b)
		case 10:
			f.InIf = uint32(n)
		case 14:
			f.OutIf = uint32(n)
		case 58: // vlanId
			f.VLAN = uint16(n)
		case 61:
			f.Direction = uint8(n)
		case 22: // FIRST_SWITCHED (v9) / flowStartSysUpTime (IPFIX)
			r.upStart, hasStart = n, isNum
		case 21:
			r.upEnd, hasEnd = n, isNum
		case 160: // systemInitTimeMilliseconds
			r.sysInitMs = n
		case 150:
			r.absStart = time.Unix(int64(n&math.MaxInt32), 0)
		case 151:
			r.absEnd = time.Unix(int64(n&math.MaxInt32), 0)
		case 152:
			r.absStart = time.UnixMilli(int64(n & (1<<62 - 1)))
		case 153:
			r.absEnd = time.UnixMilli(int64(n & (1<<62 - 1)))
		case 34: // samplingInterval (1-in-N)
			r.sampInt, r.sampSpace = n, 0
		case 305:
			r.sampInt = n
		case 306:
			r.sampSpace = n
		case 225, 281:
			f.PostNATSrc = addrOf(b)
		case 226, 282:
			f.PostNATDst = addrOf(b)
		case 227:
			f.PostNATSrcPort = uint16(n)
		case 228:
			f.PostNATDstPort = uint16(n)
		}
	}
	r.hasUp = hasStart && hasEnd
	if f.RawBytes == 0 && outBytes > 0 { // some exporters only send OUT_*
		f.RawBytes, f.RawPackets = outBytes, outPkts
	}
	if !f.SrcAddr.IsValid() && !f.DstAddr.IsValid() && f.RawBytes == 0 {
		return r, false // not a flow record (e.g. options data under a data template id)
	}
	return r, true
}

const (
	wrap32 = int64(1) << 32
	half32 = int64(1) << 31
)

// floorDiv is floor(a / b) for b > 0.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && (a < 0) {
		q--
	}
	return q
}

// unwrapUptime returns base + up (ms) for a 32-bit uptime counter that wraps
// every 2^32 ms (49.7 days): the result is shifted by the multiple of 2^32
// that brings it nearest to ref. Uptimes wider than 32 bits are used as is.
func unwrapUptime(base int64, up uint64, ref int64) int64 {
	t := base + int64(up&(1<<62-1))
	if up > math.MaxUint32 {
		return t
	}
	return t + floorDiv(ref-t+half32, wrap32)*wrap32
}

// v9Offset converts a v9/v5 FIRST/LAST_SWITCHED to its offset before the
// header SysUptime: uint32 modular subtraction (wrap-safe), read as signed so
// a value a little past the header time lands just after the export.
func v9Offset(hdrUptime uint32, up uint64) time.Duration {
	return time.Duration(int32(hdrUptime-uint32(up))) * time.Millisecond
}

// sanitizeTimes clamps nonsense (sysInit garbage, uptime wrap, absurd
// values): a start or end outside [export−2h, export+1m] falls back to the
// export time for both; end before start swaps.
func sanitizeTimes(f *Flow, export time.Time) {
	lo, hi := export.Add(-2*time.Hour), export.Add(time.Minute)
	ok := func(t time.Time) bool { return !t.IsZero() && !t.Before(lo) && !t.After(hi) }
	if !ok(f.Start) || !ok(f.End) {
		f.Start, f.End, f.TimeSource = export, export, TimeExport
		return
	}
	if f.End.Before(f.Start) {
		f.Start, f.End = f.End, f.Start
	}
}

// skewThreshold: a receive − export difference beyond it shifts flow times
// into the collector's clock (devices without NTP must not land flows in the
// wrong minutes).
const skewThreshold = 5 * time.Second

// applySkew shifts a flow's times by the clock skew when it exceeds the
// threshold.
func applySkew(f *Flow, skew time.Duration) {
	if skew > skewThreshold || skew < -skewThreshold {
		f.Start, f.End = f.Start.Add(skew), f.End.Add(skew)
	}
}

// scale applies the sampling rate (per-record fields win, then the options /
// override rate, else 1) and fills the estimated totals.
func (r *rawRecord) scale(ctxRate uint32) {
	rate := uint64(0)
	switch {
	case r.sampInt > 0 && r.sampSpace > 0:
		rate = (r.sampInt + r.sampSpace) / r.sampInt // RFC 5476 systematic count-based
	case r.sampInt > 0:
		rate = r.sampInt
	case ctxRate > 0:
		rate = uint64(ctxRate)
	}
	if rate == 0 || rate > math.MaxUint32 {
		rate = 1
	}
	r.f.SamplingRate = uint32(rate)
	r.f.Bytes, r.f.Packets = r.f.RawBytes*rate, r.f.RawPackets*rate
}

func fromV5(p *netflowlegacy.PacketNetFlowV5, samplingOverride uint32, received time.Time) []Flow {
	export := time.Unix(int64(p.UnixSecs), int64(p.UnixNSecs%1_000_000_000))
	skew := received.Sub(export)
	rate := uint32(p.SamplingInterval & 0x3fff) // top 2 bits = sampling mode
	if rate == 0 {
		rate = samplingOverride
	}
	if rate == 0 {
		rate = 1
	}
	out := make([]Flow, 0, len(p.Records))
	for _, r := range p.Records {
		var src, dst [4]byte
		binary.BigEndian.PutUint32(src[:], uint32(r.SrcAddr))
		binary.BigEndian.PutUint32(dst[:], uint32(r.DstAddr))
		f := Flow{
			Kind: KindNFv5, ObsDomain: uint32(p.EngineType)<<8 | uint32(p.EngineId),
			Proto: r.Proto, SrcAddr: netip.AddrFrom4(src), DstAddr: netip.AddrFrom4(dst),
			SrcPort: r.SrcPort, DstPort: r.DstPort, InIf: uint32(r.Input), OutIf: uint32(r.Output),
			TCPFlags: r.TCPFlags, Direction: 255,
			RawBytes: uint64(r.DOctets), RawPackets: uint64(r.DPkts), SamplingRate: rate,
			Bytes: uint64(r.DOctets) * uint64(rate), Packets: uint64(r.DPkts) * uint64(rate),
			Start:      export.Add(-v9Offset(p.SysUptime, uint64(r.First))),
			End:        export.Add(-v9Offset(p.SysUptime, uint64(r.Last))),
			TimeSource: TimeSysUptime,
			RawStart:   uint64(r.First), RawEnd: uint64(r.Last),
		}
		sanitizeTimes(&f, export)
		applySkew(&f, skew)
		out = append(out, f)
	}
	return out
}

// IfCounter is an sFlow generic interface counter sample. Counter samples are
// counted and discarded downstream (never summed with flows).
type IfCounter struct {
	IfIndex             uint32
	InOctets, OutOctets uint64
}

// sflowIf strips the sFlow v5 compact interface encoding:
// top 2 bits 00 = ifIndex, 01 = discarded (low bits = reason), 10 = multiple.
func sflowIf(v uint32) uint32 {
	if v>>30 != 0 || v == 0x3fffffff {
		return 0
	}
	return v
}

func fromSFlow(p *sflow.Packet, received time.Time) ([]Flow, []IfCounter) {
	var flows []Flow
	var ctrs []IfCounter
	for _, s := range p.Samples {
		var (
			rate, in, out uint32
			src           uint32
			recs          []sflow.FlowRecord
		)
		switch s := s.(type) {
		case sflow.FlowSample:
			rate, in, out, recs = s.SamplingRate, sflowIf(s.Input), sflowIf(s.Output), s.Records
			src = s.Header.SourceIdType<<24 | s.Header.SourceIdValue
		case sflow.ExpandedFlowSample:
			rate, recs = s.SamplingRate, s.Records
			if s.InputIfFormat == 0 {
				in = s.InputIfValue
			}
			if s.OutputIfFormat == 0 {
				out = s.OutputIfValue
			}
			src = s.Header.SourceIdType<<24 | s.Header.SourceIdValue
		case sflow.CounterSample:
			for _, r := range s.Records {
				if c, ok := r.Data.(sflow.IfCounters); ok {
					ctrs = append(ctrs, IfCounter{c.IfIndex, c.IfInOctets, c.IfOutOctets})
				}
			}
			continue
		default:
			continue
		}
		if rate == 0 {
			rate = 1
		}
		f := Flow{Kind: KindSFlow, ObsDomain: src, InIf: in, OutIf: out, Direction: 255, SamplingRate: rate,
			Start: received, End: received, TimeSource: TimeExport, RawPackets: 1}
		got := false
		for _, r := range recs {
			switch d := r.Data.(type) {
			case sflow.SampledHeader:
				f.RawBytes = uint64(d.FrameLength)
				// v2.2.6 quirk: the XDR header<> length lands in the misnamed
				// OriginalLength and HeaderData runs to the end of the record
				// (incl. XDR padding). Trim; fixed upstream on main (v3, #480).
				hd := d.HeaderData
				if n := int(d.OriginalLength); n > 0 && n <= len(hd) {
					hd = hd[:n]
				}
				if d.Protocol == 1 { // ethernet
					got = parseEthernet(hd, &f) || got
				}
			case sflow.SampledIPv4:
				f.RawBytes = uint64(d.Length)
				f.Proto, f.SrcPort, f.DstPort = uint8(d.Protocol), uint16(d.SrcPort), uint16(d.DstPort)
				f.SrcAddr, f.DstAddr = addrOf(d.SrcIP), addrOf(d.DstIP)
				got = true
			case sflow.SampledIPv6:
				f.RawBytes = uint64(d.Length)
				f.Proto, f.SrcPort, f.DstPort = uint8(d.Protocol), uint16(d.SrcPort), uint16(d.DstPort)
				f.SrcAddr, f.DstAddr = addrOf(d.SrcIP), addrOf(d.DstIP)
				got = true
			case sflow.ExtendedSwitch:
				if f.VLAN == 0 {
					f.VLAN = uint16(d.SrcVlan)
				}
			}
		}
		if !got && f.RawBytes == 0 {
			continue
		}
		// One sampled packet stands for `rate` packets of this size.
		f.Bytes, f.Packets = f.RawBytes*uint64(rate), uint64(rate)
		flows = append(flows, f)
	}
	return flows, ctrs
}
