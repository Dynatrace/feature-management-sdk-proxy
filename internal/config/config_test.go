package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestFromEnv_Defaults(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 30s", cfg.ShutdownTimeout)
	}
}

func TestFromEnv_AllVars(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("LOG_LEVEL", "DEBUG")
	t.Setenv("SHUTDOWN_TIMEOUT", "10s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != 9090 {
		t.Errorf("Port = %d, want 9090", cfg.Port)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want DEBUG", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 10s", cfg.ShutdownTimeout)
	}
}

func TestFromEnv_CDNBaseDomain_DefaultUsedWhenUnset(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CDNBaseDomain != "fm.dynatrace.com" {
		t.Errorf("CDNBaseDomain = %q, want \"fm.dynatrace.com\"", cfg.CDNBaseDomain)
	}
}

func TestFromEnv_InvalidPort(t *testing.T) {
	t.Setenv("PORT", "99999")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for out-of-range PORT")
	}
}

func TestFromEnv_InvalidLogLevel(t *testing.T) {
	t.Setenv("LOG_LEVEL", "VERBOSE")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid LOG_LEVEL")
	}
}

func TestFromEnv_InvalidShutdownTimeout(t *testing.T) {
	t.Setenv("SHUTDOWN_TIMEOUT", "notaduration")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid SHUTDOWN_TIMEOUT")
	}
}

func TestFromEnv_BillingFlushInterval_Default(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BillingFlushInterval != 60*time.Second {
		t.Errorf("BillingFlushInterval = %v, want 60s", cfg.BillingFlushInterval)
	}
}

func TestFromEnv_BillingFlushInterval_ValidOverride(t *testing.T) {
	t.Setenv("BILLING_FLUSH_INTERVAL", "30s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BillingFlushInterval != 30*time.Second {
		t.Errorf("BillingFlushInterval = %v, want 30s", cfg.BillingFlushInterval)
	}
}

func TestFromEnv_BillingFlushInterval_TooShort(t *testing.T) {
	t.Setenv("BILLING_FLUSH_INTERVAL", "5s")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for BILLING_FLUSH_INTERVAL < 10s")
	}
}

func TestFromEnv_BillingFlushInterval_Malformed(t *testing.T) {
	t.Setenv("BILLING_FLUSH_INTERVAL", "notaduration")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for malformed BILLING_FLUSH_INTERVAL")
	}
}

func TestFromEnv_ProxyInstanceID_Override(t *testing.T) {
	t.Setenv("PROXY_INSTANCE_ID", "pod-abc-123")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ProxyInstanceID != "pod-abc-123" {
		t.Errorf("ProxyInstanceID = %q, want %q", cfg.ProxyInstanceID, "pod-abc-123")
	}
}

func TestFromEnv_ProxyInstanceID_DefaultNonEmpty(t *testing.T) {
	t.Setenv("PROXY_INSTANCE_ID", "")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Default is os.Hostname() or "unknown" — either way it must not be empty.
	if cfg.ProxyInstanceID == "" {
		t.Error("ProxyInstanceID must not be empty when PROXY_INSTANCE_ID is unset")
	}
}

func TestFromEnv_BillingRetryBufferSize_Default(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BillingRetryBufferSize != 1000 {
		t.Errorf("BillingRetryBufferSize = %d, want 1000", cfg.BillingRetryBufferSize)
	}
}

func TestFromEnv_BillingRetryBufferSize_ValidOverride(t *testing.T) {
	t.Setenv("BILLING_RETRY_BUFFER_SIZE", "5000")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BillingRetryBufferSize != 5000 {
		t.Errorf("BillingRetryBufferSize = %d, want 5000", cfg.BillingRetryBufferSize)
	}
}

func TestFromEnv_BillingRetryBufferSize_Zero(t *testing.T) {
	t.Setenv("BILLING_RETRY_BUFFER_SIZE", "0")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for BILLING_RETRY_BUFFER_SIZE=0")
	}
}

func TestFromEnv_BillingRetryBufferSize_Negative(t *testing.T) {
	t.Setenv("BILLING_RETRY_BUFFER_SIZE", "-1")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for negative BILLING_RETRY_BUFFER_SIZE")
	}
}

func TestFromEnv_BillingRetryBufferSize_Malformed(t *testing.T) {
	t.Setenv("BILLING_RETRY_BUFFER_SIZE", "notanint")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for malformed BILLING_RETRY_BUFFER_SIZE")
	}
}

func TestFromEnv_BillingRetryBufferSize_TooLarge(t *testing.T) {
	t.Setenv("BILLING_RETRY_BUFFER_SIZE", "100001")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for BILLING_RETRY_BUFFER_SIZE > 100000")
	}
}

func TestFromEnv_SnapshotBackend_S3(t *testing.T) {
	t.Setenv("SNAPSHOT_BACKEND", "s3")
	t.Setenv("SNAPSHOT_S3_BUCKET", "my-bucket")
	t.Setenv("SNAPSHOT_S3_REGION", "us-east-1")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SnapshotBackend != "s3" {
		t.Errorf("SnapshotBackend = %q, want \"s3\"", cfg.SnapshotBackend)
	}
}

func TestFromEnv_SnapshotBackend_Local(t *testing.T) {
	t.Setenv("SNAPSHOT_BACKEND", "local")
	t.Setenv("SNAPSHOT_LOCAL_DIR", "/tmp/snapshots")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SnapshotBackend != "local" {
		t.Errorf("SnapshotBackend = %q, want \"local\"", cfg.SnapshotBackend)
	}
	if cfg.SnapshotLocalDir != "/tmp/snapshots" {
		t.Errorf("SnapshotLocalDir = %q, want \"/tmp/snapshots\"", cfg.SnapshotLocalDir)
	}
}

func TestFromEnv_SnapshotBackend_Local_MissingDir(t *testing.T) {
	t.Setenv("SNAPSHOT_BACKEND", "local")
	t.Setenv("SNAPSHOT_LOCAL_DIR", "")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error when SNAPSHOT_LOCAL_DIR is empty")
	}
}

func TestFromEnv_SnapshotBackend_Local_RelativePath(t *testing.T) {
	t.Setenv("SNAPSHOT_BACKEND", "local")
	t.Setenv("SNAPSHOT_LOCAL_DIR", "relative/path")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for relative SNAPSHOT_LOCAL_DIR")
	}
}

func TestFromEnv_SnapshotBackend_Invalid(t *testing.T) {
	t.Setenv("SNAPSHOT_BACKEND", "gcs")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid SNAPSHOT_BACKEND")
	}
}

func TestFromEnv_InvalidCDNBaseDomain(t *testing.T) {
	cases := []string{
		"https://fm.example.com",
		"fm.example.com/path",
		"http://fm.example.com/",
		"fm.example.com:8080",
		"fm example.com",
		"fm.example.com@evil.com",
	}
	for _, tc := range cases {
		t.Setenv("CDN_BASE_DOMAIN", tc)
		_, err := FromEnv()
		if err == nil {
			t.Errorf("CDN_BASE_DOMAIN=%q: expected error, got nil", tc)
		}
	}
}

func TestFromEnv_SnapshotMaxAge_Default(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SnapshotMaxAge != 24*time.Hour {
		t.Errorf("SnapshotMaxAge = %v, want 24h", cfg.SnapshotMaxAge)
	}
}

func TestFromEnv_SnapshotMaxAge_ValidOverride(t *testing.T) {
	t.Setenv("SNAPSHOT_BACKEND", "local")
	t.Setenv("SNAPSHOT_LOCAL_DIR", "/tmp/snapshots")
	t.Setenv("SNAPSHOT_MAX_AGE", "6h")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SnapshotMaxAge != 6*time.Hour {
		t.Errorf("SnapshotMaxAge = %v, want 6h", cfg.SnapshotMaxAge)
	}
}

func TestFromEnv_SnapshotMaxAge_Invalid(t *testing.T) {
	cases := []string{"abc", "-1h", "0s"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("SNAPSHOT_BACKEND", "local")
			t.Setenv("SNAPSHOT_LOCAL_DIR", "/tmp/snapshots")
			t.Setenv("SNAPSHOT_MAX_AGE", v)
			_, err := FromEnv()
			if err == nil {
				t.Errorf("SNAPSHOT_MAX_AGE=%q: expected error, got nil", v)
			}
		})
	}
}

func TestFromEnv_SnapshotMaxAge_WithoutBackend(t *testing.T) {
	t.Setenv("SNAPSHOT_MAX_AGE", "6h")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error when SNAPSHOT_MAX_AGE set without SNAPSHOT_BACKEND")
	}
}

func TestFromEnv_SSEDisabled_Default(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SSEDisabled {
		t.Error("SSEDisabled should default to false")
	}
}

func TestFromEnv_SSEDisabled_True(t *testing.T) {
	cases := []string{"true", "1"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("SSE_DISABLED", v)

			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("SSE_DISABLED=%q: unexpected error: %v", v, err)
			}
			if !cfg.SSEDisabled {
				t.Errorf("SSE_DISABLED=%q: expected SSEDisabled=true", v)
			}
		})
	}
}

func TestFromEnv_SSEDisabled_False(t *testing.T) {
	cases := []string{"false", "0"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("SSE_DISABLED", v)

			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("SSE_DISABLED=%q: unexpected error: %v", v, err)
			}
			if cfg.SSEDisabled {
				t.Errorf("SSE_DISABLED=%q: expected SSEDisabled=false", v)
			}
		})
	}
}

func TestFromEnv_SSEDisabled_Invalid(t *testing.T) {
	t.Setenv("SSE_DISABLED", "yes")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid SSE_DISABLED value")
	}
}

func TestFromEnv_CacheInactivityEvictAfter_Default(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CacheInactivityEvictAfter != 48*time.Hour {
		t.Errorf("CacheInactivityEvictAfter default = %v, want 48h", cfg.CacheInactivityEvictAfter)
	}
}

func TestFromEnv_CacheInactivityEvictAfter_CustomValue(t *testing.T) {
	t.Setenv("CACHE_INACTIVITY_EVICT_AFTER", "72h")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CacheInactivityEvictAfter != 72*time.Hour {
		t.Errorf("CacheInactivityEvictAfter = %v, want 72h", cfg.CacheInactivityEvictAfter)
	}
}

func TestFromEnv_CacheInactivityEvictAfter_ZeroDisables(t *testing.T) {
	t.Setenv("CACHE_INACTIVITY_EVICT_AFTER", "0")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.CacheInactivityEvictAfter != 0 {
		t.Errorf("CacheInactivityEvictAfter with 0 = %v, want 0", cfg.CacheInactivityEvictAfter)
	}
}

func TestFromEnv_CacheInactivityEvictAfter_Invalid(t *testing.T) {
	cases := []string{"abc", "-1h", "-5m"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("CACHE_INACTIVITY_EVICT_AFTER", v)
			_, err := FromEnv()
			if err == nil {
				t.Errorf("expected error for CACHE_INACTIVITY_EVICT_AFTER=%q", v)
			}
		})
	}
}

func TestFromEnv_CacheInactivityEvictAfter_TooShort(t *testing.T) {
	cases := []string{"30m", "59m", "1s"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv("CACHE_INACTIVITY_EVICT_AFTER", v)
			_, err := FromEnv()
			if err == nil {
				t.Errorf("expected error for CACHE_INACTIVITY_EVICT_AFTER=%q (shorter than sweep interval)", v)
			}
		})
	}
}

func TestFromEnv_SSEFanoutCapPerKey_Default(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SSEFanoutCapPerKey != 1000 {
		t.Errorf("SSEFanoutCapPerKey = %d, want 1000", cfg.SSEFanoutCapPerKey)
	}
	if cfg.SSESubscribeRatePerSec != 20 {
		t.Errorf("SSESubscribeRatePerSec = %d, want 20", cfg.SSESubscribeRatePerSec)
	}
	if cfg.SSESubscribeBurst != 100 {
		t.Errorf("SSESubscribeBurst = %d, want 100", cfg.SSESubscribeBurst)
	}
}

func TestFromEnv_SSEFanoutCapPerKey_Custom(t *testing.T) {
	t.Setenv("SSE_FANOUT_CAP_PER_KEY", "500")
	t.Setenv("SSE_SUBSCRIBE_RATE_PER_SEC", "10")
	t.Setenv("SSE_SUBSCRIBE_BURST", "50")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SSEFanoutCapPerKey != 500 {
		t.Errorf("SSEFanoutCapPerKey = %d, want 500", cfg.SSEFanoutCapPerKey)
	}
	if cfg.SSESubscribeRatePerSec != 10 {
		t.Errorf("SSESubscribeRatePerSec = %d, want 10", cfg.SSESubscribeRatePerSec)
	}
	if cfg.SSESubscribeBurst != 50 {
		t.Errorf("SSESubscribeBurst = %d, want 50", cfg.SSESubscribeBurst)
	}
}

func TestFromEnv_SSEFanoutCapPerKey_ZeroDisablesCap(t *testing.T) {
	t.Setenv("SSE_FANOUT_CAP_PER_KEY", "0")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SSEFanoutCapPerKey != 0 {
		t.Errorf("SSEFanoutCapPerKey = %d, want 0 (disabled)", cfg.SSEFanoutCapPerKey)
	}
}

func TestFromEnv_SSEIsolation_Invalid(t *testing.T) {
	cases := []struct {
		env string
		val string
	}{
		{"SSE_FANOUT_CAP_PER_KEY", "-1"},
		{"SSE_FANOUT_CAP_PER_KEY", "100001"},
		{"SSE_FANOUT_CAP_PER_KEY", "abc"},
		{"SSE_SUBSCRIBE_RATE_PER_SEC", "0"},
		{"SSE_SUBSCRIBE_RATE_PER_SEC", "10001"},
		{"SSE_SUBSCRIBE_BURST", "0"},
		{"SSE_SUBSCRIBE_BURST", "100001"},
	}
	for _, tc := range cases {
		t.Run(tc.env+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			_, err := FromEnv()
			if err == nil {
				t.Errorf("expected error for %s=%s", tc.env, tc.val)
			}
		})
	}
}

func TestConfig_LogValue_ZeroValue(t *testing.T) {
	cfg := Config{}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("test", slog.Any("config", cfg))
	out := buf.String()
	if !strings.Contains(out, `"port"`) {
		t.Errorf("expected port key in log output: %s", out)
	}
}

func TestFromEnv_ConfigSource_DefaultsToCDN(t *testing.T) {
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ConfigSource != "cdn" {
		t.Errorf("ConfigSource = %q, want \"cdn\"", cfg.ConfigSource)
	}
}

func TestFromEnv_StaticMode_Valid(t *testing.T) {
	t.Setenv("CONFIG_SOURCE", "static")
	t.Setenv("STATIC_FILE_PATH", "/etc/flags/flags.json")
	t.Setenv("STATIC_FILE_POLL_INTERVAL", "5s")
	t.Setenv("OFFLINE_METRICS_UPSTREAM_URL", "https://metrics.us.fm.dynatrace.com/v1/metrics")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ConfigSource != "static" {
		t.Errorf("ConfigSource = %q, want \"static\"", cfg.ConfigSource)
	}
	if cfg.StaticFilePath != "/etc/flags/flags.json" {
		t.Errorf("StaticFilePath = %q", cfg.StaticFilePath)
	}
	if cfg.StaticFilePollInterval != 5*time.Second {
		t.Errorf("StaticFilePollInterval = %v, want 5s", cfg.StaticFilePollInterval)
	}
	if cfg.OfflineMetricsUpstreamURL != "https://metrics.us.fm.dynatrace.com/v1/metrics" {
		t.Errorf("OfflineMetricsUpstreamURL = %q", cfg.OfflineMetricsUpstreamURL)
	}
}

func TestFromEnv_StaticMode_DefaultPollInterval(t *testing.T) {
	t.Setenv("CONFIG_SOURCE", "static")
	t.Setenv("STATIC_FILE_PATH", "/etc/flags/flags.json")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StaticFilePollInterval != 10*time.Second {
		t.Errorf("StaticFilePollInterval = %v, want 10s", cfg.StaticFilePollInterval)
	}
}

func TestFromEnv_StaticMode_Errors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"missing path", map[string]string{"CONFIG_SOURCE": "static"}},
		{"relative path", map[string]string{"CONFIG_SOURCE": "static", "STATIC_FILE_PATH": "flags.json"}},
		{"with snapshot backend", map[string]string{"CONFIG_SOURCE": "static", "STATIC_FILE_PATH": "/f.json", "SNAPSHOT_BACKEND": "local", "SNAPSHOT_LOCAL_DIR": "/snap"}},
		{"poll too short", map[string]string{"CONFIG_SOURCE": "static", "STATIC_FILE_PATH": "/f.json", "STATIC_FILE_POLL_INTERVAL": "500ms"}},
		{"bad metrics url", map[string]string{"CONFIG_SOURCE": "static", "STATIC_FILE_PATH": "/f.json", "OFFLINE_METRICS_UPSTREAM_URL": "not-a-url"}},
		{"metrics url no scheme", map[string]string{"CONFIG_SOURCE": "static", "STATIC_FILE_PATH": "/f.json", "OFFLINE_METRICS_UPSTREAM_URL": "metrics.example.com/v1/metrics"}},
		{"invalid source", map[string]string{"CONFIG_SOURCE": "file"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if _, err := FromEnv(); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

func TestFromEnv_StaticVars_RejectedInCDNMode(t *testing.T) {
	tests := []struct {
		name string
		env  string
		val  string
	}{
		{"static file path", "STATIC_FILE_PATH", "/f.json"},
		{"offline metrics url", "OFFLINE_METRICS_UPSTREAM_URL", "https://x/v1/metrics"},
		{"static poll interval", "STATIC_FILE_POLL_INTERVAL", "5s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.val)
			if _, err := FromEnv(); err == nil {
				t.Errorf("expected error when %s set in CDN mode", tc.env)
			}
		})
	}
}

func TestConfig_LogValue_StaticMode(t *testing.T) {
	cfg := Config{ConfigSource: "static", StaticFilePath: "/etc/flags.json", StaticFilePollInterval: 10 * time.Second}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("test", slog.Any("config", cfg))
	out := buf.String()
	for _, want := range []string{`"config_source":"static"`, `"static_file_path":"/etc/flags.json"`, `"offline_metrics_upstream_url":"disabled"`} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %s in log output: %s", want, out)
		}
	}
}
