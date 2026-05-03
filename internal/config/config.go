package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	Port            int
	CDNBaseDomain   string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// OTLPEnabled is true when OTEL_EXPORTER_OTLP_ENDPOINT is set.
	// When false, metrics are emitted to a no-op provider.
	OTLPEnabled bool
	// BillingFlushInterval is how often the telemetry flusher drains counters
	// and sends to the billing endpoint. Minimum 10s, default 60s.
	BillingFlushInterval time.Duration
	// ProxyInstanceID identifies this proxy replica in billing records.
	// Defaults to os.Hostname(); falls back to "unknown" on error.
	ProxyInstanceID string
}

// FromEnv loads configuration from environment variables.
// CDN_BASE_DOMAIN is required; all others have defaults.
func FromEnv() (Config, error) {
	cfg := Config{
		Port:                 8080,
		LogLevel:             slog.LevelInfo,
		ShutdownTimeout:      30 * time.Second,
		BillingFlushInterval: 60 * time.Second,
	}

	if v := os.Getenv("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return Config{}, fmt.Errorf("invalid PORT %q: must be 1-65535", v)
		}
		cfg.Port = p
	}

	domain := os.Getenv("CDN_BASE_DOMAIN")
	if domain == "" {
		return Config{}, errors.New("CDN_BASE_DOMAIN is required")
	}
	for _, c := range domain {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-') {
			return Config{}, fmt.Errorf("invalid CDN_BASE_DOMAIN %q: must be a bare hostname containing only letters, digits, dots, and hyphens", domain)
		}
	}
	cfg.CDNBaseDomain = domain

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			return Config{}, fmt.Errorf("invalid LOG_LEVEL %q: unrecognized log level", v)
		}
	}

	if v := os.Getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("invalid SHUTDOWN_TIMEOUT %q: must be a positive duration (e.g. 30s)", v)
		}
		cfg.ShutdownTimeout = d
	}

	cfg.OTLPEnabled = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""

	if v := os.Getenv("BILLING_FLUSH_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Second {
			return Config{}, fmt.Errorf("invalid BILLING_FLUSH_INTERVAL %q: must be a duration >= 10s (e.g. 60s)", v)
		}
		cfg.BillingFlushInterval = d
	}

	if v := os.Getenv("PROXY_INSTANCE_ID"); v != "" {
		cfg.ProxyInstanceID = v
	} else {
		h, err := os.Hostname()
		if err != nil {
			h = "unknown"
		}
		cfg.ProxyInstanceID = h
	}

	return cfg, nil
}
