package api

import (
	"net/http"
	"net/netip"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/flowview"
)

// flowSankeyResponse is /traffic/sankey?source=flows (contract §9.12).
type flowSankeyResponse struct {
	Source       string                `json:"source"` // "flows"
	Estimated    bool                  `json:"estimated"`
	Direction    string                `json:"direction"`
	Range        string                `json:"range"`
	From         time.Time             `json:"from"`
	To           time.Time             `json:"to"`
	Point        flowPointView         `json:"point"`
	SamplingRate int64                 `json:"sampling_rate"`
	Coverage     *float64              `json:"coverage"`
	TotalBps     int64                 `json:"total_bps"`
	Nodes        []flowview.SankeyNode `json:"nodes"`
	Links        []flowview.SankeyLink `json:"links"`
}

// handleFlowSankey is the measured Sankey of one point: remote endpoints (or
// apps) → point → local hosts → their attachment ports for download, the
// reverse for upload. Query: point (required), range, dir, remote =
// host|app (default host), resolve (PTR names for the remote endpoints when
// flow_resolve_ptr is on).
func (s *Server) handleFlowSankey(w http.ResponseWriter, r *http.Request) {
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
	remote := q.Get("remote")
	if remote == "" {
		remote = flowview.RemoteHost
	}
	if remote != flowview.RemoteHost && remote != flowview.RemoteApp {
		writeError(w, http.StatusBadRequest, "invalid remote")
		return
	}
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
	cov, err := s.portCoverage(*p, fq.fp, fq.pl, fq.win, fq.rows)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read port stats")
		return
	}
	secs := fq.win.seconds()
	sel := flowview.Select(fq.rows, fq.fp, dir, fq.pl)

	if remote == flowview.RemoteHost {
		// PTR names only for the remote endpoints that become nodes.
		by := flowview.GroupSrc
		if dir == flowview.Upload {
			by = flowview.GroupDst
		}
		top, _, _ := flowview.Group(sel, by, 12)
		fq.desc.resolvePTR(s, groupEndpoints(top), fq.pl)
	}
	endpoint := func(a netip.Addr) (string, string) {
		ep := fq.desc.describe(a, fq.pl)
		return ep.Name, ep.Class
	}
	nodes, links := flowview.BuildSankey(sel, flowview.SankeyOptions{Dir: dir, Remote: remote, PointID: p.ID,
		PointName: p.Name, Seconds: secs}, endpoint, fq.desc.attachment)

	resp := flowSankeyResponse{Source: "flows", Direction: string(dir), Range: rng.key, From: fq.win.start,
		To: fq.win.end, Point: fq.env.pointView(*p), SamplingRate: max(sum.MaxSampling, 1),
		TotalBps: bpsOver(flowview.Totals(sel), secs), Nodes: nodes, Links: links}
	if dir == flowview.Upload {
		resp.Coverage = cov.Up
	} else {
		resp.Coverage = cov.Down
	}
	writeJSON(w, http.StatusOK, resp)
}
