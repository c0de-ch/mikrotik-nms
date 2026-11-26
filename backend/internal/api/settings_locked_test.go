package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// A setting the environment fixes is shown with the value in force and
// marked locked, and saving the page — which sends every field back — does
// not write that value into the database. Other settings save as before.
func TestSettingsLockedByEnvironment(t *testing.T) {
	db := newTestDB(t)
	s := &Server{db: db, cfg: &config.Config{
		TopologyInterval: 5 * time.Minute, NetworkHealthInterval: time.Minute, RetentionDays: 7,
		EnvLocked: map[string]bool{"topology_interval": true},
	}}
	r := chi.NewRouter()
	r.Get("/settings", s.handleGetSettings)
	r.Put("/settings", s.handleUpdateSettings)
	if err := queries.SetSetting(db, "topology_interval", "60"); err != nil {
		t.Fatal(err)
	}

	get := func() map[string]string {
		t.Helper()
		rec := doJSON(t, r, http.MethodGet, "/settings", "")
		var m map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("GET /settings = %d %s", rec.Code, rec.Body.String())
		}
		return m
	}
	m := get()
	if m["topology_interval"] != "300" {
		t.Errorf("topology_interval shown = %q, want the env value in force (300)", m["topology_interval"])
	}
	if m["env_locked"] != "topology_interval" {
		t.Errorf("env_locked = %q, want topology_interval", m["env_locked"])
	}

	// The page saves everything it holds, the locked field and the
	// synthetic key included.
	rec := doJSON(t, r, http.MethodPut, "/settings",
		`{"topology_interval":"300","env_locked":"topology_interval","retention_days":"30","network_health_interval":"120"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /settings = %d %s", rec.Code, rec.Body.String())
	}
	var saved map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &saved)
	if saved["topology_interval"] != "300" || saved["env_locked"] != "topology_interval" {
		t.Errorf("PUT response = topology_interval %q env_locked %q, want the same view as GET", saved["topology_interval"], saved["env_locked"])
	}
	if v, _ := queries.GetSetting(db, "topology_interval"); v != "60" {
		t.Errorf("stored topology_interval = %q: a locked field was written", v)
	}
	if v, err := queries.GetSetting(db, "env_locked"); err == nil && v != "" {
		t.Errorf("the synthetic env_locked key was stored: %q", v)
	}
	if v, _ := queries.GetSetting(db, "retention_days"); v != "30" {
		t.Errorf("retention_days = %q, want 30", v)
	}
	if v, _ := queries.GetSetting(db, "network_health_interval"); v != "120" {
		t.Errorf("network_health_interval = %q, want 120", v)
	}
	if strings.Contains(get()["env_locked"], "retention_days") {
		t.Error("retention_days reported as locked")
	}
}
