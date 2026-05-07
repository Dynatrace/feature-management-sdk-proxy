package metrics

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

const defaultPushInterval = 30 * time.Second

// NewProvider constructs an OTLP/HTTP MeterProvider that pushes every 30 seconds.
// The endpoint is read from OTEL_EXPORTER_OTLP_ENDPOINT (standard OTel env var);
// callers should not pass a custom endpoint — use the standard var instead.
// Uses HTTP/protobuf transport (required by Dynatrace). TLS is enabled by default.
// The caller must defer Shutdown on the returned provider to flush pending metrics.
func NewProvider(ctx context.Context) (*metric.MeterProvider, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName("sdk-proxy")),
		resource.WithFromEnv(),      // honours OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME
		resource.WithTelemetrySDK(), // adds telemetry.sdk.* attributes
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: build resource: %w", err)
	}

	exporter, err := otlpmetrichttp.New(ctx,
		otlpmetrichttp.WithTemporalitySelector(metric.LowMemoryTemporalitySelector),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: create OTLP exporter: %w", err)
	}

	provider := metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(
			metric.NewPeriodicReader(exporter, metric.WithInterval(defaultPushInterval)),
		),
	)

	return provider, nil
}
