package api

import (
	"math"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// The saved map layout is small: one row per node of /topology.
const (
	maxMapPositions  = 5000
	maxMapNodeIDLen  = 512
	maxMapCoordinate = 1e7
)

// mountMapRoutes serves the network map's saved layout: GET for every
// logged-in user, PUT and DELETE for admins.
func (s *Server) mountMapRoutes(r chi.Router) {
	r.Get("/topology/positions", s.handleGetMapPositions)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireRole("admin"))
		r.Put("/topology/positions", s.handlePutMapPositions)
		r.Delete("/topology/positions", s.handleDeleteMapPositions)
	})
}

type mapPositionsResponse struct {
	Positions map[string]queries.MapPosition `json:"positions"`
	UpdatedAt *time.Time                     `json:"updated_at"`
}

// handleGetMapPositions: GET /topology/positions — the layout an admin saved
// for the network map, for every logged-in user. Database only.
func (s *Server) handleGetMapPositions(w http.ResponseWriter, r *http.Request) {
	pos, at, err := queries.ListMapPositions(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load map positions")
		return
	}
	out := mapPositionsResponse{Positions: pos}
	if !at.IsZero() {
		out.UpdatedAt = &at
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutMapPositions: PUT /topology/positions (admin) — replaces the
// saved layout with the body's positions (every node the map shows).
func (s *Server) handlePutMapPositions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var body struct {
		Positions map[string]queries.MapPosition `json:"positions"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.Positions) > maxMapPositions {
		writeError(w, http.StatusBadRequest, "too many positions")
		return
	}
	for id, p := range body.Positions {
		if id == "" || len(id) > maxMapNodeIDLen {
			writeError(w, http.StatusBadRequest, "invalid node id")
			return
		}
		if !finiteCoord(p.X) || !finiteCoord(p.Y) {
			writeError(w, http.StatusBadRequest, "invalid position for "+id)
			return
		}
	}
	by := ""
	if u := auth.UserFromContext(r.Context()); u != nil {
		by = u.Username
	}
	now := time.Now().UTC()
	if err := queries.ReplaceMapPositions(s.db, body.Positions, by, now); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save map positions")
		return
	}
	out := mapPositionsResponse{Positions: body.Positions}
	if out.Positions == nil {
		out.Positions = map[string]queries.MapPosition{}
	}
	if len(body.Positions) > 0 {
		out.UpdatedAt = &now
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteMapPositions: DELETE /topology/positions (admin) — forgets the
// saved layout; the map lays itself out again.
func (s *Server) handleDeleteMapPositions(w http.ResponseWriter, r *http.Request) {
	if err := queries.ClearMapPositions(s.db); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to reset map positions")
		return
	}
	s.audit(r, "map_layout_reset", "", "")
	w.WriteHeader(http.StatusNoContent)
}

func finiteCoord(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) <= maxMapCoordinate
}
