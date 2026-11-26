package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/firmware"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
	"github.com/mikrotik-nms/backend/internal/ws"
)

func firmwareTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	db := newTestDB(t)
	hub := ws.NewHub()
	s := &Server{db: db, pool: routeros.NewPool(), hub: hub, cfg: &config.Config{Pollers: config.PollersLocal}}
	r := chi.NewRouter()
	r.Post("/firmware/check", s.handleCheckFirmware)
	r.Get("/firmware/releases", s.handleFirmwareReleases)
	r.Post("/firmware/upgrade", s.handleUpgradeFirmware)
	r.Post("/firmware/channel", s.handleSetChannel)
	r.Post("/firmware/routerboard", s.handleUpgradeRouterboard)
	return s, r
}

// Device-changing firmware actions take the credentials typed for them; a
// request without creates nothing and reaches no device.
func TestFirmwareActionsRequireJobCredentials(t *testing.T) {
	s, r := firmwareTestServer(t)
	fake := routerostest.NewDevice(t)
	if err := queries.CreateDevice(s.db, &queries.Device{ID: "d", Identity: "d1", Address: fake.Host, APIPort: fake.Port, Username: "nms", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, body string }{
		{"/firmware/upgrade", `{"device_ids":["d"],"reboot":false}`},
		{"/firmware/upgrade", `{"device_ids":["d"],"username":"admin"}`},
		{"/firmware/channel", `{"device_ids":["d"],"channel":"stable","password":"p"}`},
		{"/firmware/routerboard", `{"device_ids":["d"],"username":" ","password":"p"}`},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s = %d, want 400", c.path, c.body, rec.Code)
		}
	}
	var jobs int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM upgrade_jobs`).Scan(&jobs)
	if jobs != 0 || fake.Conns() != 0 {
		t.Errorf("jobs=%d device connections=%d, want none", jobs, fake.Conns())
	}
}

// The channel change logs in with the typed credentials on a one-shot
// session (never the pool), sets the channel and re-reads the firmware facts
// so the page shows the new channel at once.
func TestSetChannelUsesTypedCredentialsOnce(t *testing.T) {
	s, r := firmwareTestServer(t)
	fake := routerostest.NewDevice(t)
	fake.SetReply("/system/package/update/print", map[string]string{"channel": "long-term", "installed-version": "7.12.1"})
	if err := queries.CreateDevice(s.db, &queries.Device{ID: "d", Identity: "d1", Address: fake.Host, APIPort: fake.Port, Username: "nms", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/firmware/channel",
		strings.NewReader(`{"device_ids":["d"],"channel":"long-term","username":"admin","password":"typed"}`)))
	var resp struct {
		Changed int      `json:"changed"`
		Errors  []string `json:"errors"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.Changed != 1 || len(resp.Errors) != 0 {
		t.Fatalf("channel = %d %s", rec.Code, rec.Body.String())
	}
	cmds := fake.Commands()
	if len(cmds) == 0 || cmds[0] != "/system/package/update/set" || !slices.Contains(fake.Args()[0], "=channel=long-term") {
		t.Errorf("commands = %v %v", cmds, fake.Args())
	}
	if s.pool.Get("d") != nil {
		t.Error("the channel change used the pool")
	}
	st, err := queries.GetFirmwareStatusByDevice(s.db, "d")
	if err != nil || st.Channel != "long-term" {
		t.Errorf("firmware status = %+v %v", st, err)
	}

	// A refused login is named, and later devices are not tried.
	fake.RefuseLogins(true)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/firmware/routerboard",
		strings.NewReader(`{"device_ids":["d"],"username":"admin","password":"wrong"}`)))
	resp.Errors = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], "login refused by d1") {
		t.Errorf("refused login = %s", rec.Body.String())
	}
}

// "Check All" fetches the feed at most once an hour and never contacts a
// device.
func TestCheckFirmwareFetchesTheFeedOnly(t *testing.T) {
	s, r := firmwareTestServer(t)
	var hits atomic.Int32
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("7.17 1732700000"))
	}))
	defer feed.Close()
	s.firmware = poller.NewFirmwareChecker(s.db, s.hub)
	s.firmware.Feed = firmware.Feed{BaseURL: feed.URL}

	fake := routerostest.NewDevice(t)
	if err := queries.CreateDevice(s.db, &queries.Device{ID: "d", Identity: "d1", Address: fake.Host, APIPort: fake.Port, Username: "nms", Status: "online"}); err != nil {
		t.Fatal(err)
	}
	fake.Dial(t, s.pool, "d")

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/firmware/check", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("first check = %d %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rels, _ := queries.ListFirmwareReleases(s.db); len(rels) == len(firmware.Channels) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the feed was not stored")
		}
		time.Sleep(10 * time.Millisecond)
	}
	n := hits.Load()
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/firmware/check", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"recent"`) {
		t.Errorf("second check = %d %s", rec.Code, rec.Body.String())
	}
	if hits.Load() != n {
		t.Error("the feed was fetched again within the hour")
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/firmware/releases", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"7.17"`) {
		t.Errorf("releases = %d %s", rec.Code, rec.Body.String())
	}
	if cmds := fake.Commands(); len(cmds) != 0 {
		t.Errorf("Check All contacted the device: %v", cmds)
	}
}

// Every device-changing firmware action leaves an audit entry on each device
// it was asked for: what was asked, never the RouterOS login typed for it.
func TestFirmwareActionsAreAudited(t *testing.T) {
	s, r := firmwareTestServer(t)
	fake := routerostest.NewDevice(t)
	fake.SetReply("/system/package/update/print", map[string]string{"channel": "stable", "installed-version": "7.12.1"})
	// "b" answers nowhere: a request is audited whether or not it succeeds.
	for id, addr := range map[string]string{"a": fake.Host, "b": "127.0.0.2"} {
		if err := queries.CreateDevice(s.db, &queries.Device{ID: id, Identity: "sw-" + id, Address: addr, APIPort: fake.Port, Username: "nms", Status: "online"}); err != nil {
			t.Fatal(err)
		}
	}
	post := func(path, body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body.String())
		}
	}
	post("/firmware/channel", `{"device_ids":["a","b"],"channel":"testing","username":"ros-admin","password":"typed-secret"}`)
	post("/firmware/routerboard", `{"device_ids":["a"],"reboot":false,"username":"ros-admin","password":"typed-secret"}`)
	post("/firmware/upgrade", `{"device_ids":["a"],"reboot":false,"username":"ros-admin","password":"typed-secret"}`)

	want := map[string][]string{
		"a": {"firmware_channel_requested", "firmware_routerboard_requested", "firmware_upgrade_requested"},
		"b": {"firmware_channel_requested"},
	}
	for id, actions := range want {
		es, err := queries.ListAuditLog(s.db, id, 10)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range es {
			got = append(got, e.Action)
			if strings.Contains(e.Detail, "typed-secret") || strings.Contains(e.Detail, "ros-admin") {
				t.Errorf("audit detail carries the typed login: %q", e.Detail)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, actions) {
			t.Errorf("audit of %s = %v, want %v", id, got, actions)
		}
	}
	es, _ := queries.ListAuditLog(s.db, "a", 10)
	var details []string
	for _, e := range es {
		details = append(details, e.Action+": "+e.Detail)
	}
	joined := strings.Join(details, "\n")
	for _, part := range []string{"→ testing", "download only", "applied on the next reboot"} {
		if !strings.Contains(joined, part) {
			t.Errorf("audit details lack %q:\n%s", part, joined)
		}
	}

	// A request refused before it reaches a device is not an action.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/firmware/channel", strings.NewReader(`{"device_ids":["b"],"channel":"stable"}`)))
	if es, _ := queries.ListAuditLog(s.db, "b", 10); rec.Code != http.StatusBadRequest || len(es) != 1 {
		t.Errorf("refused request: %d, %d audit entries on b (want 400, 1)", rec.Code, len(es))
	}
}
