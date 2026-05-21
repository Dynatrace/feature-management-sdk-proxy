package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

// NewLogProvider constructs an OTLP/HTTP LoggerProvider with a batch processor.
// The endpoint is read from OTEL_EXPORTER_OTLP_ENDPOINT (standard OTel env var).
// Uses HTTP/protobuf transport (required by Dynatrace). TLS is enabled by default.
// The caller must defer Shutdown on the returned provider to flush pending log records.
func NewLogProvider(ctx context.Context, res *resource.Resource) (*log.LoggerProvider, error) {

	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("logs: create OTLP exporter: %w", err)
	}

	provider := log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(log.NewBatchProcessor(exporter)),
	)

	return provider, nil
}
