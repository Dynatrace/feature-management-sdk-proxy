# Telemetry

The proxy emits [OpenTelemetry](https://opentelemetry.io/) metrics and distributed traces via OTLP/HTTP. When no exporter is configured, both are sent to a no-op provider and discarded — the proxy runs without an OTel collector.

## Configuring the Exporter

Set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable export. Metrics push every **30 seconds**; traces are batched and flushed continuously. Both use HTTP/protobuf transport (required by Dynatrace).

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector endpoint (e.g. `http://otel-collector:4318`). Required to enable export. |
| `OTEL_SERVICE_NAME` | Override the service name. Defaults to `sdk-proxy`. |
| `OTEL_RESOURCE_ATTRIBUTES` | Comma-separated key=value pairs added to all metrics and traces (e.g. `env=prod,region=us-east-1`). |
| `OTEL_TRACES_SAMPLER` | Sampling strategy for traces (default: `parentbased_always_on`). See [OTel SDK docs](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/#general-sdk-configuration). |

## Metrics Reference

| Metric | Instrument | Unit | Labels | Description |
|---|---|---|---|---|
| `proxy.requests.total` | Counter | `{request}` | `result` | Completed proxy requests. `result`: `hit`, `miss`, `upstream_error`, `client_error` |
| `proxy.cache.size` | Gauge | `By` | `sdk_key_public_id` | Cached payload size in bytes per SDK key |
| `proxy.config.sse_rewrite_total` | Counter | `{rewrite}` | `result` | SSE URL rewrite outcomes. `result`: `rewritten`, `no_sse_entry`, `not_found_in_body`, `unknown` |
| `proxy.sse.connections` | UpDownCounter | `{connection}` | `sdk_key_public_id` | Active upstream SSE connections |
| `proxy.sse.client.connections` | UpDownCounter | `{connection}` | `sdk_key_public_id` | Active SDK client SSE connections to the proxy |
| `proxy.sse.events_delivered_total` | Counter | `{event}` | `sdk_key_public_id` | Events successfully written to SDK clients |
| `proxy.sse.upstream.reconnects` | Counter | `{reconnect}` | `sdk_key_public_id`, `reason` | Upstream SSE reconnect attempts. `reason`: `eof`, `io_error`, `inactivity`, `url_change` |
| `proxy.sse.upstream.events` | Counter | `{event}` | `sdk_key_public_id`, `result` | Upstream SSE events received. `result`: `forwarded`, `dropped_no_subscribers`, `dropped_slow_subscriber` |
| `proxy.snapshot.age_seconds` | Gauge | `s` | `sdk_key_public_id` | Seconds since the snapshot was last written |
| `proxy.snapshot.writes.total` | Counter | `{write}` | `result`, `operation` | Snapshot write attempts. `result`: `success`, `failure`; `operation`: `put`, `delete` |
| `proxy.snapshot.load.total` | Counter | `{object}` | `result` | Snapshot objects processed at startup. `result`: `loaded`, `skipped_unknown_version`, `skipped_corrupt`, `backend_error` |
| `proxy.snapshot.write_queue.dropped_total` | Counter | `{write}` | — | Snapshot writes dropped due to write queue overflow |
| `proxy.startup.path` | Counter | `{startup}` | `path` | Startup count by path. `path`: `cold`, `snapshot`, `snapshot_stale`, `empty_persistence_cold`, `backend_unreachable` (see [snapshot.md](snapshot.md)) |
| `proxy.lifecycle.events_total` | Counter | `{event}` | `event_type`, `result` | Lifecycle SSE events received. `result`: `handled`, `malformed` |
| `proxy.lifecycle.invalidated_keys_total` | Counter | `{key}` | `event_type` | Keys invalidated by lifecycle events |
| `proxy.cache.inactivity_evictions.total` | Counter | `{entry}` | — | Cache entries evicted by the inactivity sweeper (idle for longer than `CACHE_INACTIVITY_EVICT_AFTER`) |
| `proxy.cache.refresh_evictions.total` | Counter | `{entry}` | `status_code` | Cache entries evicted during background refresh due to a 4xx response from upstream. `status_code`: `401`, `403`, `404`, `410` |
| `proxy.cache.refresh_preserved_stale.total` | Counter | `{response}` | `status_code` | Background refresh non-200/non-304 responses that preserved the stale entry (429 or 5xx). Enables alerting on sustained upstream errors without eviction. |
| `proxy.snapshot.demand_revalidation.total` | Counter | `{revalidation}` | `result` | Demand revalidations of snapshot-loaded entries triggered on the first stale hit. `result`: `hit` (fresh — no CDN call), `not_modified` (304), `refreshed` (200), `evicted` (4xx), `preserved_stale` (429/5xx), `error` (network). See [cache.md](cache.md#demand-revalidation-of-snapshot-loaded-entries). |
| `proxy.snapshot.outage_fallback.total` | Counter | `{check}` | `result` | Async snapshot store reads triggered by background CDN refresh failures (network error or 429/5xx). Only emitted when `SNAPSHOT_BACKEND` is configured. `result`: `loaded` (newer snapshot applied), `no_newer` (snapshot not newer than current entry), `same_etag` (payload unchanged), `debounced` (suppressed by 60s per-key interval), `backend_error` (store read failed), `decode_error` (corrupt or unknown version envelope), `superseded` (concurrent refresh applied first). See [snapshot.md](snapshot.md#runtime-cross-proxy-fallback). |

See [monitoring.md](monitoring.md) for guidance on interpreting these metrics by operator concern.

## Tracing

The proxy emits distributed traces via OTLP/HTTP (same `OTEL_EXPORTER_OTLP_ENDPOINT` as metrics). Traces are structured to match the cache relationship correctly: when a request is served from cache, the serving span uses a **Span Link** (not a parent-child relationship) to reference the original upstream fetch span.

### Span Catalogue

| Span name | Where | Key attributes |
|-----------|-------|----------------|
| `GET /server/{sdk_key}.json` | Proxy handler — every SDK config request (name follows OTel HTTP server semconv: `{method} {route}`) | `http.request.method`, `http.route`, `sdk_key_public_id`, `result` (`hit`, `miss`, `upstream_error`, `client_error`) |
| `proxy.serve_cache_hit` | Serving a cached entry — child of `GET /server/` with a **Span Link** to the originating fetch when available | `sdk_key_public_id`, `result` |
| `proxy.cold_fetch` | Upstream CDN fetch on a cache miss — child of `GET /server/` | `sdk_key_public_id`, `upstream.status_code`, `result` |
| `GET` | CDN HTTP call — child of `proxy.cold_fetch` or `proxy.refresh_fetch`; `url.full` is intentionally omitted (URL contains raw SDK key) | `http.request.method`, `server.address`, `http.response.status_code` |
| `proxy.refresh_fetch` | Background TTL revalidation fetch — standalone (not tied to any client request) | `sdk_key_public_id`, `if_none_match`, `result` |
| `sse.upstream.connect` | One span per upstream SSE connect attempt — reconnects emit separate spans | `sdk_key_public_id`, `reconnect.reason`, `streaming` |
| `sse.client.subscribe` | SSE client connection setup phase (auth + cache gate + headers sent) | `sdk_key_public_id`, `outcome` (`200_connected`, `412_no_config`, `500_no_flusher`) |

### Span Link: cache hit → originating fetch

The `proxy.serve_cache_hit` span includes a Span Link pointing to the `proxy.cold_fetch` (or `proxy.refresh_fetch`) span that originally populated the cache entry. This correctly models the cache relationship: the serving request did not cause the upstream fetch, but is correlated with it. The link is present only when the entry was fetched while tracing was active. In Dynatrace, span links appear under **Related spans** on the span detail view.

### W3C Trace Context propagation

The proxy extracts W3C `traceparent` / `tracestate` headers from incoming SDK requests. If the SDK has its own tracer configured, the proxy spans will appear as children of the SDK's trace in Dynatrace.
