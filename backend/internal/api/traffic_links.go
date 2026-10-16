package api

import (
	"net/http"
	"time"

	"github.com/mikrotik-nms/backend/internal/poller"
)

// handleGetTrafficLinks returns per-link throughput for the network map's
// first paint, derived from the port-stats snapshot (no device calls, so an
// open map costs a weak switch nothing). The continuous feed is the
// "topology.traffic" WS topic, built the same way once per counter cycle.
func (s *Server) handleGetTrafficLinks(w http.ResponseWriter, r *http.Request) {
	payload, err := poller.LoadLinkTraffic(s.db, s.portSnapshot(), time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load links")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
