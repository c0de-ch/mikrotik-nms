package config

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// Values of MIKROTIK_NMS_POLLERS.
const (
	PollersLocal = "local"
	PollersOff   = "off"
)

type Config struct {
	Listen        string
	DBPath        string
	EncryptionKey string
	JWTSecret     string

	// Pollers is MIKROTIK_NMS_POLLERS: "local" polls the devices from this
	// process, "off" (the code default) does no device or UDP I/O at all — a
	// checkout started against a copy of a production database then shows
	// history but never contacts the fleet. Database-only jobs (retention,
	// rollups) run either way. Restart to change.
	Pollers string
	// AllowedTargetCIDRs is MIKROTIK_NMS_ALLOWED_TARGET_CIDRS: the prefixes a
	// device dial may resolve into. Required with Pollers=local (the process
	// refuses to start otherwise); ignored while polling is off.
	AllowedTargetCIDRs []netip.Prefix

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

	// FlowListen are the UDP bind addresses of the flow collector (NetFlow
	// v5/v9, IPFIX, sFlow), e.g. [":2055"]. Empty (the default) keeps the
	// collector off: UDP input is unauthenticated, so it is opt-in. Changes
	// need a restart.
	FlowListen []string
	// FlowCaptureDir (dev only) makes the collector write the first raw
	// datagrams of each exporter to this directory for replay; "" = off.
	FlowCaptureDir string
}

func Load() (*Config, error) {
	cfg := &Config{
		Listen:                envOr("MIKROTIK_NMS_LISTEN", ":8080"),
		DBPath:                envOr("MIKROTIK_NMS_DB_PATH", "mikrotik-nms.db"),
		EncryptionKey:         os.Getenv("MIKROTIK_NMS_ENCRYPTION_KEY"),
		JWTSecret:             os.Getenv("MIKROTIK_NMS_JWT_SECRET"),
		Pollers:               strings.ToLower(strings.TrimSpace(envOr("MIKROTIK_NMS_POLLERS", PollersOff))),
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
		FlowListen:            parseFlowListen(os.Getenv("MIKROTIK_NMS_FLOW_LISTEN")),
		FlowCaptureDir:        os.Getenv("MIKROTIK_NMS_FLOW_CAPTURE_DIR"),
	}

	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("MIKROTIK_NMS_JWT_SECRET is required")
	}

	switch cfg.Pollers {
	case PollersLocal, PollersOff:
	default:
		return nil, fmt.Errorf("MIKROTIK_NMS_POLLERS must be %q or %q (got %q)", PollersLocal, PollersOff, cfg.Pollers)
	}
	cidrs, err := parseTargetCIDRs(os.Getenv("MIKROTIK_NMS_ALLOWED_TARGET_CIDRS"))
	if err != nil {
		return nil, fmt.Errorf("MIKROTIK_NMS_ALLOWED_TARGET_CIDRS: %w", err)
	}
	cfg.AllowedTargetCIDRs = cidrs
	// Fail closed: an instance that polls must say where. Starting with an
	// empty fence would be the old behaviour (dial anything the DB names).
	if cfg.PollersEnabled() && len(cidrs) == 0 {
		return nil, fmt.Errorf("MIKROTIK_NMS_POLLERS=local requires MIKROTIK_NMS_ALLOWED_TARGET_CIDRS: " +
			"list the subnet(s) the devices live in (comma-separated CIDRs) so this backend can only reach them, " +
			"or set MIKROTIK_NMS_POLLERS=off for an instance that must never contact a device")
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

// PollersEnabled reports whether this process polls devices (Pollers=local).
// A zero Config (tests) polls nothing.
func (c *Config) PollersEnabled() bool {
	return c.Pollers == PollersLocal
}

// PollersMode is the effective MIKROTIK_NMS_POLLERS value, "local" or "off".
func (c *Config) PollersMode() string {
	if c.PollersEnabled() {
		return PollersLocal
	}
	return PollersOff
}

// AllowedTargetCIDRStrings renders the allow-list for logs and the API
// (never nil, so it encodes as an empty JSON array).
func (c *Config) AllowedTargetCIDRStrings() []string {
	out := make([]string, 0, len(c.AllowedTargetCIDRs))
	for _, p := range c.AllowedTargetCIDRs {
		out = append(out, p.String())
	}
	return out
}

// parseTargetCIDRs parses MIKROTIK_NMS_ALLOWED_TARGET_CIDRS: comma-separated
// prefixes ("192.0.2.0/24, 2001:db8::/48"); a bare address stands for itself
// (/32, /128). Unlike the flow listen list an invalid entry is an error, not a
// skip — an allow-list that silently lost an entry would refuse devices with
// a confusing error later. Prefixes are normalised: masked, IPv4-mapped IPv6
// unmapped, zones dropped, duplicates removed.
func parseTargetCIDRs(v string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	seen := map[netip.Prefix]bool{}
	for _, part := range strings.Split(v, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		var p netip.Prefix
		if strings.Contains(entry, "/") {
			pp, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("entry %q: %w", entry, err)
			}
			p = pp
		} else {
			a, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, fmt.Errorf("entry %q is neither a CIDR nor an address: %w", entry, err)
			}
			a = a.WithZone("")
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if a := p.Addr(); a.Is4In6() {
			if p.Bits() < 96 {
				return nil, fmt.Errorf("entry %q: an IPv4-mapped prefix needs at least /96", entry)
			}
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		}
		p = p.Masked()
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// SMTPEnabled reports whether self-service password-reset email can be sent.
// It requires both an SMTP host/port and a public base URL (so a reset link is
// never built from an untrusted request Host header).
func (c *Config) SMTPEnabled() bool {
	return c.SMTPHost != "" && c.SMTPPort > 0 && c.PublicBaseURL != ""
}

// parseFlowListen parses MIKROTIK_NMS_FLOW_LISTEN: comma-separated UDP bind
// addresses (":2055", "0.0.0.0:2055", ":2055,:6343"). Each entry must be
// host:port with a port in 1..65535; an invalid entry is logged and skipped,
// duplicates are removed. Empty input yields nil (collector off).
func parseFlowListen(v string) []string {
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(v, ",") {
		entry := strings.TrimSpace(part)
		if entry == "" {
			continue
		}
		_, port, err := net.SplitHostPort(entry)
		if err == nil {
			if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
				err = fmt.Errorf("port %q is not in 1..65535", port)
			}
		}
		if err != nil {
			log.Printf("warning: MIKROTIK_NMS_FLOW_LISTEN entry %q ignored: %v", entry, err)
			continue
		}
		if !seen[entry] {
			seen[entry] = true
			out = append(out, entry)
		}
	}
	return out
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
