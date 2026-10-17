package api

import (
	"net/http"

	"github.com/mikrotik-nms/backend/internal/poller"
	"github.com/mikrotik-nms/backend/internal/topology"
)

func (s *Server) handleGetTopology(w http.ResponseWriter, r *http.Request) {
	// The same staleness rule as the topology loop's own rebuild — each
	// neighbor row judged by its reporting device's cadence, a paused
	// device's rows not ageing — or a page load would mark links down that
	// the loop keeps up.
	builder := topology.NewBuilder(s.db).UseNeighborAge(poller.NeighborAge(poller.NewCadence(s.db, s.cfg)))
	graph, err := builder.Build()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build topology")
		return
	}
	writeJSON(w, http.StatusOK, graph)
}
