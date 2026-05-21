# Telemetry

The proxy emits [OpenTelemetry](https://opentelemetry.io/) metrics, distributed traces, and structured logs via OTLP/HTTP. When no exporter is configured, all three signals are sent to no-op providers and discarded; the proxy runs without an OTel collector.

## Configuring the Exporter

Set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable export. Metrics push every **30 seconds**; traces and logs are batched and flushed continuously. All three signals use HTTP/protobuf transport (required by Dynatrace).

**Stdout JSON logging is always active** and is not gated on `OTEL_EXPORTER_OTLP_ENDPOINT`. When OTLP export is enabled, the OTel log bridge sends the same log records to the collector as an additive second sink; `kubectl logs` and `docker logs` continue to work unchanged.

| Variable                      | Effect                                                                                                                                                                                                     |
|-------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector endpoint (e.g. `http://otel-collector:4318`). Required to enable export of metrics, traces, and logs.                                                                                            |
| `OTEL_SERVICE_NAME`           | Override the service name. Defaults to `sdk-proxy`.                                                                                                                                                        |
| `OTEL_RESOURCE_ATTRIBUTES`    | Comma-separated key=value pairs added to all metrics, traces, and logs (e.g. `env=prod,region=us-east-1`).                                                                                                 |
| `OTEL_TRACES_SAMPLER`         | Sampling strategy for traces (default: `parentbased_always_on`). See [OTel SDK docs](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/#general-sdk-configuration).        |

## Metrics Reference

| Metric                                      | Instrument    | Unit             | Labels                        | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
|---------------------------------------------|---------------|------------------|-------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `proxy.requests.total`                      | Counter       | `{request}`      | `result`                      | Completed proxy requests. `result`: `hit`, `miss`, `upstream_error`, `client_error`                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `proxy.config.sse_rewrite_total`            | Counter       | `{rewrite}`      | `result`                      | SSE URL rewrite outcomes. `result`: `rewritten`, `no_sse_entry`, `not_found_in_body`, `unknown`                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `proxy.sse.connections`                     | UpDownCounter | `{connection}`   | `sdk_key_public_id`, `direction`, `state` | Active SSE connections. `direction=upstream`: one per SDK key; `state`: `healthy` (streaming) or `unhealthy` (connecting/reconnecting). `direction=downstream`: one per connected SDK client; `state`: `connected`. Query `{direction="upstream"}` for total upstream count; alert when `{direction="upstream", state="unhealthy"} > 0` is sustained. |
| `proxy.sse.upstream.events`                 | Counter       | `{event}`        | `sdk_key_public_id`, `result` | Upstream SSE events received. `result`: `forwarded`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `proxy.sse.subscribe.rejected_total`        | Counter       | `{request}`      | `reason`                      | SSE subscribe requests rejected by per-key isolation limits. `reason`: `rate_limit`, `fanout_cap`. No `sdk_key_public_id` label (cardinality guard).                                                                                                                                                                                                                                                                                                                                                                                                 |
| `proxy.snapshot.age_seconds`                | Gauge         | `s`              | `sdk_key_public_id`           | Seconds since the snapshot was last written                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `proxy.snapshot.writes.total`               | Counter       | `{write}`        | `result`, `operation`         | Snapshot write outcomes. `result`: `success`, `failure`, `dropped` (queue overflow, always `put`); `operation`: `put`, `delete`                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `proxy.snapshot.load.total`                 | Counter       | `{object}`       | `result`                      | Snapshot objects processed at startup. `result`: `loaded`, `skipped_unknown_version`, `skipped_corrupt`, `backend_error`                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `proxy.lifecycle.events_total`              | Counter       | `{event}`        | `event_type`, `result`        | Lifecycle SSE events received. `result`: `handled`, `malformed`                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `proxy.lifecycle.invalidated_keys_total`    | Counter       | `{key}`          | `event_type`                  | Keys invalidated by lifecycle events                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `proxy.cache.inactivity_evictions.total`    | Counter       | `{entry}`        | —                             | Cache entries evicted by the inactivity sweeper (idle for longer than `CACHE_INACTIVITY_EVICT_AFTER`)                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `proxy.cache.refresh.total`                 | Counter       | `{entry}`        | `result`, `status_code`       | Non-200/304 background refresh outcomes. `result`: `evicted` (4xx: entry removed), `preserved_stale` (429/5xx: entry kept to avoid amplifying outages). `status_code`: the upstream HTTP status code.                                                                                                                                                                                                                                                                                                                                                |
| `proxy.snapshot.demand_revalidation.total`  | Counter       | `{revalidation}` | `result`                      | Demand revalidations of snapshot-loaded entries triggered on the first stale hit. `result`: `hit` (fresh, no CDN call), `not_modified` (304), `refreshed` (200), `evicted` (4xx), `preserved_stale` (429/5xx), `error` (network). See [cache.md](cache.md#demand-revalidation-of-snapshot-loaded-entries).                                                                                                                                                                                                                                           |
| `proxy.snapshot.outage_fallback.total`      | Counter       | `{check}`        | `result`                      | Async snapshot store reads triggered by background CDN refresh failures (network error or 429/5xx). Only emitted when `SNAPSHOT_BACKEND` is configured. `result`: `loaded` (newer snapshot applied), `no_newer` (snapshot not newer than current entry), `same_etag` (payload unchanged), `debounced` (suppressed by 60s per-key interval), `backend_error` (store read failed), `decode_error` (corrupt or unknown version envelope), `superseded` (concurrent refresh applied first). See [snapshot.md](snapshot.md#runtime-cross-proxy-fallback). |
| `proxy.metrics.requests.total`              | Counter       | `{request}`      | `result`                      | POST /v1/metrics proxy requests. `result`: `forwarded`, `bad_request`, `unauthorized`, `payload_too_large`, `upstream_error`, `bad_gateway`, `internal_error`. Note: no `sdk_key_public_id` label; use OTel spans for per-key correlation. |
| `proxy.metrics.forward.latency_seconds`     | Histogram     | `s`              | —                             | Wall-clock duration of the upstream `POST /v1/metrics` call from request entry to response written. Buckets: 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 seconds. |

See [monitoring.md](monitoring.md) for guidance on interpreting these metrics by operator concern.

## Tracing

The proxy emits distributed traces via OTLP/HTTP (same `OTEL_EXPORTER_OTLP_ENDPOINT` as metrics). Traces are structured
to match the cache relationship correctly: when a request is served from cache, the serving span uses a **Span Link** (
not a parent-child relationship) to reference the original upstream fetch span.

### Span Catalogue

| Span name                    | Where                                                                                                                               | Key attributes                                                                                                       |
|------------------------------|-------------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------|
| `GET /server/{sdk_key}.json` | Proxy handler: every SDK config request (name follows OTel HTTP server semconv: `{method} {route}`)                                 | `http.request.method`, `http.route`, `sdk_key_public_id`, `result` (`hit`, `miss`, `upstream_error`, `client_error`) |
| `proxy.serve_cache_hit`      | Serving a cached entry, child of `GET /server/` with a **Span Link** to the originating fetch when available                        | `sdk_key_public_id`, `result`                                                                                        |
| `proxy.cold_fetch`           | Upstream CDN fetch on a cache miss, child of `GET /server/`                                                                         | `sdk_key_public_id`, `upstream.status_code`, `result`                                                                |
| `HTTP GET`                   | CDN HTTP call, child of `proxy.cold_fetch` or `proxy.refresh_fetch`; `url.full` is intentionally omitted (URL contains raw SDK key) | `http.request.method`, `server.address`, `http.response.status_code`                                                 |
| `proxy.refresh_fetch`        | Background TTL revalidation fetch, standalone (not tied to any client request)                                                      | `sdk_key_public_id`, `if_none_match`, `result`                                                                       |
| `sse.upstream.connect`       | One span per upstream SSE connect attempt; reconnects emit separate spans                                                           | `sdk_key_public_id`, `outcome` (`connected`, `subscribe_failed`)                                                     |
| `sse.client.subscribe`       | SSE client connection setup phase (auth + cache gate + isolation gate + headers sent)                                               | `sdk_key_public_id`, `outcome` (`200_connected`, `412_no_config`, `429_rate_limit`, `429_fanout_cap`)                |
| `sse.event.forward`          | Short-lived span per forwarded SSE event (e.g. `refetchEvaluation`); one span per event, not per connection. Kind: `Producer`. Includes a **Span Link** to the originating `sse.upstream.connect` span. | `sdk_key_public_id`, `sse.event.type` |
| `POST /v1/metrics`           | Eval metrics proxy handler: every SDK POST metrics request (OTel HTTP server semconv). Raw SDK key never appears in `url.full`. | `http.request.method`, `http.route`, `sdk_key_public_id`, `upstream.status_code`, `result` |
| `HTTP POST`                  | Upstream metrics ingest call, child of `POST /v1/metrics` | `http.request.method`, `server.address`, `http.response.status_code` |

### Span Link: cache hit → originating fetch

The `proxy.serve_cache_hit` span includes a Span Link pointing to the `proxy.cold_fetch` (or `proxy.refresh_fetch`) span
that originally populated the cache entry. This correctly models the cache relationship: the serving request did not
cause the upstream fetch, but is correlated with it. The link is present only when the entry was fetched while tracing
was active. In Dynatrace, span links appear under **Related spans** on the span detail view.

### W3C Trace Context propagation

The proxy extracts W3C `traceparent` / `tracestate` headers from incoming SDK requests via centralized Gin middleware (
`internal/middleware.TracePropagation`). The middleware is applied to API routes (`/server/*`, `/sse`, `/v1/metrics`) as a route group;
health probes are excluded. If the SDK has its own tracer configured, the proxy spans will appear as children of the
SDK's trace in Dynatrace.

## Logs

The proxy uses `log/slog` for structured JSON logging. When `OTEL_EXPORTER_OTLP_ENDPOINT` is set, all log records are
also forwarded to the OTel collector via the `otelslog` bridge and a `BatchProcessor`. Stdout JSON output is always
active regardless of this setting.

### Severity mapping

| slog level     | OTel severity |
|----------------|---------------|
| `slog.Debug`   | `DEBUG`       |
| `slog.Info`    | `INFO`        |
| `slog.Warn`    | `WARN`        |
| `slog.Error`   | `ERROR`       |

### Log attributes

All log records that reference an SDK key use the attribute name `sdk_key_public_id`, the same name used on span
attributes and metric labels. This makes it straightforward to correlate a log record with its corresponding metric
data point or span in Dynatrace.

### Operational continuity

Because stdout JSON is preserved, existing log pipelines (`kubectl logs`, `docker logs`, Fluentd/Fluent Bit collectors)
continue to work without reconfiguration. OTLP log export is purely additive.
