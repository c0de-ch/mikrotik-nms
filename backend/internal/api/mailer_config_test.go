package api

import (
	"testing"
	"time"

	"github.com/mikrotik-nms/backend/internal/config"
	"github.com/mikrotik-nms/backend/internal/database/queries"
)

func TestResolveMailerConfig(t *testing.T) {
	db := newTestDB(t)
	cfg := &config.Config{
		SMTPHost:          "env-host.example.com",
		SMTPPort:          587,
		SMTPUser:          "env-user",
		SMTPPass:          "env-pass",
		SMTPFrom:          "env-from@example.com",
		SMTPTLSMode:       "starttls",
		SMTPTLSSkipVerify: false,
		PublicBaseURL:     "https://nms.example.com",
		PasswordResetTTL:  time.Hour,
	}
	s := &Server{db: db, cfg: cfg}
	must := func(k, v string) {
		t.Helper()
		if err := queries.SetSetting(db, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}

	// 1) No settings rows -> entirely env.
	c := s.resolveMailerConfig()
	if c.SMTPHost != "env-host.example.com" || c.SMTPUser != "env-user" || c.SMTPPass != "env-pass" ||
		c.SMTPPort != 587 || c.SMTPFrom != "env-from@example.com" || c.SMTPTLSMode != "starttls" || c.SMTPTLSSkipVerify {
		t.Fatalf("env fallback wrong: %+v", c)
	}

	// 2) Settings override env where set.
	must("smtp_host", "set-host.example.com")
	must("smtp_port", "2525")
	must("smtp_user", "set-user")
	must("smtp_password", "set-pass")
	must("smtp_from_address", "set-from@example.com")
	must("smtp_tls_mode", "tls")
	must("smtp_tls_skip_verify", "true")
	c = s.resolveMailerConfig()
	if c.SMTPHost != "set-host.example.com" || c.SMTPPort != 2525 || c.SMTPUser != "set-user" ||
		c.SMTPPass != "set-pass" || c.SMTPFrom != "set-from@example.com" || c.SMTPTLSMode != "tls" || !c.SMTPTLSSkipVerify {
		t.Fatalf("settings override wrong: %+v", c)
	}

	// 3) Blank/whitespace settings fall back to env (never blank the value).
	must("smtp_user", "   ")
	must("smtp_port", "")
	c = s.resolveMailerConfig()
	if c.SMTPUser != "env-user" {
		t.Fatalf("blank user setting should fall back to env, got %q", c.SMTPUser)
	}
	if c.SMTPPort != 587 {
		t.Fatalf("blank port setting should fall back to env 587, got %d", c.SMTPPort)
	}

	// PublicBaseURL always comes from env (never a setting).
	if c.PublicBaseURL != "https://nms.example.com" {
		t.Fatalf("base url should come from env, got %q", c.PublicBaseURL)
	}
}
