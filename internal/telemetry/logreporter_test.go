package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Compile-time assertion: LogReporter satisfies BillingReporter.
var _ BillingReporter = (*LogReporter)(nil)

func TestLogReporter_Report_SDKKeyAbsent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	r := NewLogReporter(logger, "test-instance-1")

	s := Sample{
		KeyID:       "aabbccddeeff0011",
		SDKKey:      "super-secret-sdk-key-do-not-log",
		Count:       42,
		WindowStart: time.Now().Add(-time.Minute).UTC(),
		WindowEnd:   time.Now().UTC(),
	}

	if err := r.Report(context.Background(), s); err != nil {
		t.Fatalf("Report returned unexpected error: %v", err)
	}

	output := buf.String()

	if strings.Contains(output, s.SDKKey) {
		t.Errorf("log output contains raw SDKKey %q — must never appear in logs", s.SDKKey)
	}
	if !strings.Contains(output, s.KeyID) {
		t.Errorf("log output missing key_hash %q", s.KeyID)
	}
	if !strings.Contains(output, "42") {
		t.Errorf("log output missing count field")
	}
	if !strings.Contains(output, "test-instance-1") {
		t.Errorf("log output missing proxy_instance")
	}
}

func TestLogReporter_Report_ReturnsNil(t *testing.T) {
	var buf bytes.Buffer
	r := NewLogReporter(slog.New(slog.NewJSONHandler(&buf, nil)), "instance-x")
	s := Sample{KeyID: "0000000000000000", SDKKey: "any-key", Count: 1}

	if err := r.Report(context.Background(), s); err != nil {
		t.Errorf("Report() = %v, want nil", err)
	}
}
