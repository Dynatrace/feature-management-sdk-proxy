package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestFromEnv_Defaults(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")

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
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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

func TestFromEnv_MissingCDNBaseDomain(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "")
	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for missing CDN_BASE_DOMAIN")
	}
}

func TestFromEnv_InvalidPort(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("PORT", "99999")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for out-of-range PORT")
	}
}

func TestFromEnv_InvalidLogLevel(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("LOG_LEVEL", "VERBOSE")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid LOG_LEVEL")
	}
}

func TestFromEnv_InvalidShutdownTimeout(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("SHUTDOWN_TIMEOUT", "notaduration")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid SHUTDOWN_TIMEOUT")
	}
}

func TestFromEnv_BillingFlushInterval_Default(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BillingFlushInterval != 60*time.Second {
		t.Errorf("BillingFlushInterval = %v, want 60s", cfg.BillingFlushInterval)
	}
}

func TestFromEnv_BillingFlushInterval_ValidOverride(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("BILLING_FLUSH_INTERVAL", "5s")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for BILLING_FLUSH_INTERVAL < 10s")
	}
}

func TestFromEnv_BillingFlushInterval_Malformed(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("BILLING_FLUSH_INTERVAL", "notaduration")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for malformed BILLING_FLUSH_INTERVAL")
	}
}

func TestFromEnv_ProxyInstanceID_Override(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.BillingRetryBufferSize != 1000 {
		t.Errorf("BillingRetryBufferSize = %d, want 1000", cfg.BillingRetryBufferSize)
	}
}

func TestFromEnv_BillingRetryBufferSize_ValidOverride(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("BILLING_RETRY_BUFFER_SIZE", "0")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for BILLING_RETRY_BUFFER_SIZE=0")
	}
}

func TestFromEnv_BillingRetryBufferSize_Negative(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("BILLING_RETRY_BUFFER_SIZE", "-1")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for negative BILLING_RETRY_BUFFER_SIZE")
	}
}

func TestFromEnv_BillingRetryBufferSize_Malformed(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("BILLING_RETRY_BUFFER_SIZE", "notanint")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for malformed BILLING_RETRY_BUFFER_SIZE")
	}
}

func TestFromEnv_BillingRetryBufferSize_TooLarge(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("BILLING_RETRY_BUFFER_SIZE", "100001")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for BILLING_RETRY_BUFFER_SIZE > 100000")
	}
}

func TestFromEnv_SnapshotBackend_S3(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("SNAPSHOT_BACKEND", "local")
	t.Setenv("SNAPSHOT_LOCAL_DIR", "")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error when SNAPSHOT_LOCAL_DIR is empty")
	}
}

func TestFromEnv_SnapshotBackend_Local_RelativePath(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("SNAPSHOT_BACKEND", "local")
	t.Setenv("SNAPSHOT_LOCAL_DIR", "relative/path")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for relative SNAPSHOT_LOCAL_DIR")
	}
}

func TestFromEnv_SnapshotBackend_Invalid(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SnapshotMaxAge != 24*time.Hour {
		t.Errorf("SnapshotMaxAge = %v, want 24h", cfg.SnapshotMaxAge)
	}
}

func TestFromEnv_SnapshotMaxAge_ValidOverride(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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
			t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
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
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("SNAPSHOT_MAX_AGE", "6h")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error when SNAPSHOT_MAX_AGE set without SNAPSHOT_BACKEND")
	}
}
