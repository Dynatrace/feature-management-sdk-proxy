package telemetry

import (
	"context"
	"log/slog"
)

// LogReporter is a BillingReporter that logs each Sample at info level.
// It is the production placeholder until the HTTP reporter is implemented.
// It never writes the raw SDKKey — only KeyHash, Count, window, and instance.
type LogReporter struct {
	logger          *slog.Logger
	proxyInstanceID string
}

// NewLogReporter constructs a LogReporter. proxyInstanceID identifies this
// proxy instance in log output (typically the pod or container name).
func NewLogReporter(logger *slog.Logger, proxyInstanceID string) *LogReporter {
	return &LogReporter{logger: logger, proxyInstanceID: proxyInstanceID}
}

// Report logs the Sample and returns nil. Safe for concurrent callers.
func (r *LogReporter) Report(_ context.Context, s Sample) error {
	r.logger.Info("billing: flush",
		slog.String("key_hash", s.KeyHash),
		slog.Int64("count", s.Count),
		slog.Time("window_start", s.WindowStart),
		slog.Time("window_end", s.WindowEnd),
		slog.String("proxy_instance", r.proxyInstanceID),
	)
	return nil
}
