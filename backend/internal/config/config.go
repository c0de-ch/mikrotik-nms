package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Listen        string
	DBPath        string
	EncryptionKey string
	JWTSecret     string

	// Polling intervals
	HealthInterval    time.Duration
	TopologyInterval  time.Duration
	FirmwareInterval  time.Duration
	RetentionInterval time.Duration

	// Data retention
	RetentionDays int

	// Default RouterOS credentials
	DefaultROSUser string
	DefaultROSPass string
	DefaultROSPort int
	DefaultROSTLS  bool
}

func Load() (*Config, error) {
	cfg := &Config{
		Listen:            envOr("MIKROTIK_NMS_LISTEN", ":8080"),
		DBPath:            envOr("MIKROTIK_NMS_DB_PATH", "mikrotik-nms.db"),
		EncryptionKey:     os.Getenv("MIKROTIK_NMS_ENCRYPTION_KEY"),
		JWTSecret:         os.Getenv("MIKROTIK_NMS_JWT_SECRET"),
		HealthInterval:    envDurationOr("MIKROTIK_NMS_HEALTH_INTERVAL", 30*time.Second),
		TopologyInterval:  envDurationOr("MIKROTIK_NMS_TOPOLOGY_INTERVAL", 60*time.Second),
		FirmwareInterval:  envDurationOr("MIKROTIK_NMS_FIRMWARE_INTERVAL", 6*time.Hour),
		RetentionInterval: envDurationOr("MIKROTIK_NMS_RETENTION_INTERVAL", 1*time.Hour),
		RetentionDays:     envIntOr("MIKROTIK_NMS_RETENTION_DAYS", 7),
		DefaultROSUser:    envOr("MIKROTIK_NMS_DEFAULT_ROS_USER", "admin"),
		DefaultROSPass:    os.Getenv("MIKROTIK_NMS_DEFAULT_ROS_PASS"),
		DefaultROSPort:    envIntOr("MIKROTIK_NMS_DEFAULT_ROS_PORT", 8728),
		DefaultROSTLS:     envBoolOr("MIKROTIK_NMS_DEFAULT_ROS_TLS", false),
	}

	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("MIKROTIK_NMS_JWT_SECRET is required")
	}

	return cfg, nil
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

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
