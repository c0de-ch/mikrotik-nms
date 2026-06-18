package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen        string
	DBPath        string
	EncryptionKey string
	JWTSecret     string

	// AllowedOrigins is the CORS / WebSocket origin allow-list. When empty the
	// server reflects any origin (backwards-compatible) but logs a warning.
	AllowedOrigins []string

	// Polling intervals
	HealthInterval        time.Duration
	TopologyInterval      time.Duration
	FirmwareInterval      time.Duration
	RetentionInterval     time.Duration
	NetworkHealthInterval time.Duration

	// Data retention
	RetentionDays int

	// Default RouterOS credentials
	DefaultROSUser string
	DefaultROSPass string
	DefaultROSPort int
	DefaultROSTLS  bool

	// ROSTLSVerify enables RouterOS API-TLS certificate verification. Defaults
	// to false because RouterOS ships self-signed certs; set true once devices
	// present a trusted cert.
	ROSTLSVerify bool

	// SMTP / self-service password reset. SMTPHost is the master switch: when
	// empty the feature is safe-disabled. SMTPPass is a secret and is NEVER
	// logged. PublicBaseURL is REQUIRED to actually send (links are built only
	// from it, never from the request Host) — when SMTPHost is set but it is
	// empty the feature stays disabled with a startup warning.
	SMTPHost          string
	SMTPPort          int
	SMTPUser          string
	SMTPPass          string
	SMTPFrom          string
	SMTPTLSMode       string
	SMTPTLSSkipVerify bool
	PublicBaseURL     string
	PasswordResetTTL  time.Duration

	// OpenTelemetry export (metrics→dashboards, logs→Loki, traces→Tempo) to a
	// single OTLP endpoint, typically an OpenTelemetry Collector gateway. These
	// are env defaults; the Settings → Observability card overrides them in
	// app_settings. Changes take effect on backend restart. Insecure defaults to
	// true because a lab collector commonly speaks plaintext OTLP.
	OTelEnabled     bool
	OTelEndpoint    string
	OTelProtocol    string
	OTelInsecure    bool
	OTelHeaders     string
	OTelServiceName string
	OTelSampleRatio float64
}

func Load() (*Config, error) {
	cfg := &Config{
		Listen:                envOr("MIKROTIK_NMS_LISTEN", ":8080"),
		DBPath:                envOr("MIKROTIK_NMS_DB_PATH", "mikrotik-nms.db"),
		EncryptionKey:         os.Getenv("MIKROTIK_NMS_ENCRYPTION_KEY"),
		JWTSecret:             os.Getenv("MIKROTIK_NMS_JWT_SECRET"),
		HealthInterval:        envDurationOr("MIKROTIK_NMS_HEALTH_INTERVAL", 30*time.Second),
		TopologyInterval:      envDurationOr("MIKROTIK_NMS_TOPOLOGY_INTERVAL", 60*time.Second),
		FirmwareInterval:      envDurationOr("MIKROTIK_NMS_FIRMWARE_INTERVAL", 6*time.Hour),
		RetentionInterval:     envDurationOr("MIKROTIK_NMS_RETENTION_INTERVAL", 1*time.Hour),
		NetworkHealthInterval: envDurationOr("MIKROTIK_NMS_NETWORK_HEALTH_INTERVAL", 60*time.Second),
		RetentionDays:         envIntOr("MIKROTIK_NMS_RETENTION_DAYS", 7),
		DefaultROSUser:        envOr("MIKROTIK_NMS_DEFAULT_ROS_USER", "admin"),
		DefaultROSPass:        os.Getenv("MIKROTIK_NMS_DEFAULT_ROS_PASS"),
		DefaultROSPort:        envIntOr("MIKROTIK_NMS_DEFAULT_ROS_PORT", 8728),
		DefaultROSTLS:         envBoolOr("MIKROTIK_NMS_DEFAULT_ROS_TLS", false),
		AllowedOrigins:        envListOr("MIKROTIK_NMS_ALLOWED_ORIGINS", nil),
		ROSTLSVerify:          envBoolOr("MIKROTIK_NMS_ROS_TLS_VERIFY", false),
		SMTPHost:              os.Getenv("MIKROTIK_NMS_SMTP_HOST"),
		SMTPPort:              envIntOr("MIKROTIK_NMS_SMTP_PORT", 587),
		SMTPUser:              os.Getenv("MIKROTIK_NMS_SMTP_USER"),
		SMTPPass:              os.Getenv("MIKROTIK_NMS_SMTP_PASS"),
		SMTPFrom:              os.Getenv("MIKROTIK_NMS_SMTP_FROM"),
		SMTPTLSMode:           envOr("MIKROTIK_NMS_SMTP_TLS_MODE", "starttls"),
		SMTPTLSSkipVerify:     envBoolOr("MIKROTIK_NMS_SMTP_TLS_SKIP_VERIFY", false),
		PublicBaseURL:         os.Getenv("MIKROTIK_NMS_PUBLIC_BASE_URL"),
		PasswordResetTTL:      envDurationOr("MIKROTIK_NMS_PASSWORD_RESET_TTL", time.Hour),
		OTelEnabled:           envBoolOr("MIKROTIK_NMS_OTEL_ENABLED", false),
		OTelEndpoint:          os.Getenv("MIKROTIK_NMS_OTEL_ENDPOINT"),
		OTelProtocol:          envOr("MIKROTIK_NMS_OTEL_PROTOCOL", "grpc"),
		OTelInsecure:          envBoolOr("MIKROTIK_NMS_OTEL_INSECURE", true),
		OTelHeaders:           os.Getenv("MIKROTIK_NMS_OTEL_HEADERS"),
		OTelServiceName:       envOr("MIKROTIK_NMS_OTEL_SERVICE_NAME", "mikrotik-nms"),
		OTelSampleRatio:       envFloatOr("MIKROTIK_NMS_OTEL_SAMPLE_RATIO", 1.0),
	}

	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("MIKROTIK_NMS_JWT_SECRET is required")
	}

	// From falls back to the SMTP auth user when not explicitly set.
	if cfg.SMTPFrom == "" {
		cfg.SMTPFrom = cfg.SMTPUser
	}

	// SMTP configured but no public base URL: links can't be built from an
	// untrusted Host, so the feature stays disabled. Warn, but never hard-fail
	// startup (operationally hostile to brick the whole service on this).
	if cfg.SMTPHost != "" && cfg.PublicBaseURL == "" {
		log.Println("warning: MIKROTIK_NMS_SMTP_HOST set but MIKROTIK_NMS_PUBLIC_BASE_URL is empty — password-reset emails disabled")
	}

	return cfg, nil
}

// SMTPEnabled reports whether self-service password-reset email can be sent.
// It requires both an SMTP host/port and a public base URL (so a reset link is
// never built from an untrusted request Host header).
func (c *Config) SMTPEnabled() bool {
	return c.SMTPHost != "" && c.SMTPPort > 0 && c.PublicBaseURL != ""
}

// envListOr parses a comma-separated env var into a trimmed, non-empty slice.
func envListOr(key string, fallback []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func envBoolOr(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envFloatOr(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
