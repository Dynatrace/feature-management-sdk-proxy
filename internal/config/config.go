package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/snapshot"
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

	// LifecycleKeysCap is the maximum combined number of sdkKeyHashes + sdkKeys
	// entries accepted in a single lifecycle SSE event. Events exceeding this cap
	// are rejected as malformed before any SHA-256 work is performed (CPU-amplification DoS guard).
	// Range 1–100000, default 10000. Env: LIFECYCLE_KEYS_CAP.
	LifecycleKeysCap int

	// SSEDisabled disables upstream SSE connections entirely. When true, the proxy
	// relies solely on polling for flag updates. All keys poll at
	// RefreshIntervalDisconnected (10s). Env: SSE_DISABLED, default false.
	SSEDisabled bool

	// SSEFanoutCapPerKey is the maximum number of downstream SDK client SSE
	// connections allowed per channel(SDK key public ID). Connections beyond this cap are
	// rejected with 429. Range 0-100000 (0 disables the cap).
	// Default 1000. Env: SSE_FANOUT_CAP_PER_KEY.
	SSEFanoutCapPerKey int

	// SSESubscribeRatePerSec is the token-bucket rate at which new SSE subscriptions
	// are admitted per SDK key public ID. Range 1-10000, default 20.
	// Env: SSE_SUBSCRIBE_RATE_PER_SEC.
	SSESubscribeRatePerSec int

	// SSESubscribeBurst is the burst size for the per-key SSE subscribe rate bucket.
	// Range 1-100000, default 100. Env: SSE_SUBSCRIBE_BURST.
	SSESubscribeBurst int

	// SnapshotURL is the gocloud.dev/blob bucket URL for snapshots. Empty
	// disables persistence. Env: SNAPSHOT_URL.
	SnapshotURL string

	// SnapshotWriteQueueSize is the maximum pending async snapshot writes.
	// Range 1–10000, default 256.
	SnapshotWriteQueueSize int

	// SnapshotMaxAge is the threshold above which a loaded snapshot is classified
	// as stale, triggering a loud warning and metric at startup. It is a warning
	// threshold only — stale snapshots are still served. Ignored when SnapshotURL
	// is empty; setting SNAPSHOT_MAX_AGE without SNAPSHOT_URL is an error.
	// Env: SNAPSHOT_MAX_AGE, default 24h, must be > 0.
	SnapshotMaxAge time.Duration

	// CacheInactivityEvictAfter is how long an SDK key must be idle (no HTTP
	// requests and no active SSE subscribers) before the inactivity sweeper
	// evicts it. Eviction removes the in-memory cache entry, closes the upstream
	// SSE connection, and deletes the persisted snapshot (same path as lifecycle
	// invalidation). Set to 0 to disable the sweeper entirely.
	// Env: CACHE_INACTIVITY_EVICT_AFTER, default 48h.
	CacheInactivityEvictAfter time.Duration

	// MaxBodyBytes caps any body the proxy reads or accepts: upstream CDN
	// responses (rejected with 502 if exceeded) and inbound eval-metrics request
	// bodies (rejected with 413 if exceeded). Prevents unbounded memory growth.
	// Env: MAX_BODY_BYTES, default 10485760 (10 MiB).
	MaxBodyBytes int64
}

// LogValue implements slog.LogValuer. Credential fields must use the Secret
// type so they emit [REDACTED].
func (c Config) LogValue() slog.Value {
	snapshotBackend := "disabled"
	if u, err := url.Parse(c.SnapshotURL); c.SnapshotURL != "" && err == nil {
		snapshotBackend = u.Scheme
	}
	attrs := []slog.Attr{
		slog.Int("port", c.Port),
		slog.String("cdn_base_domain", c.CDNBaseDomain),
		slog.Any("log_level", c.LogLevel),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
		slog.Bool("otlp_enabled", c.OTLPEnabled),
		slog.Duration("billing_flush_interval", c.BillingFlushInterval),
		slog.Int("billing_retry_buffer_size", c.BillingRetryBufferSize),
		slog.String("proxy_instance_id", c.ProxyInstanceID),
		slog.Int("lifecycle_keys_cap", c.LifecycleKeysCap),
		slog.Bool("sse_disabled", c.SSEDisabled),
		slog.Int("sse_fanout_cap_per_key", c.SSEFanoutCapPerKey),
		slog.Int("sse_subscribe_rate_per_sec", c.SSESubscribeRatePerSec),
		slog.Int("sse_subscribe_burst", c.SSESubscribeBurst),
		slog.String("snapshot_backend", snapshotBackend),
		slog.Int("snapshot_write_queue_size", c.SnapshotWriteQueueSize),
		slog.Duration("snapshot_max_age", c.SnapshotMaxAge),
		slog.Duration("cache_inactivity_evict_after", c.CacheInactivityEvictAfter),
		slog.Int64("max_body_bytes", c.MaxBodyBytes),
	}
	if c.SnapshotURL != "" {
		attrs = append(attrs, slog.String("snapshot_url", snapshot.RedactURL(c.SnapshotURL)))
	}
	return slog.GroupValue(attrs...)
}

const (
	defaultCDNBaseDomain = "fm.dynatrace.com"
)

// FromEnv loads configuration from environment variables.
// All variables have safe defaults.
func FromEnv() (Config, error) {
	cfg := Config{
		Port:                      8080,
		CDNBaseDomain:             defaultCDNBaseDomain,
		LogLevel:                  slog.LevelInfo,
		ShutdownTimeout:           30 * time.Second,
		BillingFlushInterval:      60 * time.Second,
		BillingRetryBufferSize:    1000,
		LifecycleKeysCap:          10000,
		SnapshotWriteQueueSize:    256,
		SnapshotMaxAge:            24 * time.Hour,
		CacheInactivityEvictAfter: 48 * time.Hour,
		MaxBodyBytes:              10 * 1024 * 1024,
		SSEFanoutCapPerKey:        1000,
		SSESubscribeRatePerSec:    20,
		SSESubscribeBurst:         100,
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
		domain = defaultCDNBaseDomain
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

	if v := os.Getenv("LIFECYCLE_KEYS_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100_000 {
			return Config{}, fmt.Errorf("invalid LIFECYCLE_KEYS_CAP %q: must be an integer between 1 and 100000", v)
		}
		cfg.LifecycleKeysCap = n
	}

	// SSE_DISABLED defaults to false (SSE enabled). The negation name is
	// intentional: most deployments want SSE on; this is an escape hatch.
	if v := os.Getenv("SSE_DISABLED"); v != "" {
		switch strings.ToLower(v) {
		case "true", "1":
			cfg.SSEDisabled = true
		case "false", "0":
			cfg.SSEDisabled = false
		default:
			return Config{}, fmt.Errorf("invalid SSE_DISABLED %q: must be true or false", v)
		}
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

	if v := os.Getenv("SNAPSHOT_URL"); v != "" {
		if err := snapshot.ValidateURL(v); err != nil {
			return Config{}, fmt.Errorf("invalid SNAPSHOT_URL: %w", err)
		}
		cfg.SnapshotURL = v
	}

	if v := os.Getenv("SNAPSHOT_WRITE_QUEUE_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10_000 {
			return Config{}, fmt.Errorf("invalid SNAPSHOT_WRITE_QUEUE_SIZE %q: must be an integer between 1 and 10000", v)
		}
		cfg.SnapshotWriteQueueSize = n
	}

	if v := os.Getenv("SNAPSHOT_MAX_AGE"); v != "" {
		if cfg.SnapshotURL == "" {
			return Config{}, fmt.Errorf("SNAPSHOT_MAX_AGE is set but SNAPSHOT_URL is not configured — SNAPSHOT_MAX_AGE has no effect without a snapshot backend")
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("invalid SNAPSHOT_MAX_AGE %q: must be a positive duration (e.g. 24h)", v)
		}
		cfg.SnapshotMaxAge = d
	}

	if v := os.Getenv("CACHE_INACTIVITY_EVICT_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return Config{}, fmt.Errorf("invalid CACHE_INACTIVITY_EVICT_AFTER %q: must be a non-negative duration (e.g. 48h); use 0 to disable", v)
		}
		// The sweeper runs every 1h; values shorter than 1h would never fire.
		if d > 0 && d < time.Hour {
			return Config{}, fmt.Errorf("invalid CACHE_INACTIVITY_EVICT_AFTER %q: minimum non-zero value is 1h (sweeper runs hourly); use 0 to disable", v)
		}
		cfg.CacheInactivityEvictAfter = d
	}

	if v := os.Getenv("MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("invalid MAX_BODY_BYTES %q: must be a positive integer (bytes)", v)
		}
		cfg.MaxBodyBytes = n
	}

	if v := os.Getenv("SSE_FANOUT_CAP_PER_KEY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100_000 {
			return Config{}, fmt.Errorf("invalid SSE_FANOUT_CAP_PER_KEY %q: must be an integer between 0 and 100000 (0 disables the cap)", v)
		}
		cfg.SSEFanoutCapPerKey = n
	}

	if v := os.Getenv("SSE_SUBSCRIBE_RATE_PER_SEC"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10_000 {
			return Config{}, fmt.Errorf("invalid SSE_SUBSCRIBE_RATE_PER_SEC %q: must be an integer between 1 and 10000", v)
		}
		cfg.SSESubscribeRatePerSec = n
	}

	if v := os.Getenv("SSE_SUBSCRIBE_BURST"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100_000 {
			return Config{}, fmt.Errorf("invalid SSE_SUBSCRIBE_BURST %q: must be an integer between 1 and 100000", v)
		}
		cfg.SSESubscribeBurst = n
	}

	return cfg, nil
}
