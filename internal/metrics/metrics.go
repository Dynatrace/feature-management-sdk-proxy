package metrics

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// Instrument name constants — all proxy metrics are defined here.
const (
	RequestsTotal       = "proxy.requests.total"
	SSEConnections      = "proxy.sse.connections"
	CacheSize           = "proxy.cache.size"
	SnapshotAgeSeconds  = "proxy.snapshot.age_seconds"
	BillingFlushTotal   = "proxy.billing.flush.total"
	BillingBufferSize   = "proxy.billing.buffer.size"
	BillingFlushLag     = "proxy.billing.flush.lag_seconds"
	StartupPath         = "proxy.startup.path"
	SnapshotWritesTotal = "proxy.snapshot.writes.total"
)

// Instruments holds every OTel instrument the proxy emits. Obtain one via Register.
type Instruments struct {
	// RequestsTotal counts completed proxy requests, labeled by result
	// (hit, miss, upstream_error, client_error).
	RequestsTotal metric.Int64Counter

	// SSEConnections tracks active upstream SSE connections per sdk_key_hash.
	SSEConnections metric.Int64UpDownCounter

	// CacheSize reports the number of bytes held for a given sdk_key_hash.
	// Synchronous gauge: each component pushes updates at mutation time.
	CacheSize metric.Int64Gauge

	// SnapshotAgeSeconds reports seconds since the snapshot was last written, per sdk_key_hash.
	// Synchronous gauge: updated after each successful snapshot write and on startup.
	SnapshotAgeSeconds metric.Float64Gauge

	// BillingFlushTotal counts billing flush attempts labeled by result (success, failure).
	BillingFlushTotal metric.Int64Counter

	// BillingBufferSize reports the current number of unsent billing records.
	// Synchronous gauge: updated whenever records are added or flushed.
	BillingBufferSize metric.Int64Gauge

	// BillingFlushLag records the wall-clock lag between the oldest unsent record and flush time.
	BillingFlushLag metric.Float64Histogram

	// StartupPath counts proxy startups labeled by path (cold/snapshot/cache).
	// Each process instance increments exactly once at startup; useful for fleet-wide aggregation.
	StartupPath metric.Int64Counter

	// SnapshotWritesTotal counts snapshot write attempts labeled by result (success, failure).
	SnapshotWritesTotal metric.Int64Counter
}

// Register creates all metric instruments against meter. Call once at startup and
// pass the returned Instruments to every component that needs to record metrics.
func Register(meter metric.Meter) (*Instruments, error) {
	reqTotal, err := meter.Int64Counter(RequestsTotal,
		metric.WithDescription("Completed proxy requests by result (hit/miss/upstream_error/client_error)"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", RequestsTotal, err)
	}

	sseConns, err := meter.Int64UpDownCounter(SSEConnections,
		metric.WithDescription("Active upstream SSE connections per SDK key hash"),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SSEConnections, err)
	}

	cacheSize, err := meter.Int64Gauge(CacheSize,
		metric.WithDescription("Cached payload size in bytes per SDK key hash"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", CacheSize, err)
	}

	snapshotAge, err := meter.Float64Gauge(SnapshotAgeSeconds,
		metric.WithDescription("Seconds since the snapshot was last written per SDK key hash"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SnapshotAgeSeconds, err)
	}

	billingFlush, err := meter.Int64Counter(BillingFlushTotal,
		metric.WithDescription("Billing endpoint flush attempts by result (success/failure)"),
		metric.WithUnit("{flush}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", BillingFlushTotal, err)
	}

	billingBuf, err := meter.Int64Gauge(BillingBufferSize,
		metric.WithDescription("Current number of unsent billing records in the buffer"),
		metric.WithUnit("{record}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", BillingBufferSize, err)
	}

	billingLag, err := meter.Float64Histogram(BillingFlushLag,
		metric.WithDescription("Seconds between oldest unsent billing record and flush time"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 5, 10, 30, 60, 120, 300),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", BillingFlushLag, err)
	}

	startupPath, err := meter.Int64Counter(StartupPath,
		metric.WithDescription("Proxy startup count labeled by path (cold/snapshot/cache)"),
		metric.WithUnit("{startup}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", StartupPath, err)
	}

	snapshotWrites, err := meter.Int64Counter(SnapshotWritesTotal,
		metric.WithDescription("Snapshot write attempts by result (success/failure)"),
		metric.WithUnit("{write}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SnapshotWritesTotal, err)
	}

	return &Instruments{
		RequestsTotal:       reqTotal,
		SSEConnections:      sseConns,
		CacheSize:           cacheSize,
		SnapshotAgeSeconds:  snapshotAge,
		BillingFlushTotal:   billingFlush,
		BillingBufferSize:   billingBuf,
		BillingFlushLag:     billingLag,
		StartupPath:         startupPath,
		SnapshotWritesTotal: snapshotWrites,
	}, nil
}
