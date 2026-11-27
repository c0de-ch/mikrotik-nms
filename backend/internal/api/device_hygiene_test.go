package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// fakePassRunner answers Request with a scripted error and records what was
// asked.
type fakePassRunner struct {
	err     error
	running string
	next    map[string]time.Time
	asked   []string // "kind by user"
}

func (f *fakePassRunner) Request(deviceID, kind, user string) (*queries.AuditRun, error) {
	f.asked = append(f.asked, kind+" by "+user)
	if f.err != nil {
		return nil, f.err
	}
	return &queries.AuditRun{ID: 7, DeviceID: deviceID, Kind: kind, Reason: poller.PassAdmin, RequestedBy: user,
		StartedAt: time.Now().UTC().Truncate(time.Second), Status: queries.AuditRunning}, nil
}
func (f *fakePassRunner) Running(string) string { return f.running }
func (f *fakePassRunner) NextAllowed(_, kind string, now time.Time) time.Time {
	if at, ok := f.next[kind]; ok {
		return at
	}
	return now
}

// hygieneCall runs a hygiene handler as the given role.
func hygieneCall(t *testing.T, h http.HandlerFunc, method, id, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/devices/"+id+"/hygiene", nil)
	req.SetPathValue("id", id)
	req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, &auth.ContextUser{ID: "u1", Username: "alice", Role: role}))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestAuditNowAndDiagnoseNow(t *testing.T) {
	s, d := pollingTestServer(t)
	s.cfg = &config.Config{Pollers: config.PollersLocal}
	runner := &fakePassRunner{}
	s.auditor = runner

	// Accepted: the run comes back, and the request is in the audit log.
	for kind, h := range map[string]http.HandlerFunc{queries.AuditKindAudit: s.handleAuditNow, queries.AuditKindDiagnose: s.handleDiagnoseNow} {
		rec := hygieneCall(t, h, http.MethodPost, d.ID, "admin")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s now: %d %s", kind, rec.Code, rec.Body.String())
		}
		var run map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil || run["kind"] != kind || run["status"] != "running" || run["requested_by"] != "alice" {
			t.Fatalf("%s now: body %s (%v)", kind, rec.Body.String(), err)
		}
	}
	if len(runner.asked) != 2 {
		t.Fatalf("asked = %v", runner.asked)
	}
	entries, _ := queries.ListAuditLog(s.db, d.ID, 10)
	actions := map[string]string{}
	for _, e := range entries {
		actions[e.Action] = e.Username
	}
	if actions["audit_requested"] != "alice" || actions["diagnose_requested"] != "alice" {
		t.Fatalf("audit log = %+v", entries)
	}

	// Every refusal says why, with a status the UI can tell apart; none of
	// them is written to the audit log as a run.
	before := len(entries)
	next := time.Now().Add(40 * time.Minute)
	for name, c := range map[string]struct {
		err    error
		status int
		says   string
	}{
		"within the hour":   {&poller.PassBudgetError{Kind: queries.AuditKindAudit, Next: next}, http.StatusTooManyRequests, "less than an hour ago"},
		"a pass is running": {poller.ErrPassRunning, http.StatusConflict, "already running"},
		"no session":        {poller.ErrPassNoSession, http.StatusConflict, "not reachable"},
		"paused":            {&poller.PassPausedError{Until: next}, http.StatusConflict, "polling paused until"},
		"polling off":       {routeros.ErrPollersOff, http.StatusServiceUnavailable, "polling is off"},
	} {
		runner.err = c.err
		rec := hygieneCall(t, s.handleAuditNow, http.MethodPost, d.ID, "admin")
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.says) {
			t.Errorf("%s: %d %s; want %d mentioning %q", name, rec.Code, rec.Body.String(), c.status, c.says)
		}
		if name == "within the hour" {
			if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" {
				t.Errorf("budget refusal without a Retry-After (%q)", ra)
			}
		}
	}
	if after, _ := queries.ListAuditLog(s.db, d.ID, 50); len(after) != before {
		t.Errorf("refused requests were written to the audit log (%d → %d rows)", before, len(after))
	}

	// A paused device is refused before the runner is even asked.
	runner.err, runner.asked = nil, nil
	if err := queries.PauseDevicePolling(s.db, d.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec := hygieneCall(t, s.handleDiagnoseNow, http.MethodPost, d.ID, "admin"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "polling paused until") || len(runner.asked) != 0 {
		t.Fatalf("paused device: %d %s (asked %v)", rec.Code, rec.Body.String(), runner.asked)
	}
	if rec := hygieneCall(t, s.handleAuditNow, http.MethodPost, "missing", "admin"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown device: %d", rec.Code)
	}

	// An instance without pollers has no auditor to ask.
	s.auditor = nil
	_ = queries.ResumeDevicePolling(s.db, d.ID)
	if rec := hygieneCall(t, s.handleAuditNow, http.MethodPost, d.ID, "admin"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no auditor: %d %s", rec.Code, rec.Body.String())
	}
}

// The passes are admin-only on the router; the hygiene view is for every
// logged-in user.
func TestHygieneRoutesAndRoles(t *testing.T) {
	db := newTestDB(t)
	cfg := &config.Config{JWTSecret: strings.Repeat("s", 40)}
	h := NewRouter(db, nil, cfg, nil, nil, nil, nil, nil, nil, nil)
	d := &queries.Device{ID: "dev1", Address: "192.0.2.9", Identity: "sw", Username: "u", PasswordEnc: "p", APIPort: 8728, Status: "online", Tags: "[]"}
	if err := queries.CreateDevice(db, d); err != nil {
		t.Fatal(err)
	}
	token := func(role string) string {
		t.Helper()
		u := &queries.User{ID: "id-" + role, Username: role, PasswordHash: "x", Role: role}
		if err := queries.CreateUser(db, u); err != nil {
			t.Fatal(err)
		}
		pair, err := auth.GenerateTokenPair(cfg.JWTSecret, u.ID, u.Username, u.Role, 0, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return pair.AccessToken
	}
	viewer, admin := token("viewer"), token("admin")
	do := func(method, path, tok string) int {
		req := httptest.NewRequest(method, "/api/v1"+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(http.MethodGet, "/devices/dev1/hygiene", viewer); code != http.StatusOK {
		t.Errorf("viewer reading the hygiene view: %d", code)
	}
	for _, path := range []string{"/devices/dev1/hygiene/audit", "/devices/dev1/hygiene/diagnose"} {
		if code := do(http.MethodPost, path, viewer); code != http.StatusForbidden {
			t.Errorf("viewer POST %s: %d, want 403", path, code)
		}
		// An admin gets past the role check (and is told polling is off here).
		if code := do(http.MethodPost, path, admin); code != http.StatusServiceUnavailable {
			t.Errorf("admin POST %s: %d, want 503 on an instance without pollers", path, code)
		}
	}
}

func TestDeviceHygieneView(t *testing.T) {
	s, d := pollingTestServer(t)
	s.cfg = &config.Config{Pollers: config.PollersLocal}
	now := time.Now().UTC().Truncate(time.Second)

	// Nothing yet: an empty, well-formed view.
	var view deviceHygiene
	rec := hygieneCall(t, s.handleDeviceHygiene, http.MethodGet, d.ID, "viewer")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty view: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Audit.Last != nil || view.Diagnose.Last != nil || view.Snapshot != nil || view.Findings == nil || len(view.Findings) != 0 ||
		view.Running != "" || view.Pollers != "local" {
		t.Fatalf("empty view = %+v", view)
	}

	// An audit that found graphing, a diagnose pass half an hour ago, and a
	// finding the audit does not own.
	auditID, _ := queries.InsertAuditRun(s.db, &queries.AuditRun{DeviceID: d.ID, Kind: queries.AuditKindAudit, Reason: poller.PassScheduled, StartedAt: now.Add(-3 * time.Hour)})
	_ = queries.FinishAuditRun(s.db, &queries.AuditRun{ID: auditID, FinishedAt: now.Add(-3*time.Hour + 6*time.Minute), Status: queries.AuditComplete, Reads: 19,
		Hash: "abc", Findings: `[{"kind":"graphing_enabled","severity":"critical","title":"Graphing is enabled on a weak device","detail":"interface rule interface=all"}]`})
	diagID, _ := queries.InsertAuditRun(s.db, &queries.AuditRun{DeviceID: d.ID, Kind: queries.AuditKindDiagnose, Reason: "cpu_high", AlertID: 5, StartedAt: now.Add(-30 * time.Minute)})
	_ = queries.FinishAuditRun(s.db, &queries.AuditRun{ID: diagID, FinishedAt: now.Add(-25 * time.Minute), Status: queries.AuditPartial, Reads: 11, FailedReads: 1, Detail: "1 of 11 reads failed (sniffer)"})
	_ = queries.InsertDeviceAudit(s.db, &queries.DeviceAudit{DeviceID: d.ID, TakenAt: now.Add(-3 * time.Hour), Hash: "abc", ROSVersion: "7.20.4", Snapshot: `{"schema":1,"sections":{}}`})
	for _, f := range []*queries.DeviceFinding{
		{DeviceID: d.ID, Kind: "snmp_on_weak_device", Severity: "info", Title: "SNMP is enabled on a weak device", Detail: "SNMP enabled"},
		{DeviceID: d.ID, Kind: "graphing_enabled", Severity: "critical", Title: "Graphing is enabled on a weak device", Detail: "interface rule interface=all",
			Explanation: "why", Fix: []queries.FindingFix{{Text: "Remove it:", Commands: []string{`/tool graphing interface remove [find interface="all"]`}}}},
	} {
		if err := queries.UpsertAuditFinding(s.db, f, now); err != nil {
			t.Fatal(err)
		}
	}
	_ = queries.UpsertDeviceFinding(s.db, &queries.DeviceFinding{DeviceID: d.ID, Kind: "parallel_api_poller", Severity: "warn", Detail: "user x from 198.51.100.2 via api"})
	s.auditor = &fakePassRunner{running: queries.AuditKindDiagnose, next: map[string]time.Time{queries.AuditKindDiagnose: now.Add(30 * time.Minute)}}

	rec = hygieneCall(t, s.handleDeviceHygiene, http.MethodGet, d.ID, "viewer")
	view = deviceHygiene{}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if view.Running != queries.AuditKindDiagnose {
		t.Errorf("running = %q", view.Running)
	}
	if a := view.Audit.Last; a == nil || a.Status != queries.AuditComplete || a.Reads != 19 || len(a.Found) != 1 || a.Found[0].Kind != "graphing_enabled" {
		t.Errorf("last audit = %+v", view.Audit.Last)
	}
	if view.Audit.AvailableAt != nil { // three hours ago: an admin may ask again
		t.Errorf("audit available_at = %v", view.Audit.AvailableAt)
	}
	if dg := view.Diagnose.Last; dg == nil || dg.Status != queries.AuditPartial || dg.Reason != "cpu_high" || dg.FailedReads != 1 || dg.Found == nil {
		t.Errorf("last diagnose = %+v", view.Diagnose.Last)
	}
	if view.Diagnose.AvailableAt == nil || !view.Diagnose.AvailableAt.Equal(now.Add(30*time.Minute)) {
		t.Errorf("diagnose available_at = %v", view.Diagnose.AvailableAt)
	}
	if view.Snapshot == nil || view.Snapshot.Hash != "abc" || view.Snapshot.ROSVersion != "7.20.4" {
		t.Errorf("snapshot = %+v", view.Snapshot)
	}
	// The audit's findings only, worst first, with their fix.
	if len(view.Findings) != 2 || view.Findings[0].Kind != "graphing_enabled" || view.Findings[1].Kind != "snmp_on_weak_device" ||
		len(view.Findings[0].Fix) != 1 || view.Findings[0].Fix[0].Commands[0] != `/tool graphing interface remove [find interface="all"]` {
		t.Errorf("findings = %+v", view.Findings)
	}
	// The stored configuration itself is not part of the view.
	if strings.Contains(rec.Body.String(), `"sections"`) {
		t.Errorf("the view returns the stored snapshot: %s", rec.Body.String())
	}

	if rec := hygieneCall(t, s.handleDeviceHygiene, http.MethodGet, "missing", "viewer"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown device: %d", rec.Code)
	}
}

// The Network Health page lists every device's hygiene findings, worst
// first, with the device's name.
func TestNetworkHealthListsHygieneFindings(t *testing.T) {
	s, d := pollingTestServer(t)
	now := time.Now().UTC()
	for _, f := range []*queries.DeviceFinding{
		{DeviceID: d.ID, Kind: "flow_control_enabled", Severity: "warn", Title: "802.3x flow control is enabled", Detail: "on 6 ports",
			Fix: []queries.FindingFix{{Text: "Switch it off.", Window: true}}},
		{DeviceID: d.ID, Kind: "graphing_enabled", Severity: "critical", Title: "Graphing is enabled on a weak device", Detail: "interface rule interface=all"},
	} {
		if err := queries.UpsertAuditFinding(s.db, f, now); err != nil {
			t.Fatal(err)
		}
	}
	_ = queries.UpsertDeviceFinding(s.db, &queries.DeviceFinding{DeviceID: d.ID, Kind: "login_failure", Severity: "info", Detail: "user y"})

	rec := httptest.NewRecorder()
	s.handleNetworkHealth(rec, httptest.NewRequest(http.MethodGet, "/api/v1/network-health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Findings []struct {
			Kind       string               `json:"kind"`
			Severity   string               `json:"severity"`
			DeviceName string               `json:"device_name"`
			Fix        []queries.FindingFix `json:"fix"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Findings) != 2 || body.Findings[0].Kind != "graphing_enabled" || body.Findings[0].DeviceName != "sw" ||
		body.Findings[1].Kind != "flow_control_enabled" || !body.Findings[1].Fix[0].Window {
		t.Fatalf("findings = %+v", body.Findings)
	}
}
