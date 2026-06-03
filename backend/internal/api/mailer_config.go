package api

import (
	"strconv"
	"strings"

	"github.com/mikrotik-nms/backend/internal/database/queries"
	"github.com/mikrotik-nms/backend/internal/mailer"
)

// resolveMailerConfig builds the SMTP config from admin-editable app_settings,
// falling back to the env-based config for any field left unset. This makes SMTP
// runtime-configurable from the Settings page (no restart) while keeping env as
// the bootstrap/default source. The SMTP password lives under the "smtp_password"
// key, which isSecretSettingKey redacts from non-admin reads; the reset link host
// is always taken from PublicBaseURL (env), never a request Host.
func (s *Server) resolveMailerConfig() mailer.Config {
	get := func(key, def string) string {
		if v, err := queries.GetSetting(s.db, key); err == nil {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
		return def
	}

	port := s.cfg.SMTPPort
	if v := get("smtp_port", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			port = n
		}
	}
	skip := s.cfg.SMTPTLSSkipVerify
	if v := get("smtp_tls_skip_verify", ""); v != "" {
		skip = v == "true" || v == "1"
	}

	return mailer.Config{
		SMTPHost:          get("smtp_host", s.cfg.SMTPHost),
		SMTPPort:          port,
		SMTPUser:          get("smtp_user", s.cfg.SMTPUser),
		SMTPPass:          get("smtp_password", s.cfg.SMTPPass),
		SMTPFrom:          get("smtp_from_address", s.cfg.SMTPFrom),
		SMTPTLSMode:       get("smtp_tls_mode", s.cfg.SMTPTLSMode),
		SMTPTLSSkipVerify: skip,
		PublicBaseURL:     s.cfg.PublicBaseURL,
	}
}

// effectiveMailer returns the injected mailer when one is set (tests), otherwise
// a freshly-resolved mailer built from current settings+env (production) so a
// Settings change takes effect on the next request without a restart.
func (s *Server) effectiveMailer() mailer.Sender {
	if s.mailer != nil {
		return s.mailer
	}
	return mailer.New(s.resolveMailerConfig())
}
