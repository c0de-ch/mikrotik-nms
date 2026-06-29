package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/mikrotik-nms/backend/internal/auth"
	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/mailer"
	"github.com/mikrotik-nms/backend/internal/opnsense"
	"github.com/mikrotik-nms/backend/internal/telemetry"
)

// opnsenseSourceKey matches the app_settings keys for an OPNsense DHCP source —
// the primary (opnsense_url, …) and any number of extra sites (opnsenseN_url, …).
var opnsenseSourceKey = regexp.MustCompile(`^opnsense\d*_(url|api_key|api_secret|verify_tls)$`)

// isOpnsenseSourceKey reports whether key configures an OPNsense DHCP source, so
// the settings endpoint accepts an arbitrary number of them without an explicit
// whitelist entry per instance.
func isOpnsenseSourceKey(key string) bool {
	return opnsenseSourceKey.MatchString(key)
}

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
		// OPNsense instances (opnsense_* primary + opnsenseN_* extra sites) are
		// allowed via isOpnsenseSourceKey below rather than listed here, so any
		// number of sources can be configured. Their _api_key / _api_secret keys
		// keep the isSecretSettingKey substrings, so they're redacted from
		// non-admin reads and from backups like every other credential.
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
		// OpenTelemetry export → lab-observability (Settings → Observability). The
		// OTLP endpoint is a collector gateway that fans out to Loki/Tempo/Grafana.
		// Changes apply on backend restart. "otel_headers" may carry an auth/tenant
		// header; it's admin-only (this endpoint is admin-gated) so it isn't matched
		// by isSecretSettingKey.
		"otel_enabled":      true,
		"otel_endpoint":     true,
		"otel_protocol":     true,
		"otel_insecure":     true,
		"otel_headers":      true,
		"otel_service_name": true,
	}

	for key, value := range req {
		if !allowed[key] && !isOpnsenseSourceKey(key) {
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

// handleTestOpnsense validates an OPNsense Kea connection using the credentials
// supplied in the request body (not the saved ones), so the admin can verify a
// key/secret — and reachability over a site link — before saving. It runs the
// real lease fetch and reports the active-lease count or the failure reason.
// Always 200 with {ok,message}; a credential/connection failure is a result,
// not an HTTP error.
func (s *Server) handleTestOpnsense(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL       string `json:"url"`
		APIKey    string `json:"api_key"`
		APISecret string `json:"api_secret"`
		VerifyTLS bool   `json:"verify_tls"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" || req.APIKey == "" || req.APISecret == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "URL, API key and secret are all required"})
		return
	}
	client := opnsense.New(opnsense.Config{
		URL: req.URL, APIKey: req.APIKey, APISecret: req.APISecret, VerifyTLS: req.VerifyTLS,
	})
	if client == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "could not build client (check URL/key/secret)"})
		return
	}
	leases, err := client.GetLeases()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"leases":  len(leases),
		"message": fmt.Sprintf("Connected — %d active lease(s)", len(leases)),
	})
}

// handleTestOTel verifies the OTLP endpoint from the request body (not the saved
// settings) by exporting one throwaway span, so the admin can confirm reachability
// before saving + restarting. Always 200 with {ok,message}.
func (s *Server) handleTestOTel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint    string `json:"endpoint"`
		Protocol    string `json:"protocol"`
		Insecure    bool   `json:"insecure"`
		Headers     string `json:"headers"`
		ServiceName string `json:"service_name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	if req.Endpoint == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "OTLP endpoint (host:port) is required"})
		return
	}
	if req.ServiceName == "" {
		req.ServiceName = "mikrotik-nms"
	}
	msg, err := telemetry.TestConnection(r.Context(), telemetry.Config{
		Endpoint:    req.Endpoint,
		Protocol:    req.Protocol,
		Insecure:    req.Insecure,
		Headers:     telemetry.ParseHeaders(req.Headers),
		ServiceName: req.ServiceName,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": msg})
}

// handleTestMail sends a one-off test message so the admin can confirm the SMTP
// relay works. It starts from the saved/env config (resolveMailerConfig) and lets
// the request body override any field, so unsaved form values can be tested
// before saving — mirroring handleTestOpnsense / handleTestOTel. The test message
// carries no reset link, so the public-base-URL gate the password-reset flow needs
// is irrelevant here and is force-satisfied. Always 200 with {ok,message}.
func (s *Server) handleTestMail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To         string `json:"to"`
		Host       string `json:"host"`
		Port       string `json:"port"`
		User       string `json:"user"`
		Password   string `json:"password"`
		From       string `json:"from"`
		TLSMode    string `json:"tls_mode"`
		SkipVerify *bool  `json:"skip_verify"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.To = strings.TrimSpace(req.To)
	if req.To == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "a recipient address is required"})
		return
	}

	// Start from saved+env config; empty form fields fall back to it.
	cfg := s.resolveMailerConfig()
	if v := strings.TrimSpace(req.Host); v != "" {
		cfg.SMTPHost = v
	}
	if v := strings.TrimSpace(req.Port); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.SMTPPort = n
		}
	}
	if v := strings.TrimSpace(req.User); v != "" {
		cfg.SMTPUser = v
	}
	if req.Password != "" {
		cfg.SMTPPass = req.Password
	}
	if v := strings.TrimSpace(req.From); v != "" {
		cfg.SMTPFrom = v
	}
	if v := strings.TrimSpace(req.TLSMode); v != "" {
		cfg.SMTPTLSMode = v
	}
	// A *bool so an omitted skip_verify falls back to the saved/env value like
	// every other field above; the UI always sends it, so it normally overrides.
	if req.SkipVerify != nil {
		cfg.SMTPTLSSkipVerify = *req.SkipVerify
	}

	if cfg.SMTPHost == "" || cfg.SMTPPort <= 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "SMTP host and port are required"})
		return
	}
	// A test message needs no reset link, so satisfy the public-base-URL gate
	// (mailer.New requires it to enable sending) without depending on env.
	if cfg.PublicBaseURL == "" {
		cfg.PublicBaseURL = "test"
	}

	subject := "MikroTik NMS — test email"
	text := "This is a test message from MikroTik NMS.\r\n\r\n" +
		"If you received it, your SMTP relay settings are working.\r\n\r\n" +
		"-- MikroTik NMS (automated message, please do not reply)\r\n"
	htmlBody := "<p>This is a <strong>test message</strong> from MikroTik&nbsp;NMS.</p>" +
		"<p>If you received it, your SMTP relay settings are working.</p>"
	if err := mailer.New(cfg).Send(req.To, subject, text, htmlBody); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": fmt.Sprintf("Test email sent to %s", req.To)})
}
