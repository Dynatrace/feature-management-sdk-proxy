package tracing

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// NewProvider constructs an OTLP/HTTP TracerProvider with a batch span processor.
// The endpoint is read from OTEL_EXPORTER_OTLP_ENDPOINT (standard OTel env var);
// callers should not pass a custom endpoint — use the standard var instead.
// Uses HTTP/protobuf transport (required by Dynatrace). TLS is enabled by default.
// The caller must defer Shutdown on the returned provider to flush pending spans.
func NewProvider(ctx context.Context) (*sdktrace.TracerProvider, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("sdk-proxy")),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: build resource: %w", err)
	}

	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("tracing: create OTLP exporter: %w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
	)

	return provider, nil
}
