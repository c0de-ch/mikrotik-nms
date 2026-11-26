package api

import (
	"context"
	"database/sql"
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
)

// loadFixture is one managed device ("d1", 64 MiB) and a server whose
// minute data is final through end and hourly data through the hour of end.
type loadFixture struct {
	db   *sql.DB
	s    *Server
	end  time.Time
	hour time.Time
}

func newLoadFixture(t *testing.T) *loadFixture {
	t.Helper()
	db := newTestDB(t)
	if err := queries.CreateDevice(db, &queries.Device{ID: "d1", Identity: "sw1", Address: "192.0.2.10",
		Username: "admin", APIPort: 8728, Status: "online"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE devices SET memory_total = ? WHERE id = 'd1'`, int64(64<<20)); err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Truncate(time.Minute)
	hour := end.Truncate(time.Hour)
	return &loadFixture{db: db, end: end, hour: hour,
		s: &Server{db: db, portStats: &fakePortStats{flushed: end, rolled: hour}}}
}

func (f *loadFixture) router(role string) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := &auth.ContextUser{ID: "u-" + role, Username: role, Role: role}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), auth.UserContextKey, u)))
		})
	})
	r.Get("/devices/{id}/load", f.s.handleDeviceLoad)
	return r
}

func (f *loadFixture) get(t *testing.T, path string, out any) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.router("viewer").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if out != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("GET %s: decode %q: %v", path, rec.Body.String(), err)
		}
	}
	return rec.Code, rec.Body.String()
}

func nv(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// The minute series: quiet-window CPU, the post-poll sample, free memory,
// the NMS's own cost, reboot markers — and every bucket without coverage as
// explicit nulls with the reason.
func TestDeviceLoadMinuteSeries(t *testing.T) {
	f := newLoadFixture(t)
	ago := func(m int) time.Time { return f.end.Add(-time.Duration(m) * time.Minute) }
	if err := queries.UpsertDeviceMinutes(f.db, []queries.DeviceMinute{
		{DeviceID: "d1", Bucket: ago(6), PolledS: 60, Tier0OK: 1, ReachableS: 60, CPUSum: 10, CPUSamples: 1, CPUMax: 10,
			FreeMemoryMin: nv(20 << 20), UptimeS: nv(1000), NMSCmds: 5, NMSWallMs: 300, NMSRows: 200},
		{DeviceID: "d1", Bucket: ago(5), PolledS: 60, Tier0OK: 1, ReachableS: 60, CPUSum: 30, CPUSamples: 2, CPUMax: 20,
			CPUPost: nv(55), FreeMemoryMin: nv(18 << 20), NMSCmds: 7, NMSWallMs: 500, NMSRows: 260, NMSTimeouts: 1, NMSReconnects: 1},
		{DeviceID: "d1", Bucket: ago(4), ReachableS: 60, Paused: true, GapReason: queries.GapPaused},
		// ago(3): no row at all — the NMS was down.
		{DeviceID: "d1", Bucket: ago(2), PolledS: 20, Tier0OK: 1, ReachableS: 60, CPUSum: 90, CPUSamples: 1, CPUMax: 90,
			UptimeS: nv(30), Rebooted: true, NMSCmds: 3, NMSWallMs: 100, NMSRows: 60},
		{DeviceID: "d1", Bucket: ago(1), ReachableS: 0, GapReason: queries.GapUnreachable, NMSCmds: 1, NMSWallMs: 30000, NMSTimeouts: 1},
	}); err != nil {
		t.Fatal(err)
	}

	var r deviceLoadResponse
	code, body := f.get(t, "/devices/d1/load?range=1h", &r)
	if code != http.StatusOK {
		t.Fatalf("status %d %s", code, body)
	}
	if r.DeviceID != "d1" || r.Range != "1h" || r.Resolution != "1m" || r.StepSeconds != 60 ||
		!r.From.Equal(f.end.Add(-time.Hour)) || !r.To.Equal(f.end) || len(r.Points) != 60 {
		t.Fatalf("header = %+v with %d points", r, len(r.Points))
	}
	if r.CoverageFrom == nil || !r.CoverageFrom.Equal(ago(6)) || r.MemoryTotal == nil || *r.MemoryTotal != 64<<20 {
		t.Errorf("coverage_from %v memory_total %v", r.CoverageFrom, r.MemoryTotal)
	}
	point := func(m int) deviceLoadPoint { return r.Points[60-m] }

	if p := point(6); !p.TS.Equal(ago(6)) || p.Gap != nil || *p.CoveredS != 60 || *p.CPUAvg != 10 || *p.CPUMax != 10 ||
		p.CPUPost != nil || *p.FreeMemory != 20<<20 || p.Rebooted || *p.NMSCmds != 5 || *p.NMSWallMs != 300 || *p.NMSRows != 200 {
		t.Errorf("6 min ago = %s", mustJSON(p))
	}
	if p := point(5); *p.CPUAvg != 15 || *p.CPUMax != 20 || *p.CPUPost != 55 || *p.NMSTimeouts != 1 || *p.NMSReconnects != 1 {
		t.Errorf("5 min ago = %s", mustJSON(p))
	}
	// A paused minute: a gap that says why; the NMS sent nothing.
	if p := point(4); p.Gap == nil || *p.Gap != "paused" || !p.Paused || *p.CoveredS != 0 || p.CPUAvg != nil || p.FreeMemory != nil || *p.NMSCmds != 0 {
		t.Errorf("4 min ago = %s", mustJSON(p))
	}
	// No row: everything null, the gap is "no_data".
	if p := point(3); p.Gap == nil || *p.Gap != "no_data" || p.CoveredS != nil || p.CPUAvg != nil || p.NMSCmds != nil || p.NMSWallMs != nil {
		t.Errorf("3 min ago = %s", mustJSON(p))
	}
	if p := point(2); !p.Rebooted || p.Gap != nil || *p.CoveredS != 20 || *p.CPUAvg != 90 {
		t.Errorf("2 min ago = %s", mustJSON(p))
	}
	// Unreachable: no CPU, but what the NMS spent trying is still shown.
	if p := point(1); *p.Gap != "unreachable" || p.CPUAvg != nil || *p.NMSCmds != 1 || *p.NMSWallMs != 30000 || *p.NMSTimeouts != 1 {
		t.Errorf("1 min ago = %s", mustJSON(p))
	}
	// Before the first row there is no telemetry.
	if p := point(30); p.Gap == nil || *p.Gap != "no_data" {
		t.Errorf("30 min ago = %s", mustJSON(p))
	}
	// Gaps are explicit nulls on the wire, not omitted keys or zeros.
	if !strings.Contains(body, `"cpu_avg":null`) || !strings.Contains(body, `"nms_cmds":null`) || !strings.Contains(body, `"gap":"no_data"`) {
		t.Errorf("body lacks explicit nulls: %s", body[:min(len(body), 400)])
	}

	s := r.Summary
	if s.CoveredPct != 3.9 { // 140 covered seconds of the hour
		t.Errorf("covered_pct = %v, want 3.9", s.CoveredPct)
	}
	if s.CPUAvg == nil || *s.CPUAvg != 32.5 || s.CPUMax == nil || *s.CPUMax != 90 { // (10+30+90)/4 samples
		t.Errorf("cpu avg %v max %v, want 32.5 / 90", s.CPUAvg, s.CPUMax)
	}
	// "Is it us?": the post-poll sample against the quiet-window average of
	// the same minute.
	if s.CPUPostAvg == nil || *s.CPUPostAvg != 55 || s.CPUQuietAtPost == nil || *s.CPUQuietAtPost != 15 {
		t.Errorf("post %v vs quiet %v, want 55 vs 15", s.CPUPostAvg, s.CPUQuietAtPost)
	}
	if s.NMSCmdsPerMin == nil || *s.NMSCmdsPerMin != 3.2 || s.NMSWallMsPerMin == nil || *s.NMSWallMsPerMin != 6180 {
		t.Errorf("nms per minute = %v cmds %v ms, want 3.2 and 6180 over the 5 recorded minutes", s.NMSCmdsPerMin, s.NMSWallMsPerMin)
	}
	if s.NMSTimeouts != 2 || s.NMSReconnects != 1 || s.Reboots != 1 {
		t.Errorf("totals = %+v", s)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// Beyond 24 h the series is hourly, from device_hours, with the reboot
// markers and the NMS's wall time added from the minute rows while they last.
func TestDeviceLoadHourlySeries(t *testing.T) {
	f := newLoadFixture(t)
	h2, h1 := f.hour.Add(-2*time.Hour), f.hour.Add(-time.Hour)
	var mins []queries.DeviceMinute
	for m := 0; m < 60; m++ {
		row := queries.DeviceMinute{DeviceID: "d1", Bucket: h2.Add(time.Duration(m) * time.Minute), PolledS: 60,
			CPUSum: 20, CPUSamples: 1, CPUMax: 20, FreeMemoryMin: nv(int64(30<<20) - int64(m)), NMSCmds: 6, NMSWallMs: 200, NMSRows: 100}
		if m == 10 {
			row.CPUPost = nv(70)
		}
		if m == 40 {
			row.Rebooted, row.NMSReconnects = true, 1
		}
		mins = append(mins, row)
	}
	mins = append(mins, queries.DeviceMinute{DeviceID: "d1", Bucket: h1, GapReason: queries.GapUnreachable, NMSCmds: 2, NMSTimeouts: 2})
	if err := queries.UpsertDeviceMinutes(f.db, mins); err != nil {
		t.Fatal(err)
	}
	if err := queries.RollupDeviceHours(f.db, h2, f.hour); err != nil {
		t.Fatal(err)
	}

	var r deviceLoadResponse
	if code, body := f.get(t, "/devices/d1/load?range=7d", &r); code != http.StatusOK {
		t.Fatalf("status %d %s", code, body)
	}
	if r.Resolution != "1h" || r.StepSeconds != 3600 || len(r.Points) != 7*24 || !r.To.Equal(f.hour) {
		t.Fatalf("header = resolution %s step %d points %d to %v", r.Resolution, r.StepSeconds, len(r.Points), r.To)
	}
	p := r.Points[len(r.Points)-2]
	if !p.TS.Equal(h2) || p.Gap != nil || *p.CoveredS != 3600 || *p.CPUAvg != 20 || *p.CPUMax != 20 || *p.CPUP95 != 20 ||
		*p.FreeMemory != int64(30<<20)-59 || *p.NMSCmds != 360 {
		t.Errorf("covered hour = %s", mustJSON(p))
	}
	if !p.Rebooted || p.CPUPost == nil || *p.CPUPost != 70 || *p.NMSWallMs != 12000 || *p.NMSRows != 6000 || *p.NMSReconnects != 1 {
		t.Errorf("covered hour extras = %s, want the reboot, the post-poll sample and the wall time from the minutes", mustJSON(p))
	}
	p = r.Points[len(r.Points)-1]
	if p.Gap == nil || *p.Gap != "not_covered" || *p.CoveredS != 0 || p.CPUAvg != nil || *p.NMSCmds != 2 || *p.NMSTimeouts != 2 {
		t.Errorf("uncovered hour = %s", mustJSON(p))
	}
	if p = r.Points[0]; p.Gap == nil || *p.Gap != "no_data" || p.CoveredS != nil {
		t.Errorf("hour before any telemetry = %s", mustJSON(p))
	}
	if r.Summary.Reboots != 1 || r.Summary.CPUAvg == nil || *r.Summary.CPUAvg != 20 {
		t.Errorf("summary = %+v", r.Summary)
	}
}

func TestDeviceLoadParams(t *testing.T) {
	f := newLoadFixture(t)
	var r deviceLoadResponse
	if code, _ := f.get(t, "/devices/d1/load", &r); code != http.StatusOK || r.Range != "1h" {
		t.Errorf("default range: %d %q", code, r.Range)
	}
	// No telemetry at all: a full series of explicit gaps, null summary.
	if len(r.Points) != 60 || r.CoverageFrom != nil || r.Summary.CPUAvg != nil || r.Summary.CoveredPct != 0 {
		t.Errorf("empty device = %d points, coverage_from %v, summary %+v", len(r.Points), r.CoverageFrom, r.Summary)
	}
	for path, want := range map[string]int{
		"/devices/d1/load?range=6h":   http.StatusOK,
		"/devices/d1/load?range=24h":  http.StatusOK,
		"/devices/d1/load?range=30d":  http.StatusOK,
		"/devices/d1/load?range=2d":   http.StatusBadRequest,
		"/devices/d1/load?range=live": http.StatusBadRequest,
		"/devices/nope/load":          http.StatusNotFound,
	} {
		if code, body := f.get(t, path, nil); code != want {
			t.Errorf("GET %s = %d %s, want %d", path, code, body, want)
		}
	}
}

// The load view costs the device nothing: its pooled session is a tripwire
// that records any command, and it must see none, for any role and range.
func TestDeviceLoadNeverTouchesTheDevice(t *testing.T) {
	f := newLoadFixture(t)
	pool := routeros.NewPool(false)
	fake := routerostest.NewDevice(t)
	fake.Dial(t, pool, "d1")
	f.s.pool = pool
	f.s.cfg = &config.Config{Pollers: config.PollersLocal}
	if err := queries.UpsertDeviceMinutes(f.db, []queries.DeviceMinute{
		{DeviceID: "d1", Bucket: f.end.Add(-time.Minute), PolledS: 60, CPUSum: 5, CPUSamples: 1, CPUMax: 5},
	}); err != nil {
		t.Fatal(err)
	}

	for _, role := range []string{"admin", "viewer"} {
		h := f.router(role)
		for _, rng := range []string{"1h", "6h", "24h", "7d", "30d"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/devices/d1/load?range="+rng, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("%s GET range=%s = %d", role, rng, rec.Code)
			}
		}
	}
	if cmds := fake.Commands(); len(cmds) != 0 {
		t.Fatalf("the load view sent device commands: %v", cmds)
	}
	if n := fake.Conns(); n != 1 {
		t.Fatalf("connections = %d, want only the test dial", n)
	}
}
