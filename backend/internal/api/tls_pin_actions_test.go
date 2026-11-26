package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

// On-demand actions on a TLS device without a pin say why they cannot run —
// 409 "TLS pin missing …" — instead of failing later as "no connection";
// the firmware actions, which dial per device, report it for that device.
// The device is never contacted.
func TestOnDemandActionsOnATLSDeviceWithoutPin(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := newTestDB(t)
	fake := routerostest.NewTLSDevice(t)
	if err := queries.CreateDevice(db, &queries.Device{
		ID: "sw", Identity: "sw1", Address: fake.Host, APIPort: fake.Port, Username: "nms", PasswordEnc: "p",
		UseTLS: true, Status: "online", Tags: "[]",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreatePingTarget(db, &queries.PingTarget{ID: "pt", Kind: "internet", Address: "198.51.100.7", DeviceID: "sw", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateSpeedTest(db, &queries.SpeedTest{ID: "st", DeviceID: "sw", URL: "http://198.51.100.8/100MB.bin", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	hub := ws.NewHub()
	go hub.Run()
	s := &Server{db: db, pool: routeros.NewPool(), hub: hub, cfg: &config.Config{Pollers: config.PollersLocal}}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			u := &auth.ContextUser{ID: "u-admin", Username: "admin", Role: "admin"}
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), auth.UserContextKey, u)))
		})
	})
	r.Post("/connectivity/speedtests/{id}/run", s.handleRunSpeedTest)
	r.Post("/connectivity/targets/{id}/traceroute", s.handleRunTraceroute)
	r.Get("/devices/{id}/addresses", s.handleDeviceAddresses)
	r.Post("/firmware/channel", s.handleSetChannel)

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/connectivity/speedtests/st/run"},
		{http.MethodPost, "/connectivity/targets/pt/traceroute"},
		{http.MethodGet, "/devices/sw/addresses"},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		var resp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		if rec.Code != http.StatusConflict || !strings.HasPrefix(resp.Error, "TLS pin missing") {
			t.Errorf("%s %s = %d %q, want 409 TLS pin missing", c.method, c.path, rec.Code, resp.Error)
		}
	}

	// A firmware action names the device it could not reach, and why.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/firmware/channel",
		strings.NewReader(`{"device_ids":["sw"],"channel":"stable","username":"u","password":"p"}`)))
	var out struct {
		Changed int      `json:"changed"`
		Errors  []string `json:"errors"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.Changed != 0 || len(out.Errors) != 1 || !strings.Contains(out.Errors[0], "sw1: TLS pin missing") {
		t.Errorf("firmware channel = %d %s, want the pin refusal for sw1", rec.Code, rec.Body.String())
	}

	if fake.Conns() != 0 {
		t.Fatalf("the device was contacted %d time(s)", fake.Conns())
	}
}
