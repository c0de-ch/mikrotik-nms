package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func (s *Server) handleGetTraffic(w http.ResponseWriter, r *http.Request) {
	deviceID := r.PathValue("deviceId")
	ifaceName := r.PathValue("iface")

	// Parse time range
	now := time.Now()
	from := now.Add(-1 * time.Hour)
	to := now

	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	}

	limit := 1000
	if v := r.URL.Query().Get("limit"); v != "" {
		if l, err := strconv.Atoi(v); err == nil && l > 0 && l <= 10000 {
			limit = l
		}
	}

	samples, err := queries.GetTrafficSamples(s.db, deviceID, ifaceName, from, to, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get traffic samples")
		return
	}
	if samples == nil {
		samples = []queries.TrafficSample{}
	}
	writeJSON(w, http.StatusOK, samples)
}
