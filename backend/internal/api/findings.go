package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// handleListDeviceFindings returns what the pollers noticed about a device
// (a second API poller, login failures, …), most recent first. Readable by
// every logged-in user, like the rest of the device detail.
func (s *Server) handleListDeviceFindings(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	dev, err := queries.GetDevice(s.db, id)
	if err != nil || dev == nil {
		writeError(w, http.StatusNotFound, "device not found")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	findings, err := queries.ListDeviceFindings(s.db, id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list findings")
		return
	}
	writeJSON(w, http.StatusOK, findings)
}
