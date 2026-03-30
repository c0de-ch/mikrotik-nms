package api

import (
	"net/http"

	"github.com/mikrotik-nms/backend/internal/topology"
)

func (s *Server) handleGetTopology(w http.ResponseWriter, r *http.Request) {
	builder := topology.NewBuilder(s.db)
	graph, err := builder.Build()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build topology")
		return
	}
	writeJSON(w, http.StatusOK, graph)
}
