package api

import (
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
	return mailer.ConfigFromSettings(func(key string) string {
		v, err := queries.GetSetting(s.db, key)
		if err != nil {
			return ""
		}
		return v
	}, s.cfg.Mailer())
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
