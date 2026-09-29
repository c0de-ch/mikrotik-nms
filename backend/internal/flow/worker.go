package flow

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow/decode"
)

// Worker cadence and limits.
const (
	flushCheckEvery    = 5 * time.Second
	reconcileEvery     = 30 * time.Second
	persistEvery       = 60 * time.Second
	hintWriteEvery     = 5 * time.Minute
	templateSaveGap    = time.Minute    // at most one upsert per template key and minute
	templateDeleteGap  = time.Minute    // at most one delete-all (reboot) per exporter and minute
	templateLoadMaxAge = 24 * time.Hour // persisted templates loaded at exporter-state creation
	// maxIfacesPerExporter bounds the ifIndexes of one exporter that become
	// flow_exporter_ifaces rows and native points without being known from
	// the device's ifIndex sync (wire values: a sender could otherwise create
	// thousands of rows and points per minute). Flows on the others are
	// still stored; only the interface row and the point are skipped.
	maxIfacesPerExporter = hintIfaceCap
	captureMax           = 20        // raw datagrams captured per exporter and process
	flushCatchUpLimit    = 15 * 60   // s: a longer stall jumps ahead instead of flushing empty minutes
	interfacesSeenSince  = time.Hour // ExporterStatus.InterfacesSeen
	perMinuteWindow      = 5         // minutes averaged in ExporterStatus.PerMinute
	perMinuteSlots       = perMinuteWindow + 1
)

// runtimeSettings are the flow_* settings the worker reads on reconcile.
type runtimeSettings struct {
	topN, maxRows, pps int
	autoAccept         bool
}

func readSettings(c *Collector) runtimeSettings {
	return runtimeSettings{topN: TopN(c.db), maxRows: MaxRowsPerMinute(c.db), pps: RateLimitPPS(c.db),
		autoAccept: AutoAcceptDevices(c.db)}
}

// minuteRate counts one receive minute for ExporterStatus.PerMinute.
type minuteRate struct {
	minute                  int64
	datagrams, flows, bytes int64
}

// counterSet is the persisted part of an exporter's counters.
type counterSet struct {
	datagrams, flows, bytes, decodeErrors, templateMisses, seqLost int64
}

func (a counterSet) sub(b counterSet) counterSet {
	return counterSet{a.datagrams - b.datagrams, a.flows - b.flows, a.bytes - b.bytes,
		a.decodeErrors - b.decodeErrors, a.templateMisses - b.templateMisses, a.seqLost - b.seqLost}
}

// expState is the worker's state of one exporter.
type expState struct {
	row  queries.FlowExporter // configuration (refreshed on reconcile)
	addr netip.Addr
	gate *exporterGate
	dec  *decode.Exporter
	prev decode.Stats // decoder stats already absorbed

	agg   *aggregator
	dup   *dupFilter   // kind=opnsense only
	hints *hintTracker // exporters without a device only

	base      counterSet // DB counters when this state was created
	live      counterSet // since this state was created
	persisted counterSet // part of live already written to the DB

	replayed, pendingDropped       int64
	dupDropped, selfExport, natDst int64
	reboots, ifaceOverflow         int64
	timeSource                     [4]int64

	lastSeen, lastSeenPersisted time.Time
	protocol                    string
	sampling                    int
	skewMs                      int64
	lastError                   string
	lastErrorAt                 time.Time
	errDirty                    bool

	perMin            [perMinuteSlots]minuteRate
	tmplSaved         map[decode.TemplateKey]time.Time
	tmplDeletePending bool      // a reboot's delete-all is still to be queued
	lastTmplDelete    time.Time // when the last delete-all was queued
	firstLogged       bool
	captured          int
	lastHint          time.Time
	lastIfsyncReq     time.Time

	// ifKnown are the ifIndexes that may be touched / get a native point:
	// the exporter's flow_exporter_ifaces rows and native points (rebuilt on
	// reconcile) plus those admitted since. ifLearned counts the ones not
	// known from the device's ifIndex sync (capped at maxIfacesPerExporter).
	ifKnown   map[uint32]bool
	ifLearned int

	rebootLog, overflowLog logThrottle
}

func (st *expState) addRate(now time.Time, dg, flows, bytes int64) {
	m := floorMinute(now.Unix())
	slot := &st.perMin[(m/60)%perMinuteSlots]
	if slot.minute != m {
		*slot = minuteRate{minute: m}
	}
	slot.datagrams += dg
	slot.flows += flows
	slot.bytes += bytes
}

// rates averages the last perMinuteWindow complete minutes.
func (st *expState) rates(now time.Time) Rates {
	cur := floorMinute(now.Unix())
	var r Rates
	for _, s := range st.perMin {
		if s.minute >= cur-perMinuteWindow*60 && s.minute < cur {
			r.Datagrams += float64(s.datagrams)
			r.Flows += float64(s.flows)
			r.Bytes += float64(s.bytes)
		}
	}
	r.Datagrams /= perMinuteWindow
	r.Flows /= perMinuteWindow
	r.Bytes /= perMinuteWindow
	return r
}

// configure applies an exporter row's configuration.
func (st *expState) configure(r queries.FlowExporter, now time.Time) {
	st.row = r
	st.dec.SamplingOverride = uint32(max(r.SamplingOverride, 0))
	st.dec.SkipV9Seq = r.Kind == "opnsense"
	st.gate.enabled.Store(r.Enabled)
	switch {
	case r.Kind == "opnsense" && st.dup == nil:
		st.dup = newDupFilter()
	case r.Kind != "opnsense":
		st.dup = nil
	}
	switch {
	case r.DeviceID == "" && st.hints == nil:
		st.hints = newHintTracker(now)
	case r.DeviceID != "":
		st.hints = nil
	}
}

// newExpState builds the state of a new exporter and loads its persisted
// templates (younger than 24 h).
func (c *Collector) newExpState(r queries.FlowExporter, addr netip.Addr, now time.Time) *expState {
	st := &expState{
		addr:        addr,
		gate:        &exporterGate{id: r.ID, bucket: newTokenBucket(float64(c.settings.pps))},
		dec:         decode.NewExporter(addr),
		agg:         newAggregator(),
		base:        counterSet{r.Datagrams, r.Flows, r.Bytes, r.DecodeErrors, r.TemplateMisses, r.SeqLost},
		lastSeen:    r.LastSeen,
		protocol:    r.Protocol,
		sampling:    r.SamplingRate,
		skewMs:      r.ClockSkewMs,
		lastError:   r.LastError,
		lastErrorAt: r.LastErrorAt,
		tmplSaved:   map[decode.TemplateKey]time.Time{},
		ifKnown:     map[uint32]bool{},
	}
	st.lastSeenPersisted = r.LastSeen
	st.configure(r, now)
	if c.db != nil {
		rows, err := queries.LoadFlowTemplates(c.db, r.ID, now.Add(-templateLoadMaxAge), decode.MaxTemplates)
		if err != nil {
			log.Printf("flow collector: %s: load templates: %v", r.Name, err)
		}
		recs := make([]decode.TemplateRecord, 0, len(rows))
		for _, t := range rows {
			recs = append(recs, decode.TemplateRecord{Version: t.Version, ObsDomain: t.ObsDomain, ID: t.TemplateID,
				Kind: t.Kind, Fields: t.Fields})
		}
		loaded, errs := st.dec.LoadTemplates(recs)
		if loaded > 0 && st.protocol == "" { // status never persisted: the templates tell
			st.protocol = map[uint16]string{9: "netflow9", 10: "ipfix"}[recs[0].Version]
		}
		if len(errs) > 0 {
			log.Printf("flow collector: %s: skipped %d corrupt persisted template(s): %v", r.Name, len(errs), errs[0])
		}
		if loaded > 0 {
			log.Printf("flow collector: %s: loaded %d persisted template(s)", r.Name, loaded)
		}
	}
	st.prev = st.dec.Stats
	return st
}

// reconcile re-reads settings, exporters, devices, points and interfaces,
// rebuilds the allow-list and refreshes the status (30 s tick, Reload, and
// after an auto-accept).
func (c *Collector) reconcile(now time.Time) {
	if c.db == nil {
		return
	}
	c.settings = readSettings(c)
	rows, err := queries.ListFlowExporters(c.db)
	if err != nil {
		log.Printf("flow collector: reconcile: %v", err)
		return
	}
	devices := map[netip.Addr]deviceInfo{}
	if list, err := queries.ListDevices(c.db); err == nil {
		for _, d := range list {
			a, err := netip.ParseAddr(d.Address)
			if err != nil {
				continue
			}
			name := d.Identity
			if name == "" {
				name = d.Address
			}
			devices[a.Unmap()] = deviceInfo{id: d.ID, name: name}
		}
	}
	if points, err := queries.ListFlowPoints(c.db); err == nil {
		natives := map[pointKey]bool{}
		for _, p := range points {
			if p.Kind == "native" && len(p.IfIndexes) == 1 {
				natives[pointKey{p.ExporterID, p.IfIndexes[0]}] = true
			}
		}
		c.natives = natives
	}
	if ifs, err := queries.ListFlowIfaces(c.db, 0); err == nil {
		m := map[int64]map[uint32]queries.FlowIface{}
		for _, f := range ifs {
			if m[f.ExporterID] == nil {
				m[f.ExporterID] = map[uint32]queries.FlowIface{}
			}
			m[f.ExporterID][f.IfIndex] = f
		}
		c.ifaces = m
	}

	old := c.allow.Load()
	al := &allowList{exporters: map[netip.Addr]*exporterGate{}, candidates: map[netip.Addr]*candidateGate{},
		devices: devices}
	seen := map[int64]bool{}
	for _, r := range rows {
		a, err := netip.ParseAddr(r.Address)
		if err != nil {
			continue
		}
		a = a.Unmap()
		st := c.exps[r.ID]
		if st == nil || st.addr != a {
			st = c.newExpState(r, a, now)
			c.exps[r.ID] = st
		} else {
			st.configure(r, now)
		}
		st.gate.bucket.setRate(float64(c.settings.pps))
		al.exporters[a] = st.gate
		seen[r.ID] = true
		c.unknown.forget(a)
	}
	for id := range c.exps {
		if !seen[id] {
			delete(c.exps, id)
		}
	}
	for id, st := range c.exps {
		c.rebuildIfKnown(id, st)
	}
	if c.settings.autoAccept {
		for a, d := range devices {
			if al.exporters[a] != nil {
				continue
			}
			if old != nil && old.candidates[a] != nil && old.candidates[a].deviceID == d.id {
				cg := old.candidates[a]
				cg.bucket.setRate(float64(c.settings.pps))
				al.candidates[a] = cg
				continue
			}
			al.candidates[a] = &candidateGate{deviceID: d.id, name: d.name, bucket: newTokenBucket(float64(c.settings.pps))}
		}
	}
	c.globalBucket.setRate(float64(globalRateFactor * c.settings.pps))
	c.allow.Store(al)
	c.rows = rows
	c.devices = devices

	if n, ok := kernelDrops(c.boundPorts); ok {
		c.global.kernelDropped.Store(n)
	}
	for _, st := range c.exps {
		st.dec.ExpirePending(now)
		c.absorbDecodeStats(st)
	}
	c.rebuildStatus(now)
	if c.hub != nil && c.hub.TopicSubscriberCount(TopicStatus) > 0 {
		c.hub.Publish(TopicStatus, c.Status())
	}
}

// rebuildIfKnown resets an exporter's known ifIndexes to its interface rows
// and native points (reconcile: the DB is the truth again).
func (c *Collector) rebuildIfKnown(id int64, st *expState) {
	st.ifKnown = map[uint32]bool{}
	st.ifLearned = 0
	for idx, f := range c.ifaces[id] {
		st.ifKnown[idx] = true
		if f.Source == "learned" {
			st.ifLearned++
		}
	}
	for k := range c.natives {
		if k.exporterID == id && !st.ifKnown[k.ifIndex] {
			st.ifKnown[k.ifIndex] = true
			st.ifLearned++
		}
	}
}

// admitIface reports whether ifIndex idx of an exporter may be touched and
// get a native point: it is known, or fewer than maxIfacesPerExporter
// learned ones are (it is then admitted).
func (st *expState) admitIface(idx uint32) bool {
	if st.ifKnown[idx] {
		return true
	}
	if st.ifLearned >= maxIfacesPerExporter {
		return false
	}
	st.ifKnown[idx] = true
	st.ifLearned++
	return true
}

// absorbDecodeStats moves the decoder's counter deltas into the exporter and
// global counters.
func (c *Collector) absorbDecodeStats(st *expState) {
	s := st.dec.Stats
	p := st.prev
	st.live.decodeErrors += int64(s.DecodeErrors - p.DecodeErrors)
	st.live.templateMisses += int64(s.TemplateMisses - p.TemplateMisses)
	st.live.seqLost += int64(s.SeqLost - p.SeqLost)
	st.replayed += int64(s.Replayed - p.Replayed)
	st.pendingDropped += int64(s.PendingDropped - p.PendingDropped)
	st.reboots += int64(s.Reboots - p.Reboots)
	c.global.rejected.Add(int64(s.Rejected - p.Rejected))
	c.global.panics.Add(int64(s.Panics - p.Panics))
	st.prev = s
}

// acceptCandidate turns a managed device's first datagram into an auto
// exporter (flow_auto_accept_devices), exactly once.
func (c *Collector) acceptCandidate(d *datagram, now time.Time) *expState {
	if st := c.stateByAddr(d.addr); st != nil {
		return st
	}
	if !c.settings.autoAccept || c.db == nil {
		c.global.unknownDatagrams.Add(1)
		dev := c.devices[d.addr]
		c.unknown.record(d.addr, decode.Sniff(d.bytes()), now, dev, dev.id != "")
		return nil
	}
	e := &queries.FlowExporter{Name: d.candidate.name, Address: d.addr.String(), Kind: "routeros",
		DeviceID: d.candidate.deviceID, Enabled: true, Auto: true}
	if _, err := queries.CreateFlowExporter(c.db, e); err != nil && !errors.Is(err, queries.ErrFlowExporterExists) {
		if len(c.acceptLog) > unknownRingCap*4 {
			c.acceptLog = map[netip.Addr]*logThrottle{}
		}
		lt := c.acceptLog[d.addr]
		if lt == nil {
			lt = &logThrottle{}
			c.acceptLog[d.addr] = lt
		}
		if ok, n := lt.allow(now, wireLogEvery); ok {
			log.Printf("flow collector: auto-accept %s: %v%s", d.addr, err, heldNote(n))
		}
		return nil
	} else if err == nil {
		log.Printf("flow collector: auto-accepted managed device %s (%s) as a flow exporter", d.addr, e.Name)
	}
	c.reconcile(now)
	return c.stateByAddr(d.addr)
}

func (c *Collector) stateByAddr(a netip.Addr) *expState {
	for _, st := range c.exps {
		if st.addr == a {
			return st
		}
	}
	return nil
}

// processDatagram is the worker's per-datagram path: capture, decode,
// normalise, aggregate. A panic is recovered and counted.
func (c *Collector) processDatagram(d datagram) {
	defer c.recycle(&d)
	defer func() {
		if r := recover(); r != nil {
			c.global.panics.Add(1)
			log.Printf("flow collector: panic processing a datagram from %s: %v", d.addr, r)
		}
	}()
	now := d.received
	c.flushDue(now)
	var st *expState
	if d.candidate != nil {
		st = c.acceptCandidate(&d, now)
	} else {
		st = c.exps[d.exporterID]
	}
	if st == nil {
		return
	}
	if !st.gate.enabled.Load() {
		c.global.rejected.Add(1)
		return
	}
	pkt := d.bytes()
	c.capture(st, pkt, now)
	st.live.datagrams++
	st.lastSeen = now
	st.agg.markAlive(now, c.nextFlush)
	st.addRate(now, 1, 0, 0)

	res, err := st.dec.Decode(pkt, now)
	c.absorbDecodeStats(st)
	// Rejections (failed prechecks: truncated v5, oversized sFlow counts, …)
	// are counted globally as rejected, not as decode_errors; last_error is
	// the only per-exporter trace of them, so an exporter that sends nothing
	// but malformed datagrams does not look healthy.
	if err != nil {
		st.lastError, st.lastErrorAt, st.errDirty = err.Error(), now, true
	}
	if res.Kind != decode.KindUnknown {
		st.protocol = res.Kind.String()
		if !st.firstLogged {
			st.firstLogged = true
			log.Printf("flow collector: first datagram from %s (%s, %s)", st.addr, st.row.Name, st.protocol)
		}
	}
	if !res.ExportTime.IsZero() {
		st.skewMs = res.ClockSkew.Milliseconds()
	}
	c.persistTemplates(st, &res, now)

	var nc normCounters
	var accepted, bytes int64
	for i := range res.Flows {
		f := &res.Flows[i]
		st.timeSource[f.TimeSource]++
		nf, ok := c.normalize(st, f, now, &nc)
		if !ok {
			continue
		}
		if st.agg.add(nf, now, c.nextFlush) {
			c.global.lateRecords.Add(1)
		}
		accepted++
		bytes = satAdd(bytes, nf.bytes)
		st.sampling = int(max(f.SamplingRate, 1))
		if st.hints != nil && nf.endpoints {
			st.hints.add(f.InIf, f.SrcAddr, nf.bytes, now)
		}
	}
	st.live.flows += accepted
	st.live.bytes = satAdd(st.live.bytes, bytes)
	st.addRate(now, 0, accepted, bytes)
	st.selfExport += nc.selfExport
	st.dupDropped += nc.dup
	st.natDst += nc.natDst
	c.global.selfExportDropped.Add(nc.selfExport)
	c.global.dupDropped.Add(nc.dup)
}

// persistTemplates turns a datagram's template changes into writer ops: a
// reboot deletes the exporter's persisted templates (at most once a minute,
// retried while refused), withdrawn templates are deleted, learned ones
// upserted (at most once a minute per key). Template ops only use spare
// writer capacity (submitMaint); a refused upsert is retried on the next
// announcement. Reboot log lines are throttled per exporter.
func (c *Collector) persistTemplates(st *expState, res *decode.Result, now time.Time) {
	if res.Rebooted {
		if ok, n := st.rebootLog.allow(now, wireLogEvery); ok {
			log.Printf("flow collector: %s: exporter reboot detected, templates reset%s", st.row.Name, heldNote(n))
		}
		st.tmplDeletePending = true
	}
	if st.tmplDeletePending && (st.lastTmplDelete.IsZero() || now.Sub(st.lastTmplDelete) >= templateDeleteGap) &&
		c.submitMaint(writeOp{deleteTemplates: st.row.ID}) {
		st.tmplDeletePending = false
		st.lastTmplDelete = now
		clear(st.tmplSaved)
	}
	if len(res.Withdrawn) > 0 {
		keys := make([]queries.FlowTemplateKey, 0, len(res.Withdrawn))
		for _, k := range res.Withdrawn {
			keys = append(keys, queries.FlowTemplateKey{Version: k.Version, ObsDomain: k.ObsDomain, TemplateID: k.ID})
			delete(st.tmplSaved, k)
		}
		c.submitMaint(writeOp{withdraw: &templateWithdraw{exporterID: st.row.ID, keys: keys}})
	}
	for _, t := range res.Learned {
		k := t.Key()
		if now.Sub(st.tmplSaved[k]) < templateSaveGap {
			continue
		}
		op := writeOp{template: &queries.FlowTemplate{ExporterID: st.row.ID, Version: t.Version, ObsDomain: t.ObsDomain,
			TemplateID: t.ID, Kind: t.Kind, Fields: t.Fields, UpdatedAt: now}}
		if c.submitMaint(op) {
			st.tmplSaved[k] = now
		}
	}
	if len(res.Learned) > 0 || len(res.Withdrawn) > 0 {
		// Only keys still stored are remembered (the store holds <= 64).
		for k := range st.tmplSaved {
			if !st.dec.Templates.Has(k) {
				delete(st.tmplSaved, k)
			}
		}
	}
}

// capture writes the first raw datagrams of each exporter (dev only).
func (c *Collector) capture(st *expState, pkt []byte, now time.Time) {
	if c.cfg.CaptureDir == "" || st.captured >= captureMax {
		return
	}
	st.captured++
	name := filepath.Join(c.cfg.CaptureDir, fmt.Sprintf("%s-%d.bin", st.addr, now.UnixNano()))
	if err := os.WriteFile(name, pkt, 0o640); err != nil && st.captured == 1 {
		log.Printf("flow collector: capture %s: %v", name, err)
	}
}

// flushDue flushes every minute M with now >= M + 180 s, oldest first.
func (c *Collector) flushDue(now time.Time) {
	nowU := now.Unix()
	if c.nextFlush == 0 {
		c.nextFlush = floorMinute(nowU) - int64(maxOpenMinutes-1)*60
	}
	if nowU-c.nextFlush > flushCatchUpLimit {
		target := floorMinute(nowU) - flushDelay
		for _, st := range c.exps {
			if o, ok := st.agg.oldest(); ok && o < target {
				target = o
			}
		}
		c.nextFlush = max(c.nextFlush, target)
	}
	for c.nextFlush+flushDelay <= nowU {
		c.flushMinute(c.nextFlush, now, false)
		c.nextFlush += 60
	}
}

// flushAll flushes every open minute (shutdown; partial minutes are fine,
// the upserts are additive).
func (c *Collector) flushAll(now time.Time) {
	last := floorMinute(now.Unix())
	for _, st := range c.exps {
		for m := range st.agg.minutes {
			last = max(last, m)
		}
	}
	first := c.nextFlush
	for _, st := range c.exps {
		if o, ok := st.agg.oldest(); ok && o < first {
			first = o
		}
	}
	for m := first; m <= last; m += 60 {
		c.flushMinute(m, now, true)
	}
}

// flushMinute flushes minute m of every exporter into one writer batch.
func (c *Collector) flushMinute(m int64, now time.Time, final bool) {
	b := &flushBatch{minute: time.Unix(m, 0).UTC(), final: final, touches: map[int64][]queries.FlowIfaceSeen{}}
	ids := make([]int64, 0, len(c.exps))
	for id := range c.exps {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	topN, maxRows := max(c.settings.topN, 1), max(c.settings.maxRows, 1)
	for _, id := range ids {
		st := c.exps[id]
		out := st.agg.flush(id, m, topN, maxRows)
		if out.meta == nil && len(out.rows) == 0 {
			continue
		}
		b.rows = append(b.rows, out.rows...)
		if out.meta != nil {
			b.meta = append(b.meta, *out.meta)
			c.global.overflowRecords.Add(out.meta.Overflow)
		}
		b.exporterIDs = append(b.exporterIDs, id)
		if len(out.ifaces) == 0 {
			continue
		}
		idxs := make([]uint32, 0, len(out.ifaces))
		for idx := range out.ifaces {
			idxs = append(idxs, idx)
		}
		slices.Sort(idxs)
		writeHints := st.hints != nil && now.Sub(st.lastHint) >= hintWriteEvery
		if writeHints {
			st.lastHint = now
		}
		unknownIface := false
		overflow := 0
		for _, idx := range idxs {
			if !st.admitIface(idx) {
				overflow++
				continue
			}
			seen := queries.FlowIfaceSeen{IfIndex: idx}
			if writeHints {
				seen.Hint = st.hints.hint(idx)
			}
			b.touches[id] = append(b.touches[id], seen)
			if k := (pointKey{id, idx}); !c.natives[k] {
				c.natives[k] = true // optimistic; the next reconcile re-reads the truth
				b.points = append(b.points, k)
			}
			if f, ok := c.ifaces[id][idx]; !ok || f.Name == "" {
				unknownIface = true
			}
		}
		if overflow > 0 {
			st.ifaceOverflow += int64(overflow)
			if ok, n := st.overflowLog.allow(now, wireLogEvery); ok {
				log.Printf("flow collector: %s: %d interface index(es) beyond the cap of %d new ones per exporter "+
					"get no interface row or observation point (their flows are still stored)%s",
					st.row.Name, overflow, maxIfacesPerExporter, heldNote(n))
			}
		}
		if unknownIface && st.row.DeviceID != "" && now.Sub(st.lastIfsyncReq) >= ifsyncOnDemandGap {
			st.lastIfsyncReq = now
			select {
			case c.ifsyncReq <- id:
			default:
			}
		}
	}
	if !final && (m+60)%3600 == 0 {
		b.rollupHour = time.Unix(m+60, 0).UTC()
	}
	c.submit(writeOp{flush: b})
}

// persistStatus writes every exporter's counter deltas and health (60 s tick
// and shutdown). A delta that cannot be queued is retried next time.
func (c *Collector) persistStatus() {
	var ops []statusDelta
	var marks []*expState
	for _, st := range c.exps {
		d := st.live.sub(st.persisted)
		if d == (counterSet{}) && !st.errDirty && st.lastSeen.Equal(st.lastSeenPersisted) {
			continue
		}
		delta := queries.FlowExporterStatusDelta{
			LastSeen: st.lastSeen, Protocol: st.protocol, SamplingRate: st.sampling,
			Datagrams: d.datagrams, Flows: d.flows, Bytes: d.bytes, DecodeErrors: d.decodeErrors,
			TemplateMisses: d.templateMisses, SeqLost: d.seqLost, ClockSkewMs: st.skewMs,
		}
		if st.errDirty {
			delta.LastError, delta.LastErrorAt = st.lastError, st.lastErrorAt
		}
		ops = append(ops, statusDelta{id: st.row.ID, delta: delta})
		marks = append(marks, st)
	}
	if len(ops) == 0 {
		return
	}
	if c.submit(writeOp{status: ops}) {
		for _, st := range marks {
			st.persisted = st.live
			st.errDirty = false
			st.lastSeenPersisted = st.lastSeen
		}
	}
}

// rebuildStatus refreshes the status snapshot Status() copies.
func (c *Collector) rebuildStatus(now time.Time) {
	exps := make([]ExporterStatus, 0, len(c.rows))
	for _, r := range c.rows {
		es := ExporterStatusFromRow(r, now)
		if st := c.exps[r.ID]; st != nil {
			c.overlayLive(&es, st, now)
		}
		for _, f := range c.ifaces[r.ID] {
			if f.IfIndex != 0 && !f.LastSeen.IsZero() && now.Sub(f.LastSeen) < interfacesSeenSince {
				es.InterfacesSeen++
			}
		}
		exps = append(exps, es)
	}
	c.statusMu.Lock()
	c.status.Exporters = exps
	c.status.RcvBufBytes = c.rcvBuf
	c.status.ListenErrors = slices.Clone(c.listenErrors)
	if c.status.ListenErrors == nil {
		c.status.ListenErrors = []string{}
	}
	c.status.StartedAt = timePtr(c.startedAt)
	c.statusMu.Unlock()
}

// overlayLive fills an exporter status from the live state.
func (c *Collector) overlayLive(es *ExporterStatus, st *expState, now time.Time) {
	lastSeen := st.lastSeen
	if st.row.LastSeen.After(lastSeen) {
		lastSeen = st.row.LastSeen
	}
	es.LastSeen = timePtr(lastSeen)
	es.State = exporterState(st.row.Enabled, lastSeen, now)
	es.Protocol = st.protocol
	es.SamplingRate = max(st.sampling, 1)
	es.Templates = st.dec.Templates.Len()
	switch {
	case st.protocol != "netflow9" && st.protocol != "ipfix":
		es.TemplateState = TemplateNA
	case st.dec.PendingSets() > 0:
		es.TemplateState = TemplateWaiting
	case st.dec.Templates.PersistedOnly():
		es.TemplateState = TemplatePersisted
	default:
		es.TemplateState = TemplateOK
	}
	es.PerMinute = st.rates(now)
	total := counterSet{st.base.datagrams + st.live.datagrams, st.base.flows + st.live.flows,
		satAdd(st.base.bytes, st.live.bytes), st.base.decodeErrors + st.live.decodeErrors,
		st.base.templateMisses + st.live.templateMisses, st.base.seqLost + st.live.seqLost}
	es.Counters["datagrams"] = total.datagrams
	es.Counters["flows"] = total.flows
	es.Counters["bytes"] = total.bytes
	es.Counters["decode_errors"] = total.decodeErrors
	es.Counters["template_misses"] = total.templateMisses
	es.Counters["seq_lost"] = total.seqLost
	es.Counters["replayed"] = st.replayed
	es.Counters["pending_dropped"] = st.pendingDropped
	es.Counters["rate_limited"] = st.gate.rateLimited.Load()
	es.Counters["dup_dropped"] = st.dupDropped
	es.Counters["self_export_dropped"] = st.selfExport
	es.Counters["nat_dst_records"] = st.natDst
	es.Counters["reboots"] = st.reboots
	es.Counters["iface_overflow"] = st.ifaceOverflow
	es.Counters["oversized"] = st.gate.oversized.Load()
	for i, n := range st.timeSource {
		es.TimeSource[decode.TimeSource(i).String()] = n
	}
	es.ClockSkewMs = st.skewMs
	es.LastError, es.LastErrorAt = st.lastError, timePtr(st.lastErrorAt)
}

// workerLoop owns all decoder and aggregation state until ctx is done.
func (c *Collector) workerLoop(ctx context.Context) {
	flushT := time.NewTicker(flushCheckEvery)
	reconcileT := time.NewTicker(reconcileEvery)
	persistT := time.NewTicker(persistEvery)
	defer flushT.Stop()
	defer reconcileT.Stop()
	defer persistT.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-c.queue:
			c.processDatagram(d)
		case <-flushT.C:
			now := c.now()
			c.guard("flush", func() {
				c.flushDue(now)
				c.rebuildStatus(now)
			})
		case <-reconcileT.C:
			c.guard("reconcile", func() { c.reconcile(c.now()) })
		case <-c.reload:
			c.guard("reconcile", func() { c.reconcile(c.now()) })
		case <-persistT.C:
			c.guard("persist", c.persistStatus)
		}
	}
}

// guard runs one worker step, recovering and counting a panic.
func (c *Collector) guard(what string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			c.global.panics.Add(1)
			log.Printf("flow collector: panic in %s: %v", what, r)
		}
	}()
	f()
}

// drainQueue processes whatever the readers queued before they stopped.
func (c *Collector) drainQueue() {
	for {
		select {
		case d := <-c.queue:
			c.processDatagram(d)
		default:
			return
		}
	}
}
