package metrics

import (
	"fmt"

	"go.opentelemetry.io/otel/metric"
)

// Instrument name constants — all proxy metrics are defined here.
const (
	RequestsTotal                   = "proxy.requests.total"
	SSEConnections                  = "proxy.sse.connections"
	SSEUpstreamEvents               = "proxy.sse.upstream.events"
	SnapshotAgeSeconds              = "proxy.snapshot.age_seconds"
	BillingFlushTotal               = "proxy.billing.flush.total"
	BillingBufferSize               = "proxy.billing.buffer.size"
	BillingFlushLag                 = "proxy.billing.flush.lag_seconds"
	BillingRetryBufferDropped       = "proxy.billing.retry_buffer.dropped_total"
	SnapshotWritesTotal             = "proxy.snapshot.writes.total"
	ConfigSSERewriteTotal           = "proxy.config.sse_rewrite_total"
	SnapshotLoadTotal               = "proxy.snapshot.load.total"
	LifecycleEventsTotal            = "proxy.lifecycle.events_total"
	LifecycleInvalidatedKeys        = "proxy.lifecycle.invalidated_keys_total"
	CacheInactivityEvictionsTotal   = "proxy.cache.inactivity_evictions.total"
	CacheRefreshTotal               = "proxy.cache.refresh.total"
	SnapshotDemandRevalidationTotal = "proxy.snapshot.demand_revalidation.total"
	SnapshotOutageFallbackTotal     = "proxy.snapshot.outage_fallback.total"
	SSESubscribeRejectedTotal       = "proxy.sse.subscribe.rejected_total"
)

// Instruments holds every OTel instrument the proxy emits. Obtain one via Register.
type Instruments struct {
	// RequestsTotal counts completed proxy requests, labeled by result
	// (hit, miss, upstream_error, client_error).
	RequestsTotal metric.Int64Counter

	// SSEConnections tracks active SSE connections partitioned by direction and state.
	// direction=upstream: one per SDK key; state=healthy (streaming) or state=unhealthy (connecting/reconnecting).
	// direction=downstream: one per connected SDK client; state=connected.
	// sum{direction="upstream"} == total upstream connections; query state="unhealthy" to alert on connectivity issues.
	SSEConnections metric.Int64UpDownCounter

	// SSEUpstreamEvents counts SSE events received from upstream per sdk_key_public_id
	// labeled by result (forwarded).
	SSEUpstreamEvents metric.Int64Counter

	// SnapshotAgeSeconds reports seconds since the snapshot was last written, per sdk_key_public_id.
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

	// SnapshotWritesTotal counts snapshot write outcomes labeled by result (success, failure, dropped)
	// and operation (put, delete). dropped = write discarded at queue level due to overflow (always put).
	SnapshotWritesTotal metric.Int64Counter

	// ConfigSSERewriteTotal counts SSE URL rewrite outcomes per served config request,
	// labeled by result (rewritten/no_sse_entry/not_found_in_body/unknown).
	ConfigSSERewriteTotal metric.Int64Counter

	// SnapshotLoadTotal counts snapshot objects processed at startup, labeled by
	// result (loaded/skipped_unknown_version/skipped_corrupt/backend_error).
	SnapshotLoadTotal metric.Int64Counter

	// LifecycleEventsTotal counts lifecycle events received, labeled by event_type and result.
	LifecycleEventsTotal metric.Int64Counter

	// LifecycleInvalidatedKeys counts keys invalidated by lifecycle events, labeled by event_type.
	LifecycleInvalidatedKeys metric.Int64Counter

	// CacheInactivityEvictionsTotal counts cache entries evicted by the inactivity sweeper.
	CacheInactivityEvictionsTotal metric.Int64Counter

	// CacheRefreshTotal counts all non-200/304 background refresh outcomes labeled by result
	// (evicted/preserved_stale) and status_code. evicted = 4xx from upstream (401/403/404/410);
	// preserved_stale = 429 or 5xx — entry kept to avoid amplifying outages.
	CacheRefreshTotal metric.Int64Counter

	// SnapshotDemandRevalidationTotal counts demand revalidations of snapshot-loaded
	// entries triggered on the first stale hit, labeled by result:
	// hit (not stale — no CDN call), not_modified (304), refreshed (200),
	// evicted (4xx), preserved_stale (429/5xx), error (network).
	SnapshotDemandRevalidationTotal metric.Int64Counter

	// SnapshotOutageFallbackTotal counts async snapshot store reads triggered by
	// background CDN refresh failures, labeled by result:
	// loaded/no_newer/same_etag/debounced/backend_error/decode_error/superseded.
	SnapshotOutageFallbackTotal metric.Int64Counter

	// SSESubscribeRejectedTotal counts SSE subscribe requests rejected by per-key
	// isolation limits, labeled by reason (rate_limit/fanout_cap).
	// No sdk_key_public_id label — per-key cardinality is intentionally excluded.
	SSESubscribeRejectedTotal metric.Int64Counter
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
		metric.WithDescription("Active SSE connections by direction (upstream/downstream) and state. upstream/healthy = streaming; upstream/unhealthy = connecting or reconnecting; downstream/connected = active SDK client connection."),
		metric.WithUnit("{connection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SSEConnections, err)
	}

	sseEvents, err := meter.Int64Counter(SSEUpstreamEvents,
		metric.WithDescription("Upstream SSE events received by sdk_key_public_id and result (forwarded)"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SSEUpstreamEvents, err)
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

	snapshotWrites, err := meter.Int64Counter(SnapshotWritesTotal,
		metric.WithDescription("Snapshot write outcomes by result (success/failure/dropped) and operation (put/delete). dropped = queue overflow, always put."),
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

	cacheRefreshTotal, err := meter.Int64Counter(CacheRefreshTotal,
		metric.WithDescription("Non-200/304 background refresh outcomes labeled by result (evicted/preserved_stale) and status_code. evicted = 4xx; preserved_stale = 429 or 5xx."),
		metric.WithUnit("{entry}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", CacheRefreshTotal, err)
	}

	snapshotDemandReval, err := meter.Int64Counter(SnapshotDemandRevalidationTotal,
		metric.WithDescription("Demand revalidations of snapshot-loaded entries on first stale hit, labeled by result (hit/not_modified/refreshed/evicted/preserved_stale/error)"),
		metric.WithUnit("{revalidation}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SnapshotDemandRevalidationTotal, err)
	}

	snapshotOutageFallback, err := meter.Int64Counter(SnapshotOutageFallbackTotal,
		metric.WithDescription("Async snapshot store reads triggered by background CDN refresh failures, labeled by result (loaded/no_newer/same_etag/debounced/backend_error/decode_error/superseded)"),
		metric.WithUnit("{check}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SnapshotOutageFallbackTotal, err)
	}

	sseSubRejected, err := meter.Int64Counter(SSESubscribeRejectedTotal,
		metric.WithDescription("SSE subscribe requests rejected by per-key isolation limits, labeled by reason (rate_limit/fanout_cap)"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("metrics: register %s: %w", SSESubscribeRejectedTotal, err)
	}

	return &Instruments{
		RequestsTotal:                   reqTotal,
		SSEConnections:                  sseConns,
		SSEUpstreamEvents:               sseEvents,
		SnapshotAgeSeconds:              snapshotAge,
		BillingFlushTotal:               billingFlush,
		BillingBufferSize:               billingBuf,
		BillingFlushLag:                 billingLag,
		SnapshotWritesTotal:             snapshotWrites,
		ConfigSSERewriteTotal:           sseRewrite,
		BillingRetryBufferDropped:       retryBufDropped,
		SnapshotLoadTotal:               snapshotLoadTotal,
		LifecycleEventsTotal:            lifecycleEventsTotal,
		LifecycleInvalidatedKeys:        lifecycleInvalidatedKeys,
		CacheInactivityEvictionsTotal:   cacheInactivityEvictions,
		CacheRefreshTotal:               cacheRefreshTotal,
		SnapshotDemandRevalidationTotal: snapshotDemandReval,
		SnapshotOutageFallbackTotal:     snapshotOutageFallback,
		SSESubscribeRejectedTotal:       sseSubRejected,
	}, nil
}
