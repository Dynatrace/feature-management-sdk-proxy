package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	Port            int
	UpstreamCDNURL  string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// OTLPEnabled is true when OTEL_EXPORTER_OTLP_ENDPOINT is set.
	// When false, metrics are emitted to a no-op provider.
	OTLPEnabled bool
}

// FromEnv loads configuration from environment variables.
// UPSTREAM_CDN_URL is required; all others have defaults.
func FromEnv() (Config, error) {
	cfg := Config{
		Port:            8080,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 30 * time.Second,
	}

	if v := os.Getenv("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return Config{}, fmt.Errorf("invalid PORT %q: must be 1-65535", v)
		}
		cfg.Port = p
	}

	upstream := os.Getenv("UPSTREAM_CDN_URL")
	if upstream == "" {
		return Config{}, errors.New("UPSTREAM_CDN_URL is required")
	}
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return Config{}, fmt.Errorf("invalid UPSTREAM_CDN_URL %q: must be an absolute URL with scheme and host", upstream)
	}
	cfg.UpstreamCDNURL = upstream

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

	return cfg, nil
}
