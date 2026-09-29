package api

import (
	"cmp"
	"context"
	"errors"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flow"
	"github.com/mikrotik-nms/backend/internal/flowview"
	"github.com/mikrotik-nms/backend/internal/macvendor"
	"github.com/mikrotik-nms/backend/internal/topology"
)

// Shared plumbing of the /flows/* read endpoints and the measured Sankey:
// ranges and windows over flow_1m / flow_1h, the aggregated-rows cache, the
// FlowPointView and Endpoint JSON objects and the per-request lookups behind
// them. Rows of two exporters are never combined: every view is one point.

// ---------- ranges and windows ----------

// flowRange is one selectable range of the flow views (contract §9.0).
type flowRange struct {
	key        string
	window     time.Duration
	table      queries.FlowTable
	portTable  queries.PortStatsTable // Phase 1 counters of the same resolution
	native     time.Duration          // bucket length of table
	step       time.Duration          // /flows/coverage series step
	resolution string                 // "1m" | "1h"
}

var flowRanges = map[string]flowRange{
	rangeLive: {key: rangeLive, window: 5 * time.Minute, table: queries.Flows1m, portTable: queries.PortStats1m,
		native: time.Minute, step: time.Minute, resolution: "1m"},
	"15m": {key: "15m", window: 15 * time.Minute, table: queries.Flows1m, portTable: queries.PortStats1m,
		native: time.Minute, step: time.Minute, resolution: "1m"},
	"1h": {key: "1h", window: time.Hour, table: queries.Flows1m, portTable: queries.PortStats1m,
		native: time.Minute, step: time.Minute, resolution: "1m"},
	"6h": {key: "6h", window: 6 * time.Hour, table: queries.Flows1m, portTable: queries.PortStats1m,
		native: time.Minute, step: time.Minute, resolution: "1m"},
	"24h": {key: "24h", window: 24 * time.Hour, table: queries.Flows1m, portTable: queries.PortStats1m,
		native: time.Minute, step: 5 * time.Minute, resolution: "1m"},
	"7d": {key: "7d", window: 7 * 24 * time.Hour, table: queries.Flows1h, portTable: queries.PortStats1h,
		native: time.Hour, step: time.Hour, resolution: "1h"},
	"30d": {key: "30d", window: 30 * 24 * time.Hour, table: queries.Flows1h, portTable: queries.PortStats1h,
		native: time.Hour, step: time.Hour, resolution: "1h"},
}

// parseFlowRange validates the range query param (default 1h).
func parseFlowRange(v string) (flowRange, bool) {
	if v == "" {
		v = "1h"
	}
	r, ok := flowRanges[v]
	return r, ok
}

// parseFlowDir validates the dir query param (default download).
func parseFlowDir(v string) (flowview.Dir, bool) {
	switch v {
	case "", string(flowview.Download):
		return flowview.Download, true
	case string(flowview.Upload):
		return flowview.Upload, true
	}
	return "", false
}

// flowWindowEnd is the exclusive end of the newest final bucket of the
// range's table (contract §9.0): the collector's flush (1m) or rollup (1h)
// watermark, else the newest meta bucket + 1 bucket, else the current minute
// / hour start.
func (s *Server) flowWindowEnd(rng flowRange, now time.Time) (time.Time, error) {
	var end time.Time
	if s.flows != nil {
		if rng.table == queries.Flows1h {
			end = s.flows.RolledThrough()
		} else {
			end = s.flows.FlushedThrough()
		}
	}
	if !end.IsZero() {
		return end.UTC(), nil
	}
	latest, ok, err := queries.LatestFlowBucket(s.db, rng.table, 0)
	if err != nil {
		return time.Time{}, err
	}
	if ok {
		return latest.UTC().Add(rng.native), nil
	}
	return now.UTC().Truncate(rng.native), nil
}

// flowWindow is an end-anchored window [start, end) of one exporter's
// flow rows, with the buckets that exporter covered (meta rows present).
type flowWindow struct {
	rng        flowRange
	start, end time.Time
	buckets    []time.Time // the exporter's covered buckets in [start, end), ascending
	effStart   time.Time   // first covered bucket (1h: its first covered minute when known), or start
	cov        *queries.Coverage
	minutes    map[int64]int64 // 1h: covered minutes per hour (flow_1h_meta.minutes); nil for 1m
}

// resolveFlowWindow anchors rng at the newest final bucket and reads which
// buckets the exporter covered inside the window.
func (s *Server) resolveFlowWindow(rng flowRange, exporterID int64, now time.Time) (flowWindow, error) {
	end, err := s.flowWindowEnd(rng, now)
	if err != nil {
		return flowWindow{}, err
	}
	w := flowWindow{rng: rng, start: end.Add(-rng.window), end: end}
	w.effStart = w.start
	if w.buckets, err = queries.FlowMetaBuckets(s.db, rng.table, exporterID, w.start, w.end); err != nil {
		return w, err
	}
	if len(w.buckets) > 0 {
		w.effStart = w.buckets[0]
		// An hourly bucket counts as fully covered, but the first one is
		// usually partial (the collector started, or the exporter first
		// sent, mid-hour): start at its first covered minute while flow_1m
		// still holds it, so averages over 7d/30d are not diluted by the
		// uncovered part of that hour.
		if rng.table == queries.Flows1h {
			mins, err := queries.FlowMetaBuckets(s.db, queries.Flows1m, exporterID, w.effStart, w.effStart.Add(rng.native))
			if err != nil {
				return w, err
			}
			if len(mins) > 0 {
				w.effStart = mins[0].UTC()
			}
		}
	}
	w.cov = queries.NewCoverage(rng.native, w.buckets, w.effStart)
	if rng.table == queries.Flows1h && len(w.buckets) > 0 {
		// A partially covered hour (collector or exporter down for part of
		// it) counts its covered minutes, not 3600 s.
		if w.minutes, err = queries.FlowMetaMinutes(s.db, exporterID, w.start, w.end); err != nil {
			return w, err
		}
		w.cov.Secs = make(map[int64]float64, len(w.minutes))
		for b, m := range w.minutes {
			w.cov.Secs[b] = float64(min(max(m, 0), 60)) * 60
		}
	}
	return w, nil
}

func (w flowWindow) covered() bool { return len(w.buckets) > 0 }

// seconds is how long the exporter covered the window: the divisor of every
// average.
func (w flowWindow) seconds() float64 {
	if !w.covered() {
		return 0
	}
	return w.cov.SecondsIn(w.start, w.end)
}

// coverageFrom is the JSON coverage_from (null without meta rows).
func (w flowWindow) coverageFrom() *time.Time {
	if !w.covered() {
		return nil
	}
	return timePtr(w.effStart)
}

// pointsFrom is the step-aligned bucket holding effStart: series omit the
// steps before coverage.
func (w flowWindow) pointsFrom(step time.Duration) time.Time {
	return w.start.Add(w.effStart.Sub(w.start) / step * step)
}

// ---------- aggregated-rows cache ----------

const (
	// flowCacheMaxRows bounds the rows held by the aggregated-rows cache
	// (~45 MB of FlowAgg); a single result above it is not cached.
	flowCacheMaxRows = 400_000
	// flowAggConcurrency bounds the heavy flow aggregates running at once
	// (the target is a 1-vCPU container: parallel full-window reads only
	// multiply the heap and push every request past the write timeout).
	flowAggConcurrency = 2
)

// errFlowAggAborted is what followers of a panicking aggregate get.
var errFlowAggAborted = errors.New("flow aggregate aborted")

// flowAggSem gates AggregateFlows / AggregateFlowBytes calls of the API.
var flowAggSem = make(chan struct{}, flowAggConcurrency)

// withFlowAggSlot runs fn holding one of the flowAggConcurrency slots.
func withFlowAggSlot[T any](fn func() (T, error)) (T, error) {
	flowAggSem <- struct{}{}
	defer func() { <-flowAggSem }()
	return fn()
}

type flowAggKey struct {
	exporter int64
	ifs      string
	table    queries.FlowTable
	from, to int64
	step     time.Duration
}

type flowAggEntry struct {
	rows    []queries.FlowAgg
	expires time.Time
	added   time.Time
}

// flowAggCall is an in-flight AggregateFlows for one key: concurrent
// identical requests (the Flows tab fires six for one point and range) wait
// for it and share its read-only result.
type flowAggCall struct {
	done chan struct{}
	rows []queries.FlowAgg
	err  error
}

// flowAggCache caches AggregateFlows results per (exporter, sorted ifs,
// table, from, to, step) for 30 s (1m table) / 5 min (1h table); the window
// end moves with every flush, so keys roll over naturally. It is bounded by
// the total rows held (flowCacheMaxRows), and single-flights concurrent
// misses of the same key. Admin writes clear it. The zero value is ready to
// use; cached slices are read-only.
type flowAggCache struct {
	mu       sync.Mutex
	m        map[flowAggKey]flowAggEntry
	rows     int // rows held by m
	inflight map[flowAggKey]*flowAggCall
	gen      uint64 // bumped by clear: a query started before must not re-insert its rows
}

func (c *flowAggCache) get(k flowAggKey, now time.Time) ([]queries.FlowAgg, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(k, now)
}

func (c *flowAggCache) getLocked(k flowAggKey, now time.Time) ([]queries.FlowAgg, bool) {
	e, ok := c.m[k]
	if !ok {
		return nil, false
	}
	if now.After(e.expires) {
		c.deleteLocked(k)
		return nil, false
	}
	return e.rows, true
}

func (c *flowAggCache) deleteLocked(k flowAggKey) {
	if e, ok := c.m[k]; ok {
		c.rows -= len(e.rows)
		delete(c.m, k)
	}
}

func (c *flowAggCache) put(k flowAggKey, rows []queries.FlowAgg, ttl time.Duration, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(k, rows, ttl, now)
}

func (c *flowAggCache) putLocked(k flowAggKey, rows []queries.FlowAgg, ttl time.Duration, now time.Time) {
	if len(rows) > flowCacheMaxRows {
		return
	}
	if c.m == nil {
		c.m = make(map[flowAggKey]flowAggEntry)
	}
	c.deleteLocked(k)
	for key, e := range c.m {
		if now.After(e.expires) {
			c.deleteLocked(key)
		}
	}
	for len(c.m) > 0 && c.rows+len(rows) > flowCacheMaxRows {
		var oldest flowAggKey
		first := true
		for key, e := range c.m {
			if first || e.added.Before(c.m[oldest].added) {
				oldest, first = key, false
			}
		}
		c.deleteLocked(oldest)
	}
	rows = slices.Clip(rows)
	c.m[k] = flowAggEntry{rows: rows, expires: now.Add(ttl), added: now}
	c.rows += len(rows)
}

func (c *flowAggCache) clear() {
	c.mu.Lock()
	c.m = nil
	c.rows = 0
	c.gen++
	c.mu.Unlock()
}

// load returns the cached rows of k, or runs query once for all concurrent
// callers of the same key and caches its result (unless the cache was
// cleared meanwhile or the query failed).
func (c *flowAggCache) load(k flowAggKey, ttl time.Duration, query func() ([]queries.FlowAgg, error)) ([]queries.FlowAgg, error) {
	c.mu.Lock()
	if rows, ok := c.getLocked(k, time.Now()); ok {
		c.mu.Unlock()
		return rows, nil
	}
	if call, ok := c.inflight[k]; ok {
		c.mu.Unlock()
		<-call.done
		return call.rows, call.err
	}
	call := &flowAggCall{done: make(chan struct{})}
	if c.inflight == nil {
		c.inflight = map[flowAggKey]*flowAggCall{}
	}
	c.inflight[k] = call
	gen := c.gen
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inflight, k)
		if call.err == nil && c.gen == gen {
			c.putLocked(k, call.rows, ttl, time.Now())
		}
		c.mu.Unlock()
		close(call.done)
	}()
	call.err = errFlowAggAborted // replaced below unless query panics
	call.rows, call.err = query()
	return call.rows, call.err
}

// flowRows returns the point's rows (in_if or out_if in S) over [from, to)
// grouped by step (0 = one group), through the cache.
func (s *Server) flowRows(p queries.FlowPoint, rng flowRange, from, to time.Time, step time.Duration) ([]queries.FlowAgg, error) {
	ifs := slices.Clone(p.IfIndexes)
	slices.Sort(ifs)
	parts := make([]string, len(ifs))
	for i, v := range ifs {
		parts[i] = strconv.FormatUint(uint64(v), 10)
	}
	k := flowAggKey{exporter: p.ExporterID, ifs: strings.Join(parts, ","), table: rng.table,
		from: from.Unix(), to: to.Unix(), step: step}
	ttl := 30 * time.Second
	if rng.table == queries.Flows1h {
		ttl = 5 * time.Minute
	}
	return s.flowCache.load(k, ttl, func() ([]queries.FlowAgg, error) {
		return withFlowAggSlot(func() ([]queries.FlowAgg, error) {
			return queries.AggregateFlows(s.db, rng.table, p.ExporterID, ifs, from, to, step)
		})
	})
}

// flowBytes reads the point's per-(step bucket, in_if, out_if) byte totals
// over [from, to) — with endpoints when the point has row filters — uncached
// (see queries.AggregateFlowBytes).
func (s *Server) flowBytes(p queries.FlowPoint, fp flowview.Point, t queries.FlowTable, from, to time.Time,
	step time.Duration) ([]queries.FlowAgg, error) {
	withEndpoints := fp.LocalInternal || len(fp.ExcludeIPs) > 0
	return withFlowAggSlot(func() ([]queries.FlowAgg, error) {
		return queries.AggregateFlowBytes(s.db, t, p.ExporterID, p.IfIndexes, from, to, step, withEndpoints)
	})
}

// flowChanged is called after every admin write: the collector re-reads
// exporters / points at once and cached rows are dropped.
func (s *Server) flowChanged() {
	if s.flows != nil {
		s.flows.Reload()
	}
	s.flowCache.clear()
}

// ---------- status and per-request context ----------

// mergedFlowStatus is the collector's status with the flow_exporters table
// as the authority on which exporters exist and how they are configured:
// rows the collector has not (re)loaded yet — or every row when it is nil or
// disabled — come from flow.ExporterStatusFromRow, entries of deleted rows
// are dropped, and configuration fields are taken from the DB. Ordered by
// name, then id.
func (s *Server) mergedFlowStatus(now time.Time) (flow.Status, []queries.FlowExporter, error) {
	var st flow.Status
	if s.flows != nil {
		st = s.flows.Status()
	} else {
		st = (*flow.Collector)(nil).Status()
	}
	rows, err := queries.ListFlowExporters(s.db)
	if err != nil {
		return st, nil, err
	}
	live := make(map[int64]flow.ExporterStatus, len(st.Exporters))
	for _, e := range st.Exporters {
		live[e.ID] = e
	}
	out := make([]flow.ExporterStatus, 0, len(rows))
	for _, r := range rows {
		fromRow := flow.ExporterStatusFromRow(r, now)
		es, ok := live[r.ID]
		if !ok {
			out = append(out, fromRow)
			continue
		}
		es.Name, es.Address, es.Kind, es.DeviceID = fromRow.Name, fromRow.Address, fromRow.Kind, fromRow.DeviceID
		es.Auto, es.SamplingOverride, es.NATAddresses = fromRow.Auto, fromRow.SamplingOverride, fromRow.NATAddresses
		if es.Enabled != r.Enabled {
			es.Enabled = r.Enabled
			es.State = fromRow.State
			if r.Enabled && es.LastSeen != nil {
				es.State = flow.StateStale
				if now.Sub(*es.LastSeen) < 3*time.Minute {
					es.State = flow.StateOK
				}
			}
		}
		out = append(out, es)
	}
	slices.SortStableFunc(out, func(a, b flow.ExporterStatus) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	st.Exporters = out
	if st.Listen == nil {
		st.Listen = []string{}
	}
	if st.ListenErrors == nil {
		st.ListenErrors = []string{}
	}
	if st.Unknown == nil {
		st.Unknown = []flow.UnknownSender{}
	}
	return st, rows, nil
}

// flowEnv is what building FlowPointViews needs, read once per request.
type flowEnv struct {
	now       time.Time
	status    flow.Status
	exporters map[int64]queries.FlowExporter
	expStatus map[int64]flow.ExporterStatus
	ifaces    map[int64]map[uint32]queries.FlowIface
	devices   map[string]queries.Device
}

func (s *Server) loadFlowEnv(now time.Time) (*flowEnv, error) {
	st, rows, err := s.mergedFlowStatus(now)
	if err != nil {
		return nil, err
	}
	env := &flowEnv{now: now, status: st, exporters: make(map[int64]queries.FlowExporter, len(rows)),
		expStatus: make(map[int64]flow.ExporterStatus, len(rows)), ifaces: map[int64]map[uint32]queries.FlowIface{},
		devices: map[string]queries.Device{}}
	for _, r := range rows {
		env.exporters[r.ID] = r
	}
	for _, e := range st.Exporters {
		env.expStatus[e.ID] = e
	}
	ifs, err := queries.ListFlowIfaces(s.db, 0)
	if err != nil {
		return nil, err
	}
	for _, f := range ifs {
		m := env.ifaces[f.ExporterID]
		if m == nil {
			m = map[uint32]queries.FlowIface{}
			env.ifaces[f.ExporterID] = m
		}
		m[f.IfIndex] = f
	}
	devs, err := queries.ListDevices(s.db)
	if err != nil {
		return nil, err
	}
	for _, d := range devs {
		env.devices[d.ID] = d
	}
	return env, nil
}

// deviceName is a device's display name ("" when unknown).
func (env *flowEnv) deviceName(id string) string {
	if d, ok := env.devices[id]; ok {
		return deviceDisplayName(d)
	}
	return ""
}

// ifName is an exporter iface's name, or "if#N" when unnamed.
func (env *flowEnv) ifName(exporterID int64, idx uint32) string {
	if f, ok := env.ifaces[exporterID][idx]; ok && f.Name != "" {
		return f.Name
	}
	return "if#" + strconv.FormatUint(uint64(idx), 10)
}

// flowPointView is the FlowPointView JSON object (contract §9.1).
type flowPointView struct {
	ID              int64             `json:"id"`
	Name            string            `json:"name"`
	Kind            string            `json:"kind"`
	ExporterID      int64             `json:"exporter_id"`
	ExporterName    string            `json:"exporter_name"`
	ExporterKind    string            `json:"exporter_kind"`
	ExporterState   string            `json:"exporter_state"`
	Protocol        string            `json:"protocol"`
	IfIndexes       []uint32          `json:"if_indexes"`
	IfNames         []string          `json:"if_names"`
	Facing          string            `json:"facing"`
	PortSide        string            `json:"port_side"`
	DeviceID        *string           `json:"device_id"`
	DeviceName      string            `json:"device_name"`
	Iface           string            `json:"iface"`
	LocalInternal   bool              `json:"local_internal"`
	ExcludeAttach   []queries.PortRef `json:"exclude_attach"`
	Note            string            `json:"note"`
	Auto            bool              `json:"auto"`
	Enabled         bool              `json:"enabled"`
	SamplingRate    int               `json:"sampling_rate"`
	CoverageCapable bool              `json:"coverage_capable"`
	LastData        *time.Time        `json:"last_data"`
}

// coverageCapable reports whether a point represents a managed port whose
// Phase 1 counters it can be compared with.
func coverageCapable(p queries.FlowPoint) bool {
	return p.PortSide != "" && p.DeviceID != "" && p.Iface != ""
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// pointView renders a point with its exporter's and interfaces' details.
func (env *flowEnv) pointView(p queries.FlowPoint) flowPointView {
	e := env.exporters[p.ExporterID]
	es, ok := env.expStatus[p.ExporterID]
	if !ok {
		es = flow.ExporterStatusFromRow(e, env.now)
	}
	v := flowPointView{
		ID: p.ID, Name: p.Name, Kind: p.Kind, ExporterID: p.ExporterID, ExporterName: e.Name, ExporterKind: e.Kind,
		ExporterState: es.State, Protocol: es.Protocol, IfIndexes: slices.Clone(p.IfIndexes),
		IfNames: make([]string, 0, len(p.IfIndexes)), Facing: p.Facing, PortSide: p.PortSide,
		DeviceID: strPtr(p.DeviceID), DeviceName: env.deviceName(p.DeviceID), Iface: p.Iface,
		LocalInternal: p.LocalInternal, ExcludeAttach: p.ExcludeAttach, Note: p.Note, Auto: p.Auto,
		Enabled: p.Enabled, SamplingRate: max(es.SamplingRate, 1), CoverageCapable: coverageCapable(p),
	}
	if v.IfIndexes == nil {
		v.IfIndexes = []uint32{}
	}
	if v.ExcludeAttach == nil {
		v.ExcludeAttach = []queries.PortRef{}
	}
	var last time.Time
	for _, idx := range p.IfIndexes {
		v.IfNames = append(v.IfNames, env.ifName(p.ExporterID, idx))
		if f, ok := env.ifaces[p.ExporterID][idx]; ok && f.LastSeen.After(last) {
			last = f.LastSeen
		}
	}
	if !last.IsZero() {
		v.LastData = timePtr(last)
	}
	return v
}

// flowPointParam reads and validates the point query param (400 when
// missing or non-numeric), leaving the lookup (404) to the caller.
func flowPointParam(q url.Values) (int64, string) {
	v := q.Get("point")
	if v == "" {
		return 0, "point is required"
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, "invalid point"
	}
	return id, ""
}

// lookupFlowPoint loads a point, writing 404 / 500 itself.
func (s *Server) lookupFlowPoint(w http.ResponseWriter, id int64) (*queries.FlowPoint, bool) {
	p, err := queries.GetFlowPoint(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get point")
		return nil, false
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "point not found")
		return nil, false
	}
	return p, true
}

// ---------- endpoints ----------

// flowEndpoint is the Endpoint JSON object (contract §9.1).
type flowEndpoint struct {
	IP       string        `json:"ip"`
	Class    string        `json:"class"`
	Name     string        `json:"name"`
	MAC      string        `json:"mac"`
	Vendor   string        `json:"vendor"`
	DeviceID *string       `json:"device_id"`
	Attached *flowAttached `json:"attached"`
	NATOf    *string       `json:"nat_of"`
}

type flowAttached struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name"`
	Iface      string `json:"iface"`
}

// flowApp is the App JSON object.
type flowApp struct {
	Proto     uint8  `json:"proto"`
	ProtoName string `json:"proto_name"`
	Port      uint16 `json:"port"`
	Label     string `json:"label"`
}

func newFlowApp(proto uint8, port uint16) *flowApp {
	label, name := flowview.AppLabel(proto, port)
	return &flowApp{Proto: proto, ProtoName: name, Port: port, Label: label}
}

// flowDescriber classifies and names endpoints for one request.
type flowDescriber struct {
	env     *flowEnv
	topo    *trafficTopo
	base    *flowview.Plan
	plan    []queries.FlowPrefix
	devByIP map[netip.Addr]queries.Device
	expByIP map[netip.Addr]string
	natOf   map[netip.Addr]string
	macByIP map[netip.Addr]*queries.MACLookup
	ptr     map[netip.Addr]string
	resolve bool // PTR names for external endpoints (resolve=1 and flow_resolve_ptr=true)
}

// parseIP parses an address string, unmapped; invalid → false.
func parseIP(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// newFlowDescriber loads the address plan, prefix settings, the client cache
// (through the shared traffic topology) and the device / exporter address
// maps. resolve asks for PTR names (honoured only with flow_resolve_ptr).
func (s *Server) newFlowDescriber(env *flowEnv, resolve bool) (*flowDescriber, error) {
	t, err := s.trafficTopology()
	if err != nil {
		return nil, err
	}
	plan, err := queries.ListFlowPrefixes(s.db)
	if err != nil {
		return nil, err
	}
	setting := func(k string) string {
		v, _ := queries.GetSetting(s.db, k)
		return v
	}
	d := &flowDescriber{env: env, topo: t, plan: plan, devByIP: map[netip.Addr]queries.Device{},
		expByIP: map[netip.Addr]string{}, natOf: map[netip.Addr]string{}, macByIP: map[netip.Addr]*queries.MACLookup{},
		ptr: map[netip.Addr]string{}}
	if resolve {
		v, _ := strconv.ParseBool(strings.TrimSpace(setting("flow_resolve_ptr")))
		d.resolve = v && s.resolver != nil
	}
	for _, dev := range env.devices {
		if a, ok := parseIP(dev.Address); ok {
			d.devByIP[a] = dev
		}
	}
	exps := make([]queries.FlowExporter, 0, len(env.exporters))
	for _, e := range env.exporters {
		exps = append(exps, e)
	}
	slices.SortFunc(exps, func(a, b queries.FlowExporter) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	for _, e := range exps {
		if a, ok := parseIP(e.Address); ok {
			if _, dup := d.expByIP[a]; !dup {
				d.expByIP[a] = e.Name
			}
		}
		for _, a := range e.NATAddresses {
			if _, dup := d.natOf[a.Unmap()]; !dup && a.IsValid() {
				d.natOf[a.Unmap()] = e.Name
			}
		}
	}
	known := map[netip.Addr]bool{}
	for _, m := range t.macs {
		a, ok := parseIP(m.IPAddress)
		if !ok {
			continue
		}
		known[a] = true
		cur := d.macByIP[a]
		if cur == nil || betterMACRow(m, cur) {
			d.macByIP[a] = m
		}
	}
	d.base = flowview.NewPlan(plan, flowview.ParsePrefixList(setting("flow_internal_prefixes")),
		flowview.ParsePrefixList(setting("flow_external_prefixes")), known, nil)
	return d, nil
}

// betterMACRow prefers a row with a host name, then the most recently
// updated one (contract §7.3), then the lower MAC for determinism.
func betterMACRow(a, b *queries.MACLookup) bool {
	if (a.HostName != "") != (b.HostName != "") {
		return a.HostName != ""
	}
	if !a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.UpdatedAt.After(b.UpdatedAt)
	}
	return a.MACAddress < b.MACAddress
}

// selfAddrs are an exporter's own addresses: its source address, its
// device's management address and host-length plan rows of that device.
func (d *flowDescriber) selfAddrs(exporterID int64) map[netip.Addr]bool {
	e := d.env.exporters[exporterID]
	self := map[netip.Addr]bool{}
	if a, ok := parseIP(e.Address); ok {
		self[a] = true
	}
	if e.DeviceID == "" {
		return self
	}
	if dev, ok := d.env.devices[e.DeviceID]; ok {
		if a, ok := parseIP(dev.Address); ok {
			self[a] = true
		}
	}
	for _, r := range d.plan {
		if r.DeviceID == e.DeviceID && r.Prefix.IsSingleIP() {
			self[r.Prefix.Addr().Unmap()] = true
		}
	}
	return self
}

// planFor is the classifier of a point: the base plan with the point's
// exporter as "self".
func (d *flowDescriber) planFor(p queries.FlowPoint) *flowview.Plan {
	return d.base.WithSelf(d.selfAddrs(p.ExporterID))
}

// point resolves a point's exclude_attach into ExcludeIPs: the IPs of every
// MAC in a fresh FDB row of the listed ports (contract §6.3).
func (d *flowDescriber) point(p queries.FlowPoint) flowview.Point {
	fp := flowview.Point{FlowPoint: p}
	if len(p.ExcludeAttach) == 0 {
		return fp
	}
	fp.ExcludeIPs = map[netip.Addr]bool{}
	for _, ref := range p.ExcludeAttach {
		for _, h := range d.topo.hosts[ifaceKey{ref.DeviceID, ref.Iface}] {
			if m := d.topo.macs[h.MACAddress]; m != nil {
				if a, ok := parseIP(m.IPAddress); ok {
					fp.ExcludeIPs[a] = true
				}
			}
		}
	}
	return fp
}

// attachment is where a local endpoint's MAC attaches (Phase 1).
func (d *flowDescriber) attachment(a netip.Addr) (deviceID, iface, deviceName string, ok bool) {
	m := d.macByIP[a.Unmap()]
	if m == nil {
		return "", "", "", false
	}
	dev, port, ok := d.topo.graph.AttachmentOf(strings.ToUpper(m.MACAddress))
	if !ok {
		return "", "", "", false
	}
	return dev, port, d.topo.name(dev), true
}

// describe builds the Endpoint object of a (valid) address under plan pl
// (contract §7.3).
func (d *flowDescriber) describe(a netip.Addr, pl *flowview.Plan) *flowEndpoint {
	a = a.Unmap()
	ep := &flowEndpoint{IP: a.String(), Class: pl.Class(a)}
	if n, ok := d.natOf[a]; ok {
		ep.NATOf = &n
	}
	if dev, ok := d.devByIP[a]; ok {
		ep.DeviceID = &dev.ID
		ep.Name = deviceDisplayName(dev)
		return ep
	}
	if n, ok := d.expByIP[a]; ok {
		ep.Name = n
		return ep
	}
	if m := d.macByIP[a]; m != nil {
		ep.MAC = strings.ToUpper(m.MACAddress)
		ep.Vendor, _ = macvendor.Describe(ep.MAC)
		switch {
		case m.HostName != "":
			ep.Name = m.HostName
		case m.DNSName != "":
			ep.Name = m.DNSName
		default:
			ep.Name = ep.Vendor
		}
		if dev, port, name, ok := d.attachment(a); ok {
			ep.Attached = &flowAttached{DeviceID: dev, DeviceName: name, Iface: port}
		}
	}
	if ep.Name == "" {
		ep.Name = d.ptr[a]
	}
	return ep
}

// ptrDeadline bounds the reverse lookups of one request.
const ptrDeadline = 1500 * time.Millisecond

// resolvePTR looks up PTR names of the external addresses among ips (only
// when enabled), concurrently and within ptrDeadline in total; lookups that
// miss the deadline finish in the background into the resolver's cache.
func (d *flowDescriber) resolvePTR(s *Server, ips []netip.Addr, pl *flowview.Plan) {
	if !d.resolve || s.resolver == nil || len(ips) == 0 {
		return
	}
	var todo []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, a := range ips {
		a = a.Unmap()
		if !a.IsValid() || seen[a] || pl.Class(a) != flowview.ClassExternal {
			continue
		}
		seen[a] = true
		if _, dev := d.devByIP[a]; dev {
			continue
		}
		if _, exp := d.expByIP[a]; exp {
			continue
		}
		if m := d.macByIP[a]; m != nil && (m.HostName != "" || m.DNSName != "") {
			continue
		}
		todo = append(todo, a)
	}
	if len(todo) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ptrDeadline)
	defer cancel()
	var mu sync.Mutex
	found := map[netip.Addr]string{}
	sem := make(chan struct{}, 16)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, a := range todo {
		wg.Add(1)
		go func(a netip.Addr) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			if name := s.resolver.ResolveIP(a.String()); name != "" {
				mu.Lock()
				found[a] = name
				mu.Unlock()
			}
		}(a)
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	mu.Lock()
	for a, n := range found {
		d.ptr[a] = n
	}
	mu.Unlock()
}

// ---------- rows ----------

// flowRowJSON is the FlowRow JSON object (contract §9.1).
type flowRowJSON struct {
	Rank    int           `json:"rank"`
	Key     string        `json:"key"`
	Src     *flowEndpoint `json:"src"`
	Dst     *flowEndpoint `json:"dst"`
	App     *flowApp      `json:"app"`
	Bytes   int64         `json:"bytes"`
	Packets int64         `json:"packets"`
	Flows   int64         `json:"flows"`
	AvgBps  int64         `json:"avg_bps"`
	Share   float64       `json:"share"`
}

// flowOtherJSON is the FlowOther JSON object.
type flowOtherJSON struct {
	Bytes   int64   `json:"bytes"`
	Packets int64   `json:"packets"`
	Flows   int64   `json:"flows"`
	AvgBps  int64   `json:"avg_bps"`
	Share   float64 `json:"share"`
}

// shareOf is bytes/total rounded to 4 decimals (0 when total is 0).
func shareOf(bytes, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(float64(bytes)/float64(total)*1e4) / 1e4
}

// groupEndpoints lists the addresses the rows of a grouping will describe
// (for the PTR pass).
func groupEndpoints(top []flowview.GroupRow) []netip.Addr {
	var out []netip.Addr
	for _, g := range top {
		if g.Src.IsValid() {
			out = append(out, g.Src)
		}
		if g.Dst.IsValid() {
			out = append(out, g.Dst)
		}
	}
	return out
}

// renderGroups converts grouped rows into FlowRow / FlowOther objects;
// field presence follows the grouping (contract §9.1).
func (d *flowDescriber) renderGroups(top []flowview.GroupRow, other flowview.GroupRow, total int64, by string,
	secs float64, pl *flowview.Plan) ([]flowRowJSON, flowOtherJSON) {
	rows := make([]flowRowJSON, 0, len(top))
	for i, g := range top {
		r := flowRowJSON{Rank: i + 1, Key: g.Key, Bytes: g.Bytes, Packets: g.Packets, Flows: g.Flows,
			AvgBps: bpsOver(g.Bytes, secs), Share: shareOf(g.Bytes, total)}
		switch by {
		case flowview.GroupSrc:
			r.Src = d.describe(g.Src, pl)
		case flowview.GroupDst:
			r.Dst = d.describe(g.Dst, pl)
		case flowview.GroupPair:
			r.Src, r.Dst = d.describe(g.Src, pl), d.describe(g.Dst, pl)
		case flowview.GroupConv:
			r.Src, r.Dst = d.describe(g.Src, pl), d.describe(g.Dst, pl)
			r.App = newFlowApp(g.Proto, g.Port)
		case flowview.GroupApp:
			r.App = newFlowApp(g.Proto, g.Port)
		}
		rows = append(rows, r)
	}
	return rows, flowOtherJSON{Bytes: other.Bytes, Packets: other.Packets, Flows: other.Flows,
		AvgBps: bpsOver(other.Bytes, secs), Share: shareOf(other.Bytes, total)}
}

// ---------- coverage vs Phase 1 counters ----------

// flowPortCoverage is the flow vs port-counter comparison of one point over
// a window (contract §6.6).
type flowPortCoverage struct {
	FlowDown, FlowUp       int64
	CounterDown, CounterUp int64
	Down, Up               *float64
}

// flowCoverageJSON is the /flows/port coverage object.
type flowCoverageJSON struct {
	Download         *float64 `json:"download"`
	Upload           *float64 `json:"upload"`
	FlowDownBytes    int64    `json:"flow_down_bytes"`
	FlowUpBytes      int64    `json:"flow_up_bytes"`
	CounterDownBytes int64    `json:"counter_down_bytes"`
	CounterUpBytes   int64    `json:"counter_up_bytes"`
}

func (c flowPortCoverage) json() flowCoverageJSON {
	return flowCoverageJSON{Download: c.Down, Upload: c.Up, FlowDownBytes: c.FlowDown, FlowUpBytes: c.FlowUp,
		CounterDownBytes: c.CounterDown, CounterUpBytes: c.CounterUp}
}

func ratioPtr(flowBytes, counterBytes int64) *float64 {
	if counterBytes <= 0 {
		return nil
	}
	r := math.Round(float64(flowBytes)/float64(counterBytes)*1e4) / 1e4
	return &r
}

// portCoverage compares the point's flow bytes with its managed port's
// Phase 1 counters over the native buckets both covered (the exporter's meta
// buckets ∩ the fleet-wide port-stats buckets; at 1h resolution only fully
// covered hours when there are any, so a partial hour never compares partial
// flow bytes with a full hour of counter bytes). rowsAll are the point's
// window rows grouped over the whole window (step 0). The flow bytes of the
// buckets outside the intersection are read per run of such buckets and
// subtracted: the row filters depend only on a row's key, never on its
// bucket, so the difference is exact. Not capable → zeros and null ratios.
func (s *Server) portCoverage(p queries.FlowPoint, fp flowview.Point, pl *flowview.Plan, win flowWindow,
	rowsAll []queries.FlowAgg) (flowPortCoverage, error) {
	var c flowPortCoverage
	if !coverageCapable(p) || !win.covered() {
		return c, nil
	}
	present, err := queries.PortStatsPresentBuckets(s.db, win.rng.portTable, win.start, win.end)
	if err != nil {
		return c, err
	}
	counterBucket := make(map[int64]bool, len(present))
	for _, b := range present {
		counterBucket[b.Unix()] = true
	}
	both := make(map[int64]bool, len(win.buckets))
	for _, b := range win.buckets {
		if counterBucket[b.Unix()] {
			both[b.Unix()] = true
		}
	}
	if win.minutes != nil {
		full := make(map[int64]bool, len(both))
		for b := range both {
			if win.minutes[b] >= 60 {
				full[b] = true
			}
		}
		if len(full) > 0 {
			both = full
		}
	}
	if len(both) == 0 {
		return c, nil
	}
	c.FlowDown = flowview.Totals(flowview.Select(rowsAll, fp, flowview.Download, pl))
	c.FlowUp = flowview.Totals(flowview.Select(rowsAll, fp, flowview.Upload, pl))
	for _, run := range missingRuns(win.buckets, both, win.rng.native) {
		rows, err := s.flowBytes(p, fp, win.rng.table, run[0], run[1], 0)
		if err != nil {
			return c, err
		}
		down, up := flowview.SumByBucket(rows, fp, pl)
		for _, v := range down {
			c.FlowDown -= v
		}
		for _, v := range up {
			c.FlowUp -= v
		}
	}
	c.FlowDown, c.FlowUp = max(c.FlowDown, 0), max(c.FlowUp, 0)

	series, err := queries.GetPortStatsSeries(s.db, win.rng.portTable, p.DeviceID, p.Iface, win.start, win.end)
	if err != nil {
		return c, err
	}
	var rx, tx int64
	for _, r := range series {
		if both[r.Bucket.Unix()] {
			rx += r.RxBytes
			tx += r.TxBytes
		}
	}
	c.CounterDown, c.CounterUp = flowview.CounterBytes(p.Facing, p.PortSide, rx, tx)
	c.Down, c.Up = ratioPtr(c.FlowDown, c.CounterDown), ratioPtr(c.FlowUp, c.CounterUp)
	return c, nil
}

// missingRuns merges the buckets (ascending) that are not in keep into
// contiguous [from, to) ranges.
func missingRuns(buckets []time.Time, keep map[int64]bool, native time.Duration) [][2]time.Time {
	var runs [][2]time.Time
	for _, b := range buckets {
		if keep[b.Unix()] {
			continue
		}
		if n := len(runs); n > 0 && runs[n-1][1].Equal(b) {
			runs[n-1][1] = b.Add(native)
			continue
		}
		runs = append(runs, [2]time.Time{b, b.Add(native)})
	}
	return runs
}

// ---------- misc ----------

// isTrueParam reports a boolean query param switched on.
func isTrueParam(v string) bool { return v == "1" || strings.EqualFold(v, "true") }

// roundSeconds renders covered seconds as a whole number.
func roundSeconds(secs float64) int64 { return int64(math.Round(secs)) }

// adjacencies lists the physical up links of the role graph Suggest reads:
// peer and uplink ports, and downlinks to a single device, with a known far
// end (contract §6.5).
func adjacencies(g *topology.RoleGraph) []flowview.Adjacency {
	var out []flowview.Adjacency
	for _, r := range g.Roles() {
		switch r.Role {
		case topology.RolePeer, topology.RoleUplink, topology.RoleDownlink:
		default:
			continue
		}
		if r.NeighborCount > 1 || r.NeighborDeviceID == "" || r.NeighborIface == "" {
			continue
		}
		out = append(out, flowview.Adjacency{DevA: r.DeviceID, IfA: r.Iface, DevB: r.NeighborDeviceID, IfB: r.NeighborIface})
	}
	return out
}
