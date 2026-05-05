# Telemetry

The proxy emits [OpenTelemetry](https://opentelemetry.io/) metrics via OTLP/gRPC. When no exporter is configured, metrics are sent to a no-op provider and discarded — the proxy runs without an OTel collector.

## Configuring the Exporter

Set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable metric export. The exporter pushes every **30 seconds** over gRPC without TLS (in-cluster collector assumed).

| Variable | Effect |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Collector endpoint (e.g. `http://otel-collector:4317`). Required to enable export. |
| `OTEL_SERVICE_NAME` | Override the service name. Defaults to `sdk-proxy`. |
| `OTEL_RESOURCE_ATTRIBUTES` | Comma-separated key=value pairs added to all metrics (e.g. `env=prod,region=us-east-1`). |

## Metrics Reference

| Metric | Instrument | Unit | Labels | Description |
|---|---|---|---|---|
| `proxy.requests.total` | Counter | `{request}` | `result` | Completed proxy requests. `result`: `hit`, `miss`, `upstream_error`, `client_error` |
| `proxy.cache.size` | Gauge | `By` | `sdk_key_hash` | Cached payload size in bytes per SDK key |
| `proxy.config.sse_rewrite_total` | Counter | `{rewrite}` | `result` | SSE URL rewrite outcomes. `result`: `rewritten`, `no_sse_entry`, `not_found_in_body`, `unknown` |
| `proxy.sse.connections` | UpDownCounter | `{connection}` | `sdk_key_hash` | Active upstream SSE connections |
| `proxy.sse.client.connections` | UpDownCounter | `{connection}` | `sdk_key_hash` | Active SDK client SSE connections to the proxy |
| `proxy.sse.events_delivered_total` | Counter | `{event}` | `sdk_key_hash` | Events successfully written to SDK clients |
| `proxy.sse.upstream.reconnects` | Counter | `{reconnect}` | `sdk_key_hash`, `reason` | Upstream SSE reconnect attempts. `reason`: `eof`, `io_error`, `inactivity`, `url_change` |
| `proxy.sse.upstream.events` | Counter | `{event}` | `sdk_key_hash`, `result` | Upstream SSE events received. `result`: `forwarded`, `dropped_no_subscribers`, `dropped_slow_subscriber` |
| `proxy.snapshot.age_seconds` | Gauge | `s` | `sdk_key_hash` | Seconds since the snapshot was last written |
| `proxy.snapshot.writes.total` | Counter | `{write}` | `result`, `operation` | Snapshot write attempts. `result`: `success`, `failure`; `operation`: `put`, `delete` |
| `proxy.snapshot.load.total` | Counter | `{object}` | `result` | Snapshot objects processed at startup. `result`: `loaded`, `skipped_unknown_version`, `skipped_corrupt`, `backend_error` |
| `proxy.snapshot.write_queue.dropped_total` | Counter | `{write}` | — | Snapshot writes dropped due to write queue overflow |
| `proxy.startup.path` | Counter | `{startup}` | `path` | Startup count by path. `path`: `cold`, `snapshot`, `snapshot_stale`, `empty_persistence_cold`, `backend_unreachable` (see [snapshot.md](snapshot.md)) |
| `proxy.lifecycle.events_total` | Counter | `{event}` | `event_type`, `result` | Lifecycle SSE events received. `result`: `handled`, `malformed` |
| `proxy.lifecycle.invalidated_keys_total` | Counter | `{key}` | `event_type` | Keys invalidated by lifecycle events |

See [monitoring.md](monitoring.md) for guidance on interpreting these metrics by operator concern.
