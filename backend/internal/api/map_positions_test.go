package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func mapRouter(s *Server, role string) http.Handler {
	r := chi.NewRouter()
	s.mountMapRoutes(r)
	return withUser(role, r)
}

func doMap(t *testing.T, h http.Handler, method, body string) (*httptest.ResponseRecorder, mapPositionsResponse) {
	t.Helper()
	req := httptest.NewRequest(method, "/topology/positions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out mapPositionsResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v (%s)", method, err, rec.Body.String())
		}
	}
	return rec, out
}

// An admin's saved layout is what every user reads back; a save replaces it
// whole, and a reset empties it.
func TestMapPositionsSaveReadReset(t *testing.T) {
	s := &Server{db: newTestDB(t)}
	admin, viewer := mapRouter(s, "admin"), mapRouter(s, "viewer")

	if rec, out := doMap(t, viewer, http.MethodGet, ""); rec.Code != http.StatusOK || len(out.Positions) != 0 || out.UpdatedAt != nil {
		t.Fatalf("empty GET = %d %+v", rec.Code, out)
	}
	rec, _ := doMap(t, admin, http.MethodPut, `{"positions":{"dev-1":{"x":10.5,"y":-3},"gw:10.0.0.254":{"x":0,"y":120},"internet":{"x":0,"y":240}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	rec, out := doMap(t, viewer, http.MethodGet, "")
	if rec.Code != http.StatusOK || len(out.Positions) != 3 || out.Positions["dev-1"].X != 10.5 || out.Positions["gw:10.0.0.254"].Y != 120 || out.UpdatedAt == nil {
		t.Fatalf("GET after save = %d %+v", rec.Code, out)
	}
	// A save replaces the layout: a node left out is placed by the layout again.
	if rec, _ := doMap(t, admin, http.MethodPut, `{"positions":{"dev-1":{"x":1,"y":2}}}`); rec.Code != http.StatusOK {
		t.Fatalf("second PUT = %d", rec.Code)
	}
	if _, out := doMap(t, viewer, http.MethodGet, ""); len(out.Positions) != 1 || out.Positions["dev-1"] != (queries.MapPosition{X: 1, Y: 2}) {
		t.Fatalf("after replace = %+v", out.Positions)
	}
	if rec, _ := doMap(t, admin, http.MethodDelete, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", rec.Code)
	}
	if _, out := doMap(t, viewer, http.MethodGet, ""); len(out.Positions) != 0 || out.UpdatedAt != nil {
		t.Fatalf("after reset = %+v", out)
	}
}

func TestMapPositionsAdminOnlyAndValidated(t *testing.T) {
	s := &Server{db: newTestDB(t)}
	viewer, admin := mapRouter(s, "viewer"), mapRouter(s, "admin")
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		if rec, _ := doMap(t, viewer, m, `{"positions":{"a":{"x":1,"y":1}}}`); rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s = %d, want 403", m, rec.Code)
		}
	}
	for _, body := range []string{
		`not json`,
		`{"positions":{"":{"x":1,"y":1}}}`,
		`{"positions":{"a":{"x":1e300,"y":1}}}`,
		`{"positions":{"` + strings.Repeat("x", maxMapNodeIDLen+1) + `":{"x":1,"y":1}}}`,
	} {
		if rec, _ := doMap(t, admin, http.MethodPut, body); rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %.40s = %d, want 400", body, rec.Code)
		}
	}
	if _, out := doMap(t, viewer, http.MethodGet, ""); len(out.Positions) != 0 {
		t.Errorf("a refused save stored %+v", out.Positions)
	}
}
