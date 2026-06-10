package api

import (
	"net/http"
	"strconv"
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
	// Synthetic read-only key: lets the admin UI show whether self-service
	// password reset is actually able to send mail. Not persisted — computed
	// from the resolved settings+env config at request time.
	settings["smtp_configured"] = strconv.FormatBool(s.effectiveMailer().Enabled())
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	allowed := map[string]bool{
		"health_interval":               true,
		"topology_interval":             true,
		"firmware_interval":             true,
		"wifi_interval":                 true,
		"client_discovery_interval":     true,
		"connectivity_interval":         true,
		"connectivity_ping_count":       true,
		"speedtest_interval":            true,
		"traceroute_loss_threshold":     true,
		"network_health_interval":       true,
		"offline_threshold_seconds":     true,
		"client_inactive_after_seconds": true,
		"info_interval":                 true,
		"retention_days":                true,
		"dark_mode":                     true,
		"kea_url":                       true,
		"port_monitor_enabled":          true,
		"port_monitor_filter":           true,
		"port_flap_threshold":           true,
		"port_flap_window_seconds":      true,
		"tcn_storm_threshold":           true,
		"auto_follow_ip":                true,
		"opnsense_url":                  true,
		"opnsense_api_key":              true,
		"opnsense_api_secret":           true,
		"opnsense_verify_tls":           true,
		// Self-service password reset / SMTP. "pwreset_enabled" is named to avoid
		// the isSecretSettingKey "password" substring so it stays visible. The
		// "smtp_password" key DOES contain "password" so it is redacted from
		// non-admin reads. These override the env-based SMTP config at run time
		// (empty value = fall back to env); see resolveMailerConfig.
		"smtp_from_address":    true,
		"pwreset_enabled":      true,
		"smtp_host":            true,
		"smtp_port":            true,
		"smtp_user":            true,
		"smtp_password":        true,
		"smtp_tls_mode":        true,
		"smtp_tls_skip_verify": true,
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
