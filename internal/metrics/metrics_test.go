package metrics_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	internalmetrics "github.com/dynatrace/sdk-proxy/internal/metrics"
)

func TestRegister_NoopMeter(t *testing.T) {
	inst, err := internalmetrics.Register(noop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if inst == nil {
		t.Fatal("Register returned nil Instruments")
	}

	// Exercise each instrument to confirm it is usable without panicking.
	ctx := context.Background()
	result := attribute.String("result", "hit")
	keyID := attribute.String("sdk_key_public_id", "abc123")
	health := attribute.String("state", "healthy")

	direction := attribute.String("direction", "upstream")
	inst.RequestsTotal.Add(ctx, 1, metric.WithAttributes(result))
	inst.SSEConnections.Add(ctx, 1, metric.WithAttributes(keyID, direction, health))
	inst.SSEConnections.Add(ctx, 1, metric.WithAttributes(keyID, attribute.String("direction", "downstream"), attribute.String("state", "connected")))
	inst.SSEUpstreamEvents.Add(ctx, 1, metric.WithAttributes(keyID, attribute.String("result", "forwarded")))
	inst.SnapshotAgeSeconds.Record(ctx, 30.5, metric.WithAttributes(keyID))
	inst.BillingFlushTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "success")))
	inst.BillingBufferSize.Record(ctx, 42)
	inst.BillingFlushLag.Record(ctx, 1.5)
	inst.SnapshotWritesTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "success")))
	inst.ConfigSSERewriteTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "rewritten"), keyID))
	inst.CacheRefreshTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "evicted"), attribute.String("status_code", "404")))
	inst.SnapshotDemandRevalidationTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "not_modified")))
	inst.SnapshotOutageFallbackTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "loaded")))
}
