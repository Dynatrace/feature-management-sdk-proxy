package metrics

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// Instrument name constants — all proxy metrics are defined here.
const (
	RequestsTotal                   = "proxy.requests.total"
	SSEConnections                  = "proxy.sse.connections"
	SSEClientConnections            = "proxy.sse.client.connections"
	SSEEventsDelivered              = "proxy.sse.events_delivered_total"
	SSEUpstreamReconnects           = "proxy.sse.upstream.reconnects"
	SSEUpstreamEvents               = "proxy.sse.upstream.events"
	CacheSize                       = "proxy.cache.size"
	SnapshotAgeSeconds              = "proxy.snapshot.age_seconds"
	BillingFlushTotal               = "proxy.billing.flush.total"
	BillingBufferSize               = "proxy.billing.buffer.size"
	BillingFlushLag                 = "proxy.billing.flush.lag_seconds"
	BillingRetryBufferDropped       = "proxy.billing.retry_buffer.dropped_total"
	StartupPath                     = "proxy.startup.path"
	SnapshotWritesTotal             = "proxy.snapshot.writes.total"
	ConfigSSERewriteTotal           = "proxy.config.sse_rewrite_total"
	SnapshotLoadTotal               = "proxy.snapshot.load.total"
	SnapshotWriteQueueDropped       = "proxy.snapshot.write_queue.dropped_total"
	LifecycleEventsTotal            = "proxy.lifecycle.events_total"
	LifecycleInvalidatedKeys        = "proxy.lifecycle.invalidated_keys_total"
	CacheInactivityEvictionsTotal   = "proxy.cache.inactivity_evictions.total"
	CacheRefreshEvictionsTotal      = "proxy.cache.refresh_evictions.total"
	CacheRefreshPreservedStaleTotal = "proxy.cache.refresh_preserved_stale.total"
)

// Instruments holds every OTel instrument the proxy emits. Obtain one via Register.
type Instruments struct {
	// RequestsTotal counts completed proxy requests, labeled by result
	// (hit, miss, upstream_error, client_error).
	RequestsTotal metric.Int64Counter

	// SSEConnections tracks active upstream SSE connections per sdk_key_hash.
	SSEConnections metric.Int64UpDownCounter

	// SSEClientConnections tracks active SDK client SSE connections to the proxy
	// per sdk_key_hash.
	SSEClientConnections metric.Int64UpDownCounter

	// SSEEventsDelivered counts SSE events successfully written to SDK clients
	// per sdk_key_hash.
	SSEEventsDelivered metric.Int64Counter

	// SSEUpstreamReconnects counts upstream SSE reconnect attempts per sdk_key_hash
	// labeled by reason (eof/io_error/inactivity/url_change).
	SSEUpstreamReconnects metric.Int64Counter

	// SSEUpstreamEvents counts SSE events received from upstream per sdk_key_hash
	// labeled by result (forwarded/dropped_no_subscribers/dropped_slow_subscriber).
	SSEUpstreamEvents metric.Int64Counter

	// CacheSize reports the number of bytes held for a given sdk_key_hash.
	// Synchronous gauge: each component pushes updates at mutation time.
	CacheSize metric.Int64Gauge

	// SnapshotAgeSeconds reports seconds since the snapshot was last written, per sdk_key_hash.
	// Synchronous gauge: updated after each successful snapshot write and on startup.
	SnapshotAgeSeconds metric.Float64Gauge

	// BillingFlushTotal counts billing flush attempts labeled by result
	// (success/permanent_error/ctx_error/retry).
	BillingFlushTotal metric.Int64Counter

	// BillingBufferSize reports the current number of unsent billing records.
	// Synchronous gauge: updated whenever records are added or flushed.
	BillingBufferSize metric.Int64Gauge

	// BillingRetryBufferDropped counts samples evicted from the retry buffer due to overflow.
	BillingRetryBufferDropped metric.Int64Counter

	// BillingFlushLag records the wall-clock lag between the oldest unsent record and flush time.
	BillingFlushLag metric.Float64Histogram

	// StartupPath counts proxy startups labeled by path
	// (cold/snapshot/snapshot_stale/empty_persistence_cold/backend_unreachable).
	// cold: no persistence configured. snapshot: fresh snapshots loaded.
	// snapshot_stale: snapshots loaded but oldest entry meets or exceeds SNAPSHOT_MAX_AGE.
	// empty_persistence_cold: backend reachable but store empty — proxy starts cold.
	// backend_unreachable: backend unreachable — process exits after emit.
	// Each process instance increments exactly once at startup; useful for fleet-wide aggregation.
	StartupPath metric.Int64Counter

	// SnapshotWritesTotal counts snapshot write attempts labeled by result (success, failure).
	SnapshotWritesTotal metric.Int64Counter

	// ConfigSSERewriteTotal counts SSE URL rewrite outcomes per served config request,
	// labeled by result (rewritten/no_sse_entry/not_found_in_body/unknown).
	ConfigSSERewriteTotal metric.Int64Counter

	// SnapshotLoadTotal counts snapshot objects processed at startup, labeled by
	// result (loaded/skipped_unknown_version/skipped_corrupt/backend_error).
	SnapshotLoadTotal metric.Int64Counter

	// SnapshotWriteQueueDropped counts snapshot writes dropped due to queue overflow.
	SnapshotWriteQueueDropped metric.Int64Counter

	// LifecycleEventsTotal counts lifecycle events received, labeled by event_type and result.
	LifecycleEventsTotal metric.Int64Counter

	// LifecycleInvalidatedKeys counts keys invalidated by lifecycle events, labeled by event_type.
	LifecycleInvalidatedKeys metric.Int64Counter

	// CacheInactivityEvictionsTotal counts cache entries evicted by the inactivity sweeper.
	CacheInactivityEvictionsTotal metric.Int64Counter

	// CacheRefreshEvictionsTotal counts cache entries evicted during background refresh
	// due to a 4xx response from upstream, labeled by status_code (401/403/404/410).
	CacheRefreshEvictionsTotal metric.Int64Counter

	// CacheRefreshPreservedStaleTotal counts background refresh responses that were
	// non-200/non-304 but did NOT trigger eviction (e.g. 429, 5xx), labeled by
	// status_code. Enables alerting on sustained upstream errors without eviction.
	CacheRefreshPreservedStaleTotal metric.Int64Counter
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

	sseClientConns, err := meter.Int64UpDownCounter(SSEClientConnections,
		metric.WithDescription("Active SDK client SSE connections to the proxy per SDK key hash"),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SSEClientConnections, err)
	}

	sseEventsDelivered, err := meter.Int64Counter(SSEEventsDelivered,
		metric.WithDescription("SSE events successfully written to SDK clients per SDK key hash"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SSEEventsDelivered, err)
	}

	sseReconnects, err := meter.Int64Counter(SSEUpstreamReconnects,
		metric.WithDescription("Upstream SSE reconnect attempts by sdk_key_hash and reason (eof/io_error/inactivity/url_change)"),
		metric.WithUnit("{reconnect}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SSEUpstreamReconnects, err)
	}

	sseEvents, err := meter.Int64Counter(SSEUpstreamEvents,
		metric.WithDescription("Upstream SSE events received by sdk_key_hash and result (forwarded/dropped_no_subscribers/dropped_slow_subscriber)"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SSEUpstreamEvents, err)
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
		metric.WithDescription("Billing endpoint flush attempts by result (success/permanent_error/ctx_error/retry)"),
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
		metric.WithDescription("Proxy startup count labeled by path (cold/snapshot/snapshot_stale/empty_persistence_cold/backend_unreachable)"),
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

	sseRewrite, err := meter.Int64Counter(ConfigSSERewriteTotal,
		metric.WithDescription("SSE URL rewrite outcomes per served config request, labeled by result (rewritten/no_sse_entry/not_found_in_body/unknown)"),
		metric.WithUnit("{rewrite}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", ConfigSSERewriteTotal, err)
	}

	retryBufDropped, err := meter.Int64Counter(BillingRetryBufferDropped,
		metric.WithDescription("Billing samples evicted from the retry buffer due to overflow"),
		metric.WithUnit("{sample}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", BillingRetryBufferDropped, err)
	}

	snapshotLoadTotal, err := meter.Int64Counter(SnapshotLoadTotal,
		metric.WithDescription("Snapshot objects processed at startup by result (loaded/skipped_unknown_version/skipped_corrupt/backend_error)"),
		metric.WithUnit("{object}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SnapshotLoadTotal, err)
	}

	snapshotWriteQueueDropped, err := meter.Int64Counter(SnapshotWriteQueueDropped,
		metric.WithDescription("Snapshot writes dropped due to write queue overflow"),
		metric.WithUnit("{write}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SnapshotWriteQueueDropped, err)
	}

	lifecycleEventsTotal, err := meter.Int64Counter(LifecycleEventsTotal,
		metric.WithDescription("Lifecycle SSE events received by event_type and result (handled/malformed)"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", LifecycleEventsTotal, err)
	}

	lifecycleInvalidatedKeys, err := meter.Int64Counter(LifecycleInvalidatedKeys,
		metric.WithDescription("Keys invalidated by lifecycle SSE events, labeled by event_type"),
		metric.WithUnit("{key}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", LifecycleInvalidatedKeys, err)
	}

	cacheInactivityEvictions, err := meter.Int64Counter(CacheInactivityEvictionsTotal,
		metric.WithDescription("Cache entries evicted by the inactivity sweeper (idle for longer than CACHE_INACTIVITY_EVICT_AFTER)"),
		metric.WithUnit("{entry}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", CacheInactivityEvictionsTotal, err)
	}

	cacheRefreshEvictions, err := meter.Int64Counter(CacheRefreshEvictionsTotal,
		metric.WithDescription("Cache entries evicted during background refresh due to a 4xx response from upstream, labeled by status_code"),
		metric.WithUnit("{entry}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", CacheRefreshEvictionsTotal, err)
	}

	cacheRefreshPreservedStale, err := meter.Int64Counter(CacheRefreshPreservedStaleTotal,
		metric.WithDescription("Background refresh responses that were non-200/non-304 but did not trigger eviction (e.g. 429, 5xx), labeled by status_code"),
		metric.WithUnit("{response}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", CacheRefreshPreservedStaleTotal, err)
	}

	return &Instruments{
		RequestsTotal:                   reqTotal,
		SSEConnections:                  sseConns,
		SSEClientConnections:            sseClientConns,
		SSEEventsDelivered:              sseEventsDelivered,
		SSEUpstreamReconnects:           sseReconnects,
		SSEUpstreamEvents:               sseEvents,
		CacheSize:                       cacheSize,
		SnapshotAgeSeconds:              snapshotAge,
		BillingFlushTotal:               billingFlush,
		BillingBufferSize:               billingBuf,
		BillingFlushLag:                 billingLag,
		StartupPath:                     startupPath,
		SnapshotWritesTotal:             snapshotWrites,
		ConfigSSERewriteTotal:           sseRewrite,
		BillingRetryBufferDropped:       retryBufDropped,
		SnapshotLoadTotal:               snapshotLoadTotal,
		SnapshotWriteQueueDropped:       snapshotWriteQueueDropped,
		LifecycleEventsTotal:            lifecycleEventsTotal,
		LifecycleInvalidatedKeys:        lifecycleInvalidatedKeys,
		CacheInactivityEvictionsTotal:   cacheInactivityEvictions,
		CacheRefreshEvictionsTotal:      cacheRefreshEvictions,
		CacheRefreshPreservedStaleTotal: cacheRefreshPreservedStale,
	}, nil
}
