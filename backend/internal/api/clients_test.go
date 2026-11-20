package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
)

// fakeClientRefresher answers Start with a scripted error.
type fakeClientRefresher struct {
	status poller.ClientRefreshStatus
	err    error
	starts int
}

func (f *fakeClientRefresher) Status() poller.ClientRefreshStatus { return f.status }
func (f *fakeClientRefresher) Start() (poller.ClientRefreshStatus, error) {
	f.starts++
	return f.status, f.err
}

func clientsRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/clients", s.handleListClients)
	r.Get("/clients/cached", s.handleListClients)
	r.Post("/clients/refresh", s.handleRefreshClients)
	return r
}

type clientListBody struct {
	Clients    []networkClient   `json:"clients"`
	Total      int               `json:"total"`
	Cached     bool              `json:"cached"`
	Limited    *bool             `json:"limited"`
	TimedOut   *bool             `json:"timed_out"`
	UpdatedAt  string            `json:"updated_at"`
	AgeSeconds *int              `json:"age_seconds"`
	Refresh    clientRefreshView `json:"refresh"`
	Error      string            `json:"error"`
}

func doClients(t *testing.T, h http.Handler, method, path string) (*httptest.ResponseRecorder, clientListBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	var body clientListBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
	}
	return rec, body
}

// Both list endpoints serve the cache, with its age, the refresh state and
// the fields a tab still running the scanning frontend reads.
func TestClientListIsTheCacheWithItsAge(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := newTestDB(t)
	if err := queries.UpsertMACLookup(db, &queries.MACLookup{
		MACAddress: "02:00:00:00:00:0A", IPAddress: "192.0.2.10", HostName: "laptop", Source: "dhcp",
	}); err != nil {
		t.Fatal(err)
	}
	next := time.Now().Add(3 * time.Minute).UTC()
	s := &Server{db: db, clients: &fakeClientRefresher{status: poller.ClientRefreshStatus{
		MinIntervalSeconds: 300, NextAllowedAt: &next, Clients: 1, DevicesRead: 2,
	}}}
	h := clientsRouter(s)

	for _, path := range []string{"/clients", "/clients/cached", "/clients?limit=5&timeout=120"} {
		rec, body := doClients(t, h, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		if body.Total != 1 || len(body.Clients) != 1 || body.Clients[0].HostName != "laptop" || !body.Cached {
			t.Errorf("GET %s: clients = %+v (total %d, cached %v)", path, body.Clients, body.Total, body.Cached)
		}
		if body.Limited == nil || *body.Limited || body.TimedOut == nil || *body.TimedOut {
			t.Errorf("GET %s: the scan fields must be present and false", path)
		}
		if body.UpdatedAt == "" || body.AgeSeconds == nil || *body.AgeSeconds < 0 || *body.AgeSeconds > 60 {
			t.Errorf("GET %s: updated_at=%q age_seconds=%v; want the newest entry's age", path, body.UpdatedAt, body.AgeSeconds)
		}
		rf := body.Refresh
		if !rf.Available || rf.MinIntervalSeconds != 300 || rf.NextAllowedAt == nil || rf.DevicesRead != 2 {
			t.Errorf("GET %s: refresh = %+v", path, rf)
		}
	}
	if n := s.clients.(*fakeClientRefresher).starts; n != 0 {
		t.Fatalf("listing the clients started %d refreshes", n)
	}

	// An empty cache has no age; without a refresher the list is still served.
	empty := &Server{db: newTestDB(t)}
	rec, body := doClients(t, clientsRouter(empty), http.MethodGet, "/clients")
	if rec.Code != http.StatusOK || body.Total != 0 || body.AgeSeconds != nil || body.Refresh.Available ||
		body.Refresh.MinIntervalSeconds != 300 {
		t.Fatalf("empty cache, no refresher: %d %+v", rec.Code, body)
	}
}

func TestClientRefreshEndpointStatuses(t *testing.T) {
	routeros.SetDialPolicy(nil)
	t.Cleanup(func() { routeros.SetDialPolicy(nil) })
	started := time.Now().UTC()
	next := time.Now().Add(100 * time.Second).UTC()
	fake := &fakeClientRefresher{status: poller.ClientRefreshStatus{Running: true, StartedAt: &started, MinIntervalSeconds: 300}}
	s := &Server{db: newTestDB(t), clients: fake}
	h := clientsRouter(s)

	rec, body := doClients(t, h, http.MethodPost, "/clients/refresh")
	if rec.Code != http.StatusAccepted || !body.Refresh.Running || !body.Refresh.Available {
		t.Fatalf("started: %d %+v", rec.Code, body)
	}

	fake.err = poller.ErrClientRefreshRunning
	rec, body = doClients(t, h, http.MethodPost, "/clients/refresh")
	if rec.Code != http.StatusConflict || body.Error == "" || !body.Refresh.Running {
		t.Fatalf("running: %d %+v", rec.Code, body)
	}

	fake.err = poller.ErrClientRefreshTooSoon
	fake.status = poller.ClientRefreshStatus{StartedAt: &started, NextAllowedAt: &next, MinIntervalSeconds: 300}
	// Retry-After is the time left until next_allowed_at when the request is
	// answered: bracket it by the time left before and after the call rather
	// than assume how long the test took to get here (a slow runner did not
	// get here within a second).
	leftBefore := time.Until(next)
	rec, body = doClients(t, h, http.MethodPost, "/clients/refresh")
	leftAfter := time.Until(next)
	if rec.Code != http.StatusTooManyRequests || body.Error == "" || body.Refresh.NextAllowedAt == nil {
		t.Fatalf("too soon: %d %+v", rec.Code, body)
	}
	ra, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || ra < int(leftAfter.Seconds()) || ra > int(leftBefore.Seconds())+1 {
		t.Fatalf("Retry-After = %q, want the seconds until next_allowed_at (between %d and %d)",
			rec.Header().Get("Retry-After"), int(leftAfter.Seconds()), int(leftBefore.Seconds())+1)
	}

	fake.err = routeros.ErrPollersOff
	rec, body = doClients(t, h, http.MethodPost, "/clients/refresh")
	if rec.Code != http.StatusServiceUnavailable || body.Error == "" {
		t.Fatalf("pollers off: %d %+v", rec.Code, body)
	}

	rec, body = doClients(t, clientsRouter(&Server{db: s.db}), http.MethodPost, "/clients/refresh")
	if rec.Code != http.StatusServiceUnavailable || body.Error == "" {
		t.Fatalf("no refresher: %d %+v", rec.Code, body)
	}

	// With polling off the list says a refresh is not available.
	routeros.SetDialPolicy(&routeros.DialPolicy{Enabled: false})
	_, body = doClients(t, h, http.MethodGet, "/clients")
	if body.Refresh.Available {
		t.Fatal("refresh reported available while polling is off")
	}
}

// A client is inactive once it has gone unseen for two reads of the slowest
// ARP reader (plus a margin): on a fleet with a weak device, read every 30
// minutes, a client last seen 45 minutes ago is still active; the admin's
// own setting wins; and a fleet of strong devices keeps the shorter window.
func TestClientInactivityFollowsTheSlowestReader(t *testing.T) {
	routeros.SetDialPolicy(nil)
	db := newTestDB(t)
	if err := queries.UpsertMACLookup(db, &queries.MACLookup{MACAddress: "02:00:00:00:00:0A", IPAddress: "192.0.2.10", Source: "arp"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE mac_lookup SET updated_at = datetime('now', '-45 minutes')`); err != nil {
		t.Fatal(err)
	}
	h := clientsRouter(&Server{db: db})
	active := func() bool {
		t.Helper()
		_, body := doClients(t, h, http.MethodGet, "/clients")
		if len(body.Clients) != 1 {
			t.Fatalf("clients = %+v", body.Clients)
		}
		return body.Clients[0].Active
	}

	if active() {
		t.Fatal("no device is polled: a client unseen for 45 min must be inactive (30 min floor)")
	}

	if err := queries.CreateDevice(db, &queries.Device{ID: "r3", Identity: "r3", Address: "198.51.100.1", Username: "u", APIPort: 8728, Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetDeviceClass(db, "r3", devclass.C3, devclass.ResourceFacts{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if active() {
		t.Fatal("a strong device reads ARP every 15 min: 45 min unseen must be inactive (2 × 15 + 10)")
	}

	if err := queries.CreateDevice(db, &queries.Device{ID: "s1", Identity: "s1", Address: "198.51.100.2", Username: "u", APIPort: 8728, Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetDeviceClass(db, "s1", devclass.C1, devclass.ResourceFacts{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !active() {
		t.Fatal("a weak device reads ARP every 30 min: a client unseen for 45 min is still within two reads")
	}

	if err := queries.SetSetting(db, "client_inactive_after_seconds", "1800"); err != nil {
		t.Fatal(err)
	}
	if active() {
		t.Fatal("client_inactive_after_seconds=1800 must win over the cadence rule")
	}
}
