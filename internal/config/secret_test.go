package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

const rawSecret = "AKIAIOSFODNN7EXAMPLE-super-secret-key-value"

func TestSecret_StringMethods(t *testing.T) {
	s := Secret(rawSecret)

	if got := s.String(); got != "[REDACTED]" {
		t.Errorf("String() = %q, want [REDACTED]", got)
	}
	if got := s.GoString(); got != "[REDACTED]" {
		t.Errorf("GoString() = %q, want [REDACTED]", got)
	}
}

func TestSecret_FmtFormatting(t *testing.T) {
	s := Secret(rawSecret)

	for _, format := range []string{"%v", "%s", "%q", "%+v", "%#v"} {
		out := fmt.Sprintf(format, s)
		if strings.Contains(out, rawSecret) {
			t.Errorf("fmt.Sprintf(%q, secret) leaked raw value: %q", format, out)
		}
	}
}

func TestSecret_MarshalJSON(t *testing.T) {
	s := Secret(rawSecret)

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if strings.Contains(string(b), rawSecret) {
		t.Errorf("json.Marshal leaked raw value: %s", b)
	}
	if string(b) != `"[REDACTED]"` {
		t.Errorf("json.Marshal = %s, want \"[REDACTED]\"", b)
	}
}

func TestSecret_MarshalText(t *testing.T) {
	s := Secret(rawSecret)

	b, err := s.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(b) != "[REDACTED]" {
		t.Errorf("MarshalText = %q, want [REDACTED]", string(b))
	}
}

func TestSecret_LogValue(t *testing.T) {
	s := Secret(rawSecret)

	val := s.LogValue()
	if strings.Contains(val.String(), rawSecret) {
		t.Errorf("LogValue leaked raw value: %v", val)
	}
}

func TestSecret_SlogOutput(t *testing.T) {
	s := Secret(rawSecret)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("test", slog.Any("secret", s))

	if strings.Contains(buf.String(), rawSecret) {
		t.Errorf("slog output leaked raw value: %s", buf.String())
	}
}

func TestSecret_Unwrap(t *testing.T) {
	s := Secret(rawSecret)

	if got := s.Unwrap(); got != rawSecret {
		t.Errorf("Unwrap() = %q, want %q", got, rawSecret)
	}
}

func TestConfig_NoCredentialLeak(t *testing.T) {
	t.Setenv("CDN_BASE_DOMAIN", "fm.dynatracelabs.com")
	t.Setenv("SNAPSHOT_BACKEND", "s3")
	t.Setenv("SNAPSHOT_S3_BUCKET", "my-bucket")
	t.Setenv("SNAPSHOT_S3_REGION", "us-east-1")
	t.Setenv("SNAPSHOT_S3_ACCESS_KEY", rawSecret+"-access")
	t.Setenv("SNAPSHOT_S3_SECRET_KEY", rawSecret+"-secret")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}

	for _, format := range []string{"%v", "%+v", "%#v"} {
		out := fmt.Sprintf(format, cfg)
		if strings.Contains(out, rawSecret) {
			t.Errorf("fmt.Sprintf(%q, cfg) leaked credential: %q", format, out)
		}
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal(cfg): %v", err)
	}
	if strings.Contains(string(b), rawSecret) {
		t.Errorf("json.Marshal(cfg) leaked credential: %s", b)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("config", slog.Any("cfg", cfg))
	if strings.Contains(buf.String(), rawSecret) {
		t.Errorf("slog output leaked credential: %s", buf.String())
	}
}
