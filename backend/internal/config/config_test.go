package config

import (
	"os"
	"testing"
)

func TestLoad_WithJWTSecret(t *testing.T) {
	// Save and restore environment
	orig := os.Getenv("MIKROTIK_NMS_JWT_SECRET")
	defer os.Setenv("MIKROTIK_NMS_JWT_SECRET", orig)

	os.Setenv("MIKROTIK_NMS_JWT_SECRET", "my-test-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.JWTSecret != "my-test-secret" {
		t.Errorf("JWTSecret = %q, want %q", cfg.JWTSecret, "my-test-secret")
	}
}

func TestLoad_MissingJWTSecret(t *testing.T) {
	// Save and restore environment
	orig := os.Getenv("MIKROTIK_NMS_JWT_SECRET")
	defer os.Setenv("MIKROTIK_NMS_JWT_SECRET", orig)

	os.Unsetenv("MIKROTIK_NMS_JWT_SECRET")

	_, err := Load()
	if err == nil {
		t.Error("expected error when JWT_SECRET is missing, got nil")
	}
}

func TestLoad_Defaults(t *testing.T) {
	// Save and restore environment
	orig := os.Getenv("MIKROTIK_NMS_JWT_SECRET")
	defer os.Setenv("MIKROTIK_NMS_JWT_SECRET", orig)

	os.Setenv("MIKROTIK_NMS_JWT_SECRET", "test-secret")

	// Clear any overrides for fields we want to check defaults on
	for _, key := range []string{
		"MIKROTIK_NMS_LISTEN",
		"MIKROTIK_NMS_DB_PATH",
		"MIKROTIK_NMS_DEFAULT_ROS_USER",
		"MIKROTIK_NMS_DEFAULT_ROS_PORT",
	} {
		prev := os.Getenv(key)
		os.Unsetenv(key)
		defer os.Setenv(key, prev)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":8080")
	}
	if cfg.DBPath != "mikrotik-nms.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "mikrotik-nms.db")
	}
	if cfg.DefaultROSUser != "admin" {
		t.Errorf("DefaultROSUser = %q, want %q", cfg.DefaultROSUser, "admin")
	}
	if cfg.DefaultROSPort != 8728 {
		t.Errorf("DefaultROSPort = %d, want %d", cfg.DefaultROSPort, 8728)
	}
}

func TestLoad_CustomEnvValues(t *testing.T) {
	// Save and restore environment
	envVars := map[string]string{
		"MIKROTIK_NMS_JWT_SECRET":       "custom-secret",
		"MIKROTIK_NMS_LISTEN":           ":9090",
		"MIKROTIK_NMS_DB_PATH":          "/tmp/custom.db",
		"MIKROTIK_NMS_DEFAULT_ROS_USER": "customuser",
		"MIKROTIK_NMS_DEFAULT_ROS_PORT": "8729",
		"MIKROTIK_NMS_DEFAULT_ROS_TLS":  "true",
		"MIKROTIK_NMS_RETENTION_DAYS":   "30",
	}
	originals := make(map[string]string)
	for key := range envVars {
		originals[key] = os.Getenv(key)
	}
	defer func() {
		for key, val := range originals {
			os.Setenv(key, val)
		}
	}()

	for key, val := range envVars {
		os.Setenv(key, val)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Listen != ":9090" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":9090")
	}
	if cfg.DBPath != "/tmp/custom.db" {
		t.Errorf("DBPath = %q, want %q", cfg.DBPath, "/tmp/custom.db")
	}
	if cfg.DefaultROSUser != "customuser" {
		t.Errorf("DefaultROSUser = %q, want %q", cfg.DefaultROSUser, "customuser")
	}
	if cfg.DefaultROSPort != 8729 {
		t.Errorf("DefaultROSPort = %d, want %d", cfg.DefaultROSPort, 8729)
	}
	if !cfg.DefaultROSTLS {
		t.Error("DefaultROSTLS should be true")
	}
	if cfg.RetentionDays != 30 {
		t.Errorf("RetentionDays = %d, want %d", cfg.RetentionDays, 30)
	}
}

// smtpEnvKeys are cleared before each SMTP test so ambient env can't leak in.
var smtpEnvKeys = []string{
	"MIKROTIK_NMS_SMTP_HOST",
	"MIKROTIK_NMS_SMTP_PORT",
	"MIKROTIK_NMS_SMTP_USER",
	"MIKROTIK_NMS_SMTP_PASS",
	"MIKROTIK_NMS_SMTP_FROM",
	"MIKROTIK_NMS_SMTP_TLS_MODE",
	"MIKROTIK_NMS_SMTP_TLS_SKIP_VERIFY",
	"MIKROTIK_NMS_PUBLIC_BASE_URL",
	"MIKROTIK_NMS_PASSWORD_RESET_TTL",
}

func withCleanSMTPEnv(t *testing.T) {
	t.Helper()
	prevSecret := os.Getenv("MIKROTIK_NMS_JWT_SECRET")
	os.Setenv("MIKROTIK_NMS_JWT_SECRET", "test-secret")
	t.Cleanup(func() { os.Setenv("MIKROTIK_NMS_JWT_SECRET", prevSecret) })
	for _, k := range smtpEnvKeys {
		prev := os.Getenv(k)
		os.Unsetenv(k)
		key := k
		t.Cleanup(func() { os.Setenv(key, prev) })
	}
}

func TestSMTPDefaultsAndFromFallback(t *testing.T) {
	withCleanSMTPEnv(t)
	os.Setenv("MIKROTIK_NMS_SMTP_HOST", "smtp.example.com")
	os.Setenv("MIKROTIK_NMS_SMTP_USER", "relay@example.com")
	os.Setenv("MIKROTIK_NMS_PUBLIC_BASE_URL", "https://nms.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SMTPPort != 587 {
		t.Errorf("SMTPPort = %d, want default 587", cfg.SMTPPort)
	}
	if cfg.SMTPTLSMode != "starttls" {
		t.Errorf("SMTPTLSMode = %q, want default starttls", cfg.SMTPTLSMode)
	}
	if cfg.PasswordResetTTL.Hours() != 1 {
		t.Errorf("PasswordResetTTL = %v, want default 1h", cfg.PasswordResetTTL)
	}
	// From falls back to user when SMTP_FROM is unset.
	if cfg.SMTPFrom != "relay@example.com" {
		t.Errorf("SMTPFrom = %q, want fallback to SMTP_USER", cfg.SMTPFrom)
	}
	if !cfg.SMTPEnabled() {
		t.Error("SMTPEnabled() should be true with host+port+base url")
	}
}

func TestSMTPEnabledRequiresBaseURL(t *testing.T) {
	withCleanSMTPEnv(t)
	os.Setenv("MIKROTIK_NMS_SMTP_HOST", "smtp.example.com")
	// no PUBLIC_BASE_URL

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load should not hard-fail when base URL is missing: %v", err)
	}
	if cfg.SMTPEnabled() {
		t.Error("SMTPEnabled() should be false without a public base URL")
	}
}

func TestSMTPDisabledWhenHostUnset(t *testing.T) {
	withCleanSMTPEnv(t)
	os.Setenv("MIKROTIK_NMS_PUBLIC_BASE_URL", "https://nms.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SMTPEnabled() {
		t.Error("SMTPEnabled() should be false without an SMTP host")
	}
}
