package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/mikrotik-nms/backend/internal/routeros"
)

func (s *Server) handleDiscoverDevices(w http.ResponseWriter, r *http.Request) {
	durationSec := 10
	if v := r.URL.Query().Get("duration"); v != "" {
		if d, err := strconv.Atoi(v); err == nil && d >= 1 && d <= 30 {
			durationSec = d
		}
	}

	devices, err := routeros.ScanMNDP(time.Duration(durationSec) * time.Second)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "discovery failed: "+err.Error())
		return
	}
	if devices == nil {
		devices = []routeros.DiscoveredDevice{}
	}

	writeJSON(w, http.StatusOK, devices)
}
