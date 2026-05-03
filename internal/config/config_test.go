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
