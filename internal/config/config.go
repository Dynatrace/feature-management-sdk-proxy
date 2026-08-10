package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	SnapshotS3AccessKey Secret // optional; uses default credential chain when empty
	SnapshotS3SecretKey Secret // optional; uses default credential chain when empty

	// Local filesystem settings. Used when SnapshotBackend == "local".
	SnapshotLocalDir string // required when backend is "local"; must be an absolute path

	// SnapshotWriteQueueSize is the maximum pending async snapshot writes.
	// Range 1–10000, default 256.
	SnapshotWriteQueueSize int

	// SnapshotMaxAge is the threshold above which a loaded snapshot is classified
	// as stale, triggering a loud warning and metric at startup. It is a warning
	// threshold only — stale snapshots are still served. Ignored when SnapshotBackend
	// is empty; setting SNAPSHOT_MAX_AGE without SNAPSHOT_BACKEND is an error.
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

	// ConfigSource selects where flag configs are served from: "cdn" (default,
	// fetch-on-miss from the Dynatrace CDN) or "static" (a single offline flagd
	// JSON file). In "static" mode the CDN is never contacted, upstream SSE is
	// disabled, and config updates are driven by changes to the backing file.
	// Env: CONFIG_SOURCE, default "cdn".
	ConfigSource string

	// StaticFilePath is the absolute path to the single flagd JSON file that backs
	// config in "static" mode. Required when ConfigSource == "static"; ignored
	// otherwise. Env: STATIC_FILE_PATH.
	StaticFilePath string

	// StaticFilePollInterval is how often the static-source watcher checks the
	// backing file for changes. Range: >= 1s, default 10s. Ignored unless
	// ConfigSource == "static". Env: STATIC_FILE_POLL_INTERVAL.
	StaticFilePollInterval time.Duration

	// OfflineMetricsUpstreamURL is the full upstream URL that POST /v1/metrics is
	// forwarded to in "static" mode. When empty, the metrics passthrough is
	// disabled (501). There is no geo segment to derive a host from in static
	// mode, so the full URL is configured explicitly. Ignored unless
	// ConfigSource == "static". Env: OFFLINE_METRICS_UPSTREAM_URL.
	OfflineMetricsUpstreamURL string
}

// LogValue implements slog.LogValuer. Secrets emit [REDACTED] automatically
// via Secret's own LogValue — do not format credential fields with fmt or json.
// Backend-specific fields (snapshot_s3_*, snapshot_local_dir) are only included
// when the corresponding backend is active, to avoid [REDACTED] noise for operators
// who have not configured snapshot persistence.
func (c Config) LogValue() slog.Value {
	snapshotBackend := c.SnapshotBackend
	if snapshotBackend == "" {
		snapshotBackend = "disabled"
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
		slog.String("config_source", c.ConfigSource),
	}
	if c.ConfigSource == configSourceStatic {
		attrs = append(attrs,
			slog.String("static_file_path", c.StaticFilePath),
			slog.Duration("static_file_poll_interval", c.StaticFilePollInterval),
			// The upstream URL is not a secret; log it (or "disabled" when unset).
			slog.String("offline_metrics_upstream_url", orDisabled(c.OfflineMetricsUpstreamURL)),
		)
	}
	if c.SnapshotBackend == snapshotBackendS3 {
		attrs = append(attrs,
			slog.String("snapshot_s3_bucket", c.SnapshotS3Bucket),
			slog.String("snapshot_s3_region", c.SnapshotS3Region),
			slog.String("snapshot_s3_endpoint", c.SnapshotS3Endpoint),
			slog.String("snapshot_s3_prefix", c.SnapshotS3Prefix),
			slog.Any("snapshot_s3_access_key", c.SnapshotS3AccessKey),
			slog.Any("snapshot_s3_secret_key", c.SnapshotS3SecretKey),
		)
	}
	if c.SnapshotBackend == snapshotBackendLocal {
		attrs = append(attrs, slog.String("snapshot_local_dir", c.SnapshotLocalDir))
	}
	return slog.GroupValue(attrs...)
}

const (
	defaultCDNBaseDomain = "fm.dynatrace.com"
	snapshotBackendS3    = "s3"
	snapshotBackendLocal = "local"
	configSourceCDN      = "cdn"
	configSourceStatic   = "static"
)

// orDisabled returns s, or the literal "disabled" when s is empty. Used for
// log fields where an unset optional value is more meaningful as "disabled".
func orDisabled(s string) string {
	if s == "" {
		return "disabled"
	}
	return s
}

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
		ConfigSource:              configSourceCDN,
		StaticFilePollInterval:    10 * time.Second,
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

	if v := os.Getenv("SNAPSHOT_BACKEND"); v != "" {
		if v != snapshotBackendS3 && v != snapshotBackendLocal {
			return Config{}, fmt.Errorf("invalid SNAPSHOT_BACKEND %q: must be \"s3\" or \"local\"", v)
		}
		cfg.SnapshotBackend = v
	}

	if cfg.SnapshotBackend == snapshotBackendS3 {
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
		cfg.SnapshotS3AccessKey = Secret(os.Getenv("SNAPSHOT_S3_ACCESS_KEY"))
		cfg.SnapshotS3SecretKey = Secret(os.Getenv("SNAPSHOT_S3_SECRET_KEY"))
	}

	if cfg.SnapshotBackend == snapshotBackendLocal {
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

	if v := os.Getenv("SNAPSHOT_MAX_AGE"); v != "" {
		if cfg.SnapshotBackend == "" {
			return Config{}, fmt.Errorf("SNAPSHOT_MAX_AGE is set but SNAPSHOT_BACKEND is not configured — SNAPSHOT_MAX_AGE has no effect without a snapshot backend")
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

	if v := os.Getenv("CONFIG_SOURCE"); v != "" {
		if v != configSourceCDN && v != configSourceStatic {
			return Config{}, fmt.Errorf("invalid CONFIG_SOURCE %q: must be %q or %q", v, configSourceCDN, configSourceStatic)
		}
		cfg.ConfigSource = v
	}

	staticFilePath := os.Getenv("STATIC_FILE_PATH")
	offlineMetricsURL := os.Getenv("OFFLINE_METRICS_UPSTREAM_URL")
	staticPollInterval := os.Getenv("STATIC_FILE_POLL_INTERVAL")

	if cfg.ConfigSource == configSourceStatic {
		if staticFilePath == "" {
			return Config{}, errors.New("STATIC_FILE_PATH is required when CONFIG_SOURCE=static")
		}
		if !filepath.IsAbs(staticFilePath) {
			return Config{}, fmt.Errorf("invalid STATIC_FILE_PATH %q: must be an absolute path", staticFilePath)
		}
		cfg.StaticFilePath = staticFilePath

		// The static file is the sole source of truth in this mode; snapshot
		// persistence would be a second, conflicting source. Reject the combination.
		if cfg.SnapshotBackend != "" {
			return Config{}, errors.New("SNAPSHOT_BACKEND cannot be combined with CONFIG_SOURCE=static: the static file is the source of truth")
		}

		if staticPollInterval != "" {
			d, err := time.ParseDuration(staticPollInterval)
			if err != nil || d < time.Second {
				return Config{}, fmt.Errorf("invalid STATIC_FILE_POLL_INTERVAL %q: must be a duration >= 1s (e.g. 10s)", staticPollInterval)
			}
			cfg.StaticFilePollInterval = d
		}

		if offlineMetricsURL != "" {
			if err := validateUpstreamURL(offlineMetricsURL); err != nil {
				return Config{}, fmt.Errorf("invalid OFFLINE_METRICS_UPSTREAM_URL %q: %w", offlineMetricsURL, err)
			}
			cfg.OfflineMetricsUpstreamURL = offlineMetricsURL
		}
	} else {
		// CDN mode: reject static-only variables so a misplaced setting is loud
		// rather than silently ignored (mirrors the SNAPSHOT_MAX_AGE guard).
		if staticFilePath != "" {
			return Config{}, errors.New("STATIC_FILE_PATH is set but CONFIG_SOURCE is not \"static\" — it has no effect")
		}
		if offlineMetricsURL != "" {
			return Config{}, errors.New("OFFLINE_METRICS_UPSTREAM_URL is set but CONFIG_SOURCE is not \"static\" — it has no effect")
		}
		if staticPollInterval != "" {
			return Config{}, errors.New("STATIC_FILE_POLL_INTERVAL is set but CONFIG_SOURCE is not \"static\" — it has no effect")
		}
	}

	return cfg, nil
}

// validateUpstreamURL reports whether s is an absolute http(s) URL with a host,
// suitable as a forwarding target. It rejects relative URLs, missing hosts, and
// non-http schemes.
func validateUpstreamURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("must be a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must use the http or https scheme")
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	return nil
}
