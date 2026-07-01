package api

import (
	"net/http"

	"github.com/mikrotik-nms/backend/internal/poller"
)

// handleGetTrafficLinks returns a one-shot snapshot of per-link throughput for
// the network map's initial paint. The continuous feed arrives on the
// "topology.traffic" WS topic (see poller.LiveTrafficCollector).
func (s *Server) handleGetTrafficLinks(w http.ResponseWriter, r *http.Request) {
	c := poller.NewLiveTrafficCollector(s.db, s.pool, s.hub)
	links := c.Collect(r.Context())
	writeJSON(w, http.StatusOK, map[string]interface{}{"links": links})
}
