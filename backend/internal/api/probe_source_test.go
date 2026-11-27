package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
)

// probeSourceFixture: a router (C3), a switch (C2), a weak switch (C1) and a
// device no probe has classified yet, behind the probe-configuration routes.
func probeSourceFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	db := newTestDB(t)
	s := &Server{db: db}
	for id, class := range map[string]devclass.Class{"router": devclass.C3, "switch": devclass.C2, "weak": devclass.C1, "new": ""} {
		if err := queries.CreateDevice(db, &queries.Device{ID: id, Identity: id, Address: "192.0.2." + map[string]string{"router": "1", "switch": "2", "weak": "3", "new": "4"}[id], APIPort: 8728, Username: "nms", Status: "online"}); err != nil {
			t.Fatal(err)
		}
		if class != "" {
			if err := queries.SetDeviceClass(db, id, class, devclass.ResourceFacts{}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := chi.NewRouter()
	r.Post("/connectivity/targets", s.handleCreatePingTarget)
	r.Put("/connectivity/targets/{id}", s.handleUpdatePingTarget)
	r.Post("/speedtests", s.handleCreateSpeedTest)
	r.Put("/speedtests/{id}", s.handleUpdateSpeedTest)
	return s, r
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// A ping or a download runs on the probing device's CPU, so only a strong
// (C3) device can be chosen as its source; a switch, a weak device and a
// device not classified yet are refused with a reason that names the device.
func TestProbesAreConfiguredOnStrongDevicesOnly(t *testing.T) {
	s, h := probeSourceFixture(t)

	for _, dev := range []string{"switch", "weak", "new"} {
		rec := doJSON(t, h, http.MethodPost, "/speedtests", `{"device_id":"`+dev+`","url":"http://example.com/100MB.bin"}`)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), dev) {
			t.Errorf("speed test on %s = %d %s, want 422 naming the device", dev, rec.Code, rec.Body.String())
		}
		rec = doJSON(t, h, http.MethodPost, "/connectivity/targets", `{"kind":"internet","address":"198.51.100.1","device_id":"`+dev+`"}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("internet target on %s = %d %s, want 422", dev, rec.Code, rec.Body.String())
		}
		rec = doJSON(t, h, http.MethodPost, "/connectivity/targets", `{"kind":"client","mac_address":"AA:BB:CC:00:00:01","device_id":"`+dev+`"}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("watch pinned to %s = %d %s, want 422", dev, rec.Code, rec.Body.String())
		}
	}

	if rec := doJSON(t, h, http.MethodPost, "/speedtests", `{"device_id":"router","url":"http://example.com/100MB.bin"}`); rec.Code != http.StatusCreated {
		t.Errorf("speed test on the router = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodPost, "/connectivity/targets", `{"kind":"internet","address":"198.51.100.1","device_id":"router"}`); rec.Code != http.StatusCreated {
		t.Errorf("internet target on the router = %d %s", rec.Code, rec.Body.String())
	}
	// A watch that follows its client names no device: nothing to refuse.
	if rec := doJSON(t, h, http.MethodPost, "/connectivity/targets", `{"kind":"client","mac_address":"AA:BB:CC:00:00:02"}`); rec.Code != http.StatusCreated {
		t.Errorf("auto watch = %d %s", rec.Code, rec.Body.String())
	}
	// A router an admin put on the light profile is spared like a weak device.
	if err := queries.SetDevicePollProfile(s.db, "router", devclass.ProfileLight); err != nil {
		t.Fatal(err)
	}
	if rec := doJSON(t, h, http.MethodPost, "/speedtests", `{"device_id":"router","url":"http://example.com/b"}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "light") {
		t.Errorf("speed test on a light-profile router = %d %s, want 422 naming the profile", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodPost, "/speedtests", `{"device_id":"nope","url":"http://example.com/x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("speed test on an unknown device = %d %s, want 400", rec.Code, rec.Body.String())
	}
}

// What is already configured on a weaker device keeps working and stays
// editable — only moving a probe to another weak device is refused.
func TestExistingProbesOnWeakDevicesStayEditable(t *testing.T) {
	s, h := probeSourceFixture(t)
	if err := queries.CreateSpeedTest(s.db, &queries.SpeedTest{ID: "st", DeviceID: "switch", URL: "http://example.com/a", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreatePingTarget(s.db, &queries.PingTarget{ID: "pt", Kind: "internet", Address: "198.51.100.1", DeviceID: "weak", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	if rec := doJSON(t, h, http.MethodPut, "/speedtests/st", `{"label":"renamed","device_id":"switch"}`); rec.Code != http.StatusOK {
		t.Errorf("edit without moving = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodPut, "/connectivity/targets/pt", `{"label":"renamed","enabled":false}`); rec.Code != http.StatusOK {
		t.Errorf("edit a target without moving = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodPut, "/speedtests/st", `{"device_id":"weak"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("move to a weak device = %d %s, want 422", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, h, http.MethodPut, "/connectivity/targets/pt", `{"device_id":"switch"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("move a target to a switch = %d %s, want 422", rec.Code, rec.Body.String())
	}
	rec := doJSON(t, h, http.MethodPut, "/speedtests/st", `{"device_id":"router"}`)
	var got struct {
		DeviceID string `json:"device_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.DeviceID != "router" {
		t.Errorf("move to the router = %d %s", rec.Code, rec.Body.String())
	}
}
