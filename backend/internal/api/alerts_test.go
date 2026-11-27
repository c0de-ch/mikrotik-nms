package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/alerts"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// fakeAlertTester records the channel a test notification went to.
type fakeAlertTester struct {
	targets []string
	err     error
}

func (f *fakeAlertTester) DeliverTo(ch queries.NotificationChannel, _ queries.Alert, event alerts.Event) error {
	f.targets = append(f.targets, string(event)+" "+ch.Target)
	return f.err
}

type alertsFixture struct {
	t      *testing.T
	s      *Server
	h      http.Handler
	tokens map[string]string
}

const alertsSecret = "alerts-test-secret-alerts-test-secret"

func newAlertsFixture(t *testing.T) *alertsFixture {
	t.Helper()
	db := newTestDB(t)
	cfg := &config.Config{JWTSecret: alertsSecret, AllowedOrigins: []string{"http://x"}}
	f := &alertsFixture{t: t, tokens: map[string]string{}}
	f.s = &Server{db: db, cfg: cfg}
	// The real router, so the role gates are the production ones.
	r := NewRouter(db, nil, cfg, nil, nil, nil, nil, nil, nil, nil)
	f.h = r
	for _, role := range []string{"admin", "viewer"} {
		if err := queries.CreateUser(db, &queries.User{ID: "u-" + role, Username: role, PasswordHash: "x", Role: role}); err != nil {
			t.Fatal(err)
		}
		pair, err := auth.GenerateTokenPair(alertsSecret, "u-"+role, role, role, 0, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		f.tokens[role] = pair.AccessToken
	}
	return f
}

func (f *alertsFixture) do(role, method, path string, body any, out any) int {
	f.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			f.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, "/api/v1"+path, &buf)
	if role != "" {
		req.Header.Set("Authorization", "Bearer "+f.tokens[role])
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if out != nil && rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			f.t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

type alertsBody struct {
	Alerts []struct {
		ID         int64  `json:"id"`
		Rule       string `json:"rule"`
		State      string `json:"state"`
		Severity   string `json:"severity"`
		DeviceName string `json:"device_name"`
		Summary    string `json:"summary"`
		RunbookURL string `json:"runbook_url"`
		Silenced   bool   `json:"silenced"`
		AckedBy    string `json:"acked_by"`
	} `json:"alerts"`
	Firing   int                    `json:"firing"`
	Acked    int                    `json:"acked"`
	Silences []queries.AlertSilence `json:"silences"`
	Rules    []string               `json:"rules"`
}

// Everyone may read the alerts; acknowledging and silencing are for admins.
// A silence shows on the alerts it covers and must expire.
func TestAlertsListAckAndSilence(t *testing.T) {
	f := newAlertsFixture(t)
	db := f.s.db
	if err := queries.CreateDevice(db, &queries.Device{ID: "d1", Identity: "sw01", Address: "192.0.2.1", Username: "u", APIPort: 8728, Status: "offline", Tags: "[]"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	payload := `{"summary":"sw01 (192.0.2.1) is not reachable"}`
	id, err := queries.InsertAlert(db, &queries.Alert{Rule: alerts.RuleDeviceUnreachable, Key: "d1", DeviceID: "d1",
		Severity: alerts.SeverityCritical, FirstAt: now, LastAt: now, Confirmed: true, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}

	if code := f.do("", http.MethodGet, "/alerts", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /alerts = %d, want 401", code)
	}
	var body alertsBody
	if code := f.do("viewer", http.MethodGet, "/alerts", nil, &body); code != http.StatusOK {
		t.Fatalf("viewer GET /alerts = %d", code)
	}
	if len(body.Alerts) != 1 || body.Firing != 1 || body.Alerts[0].DeviceName != "sw01" ||
		body.Alerts[0].Summary != "sw01 (192.0.2.1) is not reachable" || body.Alerts[0].RunbookURL != alerts.RunbookURL(alerts.RuleDeviceUnreachable) ||
		body.Alerts[0].Silenced || len(body.Rules) == 0 {
		t.Fatalf("alerts = %+v", body)
	}

	ackPath := "/alerts/" + itoa(id) + "/ack"
	if code := f.do("viewer", http.MethodPost, ackPath, nil, nil); code != http.StatusForbidden {
		t.Fatalf("viewer ack = %d, want 403", code)
	}
	if code := f.do("admin", http.MethodPost, ackPath, nil, nil); code != http.StatusOK {
		t.Fatalf("admin ack = %d", code)
	}
	if code := f.do("admin", http.MethodPost, ackPath, nil, nil); code != http.StatusOK {
		t.Fatalf("acknowledging twice = %d, want 200 (already acknowledged)", code)
	}

	// Silences: validated, bounded, admin-only.
	for name, req := range map[string]map[string]any{
		"unknown scope":  {"scope": "everything", "hours": 1},
		"unknown device": {"scope": "device", "target": "nope", "hours": 1},
		"unknown rule":   {"scope": "rule", "target": "nope", "hours": 1},
		"no expiry":      {"scope": "device", "target": "d1"},
		"too long":       {"scope": "device", "target": "d1", "hours": maxSilenceHours + 1},
	} {
		if code := f.do("admin", http.MethodPost, "/alerts/silences", req, nil); code != http.StatusBadRequest {
			t.Errorf("silence with %s = %d, want 400", name, code)
		}
	}
	silence := map[string]any{"scope": "device", "target": "d1", "hours": 2, "reason": "replacing the PSU"}
	if code := f.do("viewer", http.MethodPost, "/alerts/silences", silence, nil); code != http.StatusForbidden {
		t.Fatalf("viewer silence = %d, want 403", code)
	}
	var created queries.AlertSilence
	if code := f.do("admin", http.MethodPost, "/alerts/silences", silence, &created); code != http.StatusCreated || created.ID == 0 || created.CreatedBy != "admin" {
		t.Fatalf("admin silence = %d %+v", code, created)
	}

	body = alertsBody{}
	f.do("viewer", http.MethodGet, "/alerts", nil, &body)
	if body.Firing != 0 || body.Acked != 1 || body.Alerts[0].State != queries.AlertAcked || body.Alerts[0].AckedBy != "admin" ||
		!body.Alerts[0].Silenced || len(body.Silences) != 1 || body.Silences[0].Reason != "replacing the PSU" {
		t.Fatalf("after ack + silence: %+v", body)
	}

	if code := f.do("admin", http.MethodDelete, "/alerts/silences/"+itoa(created.ID), nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete silence = %d", code)
	}
	// A resolved alert cannot be acknowledged.
	if err := queries.ResolveAlert(db, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	if code := f.do("admin", http.MethodPost, ackPath, nil, nil); code != http.StatusConflict {
		t.Fatalf("ack of a resolved alert = %d, want 409", code)
	}
}

// Channels are admin-only, validated, and their target is write-only: no
// response ever carries the address or URL again.
func TestNotificationChannels(t *testing.T) {
	f := newAlertsFixture(t)
	const hookURL = "https://hooks.example.com/services/T0/B0/very-secret"

	if code := f.do("viewer", http.MethodGet, "/alerts/channels", nil, nil); code != http.StatusForbidden {
		t.Fatalf("viewer GET channels = %d, want 403", code)
	}
	for name, req := range map[string]map[string]any{
		"bad kind":     {"kind": "pager", "target": "x"},
		"bad address":  {"kind": "smtp", "target": "not-an-address"},
		"bad url":      {"kind": "webhook", "target": "hooks.example.com"},
		"bad severity": {"kind": "smtp", "target": "ops@example.com", "min_severity": "fatal"},
	} {
		if code := f.do("admin", http.MethodPost, "/alerts/channels", req, nil); code != http.StatusBadRequest {
			t.Errorf("channel with %s = %d, want 400", name, code)
		}
	}

	rec := httptest.NewRecorder()
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]any{"name": "ops chat", "kind": "webhook", "target": hookURL})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/alerts/channels", &buf)
	req.Header.Set("Authorization", "Bearer "+f.tokens["admin"])
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "very-secret") {
		t.Fatalf("create = %d %s (the target must not be echoed)", rec.Code, rec.Body.String())
	}
	var created channelView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.TargetHint != "https://hooks.example.com/…" || created.MinSeverity != alerts.SeverityWarning || !created.Enabled {
		t.Fatalf("created = %+v", created)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/alerts/channels", nil)
	req.Header.Set("Authorization", "Bearer "+f.tokens["admin"])
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "very-secret") || !strings.Contains(rec.Body.String(), "ops chat") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	// An update without a target keeps the stored one.
	path := "/alerts/channels/" + itoa(created.ID)
	var updated channelView
	off := false
	if code := f.do("admin", http.MethodPut, path, map[string]any{"min_severity": "critical", "enabled": off}, &updated); code != http.StatusOK ||
		updated.MinSeverity != alerts.SeverityCritical || updated.Enabled || updated.TargetHint != created.TargetHint {
		t.Fatalf("update = %d %+v", code, updated)
	}
	if c, err := queries.GetNotificationChannel(f.s.db, created.ID); err != nil || c.Target != hookURL {
		t.Fatalf("stored target after an update without one = %q (%v)", c.Target, err)
	}
	if code := f.do("admin", http.MethodPut, path, map[string]any{"target": "ops@example.com"}, nil); code != http.StatusBadRequest {
		t.Fatalf("an e-mail address as a webhook target = %d, want 400", code)
	}

	if code := f.do("admin", http.MethodDelete, path, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d", code)
	}
	if code := f.do("admin", http.MethodPost, path+"/test", nil, nil); code != http.StatusNotFound {
		t.Fatalf("test of a deleted channel = %d, want 404", code)
	}
}

// The channel test delivers to that one channel — enabled or not — and
// reports a failure as an answer.
func TestChannelTest(t *testing.T) {
	db := newTestDB(t)
	tester := &fakeAlertTester{}
	s := &Server{db: db, cfg: &config.Config{}, alertNotifier: tester}
	id, err := queries.InsertNotificationChannel(db, &queries.NotificationChannel{
		Kind: alerts.ChannelSMTP, Target: "ops@example.com", MinSeverity: alerts.SeverityCritical, Enabled: false, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	call := func() (int, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/alerts/channels/"+itoa(id)+"/test", nil)
		req.SetPathValue("id", itoa(id))
		rec := httptest.NewRecorder()
		s.handleTestChannel(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, out := call()
	if code != http.StatusOK || out["ok"] != true || len(tester.targets) != 1 || tester.targets[0] != "test ops@example.com" {
		t.Fatalf("test = %d %v, delivered %v", code, out, tester.targets)
	}
	if msg, _ := out["message"].(string); strings.Contains(msg, "ops@example.com") {
		t.Fatalf("the answer shows the full target: %q", msg)
	}
	tester.err = errors.New("webhook hooks.example.com answered HTTP 403")
	if code, out := call(); code != http.StatusOK || out["ok"] != false || out["message"] != tester.err.Error() {
		t.Fatalf("failing test = %d %v", code, out)
	}
}
