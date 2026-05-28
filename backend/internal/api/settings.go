package api

import (
	"net/http"
	"strings"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

// isSecretSettingKey reports whether a settings value is a credential that must
// not be exposed to non-admin users (e.g. opnsense_api_secret / api_key).
func isSecretSettingKey(key string) bool {
	k := strings.ToLower(key)
	return strings.Contains(k, "secret") ||
		strings.Contains(k, "api_key") ||
		strings.Contains(k, "password") ||
		strings.Contains(k, "token")
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := queries.GetAllSettings(s.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get settings")
		return
	}
	// Redact integration secrets for non-admins (this endpoint is readable by
	// any authenticated user, including viewers).
	if user := auth.UserFromContext(r.Context()); user == nil || user.Role != "admin" {
		for k := range settings {
			if isSecretSettingKey(k) && settings[k] != "" {
				settings[k] = ""
			}
		}
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
		"offline_threshold_seconds": true,
		"info_interval":             true,
		"retention_days":            true,
		"dark_mode":                 true,
		"kea_url":                   true,
		"port_monitor_enabled":      true,
		"port_monitor_filter":       true,
		"port_flap_threshold":       true,
		"port_flap_window_seconds":  true,
		"tcn_storm_threshold":       true,
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
