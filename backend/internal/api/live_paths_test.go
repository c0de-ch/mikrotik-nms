package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/devclass"
	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/routeros"
	"github.com/mikrotik-nms/backend/internal/routeros/routerostest"
)

// liveRouter mounts the read endpoints the network map and the port grid
// load, as the given role.
func liveRouter(s *Server, role string) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := &auth.ContextUser{ID: "u-" + role, Username: role, Role: role}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auth.UserContextKey, u)))
		})
	})
	r.Get("/topology", s.handleGetTopology)
	r.Get("/devices", s.handleListDevices)
	r.Get("/devices/{id}/ports", s.handleGetDevicePorts)
	r.Get("/devices/{id}/live-limits", s.handleDeviceLiveLimits)
	r.Get("/wifi/current", s.handleWifiCurrent)
	r.Get("/vlans", s.handleListVLANs)
	r.Get("/clients/cached", s.handleCachedClients)
	s.mountTrafficRoutes(r)
	return r
}

func liveGet(t *testing.T, h http.Handler, path string, out any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: decode %q: %v", path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

// Opening the map must cost the devices nothing. Every device has a live
// pooled client to a tripwire endpoint that records any command, so a
// handler reaching a device by any helper — GetLive, monitor-traffic, a
// print — fails this test.
func TestMapReadPathsNeverTouchDevices(t *testing.T) {
	f := newTrafficFixture(t)
	pool := routeros.NewPool(false)
	fake := routerostest.NewDevice(t)
	ids := []string{"r", "c", "s", "a1", "a2"}
	for _, id := range ids {
		fake.Dial(t, pool, id)
	}
	f.s.pool = pool
	f.s.cfg = &config.Config{Pollers: config.PollersLocal}

	for _, role := range []string{"admin", "viewer"} {
		h := liveRouter(f.s, role)
		paths := []string{"/topology", "/devices", "/wifi/current", "/vlans", "/clients/cached", "/traffic/links"}
		for _, id := range ids {
			paths = append(paths, "/devices/"+id+"/ports", "/devices/"+id+"/live-limits")
		}
		for _, p := range paths {
			if code := liveGet(t, h, p, nil); code != http.StatusOK {
				t.Errorf("%s GET %s = %d", role, p, code)
			}
		}
	}
	if cmds := fake.Commands(); len(cmds) != 0 {
		t.Fatalf("map read paths sent device commands: %v", cmds)
	}
	if n := fake.Conns(); n != len(ids) {
		t.Fatalf("connections = %d, want only the %d test dials", n, len(ids))
	}
}

func TestDevicePortsFromSnapshot(t *testing.T) {
	f := newTrafficFixture(t)
	// The snapshot is seconds old, the inventory hourly: ether7 went down
	// and ether9 appeared since the last inventory read.
	for i, p := range f.fake.snap.Ports {
		if p.DeviceID == "s" && p.Iface == "ether7" {
			f.fake.snap.Ports[i].Running = false
		}
	}
	f.fake.snap.Ports = append(f.fake.snap.Ports, poller.PortRate{DeviceID: "s", Iface: "ether9", Type: "ether", Running: true, RxBps: 7, TxBps: 3})
	h := liveRouter(f.s, "viewer")

	var resp devicePortsResponse
	if code := liveGet(t, h, "/devices/s/ports", &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.Source != poller.SourceSnapshot || resp.SampledAt == nil || resp.AgeSeconds == nil || !resp.Ready || resp.IntervalSeconds != 15 {
		t.Errorf("meta = %+v", resp.SnapshotMeta)
	}
	var names []string
	byName := map[string]portInfo{}
	for _, p := range resp.Ports {
		names = append(names, p.Name)
		byName[p.Name] = p
	}
	// Inventory order (physical ports only), then snapshot-only ports.
	if want := []string{"ether23", "ether5", "ether7", "sfp1", "ether9"}; !equalStrings(names, want) {
		t.Errorf("ports = %v, want %v", names, want)
	}
	if p := byName["sfp1"]; p.RxBps != 90*mbps || p.TxBps != 8*mbps {
		t.Errorf("sfp1 = %+v, want the snapshot rates", p)
	}
	if byName["ether7"].Running {
		t.Error("ether7 running from the hourly inventory instead of the snapshot")
	}

	// A device the snapshot has nothing for: inventory only, no timestamp.
	if err := queries.CreateDevice(f.db, &queries.Device{ID: "z", Identity: "Z", Address: "10.0.0.9", Username: "admin", APIPort: 8728, Status: "offline"}); err != nil {
		t.Fatal(err)
	}
	if err := queries.UpsertInterface(f.db, &queries.Interface{ID: uuid.NewString(), DeviceID: "z", Name: "ether1", Type: "ether"}); err != nil {
		t.Fatal(err)
	}
	resp = devicePortsResponse{}
	if code := liveGet(t, h, "/devices/z/ports", &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.SampledAt != nil || resp.AgeSeconds != nil || len(resp.Ports) != 1 || resp.Ports[0].RxBps != 0 {
		t.Errorf("device without counters = %+v", resp)
	}

	if code := liveGet(t, h, "/devices/nope/ports", nil); code != http.StatusNotFound {
		t.Errorf("unknown device = %d, want 404", code)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTrafficLinksFromSnapshot(t *testing.T) {
	f := newTrafficFixture(t)
	var resp poller.LinkTrafficPayload
	if code := liveGet(t, liveRouter(f.s, "viewer"), "/traffic/links", &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.Source != poller.SourceSnapshot || resp.SampledAt == nil || !resp.SampledAt.Equal(*f.fake.snap.TS) {
		t.Errorf("meta = %+v", resp.SnapshotMeta)
	}
	links, err := queries.ListLinks(f.db)
	if err != nil {
		t.Fatal(err)
	}
	var rc string
	for _, l := range links {
		if l.DeviceAID == "r" && l.DeviceBID == "c" {
			rc = l.ID
		}
	}
	got := map[string]poller.LinkTraffic{}
	for _, l := range resp.Links {
		got[l.ID] = l
	}
	// Both ends read alike, so the A end (R's sfp1) is used, in R's frame.
	if l := got[rc]; l.DeviceID != "r" || l.Iface != "sfp1" || l.RxBps != 9*mbps || l.TxBps != 95*mbps {
		t.Errorf("R–C link = %+v", l)
	}
	if l := got["up:r:eth1-wan:203.0.113.1"]; l.Target != "internet" || l.RxBps != 100*mbps || l.TxBps != 10*mbps {
		t.Errorf("R's WAN edge = %+v", l)
	}
	// The other devices route through R (a managed next-hop): no egress edge.
	for id := range got {
		if len(id) > 3 && id[:3] == "up:" && id != "up:r:eth1-wan:203.0.113.1" {
			t.Errorf("unexpected egress edge %s", id)
		}
	}

	// Before the collector's first cycle: an empty, timestamp-less answer.
	resp = poller.LinkTrafficPayload{}
	if code := liveGet(t, liveRouter(&Server{db: f.db}, "viewer"), "/traffic/links", &resp); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.SampledAt != nil || resp.Ready || resp.Links == nil || len(resp.Links) != 0 {
		t.Errorf("without a collector = %+v", resp)
	}
}

func TestDeviceLiveLimits(t *testing.T) {
	f := newTrafficFixture(t)
	f.s.cfg = &config.Config{Pollers: config.PollersLocal}
	if err := queries.SetDeviceClass(f.db, "r", devclass.C3, devclass.ResourceFacts{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetDeviceClass(f.db, "s", devclass.C1, devclass.ResourceFacts{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := queries.PauseDevicePolling(f.db, "a2", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, role, device string
		s                  *Server
		allowed            bool
		reason             string
		class              devclass.Class
		minPeriod          int
	}{
		{"C1 viewer", "viewer", "s", f.s, false, poller.LiveRefusedAdminOnly, devclass.C1, 15},
		{"C1 admin", "admin", "s", f.s, true, "", devclass.C1, 15},
		{"unprobed viewer", "viewer", "c", f.s, false, poller.LiveRefusedAdminOnly, devclass.C1, 15},
		{"C3 viewer", "viewer", "r", f.s, true, "", devclass.C3, 1},
		{"paused", "admin", "a2", f.s, false, poller.LiveRefusedPaused, devclass.C1, 15},
		{"polling off", "admin", "r", &Server{db: f.db, portStats: f.fake}, false, poller.LiveRefusedPollersOff, devclass.C3, 1},
	}
	for _, tt := range tests {
		var resp liveLimitsResponse
		if code := liveGet(t, liveRouter(tt.s, tt.role), "/devices/"+tt.device+"/live-limits", &resp); code != http.StatusOK {
			t.Fatalf("%s: status %d", tt.name, code)
		}
		if resp.Allowed != tt.allowed || resp.Reason != tt.reason || resp.Class != tt.class || resp.MinPeriodSeconds != tt.minPeriod {
			t.Errorf("%s: %+v", tt.name, resp)
		}
		if !tt.allowed && resp.Message == "" {
			t.Errorf("%s: refused without a message", tt.name)
		}
		if resp.SnapshotIntervalSeconds != 15 {
			t.Errorf("%s: snapshot interval %d", tt.name, resp.SnapshotIntervalSeconds)
		}
	}
	var resp liveLimitsResponse
	_ = liveGet(t, liveRouter(f.s, "viewer"), "/devices/s/live-limits", &resp)
	if resp.Message != "Live view is limited on weak devices — showing 15 s snapshot data" {
		t.Errorf("C1 viewer message = %q", resp.Message)
	}
	if code := liveGet(t, liveRouter(f.s, "viewer"), "/devices/nope/live-limits", nil); code != http.StatusNotFound {
		t.Errorf("unknown device = %d, want 404", code)
	}
}
