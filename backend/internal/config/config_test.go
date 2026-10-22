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

func TestFlowListen(t *testing.T) {
	prevSecret := os.Getenv("MIKROTIK_NMS_JWT_SECRET")
	os.Setenv("MIKROTIK_NMS_JWT_SECRET", "test-secret")
	t.Cleanup(func() { os.Setenv("MIKROTIK_NMS_JWT_SECRET", prevSecret) })
	for _, k := range []string{"MIKROTIK_NMS_FLOW_LISTEN", "MIKROTIK_NMS_FLOW_CAPTURE_DIR"} {
		prev, had := os.LookupEnv(k)
		os.Unsetenv(k)
		key := k
		t.Cleanup(func() {
			if had {
				os.Setenv(key, prev)
			} else {
				os.Unsetenv(key)
			}
		})
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.FlowListen) != 0 || cfg.FlowCaptureDir != "" {
		t.Fatalf("defaults: FlowListen = %q, FlowCaptureDir = %q; want collector off", cfg.FlowListen, cfg.FlowCaptureDir)
	}

	cases := []struct {
		in   string
		want []string
	}{
		{":2055", []string{":2055"}},
		{" :2055 , bogus, :0, :2055", []string{":2055"}},
		{"0.0.0.0:2055,:6343", []string{"0.0.0.0:2055", ":6343"}},
		{"[::1]:2055, :65536, :http", []string{"[::1]:2055"}},
		{" , ", nil},
	}
	for _, c := range cases {
		os.Setenv("MIKROTIK_NMS_FLOW_LISTEN", c.in)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load(%q): %v", c.in, err)
		}
		if len(cfg.FlowListen) != len(c.want) {
			t.Errorf("FlowListen(%q) = %q, want %q", c.in, cfg.FlowListen, c.want)
			continue
		}
		for i := range c.want {
			if cfg.FlowListen[i] != c.want[i] {
				t.Errorf("FlowListen(%q) = %q, want %q", c.in, cfg.FlowListen, c.want)
			}
		}
	}

	os.Setenv("MIKROTIK_NMS_FLOW_CAPTURE_DIR", "/tmp/cap")
	if cfg, _ := Load(); cfg.FlowCaptureDir != "/tmp/cap" {
		t.Errorf("FlowCaptureDir = %q", cfg.FlowCaptureDir)
	}
}

func TestPollersAndTargetCIDRs(t *testing.T) {
	t.Setenv("MIKROTIK_NMS_JWT_SECRET", "test-secret")
	t.Setenv("MIKROTIK_NMS_POLLERS", "")
	t.Setenv("MIKROTIK_NMS_ALLOWED_TARGET_CIDRS", "")

	// The code default is off: no device I/O, and no fence is needed.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PollersEnabled() || cfg.PollersMode() != PollersOff || len(cfg.AllowedTargetCIDRs) != 0 {
		t.Fatalf("defaults: Pollers=%q cidrs=%v; want off and none", cfg.Pollers, cfg.AllowedTargetCIDRs)
	}
	if got := cfg.AllowedTargetCIDRStrings(); got == nil || len(got) != 0 {
		t.Errorf("AllowedTargetCIDRStrings() = %#v, want an empty non-nil slice", got)
	}
	if (&Config{}).PollersEnabled() {
		t.Error("a zero Config must not poll")
	}

	// local without a fence refuses to start.
	t.Setenv("MIKROTIK_NMS_POLLERS", "local")
	if _, err := Load(); err == nil {
		t.Fatal("Load with MIKROTIK_NMS_POLLERS=local and no CIDRs should fail")
	}

	// local with a fence: parsed, normalised, deduplicated.
	t.Setenv("MIKROTIK_NMS_POLLERS", " Local ")
	t.Setenv("MIKROTIK_NMS_ALLOWED_TARGET_CIDRS", " 192.0.2.5/24, 192.0.2.0/24 ,2001:db8::/48, 198.51.100.7, ::ffff:203.0.113.0/120, fe80::1%eth0 ")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.PollersEnabled() || cfg.PollersMode() != PollersLocal {
		t.Errorf("Pollers = %q, want local", cfg.Pollers)
	}
	want := []string{"192.0.2.0/24", "2001:db8::/48", "198.51.100.7/32", "203.0.113.0/24", "fe80::1/128"}
	got := cfg.AllowedTargetCIDRStrings()
	if len(got) != len(want) {
		t.Fatalf("AllowedTargetCIDRs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AllowedTargetCIDRs[%d] = %s, want %s", i, got[i], want[i])
		}
	}

	// Mistakes are errors, not silently dropped entries.
	for _, bad := range []string{"192.0.2.0/33", "not-an-address", "10.0.0.0/8,oops", "::ffff:10.0.0.0/8"} {
		t.Setenv("MIKROTIK_NMS_ALLOWED_TARGET_CIDRS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("Load with MIKROTIK_NMS_ALLOWED_TARGET_CIDRS=%q should fail", bad)
		}
	}

	// Anything but local/off is refused rather than guessed.
	t.Setenv("MIKROTIK_NMS_ALLOWED_TARGET_CIDRS", "192.0.2.0/24")
	t.Setenv("MIKROTIK_NMS_POLLERS", "remote")
	if _, err := Load(); err == nil {
		t.Error("Load with MIKROTIK_NMS_POLLERS=remote should fail")
	}

	// off with a fence configured is fine (the fence is simply unused).
	t.Setenv("MIKROTIK_NMS_POLLERS", "off")
	if cfg, err := Load(); err != nil || cfg.PollersEnabled() {
		t.Errorf("Load off+cidrs: cfg=%+v err=%v", cfg, err)
	}
}
