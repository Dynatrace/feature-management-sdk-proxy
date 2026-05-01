package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestFromEnv_Defaults(t *testing.T) {
	t.Setenv("UPSTREAM_CDN_URL", "https://cdn.example.com")

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
	t.Setenv("UPSTREAM_CDN_URL", "https://cdn.example.com")
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

func TestFromEnv_MissingUpstream(t *testing.T) {
	t.Setenv("UPSTREAM_CDN_URL", "")
	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for missing UPSTREAM_CDN_URL")
	}
}

func TestFromEnv_InvalidPort(t *testing.T) {
	t.Setenv("UPSTREAM_CDN_URL", "https://cdn.example.com")
	t.Setenv("PORT", "99999")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for out-of-range PORT")
	}
}

func TestFromEnv_InvalidLogLevel(t *testing.T) {
	t.Setenv("UPSTREAM_CDN_URL", "https://cdn.example.com")
	t.Setenv("LOG_LEVEL", "VERBOSE")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid LOG_LEVEL")
	}
}

func TestFromEnv_InvalidShutdownTimeout(t *testing.T) {
	t.Setenv("UPSTREAM_CDN_URL", "https://cdn.example.com")
	t.Setenv("SHUTDOWN_TIMEOUT", "notaduration")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid SHUTDOWN_TIMEOUT")
	}
}

func TestFromEnv_InvalidUpstreamURL(t *testing.T) {
	t.Setenv("UPSTREAM_CDN_URL", "not-a-url")

	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for invalid UPSTREAM_CDN_URL")
	}
}
