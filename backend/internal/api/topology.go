package api

import (
	"net/http"

	"github.com/mikrotik-nms/backend/internal/poller"
)

func (s *Server) handleGetTopology(w http.ResponseWriter, r *http.Request) {
	// The same staleness rules as the topology loop's own rebuild — each
	// neighbor and egress row judged by its device's cadence, a paused
	// device's rows not ageing — or a page load would mark links down that
	// the loop keeps up.
	graph, err := poller.NewTopologyBuilder(s.db, s.cadenceSource()).Build()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build topology")
		return
	}
	writeJSON(w, http.StatusOK, graph)
}
