package decode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/netsampler/goflow2/v2/decoders/netflow"
	"github.com/netsampler/goflow2/v2/decoders/netflowlegacy"
	"github.com/netsampler/goflow2/v2/decoders/sflow"
)

// Limits for data sets that arrive before their template.
const (
	MaxPendingSets  = 64
	MaxPendingBytes = 256 << 10
	PendingTTL      = 2 * time.Minute
)

// Reboot detection thresholds.
const (
	v9UptimeJump   = 60 * time.Second // header SysUptime vs wall clock
	sysInitJump    = 5000             // ms: IPFIX IE 160 change
	ipfixSeqHigh   = 1000             // IPFIX sequence reset from above ...
	ipfixSeqLow    = 100              // ... to below
	estWindow      = 10 * time.Minute // uptime_est: bounds kept
	maxEstBounds   = 256
	maxSeqGap      = 1 << 20 // larger jumps are resets, not loss
	maxIPFIXSeqGap = 1 << 24
)

// Stats are per-exporter counters, cumulative since the Exporter was built.
type Stats struct {
	Datagrams, Flows         uint64
	DecodeErrors, Rejected   uint64
	TemplateMisses, Replayed uint64 // data sets buffered before their template / replayed later
	PendingDropped           uint64 // buffered sets dropped (cap, TTL, reboot)
	SeqLost                  uint64 // sequence gaps (informational)
	Panics                   uint64
	Reboots                  uint64
	TimeSources              [numTimeSources]uint64
	LastError                string
}

// domainKey is (version, observation domain / source id / sFlow agent hash).
type domainKey struct {
	version uint16
	obs     uint32
}

type pendingSet struct {
	key TemplateKey
	raw []byte // copied
	ctx recordCtx
}

type v9UptimeState struct {
	uptime   uint32
	received time.Time
}

type ipfixSeqState struct {
	last, next uint32
	nextKnown  bool
}

type estBound struct {
	at    time.Time
	bound int64 // ms: an upper bound of sysInit
}

// Exporter holds all decoding state for ONE exporter. It is not safe for
// concurrent use: the collector's single worker owns it.
type Exporter struct {
	Addr             netip.Addr
	SamplingOverride uint32 // admin-configured 1-in-N when the exporter does not announce it
	// SkipV9Seq disables v9 sequence-loss accounting (OPNsense: every
	// ng_netflow node keeps its own counter under source_id 0).
	SkipV9Seq bool

	Templates *SafeTemplates
	Stats     Stats

	domains  map[domainKey]struct{}
	sampling map[domainKey]uint32
	pending  []pendingSet
	pendSize int
	v9Up     map[domainKey]v9UptimeState
	v9Seq    map[domainKey]uint32
	ipfixSeq map[domainKey]ipfixSeqState
	sfSeq    map[domainKey]uint32
	sysInit  map[domainKey]uint64
	est      map[domainKey][]estBound
}

// NewExporter creates decoding state for one exporter.
func NewExporter(addr netip.Addr) *Exporter {
	e := &Exporter{Addr: addr, Templates: NewSafeTemplates(MaxTemplates)}
	e.clearState()
	return e
}

func (e *Exporter) clearState() {
	e.domains = map[domainKey]struct{}{}
	e.sampling = map[domainKey]uint32{}
	e.v9Up = map[domainKey]v9UptimeState{}
	e.v9Seq = map[domainKey]uint32{}
	e.ipfixSeq = map[domainKey]ipfixSeqState{}
	e.sfSeq = map[domainKey]uint32{}
	e.sysInit = map[domainKey]uint64{}
	e.est = map[domainKey][]estBound{}
}

// PendingSets returns how many data sets wait for their template.
func (e *Exporter) PendingSets() int { return len(e.pending) }

// Result is what one datagram produced.
type Result struct {
	Kind       Kind
	Flows      []Flow
	Counters   []IfCounter   // sFlow counter samples (counted and discarded downstream)
	ExportTime time.Time     // header export time; zero for sFlow
	ClockSkew  time.Duration // received − export; flow times were shifted when |skew| > 5 s
	Rebooted   bool          // exporter reboot detected: templates reset, persisted ones must be deleted
	Learned    []TemplateRecord
	Withdrawn  []TemplateKey // stored templates the exporter withdrew (their persisted rows must be deleted)
}

// Decode decodes one datagram received at now. The returned flows never
// alias pkt. Errors matching ErrRejected mean the datagram was rejected
// before decoding; other errors are decode errors and partial results may
// still be returned.
func (e *Exporter) Decode(pkt []byte, now time.Time) (res Result, err error) {
	e.Stats.Datagrams++
	defer func() {
		if r := recover(); r != nil { // defence in depth: a decoder bug must not kill the NMS
			e.Stats.Panics++
			res.Flows, res.Counters = nil, nil
			err = fmt.Errorf("decode: decoder panic: %v", r)
		}
		if err != nil {
			if errors.Is(err, ErrRejected) {
				e.Stats.Rejected++
			} else {
				e.Stats.DecodeErrors++
			}
			e.Stats.LastError = err.Error()
		}
		e.Stats.Flows += uint64(len(res.Flows))
		for i := range res.Flows {
			e.Stats.TimeSources[res.Flows[i].TimeSource]++
		}
	}()
	if len(pkt) < 4 {
		return res, ErrShort
	}
	res.Kind = Sniff(pkt)
	switch res.Kind {
	case KindNFv5:
		if err := PrecheckV5(pkt); err != nil {
			return res, err
		}
		var p netflowlegacy.PacketNetFlowV5
		if err := netflowlegacy.DecodeMessageVersion(bytes.NewBuffer(pkt), &p); err != nil {
			return res, err
		}
		res.ExportTime = time.Unix(int64(p.UnixSecs), int64(p.UnixNSecs%1_000_000_000))
		res.ClockSkew = now.Sub(res.ExportTime)
		res.Flows = fromV5(&p, e.SamplingOverride, now)
		return res, nil
	case KindNFv9, KindIPFIX:
		return e.decodeTemplated(pkt, now, res)
	case KindSFlow:
		if err := PrecheckSFlow(pkt); err != nil {
			return res, err
		}
		var p sflow.Packet
		if err := sflow.DecodeMessageVersion(bytes.NewBuffer(pkt), &p); err != nil {
			return res, err
		}
		dk := domainKey{version: 5, obs: agentKey(p.AgentIP, p.SubAgentId)}
		if !e.canAdmit(dk) {
			return res, ErrTooManyDomains
		}
		res.Flows, res.Counters = fromSFlow(&p, now)
		if len(res.Flows) > 0 || len(res.Counters) > 0 {
			e.domains[dk] = struct{}{}
		}
		if _, ok := e.domains[dk]; ok {
			if last, ok := e.sfSeq[dk]; ok {
				e.countGap(p.SequenceNumber, last+1, maxSeqGap)
			}
			e.sfSeq[dk] = p.SequenceNumber
		}
		return res, nil
	}
	return res, rejectf("decode: unknown datagram version 0x%04x", binary.BigEndian.Uint16(pkt[0:2]))
}

// agentKey folds an sFlow agent address + sub-agent into a domain key.
func agentKey(ip []byte, sub uint32) uint32 {
	h := uint32(2166136261) // FNV-1a
	for _, c := range ip {
		h = (h ^ uint32(c)) * 16777619
	}
	return h ^ sub*2654435761
}

// canAdmit reports whether a datagram of observation domain dk may be
// decoded: the domain is already admitted, or fewer than MaxDomains are. A
// domain is only recorded (e.domains) once a datagram in it produced a
// stored template or a decoded record, so header-only junk cannot fill the
// cap and lock the exporter's real domain out.
func (e *Exporter) canAdmit(dk domainKey) bool {
	if _, ok := e.domains[dk]; ok {
		return true
	}
	return len(e.domains) < MaxDomains
}

// forgetDomain drops the per-domain state of a domain that was not admitted
// (the header-driven state a refused or empty datagram left behind).
func (e *Exporter) forgetDomain(dk domainKey) {
	delete(e.v9Up, dk)
	delete(e.v9Seq, dk)
	delete(e.ipfixSeq, dk)
	delete(e.sfSeq, dk)
	delete(e.sampling, dk)
	delete(e.sysInit, dk)
	delete(e.est, dk)
}

// countGap adds the sequence gap between got and want to SeqLost; negative
// gaps (reordering, duplicates) and huge ones (resets) are ignored.
func (e *Exporter) countGap(got, want uint32, maxGap int64) {
	if gap := int64(int32(got - want)); gap > 0 && gap < maxGap {
		e.Stats.SeqLost += uint64(gap)
	}
}

// reboot drops every template (except keep, learned in the current datagram
// from the rebooted exporter), sampling, sysInit estimates and pending sets.
func (e *Exporter) reboot(keep []TemplateKey) {
	var saved []TemplateRecord
	for _, k := range keep {
		if r, ok := e.Templates.Export(k); ok {
			saved = append(saved, r)
		}
	}
	e.Templates.Reset()
	domains := map[domainKey]struct{}{}
	for _, r := range saved {
		if t, err := recordToTemplate(r); err == nil {
			_ = e.Templates.AddTemplate(r.Version, r.ObsDomain, r.ID, t)
			domains[domainKey{r.Version, r.ObsDomain}] = struct{}{}
		}
	}
	e.Templates.drainAdded() // the caller already holds keep
	e.Stats.PendingDropped += uint64(len(e.pending))
	e.pending, e.pendSize = nil, 0
	// Only the domains of the kept templates stay admitted; the others
	// (and their sequence / uptime state) are re-admitted by their next
	// productive datagram.
	for dk := range e.domains {
		if _, ok := domains[dk]; !ok {
			e.forgetDomain(dk)
		}
	}
	e.domains = domains
	e.sampling = map[domainKey]uint32{}
	e.sysInit = map[domainKey]uint64{}
	e.est = map[domainKey][]estBound{}
	e.Stats.Reboots++
}

func (e *Exporter) decodeTemplated(pkt []byte, now time.Time, res Result) (Result, error) {
	var (
		ctx      recordCtx
		dk       domainKey
		version  uint16
		v9Err    error
		ipfixSeq uint32
	)
	if res.Kind == KindNFv9 {
		if len(pkt) < 20 {
			return res, ErrShort
		}
		version = 9
		uptime := binary.BigEndian.Uint32(pkt[4:8])
		unix := binary.BigEndian.Uint32(pkt[8:12])
		seq := binary.BigEndian.Uint32(pkt[12:16])
		dk = domainKey{9, binary.BigEndian.Uint32(pkt[16:20])}
		if !e.canAdmit(dk) {
			return res, ErrTooManyDomains
		}
		pkt, v9Err = normalizeV9(pkt)
		if err := e.precheckTemplated(pkt, version, dk.obs); err != nil {
			return res, err
		}
		// Reboot: the header SysUptime left the wall clock by more than 60 s
		// (it went back, or jumped far ahead — both mean a restarted
		// uptime). Checked before decoding so a template re-announced in
		// this very datagram survives the reset.
		if st, ok := e.v9Up[dk]; ok {
			drift := time.Duration(int32(uptime-st.uptime))*time.Millisecond - now.Sub(st.received)
			if drift < -v9UptimeJump || drift > v9UptimeJump {
				e.reboot(nil)
				res.Rebooted = true
			}
		}
		e.v9Up[dk] = v9UptimeState{uptime, now}
		if !e.SkipV9Seq {
			if last, ok := e.v9Seq[dk]; ok && !res.Rebooted {
				e.countGap(seq, last+1, maxSeqGap)
			}
			e.v9Seq[dk] = seq
		}
		ctx = recordCtx{kind: KindNFv9, obs: dk.obs, exportTime: time.Unix(int64(unix), 0), received: now,
			sysUptimeMs: uptime}
	} else {
		if len(pkt) < 16 {
			return res, ErrShort
		}
		version = 10
		export := binary.BigEndian.Uint32(pkt[4:8])
		ipfixSeq = binary.BigEndian.Uint32(pkt[8:12])
		dk = domainKey{10, binary.BigEndian.Uint32(pkt[12:16])}
		if !e.canAdmit(dk) {
			return res, ErrTooManyDomains
		}
		if err := e.precheckTemplated(pkt, version, dk.obs); err != nil {
			return res, err
		}
		// Reboot: the sequence number restarted from > 1000 to < 100. A
		// forward wrap past 2^32 (0xFFFFFFFA -> 4) is a small modular step,
		// not a reset.
		if st, ok := e.ipfixSeq[dk]; ok && st.last > ipfixSeqHigh && ipfixSeq < ipfixSeqLow &&
			ipfixSeq-st.last >= maxIPFIXSeqGap {
			e.reboot(nil)
			res.Rebooted = true
		}
		ctx = recordCtx{kind: KindIPFIX, obs: dk.obs, exportTime: time.Unix(int64(export), 0), received: now}
	}
	res.ExportTime = ctx.exportTime
	res.ClockSkew = now.Sub(ctx.exportTime)

	var p9 netflow.NFv9Packet
	var pfx netflow.IPFIXPacket
	derr := netflow.DecodeMessageVersion(bytes.NewBuffer(pkt), e.Templates, &p9, &pfx)
	sets := p9.FlowSets
	if version == 10 {
		sets = pfx.FlowSets
	}

	// Pass 1: options data first (sampling), so data in the same datagram uses it.
	dataRecords, rawSets := 0, false
	for _, s := range sets {
		if od, ok := s.(netflow.OptionsDataFlowSet); ok {
			e.learnSampling(dk, od.Records)
			dataRecords += len(od.Records)
		}
	}
	ctx.sampling = e.samplingFor(dk)

	// Pass 2: data sets; buffer the ones whose template is unknown.
	var recs []rawRecord
	for _, s := range sets {
		switch s := s.(type) {
		case netflow.DataFlowSet:
			dataRecords += len(s.Records)
			for _, r := range s.Records {
				if rr, ok := parseDataRecord(r.Values, ctx); ok {
					recs = append(recs, rr)
				}
			}
		case netflow.RawFlowSet:
			if s.Id >= 256 {
				rawSets = true
				e.Stats.TemplateMisses++
				e.bufferPending(TemplateKey{version, ctx.obs, s.Id}, s.Records, ctx)
			}
		}
	}
	if version == 10 {
		if st, ok := e.ipfixSeq[dk]; ok && st.nextKnown && !res.Rebooted {
			e.countGap(ipfixSeq, st.next, maxIPFIXSeqGap)
		}
		e.ipfixSeq[dk] = ipfixSeqState{last: ipfixSeq, next: ipfixSeq + uint32(dataRecords), nextKnown: !rawSets}
	}

	learned := e.Templates.drainAdded()
	res.Withdrawn = e.Templates.drainWithdrawn()
	flows, rebooted := e.finish(recs, ctx, learned, true)
	res.Rebooted = res.Rebooted || rebooted
	res.Flows = flows

	// Pass 3: replay buffered sets whose template was just learned.
	if len(learned) > 0 {
		res.Flows = append(res.Flows, e.replay(learned, now)...)
		for _, k := range dedupKeys(learned) {
			if r, ok := e.Templates.Export(k); ok {
				res.Learned = append(res.Learned, r)
			}
		}
	}

	// The domain is admitted once a datagram in it produced something.
	if len(learned) > 0 || dataRecords > 0 || len(res.Flows) > 0 {
		e.domains[dk] = struct{}{}
	}
	if _, ok := e.domains[dk]; !ok {
		e.forgetDomain(dk)
	}

	if derr != nil && !errors.Is(derr, netflow.ErrorTemplateNotFound) {
		return res, derr // partial results are still returned
	}
	return res, v9Err
}

func dedupKeys(keys []TemplateKey) []TemplateKey {
	seen := make(map[TemplateKey]bool, len(keys))
	out := keys[:0:0]
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// finish turns the parsed records of one datagram (or one replayed set) into
// flows: IE 160 reboot check, uptime_est bound, times, sanitising, clock-skew
// correction and sampling. rebooted reports an IE 160 change (templates were
// reset, keeping those learned in this datagram). live is false for replayed
// sets: their older data neither triggers reboot detection nor feeds the
// sysInit state; their uptime_est bound is used but not stored.
func (e *Exporter) finish(recs []rawRecord, ctx recordCtx, learned []TemplateKey, live bool) (flows []Flow, rebooted bool) {
	if len(recs) == 0 {
		return nil, false
	}
	dk := domainKey{10, ctx.obs}
	exportMs := ctx.exportTime.UnixMilli()
	var est int64
	var hasEst bool
	if ctx.kind == KindIPFIX {
		var maxEnd uint64
		needEst := false
		for i := range recs {
			r := &recs[i]
			if r.sysInitMs > 0 {
				if !live {
					continue
				}
				if last, ok := e.sysInit[dk]; ok && !rebooted && absDiff(last, r.sysInitMs) > sysInitJump {
					e.reboot(learned)
					rebooted = true
				}
				e.sysInit[dk] = r.sysInitMs
			} else if r.hasUp {
				needEst = true
				maxEnd = max(maxEnd, r.upEnd&(1<<62-1))
			}
		}
		if needEst {
			// sysInit <= exportTime_ms + 999 − uptime at export <= this bound:
			// the export time has 1 s resolution and no flow ends after the
			// export. The minimum over recent datagrams is the tightest.
			bound := exportMs + 999 - int64(maxEnd)
			if live {
				e.addEstBound(dk, ctx.received, bound)
			}
			est, hasEst = e.estimate(dk, ctx.received)
			if !hasEst || bound < est {
				est, hasEst = bound, true
			}
		}
	}
	skew := ctx.received.Sub(ctx.exportTime)
	flows = make([]Flow, 0, len(recs))
	for i := range recs {
		r := &recs[i]
		f := &r.f
		if r.hasUp {
			f.RawStart, f.RawEnd = r.upStart, r.upEnd
		}
		switch {
		case !r.absStart.IsZero() && !r.absEnd.IsZero():
			f.Start, f.End, f.TimeSource = r.absStart, r.absEnd, TimeAbsolute
		case !r.absStart.IsZero() || !r.absEnd.IsZero():
			t := r.absEnd
			if t.IsZero() {
				t = r.absStart
			}
			f.Start, f.End, f.TimeSource = t, t, TimeAbsolute
		case ctx.kind == KindNFv9 && r.hasUp:
			f.Start = ctx.exportTime.Add(-v9Offset(ctx.sysUptimeMs, r.upStart))
			f.End = ctx.exportTime.Add(-v9Offset(ctx.sysUptimeMs, r.upEnd))
			f.TimeSource = TimeSysUptime
		case ctx.kind == KindIPFIX && r.hasUp && r.sysInitMs > 0:
			base := int64(r.sysInitMs & (1<<62 - 1))
			f.Start = time.UnixMilli(unwrapUptime(base, r.upStart, exportMs))
			f.End = time.UnixMilli(unwrapUptime(base, r.upEnd, exportMs))
			f.TimeSource = TimeSysUptime
		case ctx.kind == KindIPFIX && r.hasUp && hasEst:
			f.Start = time.UnixMilli(unwrapUptime(est, r.upStart, exportMs))
			f.End = time.UnixMilli(unwrapUptime(est, r.upEnd, exportMs))
			f.TimeSource = TimeUptimeEst
		default:
			f.Start, f.End, f.TimeSource = ctx.exportTime, ctx.exportTime, TimeExport
		}
		sanitizeTimes(f, ctx.exportTime)
		applySkew(f, skew)
		r.scale(ctx.sampling)
		flows = append(flows, *f)
	}
	return flows, rebooted
}

func absDiff(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return b - a
}

func (e *Exporter) addEstBound(dk domainKey, at time.Time, bound int64) {
	bs := e.est[dk]
	kept := bs[:0]
	for _, b := range bs {
		if at.Sub(b.at) < estWindow {
			kept = append(kept, b)
		}
	}
	if len(kept) >= maxEstBounds {
		kept = append(kept[:0], kept[1:]...)
	}
	e.est[dk] = append(kept, estBound{at, bound})
}

// estimate is the uptime_est sysInit: the minimum bound of the last 10 min.
func (e *Exporter) estimate(dk domainKey, now time.Time) (int64, bool) {
	var best int64
	ok := false
	for _, b := range e.est[dk] {
		if now.Sub(b.at) >= estWindow {
			continue
		}
		if !ok || b.bound < best {
			best, ok = b.bound, true
		}
	}
	return best, ok
}

// learnSampling reads a sampling announcement from options data: IE 34/50
// give 1-in-N, 305+306 give (interval+space)/interval (RFC 5476).
func (e *Exporter) learnSampling(dk domainKey, recs []netflow.OptionsDataRecord) {
	for _, r := range recs {
		var interval, space uint64
		for _, v := range r.OptionsValues {
			b, _ := v.Value.([]byte)
			n, ok := beUint(b)
			if !ok || v.PenProvided {
				continue
			}
			switch v.Type {
			case 34, 50: // samplingInterval, samplerRandomInterval
				interval, space = n, 0
			case 305:
				interval = n
			case 306:
				space = n
			}
		}
		if interval == 0 {
			continue
		}
		rate := interval
		if space > 0 {
			rate = (interval + space) / interval
		}
		if rate > 0 && rate <= 1<<31 {
			e.sampling[dk] = uint32(rate)
		}
	}
}

func (e *Exporter) samplingFor(dk domainKey) uint32 {
	if r, ok := e.sampling[dk]; ok && r > 0 {
		return r
	}
	return e.SamplingOverride
}

// SamplingRate returns the effective 1-in-N of a v9 (version 9) or IPFIX
// (version 10) domain: announced, else the override, else 1.
func (e *Exporter) SamplingRate(version uint16, obs uint32) uint32 {
	return max(e.samplingFor(domainKey{version, obs}), 1)
}

// expirePending drops buffered sets older than PendingTTL.
func (e *Exporter) expirePending(now time.Time) {
	kept := e.pending[:0]
	e.pendSize = 0
	for _, p := range e.pending {
		if now.Sub(p.ctx.received) < PendingTTL {
			kept = append(kept, p)
			e.pendSize += len(p.raw)
		} else {
			e.Stats.PendingDropped++
		}
	}
	clear(e.pending[len(kept):])
	e.pending = kept
}

// ExpirePending drops buffered sets older than PendingTTL (the collector
// calls it periodically so an idle exporter's buffer does not linger).
func (e *Exporter) ExpirePending(now time.Time) { e.expirePending(now) }

func (e *Exporter) bufferPending(k TemplateKey, raw []byte, ctx recordCtx) {
	e.expirePending(ctx.received)
	if len(e.pending) >= MaxPendingSets || e.pendSize+len(raw) > MaxPendingBytes {
		e.Stats.PendingDropped++
		return
	}
	cp := append([]byte(nil), raw...) // raw aliases the datagram buffer
	e.pending = append(e.pending, pendingSet{key: k, raw: cp, ctx: ctx})
	e.pendSize += len(cp)
}

func (e *Exporter) replay(added []TemplateKey, now time.Time) []Flow {
	e.expirePending(now)
	want := map[TemplateKey]bool{}
	for _, k := range added {
		want[k] = true
	}
	var out []Flow
	kept := e.pending[:0]
	var replayed []pendingSet
	for _, p := range e.pending {
		if want[p.key] {
			replayed = append(replayed, p)
		} else {
			kept = append(kept, p)
		}
	}
	clear(e.pending[len(kept):])
	e.pending = kept
	e.pendSize = 0
	for _, p := range kept {
		e.pendSize += len(p.raw)
	}
	budget := maxReplayRecords
	for _, p := range replayed {
		t, err := e.Templates.GetTemplate(p.key.Version, p.key.ObsDomain, p.key.ID)
		if err != nil {
			e.Stats.PendingDropped++
			continue
		}
		// Bound the records one replay decodes (up to 256 KiB of buffered
		// sets of a tiny template would otherwise be ~1 record per byte).
		if fields, _, err := templateFields(t); err == nil {
			n := minRecordLen(fields)
			if n <= 0 || len(p.raw)/n > budget {
				e.Stats.PendingDropped++
				continue
			}
			budget -= len(p.raw) / n
		}
		dk := domainKey{p.key.Version, p.key.ObsDomain}
		switch tr := t.(type) {
		case netflow.TemplateRecord:
			recs, err := netflow.DecodeDataSet(p.key.Version, bytes.NewBuffer(p.raw), tr.Fields)
			if err != nil && len(recs) == 0 {
				e.Stats.PendingDropped++
				continue
			}
			p.ctx.sampling = e.samplingFor(dk)
			var rr []rawRecord
			for _, r := range recs {
				if x, ok := parseDataRecord(r.Values, p.ctx); ok {
					rr = append(rr, x)
				}
			}
			flows, _ := e.finish(rr, p.ctx, nil, false)
			out = append(out, flows...)
		case netflow.IPFIXOptionsTemplateRecord:
			recs, _ := netflow.DecodeOptionsDataSet(p.key.Version, bytes.NewBuffer(p.raw), tr.Scopes, tr.Options)
			e.learnSampling(dk, recs)
		case netflow.NFv9OptionsTemplateRecord:
			recs, _ := netflow.DecodeOptionsDataSet(p.key.Version, bytes.NewBuffer(p.raw), tr.Scopes, tr.Options)
			e.learnSampling(dk, recs)
		}
		e.Stats.Replayed++
	}
	return out
}

// LoadTemplates installs persisted templates (marked as loaded from the
// database). It returns how many were accepted; corrupt ones are skipped and
// reported through errs.
func (e *Exporter) LoadTemplates(recs []TemplateRecord) (loaded int, errs []error) {
	for _, r := range recs {
		dk := domainKey{r.Version, r.ObsDomain}
		if !e.canAdmit(dk) {
			errs = append(errs, fmt.Errorf("template %d/%d/%d: %w", r.Version, r.ObsDomain, r.ID, ErrTooManyDomains))
			continue
		}
		if err := e.Templates.Import(r); err != nil {
			errs = append(errs, fmt.Errorf("template %d/%d/%d: %w", r.Version, r.ObsDomain, r.ID, err))
			continue
		}
		e.domains[dk] = struct{}{}
		loaded++
	}
	return loaded, errs
}
