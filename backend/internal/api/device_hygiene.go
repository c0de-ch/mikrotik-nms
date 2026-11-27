package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
)

// The hygiene audit and the diagnose pass on the device page: what the last
// passes found, when the next audit runs, and — for admins — "Audit now" and
// "Diagnose now". Reading the view costs no device a command; a pass is the
// poller's (poller.Auditor), paced and budgeted there.

// passRunner is the part of *poller.Auditor the API uses; tests substitute a
// fake. nil = this instance has no auditor (polling off, tests).
type passRunner interface {
	Request(deviceID, kind, user string) (*queries.AuditRun, error)
	Running(deviceID string) string
	NextAllowed(deviceID, kind string, now time.Time) time.Time
}

// passFinding is what a pass stored of a finding with its run.
type passFinding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// passView is one pass as the API returns it.
type passView struct {
	queries.AuditRun
	// Found is what this pass found (never nil).
	Found []passFinding `json:"found"`
}

func toPassView(r *queries.AuditRun) *passView {
	if r == nil {
		return nil
	}
	v := &passView{AuditRun: *r, Found: []passFinding{}}
	_ = json.Unmarshal([]byte(r.Findings), &v.Found)
	if v.Found == nil {
		v.Found = []passFinding{}
	}
	return v
}

// passState is the state of one kind of pass on a device.
type passState struct {
	// Last is the newest pass that judged something (complete or partial),
	// or the newest one at all when none did.
	Last *passView `json:"last"`
	// NextRun is when the next scheduled audit runs (audits only; nil while
	// none is scheduled: paused, offline, polling off).
	NextRun *time.Time `json:"next_run,omitempty"`
	// AvailableAt is when an admin may next ask for this pass; nil = now.
	AvailableAt *time.Time `json:"available_at"`
}

// snapshotInfo says which configuration snapshot is the device's current
// one. The snapshot itself is not returned.
type snapshotInfo struct {
	TakenAt    time.Time `json:"taken_at"`
	Hash       string    `json:"hash"`
	ROSVersion string    `json:"ros_version"`
}

// deviceHygiene is GET /devices/{id}/hygiene.
type deviceHygiene struct {
	// Pollers is this instance's polling mode: with "off" no pass ever runs.
	Pollers string `json:"pollers"`
	// Running is the kind of pass in progress ("" = none).
	Running  string        `json:"running"`
	Audit    passState     `json:"audit"`
	Diagnose passState     `json:"diagnose"`
	Snapshot *snapshotInfo `json:"snapshot"`
	// Findings are the device's hygiene findings, most severe first.
	Findings []queries.DeviceFinding `json:"findings"`
}

// lastPass returns a device's newest pass of a kind that judged something,
// else its newest pass at all.
func (s *Server) lastPass(deviceID, kind string) (*queries.AuditRun, error) {
	r, err := queries.LatestFinishedAuditRun(s.db, deviceID, kind)
	if err != nil || r != nil {
		return r, err
	}
	return queries.LatestAuditRun(s.db, deviceID, kind)
}

// availableAt is when an admin may next ask for a pass of kind (nil = now).
func (s *Server) availableAt(deviceID, kind string, now time.Time) *time.Time {
	var next time.Time
	if s.auditor != nil {
		next = s.auditor.NextAllowed(deviceID, kind, now)
	} else if last, err := queries.LatestAuditRun(s.db, deviceID, kind); err == nil && last != nil {
		next = last.StartedAt.Add(poller.PassBudget)
	}
	if !next.After(now) {
		return nil
	}
	next = next.UTC()
	return &next
}

// handleDeviceHygiene — GET /devices/{id}/hygiene
func (s *Server) handleDeviceHygiene(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.loadDevice(w, id); !ok {
		return
	}
	now := time.Now()
	out := deviceHygiene{Pollers: s.pollersMode(), Findings: []queries.DeviceFinding{}}
	if s.auditor != nil {
		out.Running = s.auditor.Running(id)
	}
	for _, k := range []struct {
		kind  string
		state *passState
	}{{queries.AuditKindAudit, &out.Audit}, {queries.AuditKindDiagnose, &out.Diagnose}} {
		last, err := s.lastPass(id, k.kind)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read the passes")
			return
		}
		k.state.Last = toPassView(last)
		k.state.AvailableAt = s.availableAt(id, k.kind, now)
	}
	if next, ok := poller.Schedule().NextRuns(id)["audit"]; ok {
		next = next.UTC().Truncate(time.Second)
		out.Audit.NextRun = &next
	}
	snap, err := queries.LatestDeviceAudit(s.db, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read the snapshot")
		return
	}
	if snap != nil {
		out.Snapshot = &snapshotInfo{TakenAt: snap.TakenAt, Hash: snap.Hash, ROSVersion: snap.ROSVersion}
	}
	findings, err := queries.ListAuditFindings(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list findings")
		return
	}
	for _, f := range findings { // already most severe first
		if f.DeviceID == id {
			out.Findings = append(out.Findings, f)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAuditNow — POST /devices/{id}/hygiene/audit
func (s *Server) handleAuditNow(w http.ResponseWriter, r *http.Request) {
	s.requestPass(w, r, queries.AuditKindAudit, "audit_requested")
}

// handleDiagnoseNow — POST /devices/{id}/hygiene/diagnose
func (s *Server) handleDiagnoseNow(w http.ResponseWriter, r *http.Request) {
	s.requestPass(w, r, queries.AuditKindDiagnose, "diagnose_requested")
}

// requestPass starts a pass an admin asked for. 202 with the run; 404 for an
// unknown device; 409 while the device's polling is paused, while a pass is
// already running on it or while it has no API session; 429 with Retry-After
// within an hour of its last pass of that kind; 503 while polling is off on
// this instance. An accepted request is written to audit_log.
func (s *Server) requestPass(w http.ResponseWriter, r *http.Request, kind, action string) {
	id := r.PathValue("id")
	if _, ok := s.loadDevice(w, id); !ok {
		return
	}
	if s.writePollingPaused(w, id) {
		return
	}
	if s.auditor == nil {
		writeError(w, http.StatusServiceUnavailable,
			"polling is off on this instance (MIKROTIK_NMS_POLLERS=off) — devices are not contacted")
		return
	}
	who := ""
	if u := auth.UserFromContext(r.Context()); u != nil {
		who = u.Username
	}
	run, err := s.auditor.Request(id, kind, who)
	var budget *poller.PassBudgetError
	var paused *poller.PassPausedError
	switch {
	case err == nil:
	case writeDialPolicyError(w, err):
		return
	case errors.As(err, &budget):
		wait := int(time.Until(budget.Next).Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(max(wait, 1)))
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	case errors.As(err, &paused), errors.Is(err, poller.ErrPassRunning), errors.Is(err, poller.ErrPassNoSession):
		writeError(w, http.StatusConflict, err.Error())
		return
	default:
		writeError(w, http.StatusInternalServerError, "failed to start the pass")
		return
	}
	s.audit(r, action, id, "")
	writeJSON(w, http.StatusAccepted, toPassView(run))
}
