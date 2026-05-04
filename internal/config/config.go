package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	// BillingRetryBufferSize is the maximum number of failed billing samples
	// held in the retry buffer. Oldest samples are dropped on overflow.
	// Range 1–100000, default 1000.
	BillingRetryBufferSize int

	// Snapshot persistence (optional). When SnapshotBackend is "", persistence
	// is disabled and all other Snapshot* fields are ignored.
	//
	// SnapshotBackend selects the persistence backend: "s3" or "local".
	SnapshotBackend string

	// S3-compatible object storage settings. Used when SnapshotBackend == "s3".
	SnapshotS3Endpoint  string // optional; leave empty for AWS S3
	SnapshotS3Bucket    string // required when backend is "s3"
	SnapshotS3Region    string // required when backend is "s3"
	SnapshotS3Prefix    string // optional key prefix; defaults to ""
	SnapshotS3AccessKey string // optional; uses default credential chain when empty
	SnapshotS3SecretKey string // optional; uses default credential chain when empty

	// Local filesystem settings. Used when SnapshotBackend == "local".
	SnapshotLocalDir string // required when backend is "local"; must be an absolute path

	// SnapshotWriteQueueSize is the maximum pending async snapshot writes.
	// Range 1–10000, default 256.
	SnapshotWriteQueueSize int
}

// FromEnv loads configuration from environment variables.
// CDN_BASE_DOMAIN is required; all others have defaults.
func FromEnv() (Config, error) {
	cfg := Config{
		Port:                   8080,
		LogLevel:               slog.LevelInfo,
		ShutdownTimeout:        30 * time.Second,
		BillingFlushInterval:   60 * time.Second,
		BillingRetryBufferSize: 1000,
		SnapshotWriteQueueSize: 256,
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

	if v := os.Getenv("BILLING_RETRY_BUFFER_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100_000 {
			return Config{}, fmt.Errorf("invalid BILLING_RETRY_BUFFER_SIZE %q: must be an integer between 1 and 100000", v)
		}
		cfg.BillingRetryBufferSize = n
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

	if v := os.Getenv("SNAPSHOT_BACKEND"); v != "" {
		if v != "s3" && v != "local" {
			return Config{}, fmt.Errorf("invalid SNAPSHOT_BACKEND %q: must be \"s3\" or \"local\"", v)
		}
		cfg.SnapshotBackend = v
	}

	if cfg.SnapshotBackend == "s3" {
		bucket := os.Getenv("SNAPSHOT_S3_BUCKET")
		if bucket == "" {
			return Config{}, errors.New("SNAPSHOT_S3_BUCKET is required when SNAPSHOT_BACKEND=s3")
		}
		region := os.Getenv("SNAPSHOT_S3_REGION")
		if region == "" {
			return Config{}, errors.New("SNAPSHOT_S3_REGION is required when SNAPSHOT_BACKEND=s3")
		}
		cfg.SnapshotS3Bucket = bucket
		cfg.SnapshotS3Region = region
		cfg.SnapshotS3Endpoint = os.Getenv("SNAPSHOT_S3_ENDPOINT")
		cfg.SnapshotS3Prefix = os.Getenv("SNAPSHOT_S3_PREFIX")
		cfg.SnapshotS3AccessKey = os.Getenv("SNAPSHOT_S3_ACCESS_KEY")
		cfg.SnapshotS3SecretKey = os.Getenv("SNAPSHOT_S3_SECRET_KEY")
	}

	if cfg.SnapshotBackend == "local" {
		dir := os.Getenv("SNAPSHOT_LOCAL_DIR")
		if dir == "" {
			return Config{}, errors.New("SNAPSHOT_LOCAL_DIR is required when SNAPSHOT_BACKEND=local")
		}
		if !filepath.IsAbs(dir) {
			return Config{}, fmt.Errorf("invalid SNAPSHOT_LOCAL_DIR %q: must be an absolute path", dir)
		}
		cfg.SnapshotLocalDir = dir
	}

	if v := os.Getenv("SNAPSHOT_WRITE_QUEUE_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10_000 {
			return Config{}, fmt.Errorf("invalid SNAPSHOT_WRITE_QUEUE_SIZE %q: must be an integer between 1 and 10000", v)
		}
		cfg.SnapshotWriteQueueSize = n
	}

	return cfg, nil
}
