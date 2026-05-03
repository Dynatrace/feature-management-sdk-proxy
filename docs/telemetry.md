# Telemetry

The proxy emits OpenTelemetry metrics and tracks per-SDK-token request counts for billing. Both subsystems are wired at startup and require no per-component configuration.

## OTel Metrics

All metric instruments are defined in `internal/metrics/metrics.go` and registered once at startup via `metrics.Register(meter)`. Components receive the returned `*metrics.Instruments` struct; they never create instruments inline.

**OTel SDK configuration** is driven by standard environment variables:

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Enables OTLP/gRPC export. When unset, metrics are emitted to a no-op provider. |
| `OTEL_RESOURCE_ATTRIBUTES` | Additional resource attributes attached to all metrics. |
| `OTEL_SERVICE_NAME` | Overrides the default service name (`sdk-proxy`). |

The exporter pushes every **30 seconds** over gRPC without TLS (in-cluster collector traffic assumed). The meter name is `github.com/dynatrace/sdk-proxy`.

### Full Metrics Reference

| Metric | Instrument | Unit | Labels | Description |
|---|---|---|---|---|
| `proxy.requests.total` | Counter | `{request}` | `result` | Completed proxy requests. `result`: `hit`, `miss`, `upstream_error`, `client_error` |
| `proxy.cache.size` | Gauge | `By` | `sdk_key_hash` | Cached payload size in bytes |
| `proxy.sse.connections` | UpDownCounter | `{connection}` | `sdk_key_hash` | Active upstream SSE connections |
| `proxy.sse.client.connections` | UpDownCounter | `{connection}` | `sdk_key_hash` | Active SDK client SSE connections to the proxy |
| `proxy.sse.events_delivered_total` | Counter | `{event}` | `sdk_key_hash` | Events successfully written to SDK clients |
| `proxy.sse.upstream.reconnects` | Counter | `{reconnect}` | `sdk_key_hash`, `reason` | Upstream SSE reconnect attempts. `reason`: `eof`, `io_error`, `inactivity`, `url_change` |
| `proxy.sse.upstream.events` | Counter | `{event}` | `sdk_key_hash`, `result` | Upstream SSE events received. `result`: `forwarded`, `dropped_no_subscribers`, `dropped_slow_subscriber` |
| `proxy.snapshot.age_seconds` | Gauge | `s` | `sdk_key_hash` | Seconds since the snapshot was last written |
| `proxy.snapshot.writes.total` | Counter | `{write}` | `result` | Snapshot write attempts. `result`: `success`, `failure` |
| `proxy.billing.flush.total` | Counter | `{flush}` | `result` | Billing flush attempts. `result`: `success`, `permanent_error`, `ctx_error`, `retry` |
| `proxy.billing.buffer.size` | Gauge | `{record}` | — | Current unsent billing records in the buffer |
| `proxy.billing.flush.lag_seconds` | Histogram | `s` | — | Seconds between oldest unsent record and flush time |
| `proxy.billing.retry_buffer.dropped_total` | Counter | `{sample}` | — | Billing samples evicted from the retry buffer due to overflow |
| `proxy.startup.path` | Counter | `{startup}` | `path` | Startup count by path. `path`: `cold`, `snapshot`, `cache` (see startup docs) |
| `proxy.config.sse_rewrite_total` | Counter | `{rewrite}` | `result` | SSE URL rewrite outcomes. `result`: `rewritten`, `no_sse_entry`, `not_found_in_body`, `unknown` |

> ⚠️ All `sdk_key_hash` label values are `SHA-256(sdk_key)[:16]`. Raw SDK keys never appear in metric labels.

## Per-SDK-Token Request Counter

The `Counter` (`internal/telemetry/counter.go`) tracks request counts per SDK token for billing. It is incremented on every request with a recognized SDK key — cache hits, cache misses, upstream errors (503), and HEAD requests all count, because they all reflect real SDK usage.

**`Counter.Increment`** is O(1) on the hot path. It uses a read-lock for known keys and a write-lock only when a new SDK key is first seen.

**`Counter.Drain`** atomically resets all counts and returns non-zero samples. It is called by the `Flusher` on each billing interval. An `Increment` that races a `Drain` for the same key will appear in the next cycle — the per-key drain is atomic but the full snapshot across all keys is not.

The raw SDK key is retained in memory (needed for billing flush authentication) and is never logged. `Sample.String()` redacts it in all formatted output.

## Batch-and-Flush Pipeline

The `Flusher` (`internal/telemetry/flush.go`) drains the `Counter` on a configurable interval and dispatches each `Sample` to a `BillingReporter`.

**Flush interval**: set by `BILLING_FLUSH_INTERVAL` (minimum 10s, default 60s).

### Flush Cycle

On each tick:

1. **Retry buffer first** — previously failed samples are retried before fresh ones. Their original window timestamps are preserved.
2. **Fresh samples** — the counter is drained and samples are stamped with `[windowStart, windowEnd)` before dispatch.
3. `windowStart` advances to `windowEnd` at the end of each tick.

### Error Handling

Each `Sample` is dispatched with a per-call timeout of `min(2 * flushInterval, 30s)`.

| Error type | Action |
|---|---|
| `nil` (2xx) | Success; `BillingFlushLag` recorded |
| `ErrPermanent` (non-retriable 4xx) | Sample dropped; warning logged |
| Context cancelled/deadline (per-call timeout) | Re-enqueued to retry buffer if outer context is live; dropped on shutdown |
| Any other error | Re-enqueued to retry buffer |

### Retry Buffer

Failed samples are held in a **bounded retry buffer** (`BoundedRetryBuffer`). When the buffer is full, the oldest sample is evicted and `proxy.billing.retry_buffer.dropped_total` is incremented. Buffer size is set by `BILLING_RETRY_BUFFER_SIZE` (range 1–100000, default 1000).

### Billing Endpoint Contract

The reporter sends one HTTP POST per SDK token:

```
POST <BILLING_ENDPOINT_BASE>/v1/usage
Authorization: Api-Token <sdk_key>
Content-Type: application/json

{
  "schema_version": 1,
  "key_hash":       "<16 hex chars>",
  "request_count":  <int64>,
  "window_start":   "<RFC3339Nano UTC>",
  "window_end":     "<RFC3339Nano UTC>",
  "proxy_instance": "<pod/container identity>"
}
```

- The raw SDK key is the auth credential only — it never appears in the request body.
- `key_hash` is `SHA-256(sdk_key)[:16]` (same as `cache.Key()`).
- Zero-count samples are not reported.
- The Worker deduplicates by `(key_hash, window_start)`.

**Response handling**: 2xx = success; 408/429/5xx = retriable; other 4xx = permanent failure (dropped).

### Shutdown Flush

On SIGTERM, after the HTTP server finishes draining in-flight requests, the flusher performs one final tick with a **10-second deadline** (`billingShutdownGrace`). Samples that cannot be dispatched before the deadline are logged and dropped — they cannot be re-enqueued because the process is exiting.
