package api

import (
	"context"
	"net/http"
	"time"

	"github.com/mikrotik-nms/backend/internal/poller"
)

// handleGetTrafficLinks returns a one-shot snapshot of per-link throughput for
// the network map's initial paint. The continuous feed arrives on the
// "topology.traffic" WS topic (see poller.LiveTrafficCollector).
func (s *Server) handleGetTrafficLinks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	c := poller.NewLiveTrafficCollector(s.db, s.pool, s.hub)
	writeJSON(w, http.StatusOK, map[string]interface{}{"links": c.Collect(ctx)})
}
