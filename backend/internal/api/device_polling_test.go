package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

func pollingTestServer(t *testing.T) (*Server, *queries.Device) {
	t.Helper()
	db := newTestDB(t)
	s := newTestServer(t, db, &fakeMailer{})
	d := &queries.Device{ID: uuid.NewString(), Address: "10.0.0.9", Identity: "sw", Username: "u", PasswordEnc: "p", APIPort: 8728, Status: "online", Tags: "[]"}
	if err := queries.CreateDevice(db, d); err != nil {
		t.Fatal(err)
	}
	return s, d
}

// call runs a handler as an authenticated admin on /devices/{id}/… and
// decodes the JSON body.
func call(t *testing.T, h http.HandlerFunc, method, id, body string, out any) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/devices/"+id+"/polling", strings.NewReader(body))
	req.SetPathValue("id", id)
	req = req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, &auth.ContextUser{ID: "u1", Username: "alice", Role: "admin"}))
	rec := httptest.NewRecorder()
	h(rec, req)
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
	return rec
}

func TestPauseAndResumePolling(t *testing.T) {
	s, d := pollingTestServer(t)

	var dev map[string]any
	rec := call(t, s.handlePausePolling, http.MethodPost, d.ID, `{"hours": 24}`, &dev)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", rec.Code, rec.Body.String())
	}
	if dev["polling_paused"] != true || dev["polling_paused_until"] == nil {
		t.Fatalf("paused device json: %v", dev)
	}
	until, err := time.Parse(time.RFC3339, dev["polling_paused_until"].(string))
	if err != nil || time.Until(until) < 23*time.Hour || time.Until(until) > 24*time.Hour {
		t.Fatalf("paused until %v (%v)", dev["polling_paused_until"], err)
	}

	for _, bad := range []string{`{"hours": 0}`, `{"hours": 9999}`, `{}`, `nope`} {
		if rec := call(t, s.handlePausePolling, http.MethodPost, d.ID, bad, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("pause %s: %d", bad, rec.Code)
		}
	}
	if rec := call(t, s.handlePausePolling, http.MethodPost, "missing", `{"hours": 1}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown device: %d", rec.Code)
	}

	rec = call(t, s.handleResumePolling, http.MethodPost, d.ID, "", &dev)
	if rec.Code != http.StatusOK || dev["polling_paused"] != false || dev["polling_paused_until"] != nil {
		t.Fatalf("resume: %d %v", rec.Code, dev)
	}
	// Resuming an unpaused device is a no-op that writes no audit row.
	call(t, s.handleResumePolling, http.MethodPost, d.ID, "", nil)

	var audit []queries.AuditEntry
	rec = call(t, s.handleDeviceAudit, http.MethodGet, d.ID, "", &audit)
	if rec.Code != http.StatusOK || len(audit) != 2 {
		t.Fatalf("audit: %d %+v", rec.Code, audit)
	}
	if audit[0].Action != "polling_resumed" || audit[1].Action != "polling_paused" || audit[1].Username != "alice" ||
		!strings.HasPrefix(audit[1].Detail, "24 h, until ") {
		t.Fatalf("audit rows: %+v", audit)
	}
}

func TestUpdatePollingProfileAndOverride(t *testing.T) {
	s, d := pollingTestServer(t)
	facts := devclass.ResourceFacts{Architecture: "arm", CPUCount: 1, CPUFrequencyMHz: 800, TotalMemoryBytes: 512 << 20}
	if err := queries.SetDeviceClass(s.db, d.ID, devclass.Classify(facts), facts, time.Now()); err != nil {
		t.Fatal(err)
	}

	var dev map[string]any
	rec := call(t, s.handleUpdatePolling, http.MethodPut, d.ID, `{"profile": "light", "class_override": "C1"}`, &dev)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	if dev["poll_profile"] != "light" || dev["class_override"] != "C1" || dev["effective_class"] != "C1" || dev["device_class"] != "C2" {
		t.Fatalf("after update: %v", dev)
	}

	// More capable than the probed C2: refused, nothing changes.
	rec = call(t, s.handleUpdatePolling, http.MethodPut, d.ID, `{"class_override": "C3"}`, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "gentler") {
		t.Fatalf("clamped override: %d %s", rec.Code, rec.Body.String())
	}
	// Omitted keys stay; null clears the override.
	rec = call(t, s.handleUpdatePolling, http.MethodPut, d.ID, `{"class_override": null}`, &dev)
	if rec.Code != http.StatusOK || dev["class_override"] != "" || dev["effective_class"] != "C2" || dev["poll_profile"] != "light" {
		t.Fatalf("clear override: %d %v", rec.Code, dev)
	}
	for _, bad := range []string{`{"profile": "heavy"}`, `{"class_override": "C0"}`, `{"class_override": "weak"}`, `{"profile": 3}`, `[]`} {
		if rec := call(t, s.handleUpdatePolling, http.MethodPut, d.ID, bad, nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	// An unprobed device is budgeted as C1, so only C1 may be set.
	fresh := &queries.Device{ID: uuid.NewString(), Address: "10.0.0.10", Username: "u", PasswordEnc: "p", APIPort: 8728, Status: "unknown", Tags: "[]"}
	if err := queries.CreateDevice(s.db, fresh); err != nil {
		t.Fatal(err)
	}
	if rec := call(t, s.handleUpdatePolling, http.MethodPut, fresh.ID, `{"class_override": "C2"}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unprobed override to C2: %d", rec.Code)
	}
	if rec := call(t, s.handleUpdatePolling, http.MethodPut, fresh.ID, `{"class_override": "C1"}`, &dev); rec.Code != http.StatusOK || dev["effective_class"] != "C1" {
		t.Fatalf("unprobed override to C1: %d %v", rec.Code, dev)
	}

	var audit []queries.AuditEntry
	call(t, s.handleDeviceAudit, http.MethodGet, d.ID, "", &audit)
	var actions []string
	for _, e := range audit {
		actions = append(actions, e.Action+":"+e.Detail)
	}
	want := []string{"class_override_changed:C1 → cleared", "class_override_changed:none → C1", "poll_profile_changed:auto → light"}
	if strings.Join(actions, "|") != strings.Join(want, "|") {
		t.Fatalf("audit = %v, want %v", actions, want)
	}

	// A bad limit is a 400; a small one truncates.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/devices/"+d.ID+"/audit?limit=x", nil)
	req.SetPathValue("id", d.ID)
	rec = httptest.NewRecorder()
	s.handleDeviceAudit(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=x: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/devices/"+d.ID+"/audit?limit=1", nil)
	req.SetPathValue("id", d.ID)
	rec = httptest.NewRecorder()
	s.handleDeviceAudit(rec, req)
	audit = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &audit); err != nil || len(audit) != 1 {
		t.Fatalf("limit=1: %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
}

// GET /devices/{id} carries the pollers' live view of the device: class,
// profile, pause and the next scheduled reads (none while paused).
func TestGetDeviceCarriesPollingView(t *testing.T) {
	s, d := pollingTestServer(t)
	get := func() map[string]any {
		t.Helper()
		var dev map[string]any
		if rec := call(t, s.handleGetDevice, http.MethodGet, d.ID, "", &dev); rec.Code != http.StatusOK {
			t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
		}
		p, ok := dev["polling"].(map[string]any)
		if !ok {
			t.Fatalf("no polling view in %v", dev)
		}
		return p
	}
	p := get()
	if p["class"] != "C1" || p["profile"] != "auto" || p["paused_until"] != nil {
		t.Fatalf("unprobed device view = %v", p)
	}
	if runs, ok := p["next_runs"].(map[string]any); !ok || len(runs) != 0 {
		t.Fatalf("next_runs = %v, want an empty object before any loop ran", p["next_runs"])
	}

	var dev map[string]any
	if rec := call(t, s.handlePausePolling, http.MethodPost, d.ID, `{"hours": 1}`, &dev); rec.Code != http.StatusOK {
		t.Fatalf("pause: %d", rec.Code)
	}
	if pv, ok := dev["polling"].(map[string]any); !ok || pv["paused_until"] == nil {
		t.Fatalf("the pause response carries the view: %v", dev["polling"])
	}
	if p = get(); p["paused_until"] == nil {
		t.Fatalf("paused view = %v", p)
	}

	// The device list stays lean: no polling view per row.
	rec := httptest.NewRecorder()
	s.handleListDevices(rec, httptest.NewRequest(http.MethodGet, "/api/v1/devices", nil))
	if strings.Contains(rec.Body.String(), `"polling"`) {
		t.Fatalf("list carries polling views: %s", rec.Body.String())
	}
}
