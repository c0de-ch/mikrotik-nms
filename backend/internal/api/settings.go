package api

import (
	"net/http"

	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := queries.GetAllSettings(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get settings")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	allowed := map[string]bool{
		"health_interval":           true,
		"topology_interval":         true,
		"firmware_interval":         true,
		"wifi_interval":             true,
		"client_discovery_interval": true,
		"network_health_interval":   true,
		"retention_days":            true,
		"dark_mode":                 true,
		"kea_url":                   true,
		"port_monitor_enabled":      true,
		"port_monitor_filter":       true,
		"port_flap_threshold":       true,
		"port_flap_window_seconds":  true,
		"opnsense_url":              true,
		"opnsense_api_key":          true,
		"opnsense_api_secret":       true,
		"opnsense_verify_tls":       true,
	}

	for key, value := range req {
		if !allowed[key] {
			continue
		}
		if err := queries.SetSetting(s.db, key, value); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to save setting: "+key)
			return
		}
	}

	// Return updated settings
	settings, err := queries.GetAllSettings(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get settings")
		return
	}
	writeJSON(w, http.StatusOK, settings)
}
