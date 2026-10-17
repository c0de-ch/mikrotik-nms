package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// On-demand device actions honour an admin pause like the scheduled pollers:
// each answers 409 "polling paused until <time>" before reaching the device
// — the tripwire endpoint behind the pooled client sees no command, and no
// firmware job is created — and resuming the device lifts the refusal.
func TestOnDemandActionsRefusePausedDevice(t *testing.T) {
	db := newTestDB(t)
	fake := routerostest.NewDevice(t)
	pool := routeros.NewPool(false)
	for _, d := range []*queries.Device{
		{ID: "sw", Identity: "sw1", Address: fake.Host, APIPort: fake.Port, Username: "nms", Status: "online"},
		{ID: "rt", Identity: "rt1", Address: "192.0.2.1", APIPort: 8728, Username: "nms", Status: "online"},
	} {
		if err := queries.CreateDevice(db, d); err != nil {
			t.Fatal(err)
		}
	}
	fake.Dial(t, pool, "sw")
	if err := queries.CreatePingTarget(db, &queries.PingTarget{ID: "pt", Kind: "internet", Address: "198.51.100.7", DeviceID: "sw", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateSpeedTest(db, &queries.SpeedTest{ID: "st", DeviceID: "sw", URL: "http://198.51.100.8/100MB.bin", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if err := queries.PauseDevicePolling(db, "sw", until); err != nil {
		t.Fatal(err)
	}

	hub := ws.NewHub()
	go hub.Run()
	s := &Server{db: db, pool: pool, hub: hub, cfg: &config.Config{Pollers: config.PollersLocal}}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			u := &auth.ContextUser{ID: "u-admin", Username: "admin", Role: "admin"}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, u)))
		})
	})
	r.Post("/connectivity/speedtests/{id}/run", s.handleRunSpeedTest)
	r.Post("/connectivity/targets/{id}/run", s.handleRunPingTarget)
	r.Post("/connectivity/targets/{id}/traceroute", s.handleRunTraceroute)
	r.Get("/devices/{id}/addresses", s.handleDeviceAddresses)
	r.Post("/firmware/upgrade", s.handleUpgradeFirmware)
	r.Post("/firmware/channel", s.handleSetChannel)
	r.Post("/firmware/routerboard", s.handleUpgradeRouterboard)
	r.Get("/debug/wifi", s.handleDebugWifiRaw)

	want := "polling paused until " + until.UTC().Format(time.RFC3339)
	for _, c := range []struct {
		method, path, body, wantMsg string
	}{
		{http.MethodPost, "/connectivity/speedtests/st/run", "", want},
		{http.MethodPost, "/connectivity/targets/pt/run", "", want},
		{http.MethodPost, "/connectivity/targets/pt/traceroute", "", want},
		{http.MethodGet, "/devices/sw/addresses", "", want},
		// Several devices: the paused one is named.
		{http.MethodPost, "/firmware/upgrade", `{"device_ids":["rt","sw"],"reboot":true}`, "sw1: " + want},
		{http.MethodPost, "/firmware/channel", `{"device_ids":["sw"],"channel":"stable"}`, want},
		{http.MethodPost, "/firmware/routerboard", `{"device_ids":["sw"]}`, want},
		{http.MethodGet, "/debug/wifi?device_id=sw", "", want},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, strings.NewReader(c.body)))
		var resp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if rec.Code != http.StatusConflict || resp.Error != c.wantMsg {
			t.Errorf("%s %s = %d %q, want 409 %q", c.method, c.path, rec.Code, resp.Error, c.wantMsg)
		}
	}
	if cmds := fake.Commands(); len(cmds) != 0 {
		t.Errorf("a paused device received commands: %v", cmds)
	}
	var jobs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM upgrade_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Errorf("upgrade jobs = %d (%v), want none for a request naming a paused device", jobs, err)
	}

	// Resumed: the same request reaches the device again.
	if err := queries.ResumeDevicePolling(db, "sw"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/wifi?device_id=sw", nil))
	if rec.Code != http.StatusOK || len(fake.Commands()) == 0 {
		t.Errorf("after resume: %d, device commands %v — want the reads to run", rec.Code, fake.Commands())
	}
}
