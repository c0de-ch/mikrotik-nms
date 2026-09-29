package api

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowview"
)

// ---------- /flows/top ----------

type flowTopResponse struct {
	Point        flowPointView `json:"point"`
	Range        string        `json:"range"`
	Dir          string        `json:"dir"`
	Group        string        `json:"group"`
	Resolution   string        `json:"resolution"`
	From         time.Time     `json:"from"`
	To           time.Time     `json:"to"`
	CoverageFrom *time.Time    `json:"coverage_from"`
	Seconds      int64         `json:"seconds"`
	TotalBytes   int64         `json:"total_bytes"`
	TotalBps     int64         `json:"total_bps"`
	SamplingRate int64         `json:"sampling_rate"`
	Overflow     int64         `json:"overflow"`
	Rows         []flowRowJSON `json:"rows"`
	Other        flowOtherJSON `json:"other"`
}

const (
	flowTopDefaultLimit  = 20
	flowTopMaxLimit      = 200
	flowPortDefaultLimit = 10
	flowPortMaxLimit     = 50
)

// limitParam parses limit (non-numeric → def), clamped to 1..maxLimit.
func limitParam(v string, def, maxLimit int) int {
	if n, err := strconv.Atoi(v); err == nil {
		return min(max(n, 1), maxLimit)
	}
	return def
}

// flowQuery is what every measured view of one point needs.
type flowQuery struct {
	env  *flowEnv
	desc *flowDescriber
	p    queries.FlowPoint
	fp   flowview.Point
	pl   *flowview.Plan
	win  flowWindow
	rows []queries.FlowAgg // whole window, step 0
}

// loadFlowQuery resolves the window of point p over rng and reads its rows.
func (s *Server) loadFlowQuery(p queries.FlowPoint, rng flowRange, resolve bool) (*flowQuery, error) {
	now := time.Now()
	env, err := s.loadFlowEnv(now)
	if err != nil {
		return nil, err
	}
	desc, err := s.newFlowDescriber(env, resolve)
	if err != nil {
		return nil, err
	}
	q := &flowQuery{env: env, desc: desc, p: p, fp: desc.point(p), pl: desc.planFor(p)}
	if q.win, err = s.resolveFlowWindow(rng, p.ExporterID, now); err != nil {
		return nil, err
	}
	if q.win.covered() {
		if q.rows, err = s.flowRows(p, rng, q.win.start, q.win.end, 0); err != nil {
			return nil, err
		}
	}
	return q, nil
}

// handleFlowTop ranks one point's traffic. Query: point (required), range
// (default 1h), dir = download|upload, group = src|dst|pair|conv|app
// (default pair), limit (1..200, default 20), resolve (PTR names for
// external endpoints when flow_resolve_ptr is on).
func (s *Server) handleFlowTop(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, msg := flowPointParam(q)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	rng, ok := parseFlowRange(q.Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid range")
		return
	}
	dir, ok := parseFlowDir(q.Get("dir"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid dir")
		return
	}
	group := q.Get("group")
	if group == "" {
		group = flowview.GroupPair
	}
	if !flowview.ValidGroup(group) {
		writeError(w, http.StatusBadRequest, "invalid group")
		return
	}
	limit := limitParam(q.Get("limit"), flowTopDefaultLimit, flowTopMaxLimit)
	p, ok := s.lookupFlowPoint(w, id)
	if !ok {
		return
	}
	fq, err := s.loadFlowQuery(*p, rng, isTrueParam(q.Get("resolve")))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read flows")
		return
	}
	sum, err := queries.FlowMetaSummary(s.db, rng.table, p.ExporterID, fq.win.start, fq.win.end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read flows")
		return
	}
	secs := fq.win.seconds()
	top, other, total := flowview.Group(flowview.Select(fq.rows, fq.fp, dir, fq.pl), group, limit)
	fq.desc.resolvePTR(s, groupEndpoints(top), fq.pl)
	rows, oth := fq.desc.renderGroups(top, other, total, group, secs, fq.pl)
	writeJSON(w, http.StatusOK, flowTopResponse{
		Point: fq.env.pointView(*p), Range: rng.key, Dir: string(dir), Group: group, Resolution: rng.resolution,
		From: fq.win.start, To: fq.win.end, CoverageFrom: fq.win.coverageFrom(), Seconds: roundSeconds(secs),
		TotalBytes: total, TotalBps: bpsOver(total, secs), SamplingRate: max(sum.MaxSampling, 1),
		Overflow: sum.Overflow, Rows: rows, Other: oth,
	})
}

// ---------- /flows/port ----------

type flowPortDir struct {
	Rows       []flowRowJSON `json:"rows"`
	TotalBytes int64         `json:"total_bytes"`
	Other      flowOtherJSON `json:"other"`
}

type flowPortResponse struct {
	Reason       string           `json:"reason"` // ok | no_point | collector_disabled
	Point        *flowPointView   `json:"point"`
	Alternatives []flowPointView  `json:"alternatives"`
	Range        string           `json:"range"`
	Resolution   string           `json:"resolution"`
	From         *time.Time       `json:"from"`
	To           *time.Time       `json:"to"`
	CoverageFrom *time.Time       `json:"coverage_from"`
	Seconds      int64            `json:"seconds"`
	Coverage     flowCoverageJSON `json:"coverage"`
	Download     flowPortDir      `json:"download"`
	Upload       flowPortDir      `json:"upload"`
}

func emptyPortDir() flowPortDir { return flowPortDir{Rows: []flowRowJSON{}} }

// kindRank orders native before derived for the port-detail point choice.
func kindRank(kind string) int {
	if kind == "native" {
		return 0
	}
	return 1
}

// handleFlowPort backs the port-detail "Top conversations" card. Query:
// device, iface (required), range, limit (1..50, default 10), point
// (optional: one of the points covering this port). The point is the
// enabled point whose managed port is (device, iface): native first, then
// derived, then lowest id; the others are listed as alternatives.
func (s *Server) handleFlowPort(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rng, ok := parseFlowRange(q.Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid range")
		return
	}
	limit := limitParam(q.Get("limit"), flowPortDefaultLimit, flowPortMaxLimit)
	var want int64
	if v := q.Get("point"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid point")
			return
		}
		want = id
	}
	deviceID, iface, ok := s.portParams(w, r)
	if !ok {
		return
	}
	all, err := queries.ListFlowPoints(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list points")
		return
	}
	var match []queries.FlowPoint
	for _, p := range all {
		if p.Enabled && p.DeviceID == deviceID && p.Iface == iface {
			match = append(match, p)
		}
	}
	slices.SortFunc(match, func(a, b queries.FlowPoint) int {
		return cmp.Or(cmp.Compare(kindRank(a.Kind), kindRank(b.Kind)), cmp.Compare(a.ID, b.ID))
	})
	chosen := 0
	if want != 0 {
		chosen = slices.IndexFunc(match, func(p queries.FlowPoint) bool { return p.ID == want })
		if chosen < 0 {
			writeError(w, http.StatusBadRequest, "point does not cover this port")
			return
		}
	}

	resp := flowPortResponse{Range: rng.key, Resolution: rng.resolution, Alternatives: []flowPointView{},
		Download: emptyPortDir(), Upload: emptyPortDir()}
	if len(match) == 0 {
		resp.Reason = "no_point"
		if s.flows == nil || !s.flows.Enabled() {
			resp.Reason = "collector_disabled"
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p := match[chosen]
	fq, err := s.loadFlowQuery(p, rng, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read flows")
		return
	}
	resp.Reason = "ok"
	view := fq.env.pointView(p)
	resp.Point = &view
	for i, alt := range match {
		if i != chosen {
			resp.Alternatives = append(resp.Alternatives, fq.env.pointView(alt))
		}
	}
	secs := fq.win.seconds()
	resp.From, resp.To, resp.CoverageFrom = timePtr(fq.win.start), timePtr(fq.win.end), fq.win.coverageFrom()
	resp.Seconds = roundSeconds(secs)
	for _, d := range []struct {
		dir flowview.Dir
		out *flowPortDir
	}{{flowview.Download, &resp.Download}, {flowview.Upload, &resp.Upload}} {
		top, other, total := flowview.Group(flowview.Select(fq.rows, fq.fp, d.dir, fq.pl), flowview.GroupConv, limit)
		rows, oth := fq.desc.renderGroups(top, other, total, flowview.GroupConv, secs, fq.pl)
		*d.out = flowPortDir{Rows: rows, TotalBytes: total, Other: oth}
	}
	cov, err := s.portCoverage(p, fq.fp, fq.pl, fq.win, fq.rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read port stats")
		return
	}
	resp.Coverage = cov.json()
	writeJSON(w, http.StatusOK, resp)
}

// ---------- /flows/coverage ----------

type flowCoveragePoint struct {
	TS             time.Time `json:"ts"`
	FlowDownBps    *int64    `json:"flow_down_bps"`
	FlowUpBps      *int64    `json:"flow_up_bps"`
	CounterDownBps *int64    `json:"counter_down_bps"`
	CounterUpBps   *int64    `json:"counter_up_bps"`
}

type flowRatio struct {
	Download *float64 `json:"download"`
	Upload   *float64 `json:"upload"`
}

type flowCoverageResponse struct {
	Point        flowPointView       `json:"point"`
	Range        string              `json:"range"`
	StepSeconds  int                 `json:"step_seconds"`
	From         time.Time           `json:"from"`
	To           time.Time           `json:"to"`
	CoverageFrom *time.Time          `json:"coverage_from"`
	Points       []flowCoveragePoint `json:"points"`
	Ratio        flowRatio           `json:"ratio"`
}

func int64Ptr(v int64) *int64 { return &v }

// handleFlowCoverage returns one point's flow bit rates per step next to its
// managed port's Phase 1 counter rates (the flow vs counter overlay), plus
// the overall ratio per direction. Query: point (required), range. Flow
// values are null for steps without flow meta rows; counter values are null
// for steps without Phase 1 buckets and for points without a managed port.
func (s *Server) handleFlowCoverage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, msg := flowPointParam(q)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	rng, ok := parseFlowRange(q.Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid range")
		return
	}
	p, ok := s.lookupFlowPoint(w, id)
	if !ok {
		return
	}
	fq, err := s.loadFlowQuery(*p, rng, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read flows")
		return
	}
	win := fq.win
	resp := flowCoverageResponse{Point: fq.env.pointView(*p), Range: rng.key, StepSeconds: int(rng.step / time.Second),
		From: win.start, To: win.end, CoverageFrom: win.coverageFrom(), Points: []flowCoveragePoint{}}
	if !win.covered() {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	from := win.pointsFrom(rng.step)
	// Per-step totals only need (step, in_if, out_if) — plus the endpoints
	// when the point filters rows — not every conversation of every step.
	stepRows, err := s.flowBytes(*p, fq.fp, rng.table, from, win.end, rng.step)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read flows")
		return
	}
	down, up := flowview.SumByBucket(stepRows, fq.fp, fq.pl)

	var counters []queries.SeriesPoint
	if coverageCapable(*p) {
		present, err := queries.PortStatsPresentBuckets(s.db, rng.portTable, from, win.end)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read port stats")
			return
		}
		series, err := queries.GetPortStatsSeries(s.db, rng.portTable, p.DeviceID, p.Iface, from, win.end)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read port stats")
			return
		}
		counters = queries.DownsampleSeriesCovered(series, from, win.end, rng.step,
			queries.NewCoverage(rng.native, present, from))
	}

	for k, ts := 0, from; ts.Before(win.end); k, ts = k+1, ts.Add(rng.step) {
		pt := flowCoveragePoint{TS: ts.UTC()}
		if secs := win.cov.SecondsIn(ts, ts.Add(rng.step)); secs > 0 {
			pt.FlowDownBps = int64Ptr(bpsOver(down[ts.Unix()], secs))
			pt.FlowUpBps = int64Ptr(bpsOver(up[ts.Unix()], secs))
		}
		if k < len(counters) && !counters[k].Gap {
			d, u := flowview.CounterBytes(p.Facing, p.PortSide, counters[k].RxBps, counters[k].TxBps)
			pt.CounterDownBps, pt.CounterUpBps = int64Ptr(d), int64Ptr(u)
		}
		resp.Points = append(resp.Points, pt)
	}
	cov, err := s.portCoverage(*p, fq.fp, fq.pl, win, fq.rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read port stats")
		return
	}
	resp.Ratio = flowRatio{Download: cov.Down, Upload: cov.Up}
	writeJSON(w, http.StatusOK, resp)
}
