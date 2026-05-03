# sdk-proxy

sdk-proxy is a caching, multiplexing proxy for the Dynatrace Feature Management SDK. It sits between your server-side SDK instances and the Dynatrace CDN, reducing upstream connections and tracking per-token usage for billing.

Without the proxy, each SDK replica opens its own CDN connection and its own upstream SSE connection. With the proxy, all replicas share one CDN connection and one SSE connection per SDK key, regardless of how many instances are running.

## How It Works

```
SDK instances (N)           sdk-proxy (1)           Dynatrace CDN / SSE
─────────────────           ─────────────           ────────────────────
GET /v1/config/{key}.json ──▶ cache hit: serve
                              cache miss: ──────────▶ GET /server/{key}.json
                              SSE subscribe ─────────▶ 1 upstream SSE conn
GET /sse?sdk_key={key}    ──▶ fan-out ◀──────────────  events broadcast
```

**Core components:**

- **HTTP proxy + cache** — Serves flagd config JSON from an in-memory cache with a 5-minute TTL. Fetches from the upstream CDN on a miss and revalidates stale entries with `If-None-Match`. See [docs/cache.md](docs/cache.md).
- **SSE fan-out** — Maintains exactly one upstream SSE connection per SDK key. Fans `refetchEvaluation` events out to all connected SDK clients via `GET /sse`. See [docs/sse.md](docs/sse.md).
- **Usage telemetry** — Counts requests per SDK token and batch-flushes to the Dynatrace billing endpoint on a configurable interval. See [docs/telemetry.md](docs/telemetry.md).
- **Snapshot persistence** — Optional. Persists cache entries to S3-compatible storage or a local file so the proxy can serve config immediately after a restart without hitting the CDN first.

## Build

```bash
# Binary
make build          # outputs ./sdk-proxy
go build -o sdk-proxy ./cmd/proxy

# OCI image
make docker         # docker build -t sdk-proxy .
```

**Requirements:** Go 1.26+, or Docker to build the image.

## Run

```bash
CDN_BASE_DOMAIN=fm.dynatracelabs.com ./sdk-proxy
```

The proxy listens on `:8080` by default. SDK clients point their `proxyUrl` at this address.

### Docker

```bash
docker run --rm \
  -e CDN_BASE_DOMAIN=fm.dynatracelabs.com \
  -p 8080:8080 \
  sdk-proxy
```

## Endpoints

| Endpoint | Description |
|---|---|
| `GET /v1/config/{sdk_key}.json` | Serve flagd config for an SDK key (cache hit or CDN fetch). The SDK key is the last path segment before `.json`. |
| `GET /sse?sdk_key={sdk_key}` | SSE fan-out. SDK clients subscribe here to receive `refetchEvaluation` events. Requires the config to be cached first; returns 412 otherwise. |

## Configuration

All configuration is via environment variables. `CDN_BASE_DOMAIN` is the only required variable.

| Variable | Required | Default | Description |
|---|---|---|---|
| `CDN_BASE_DOMAIN` | ✅ | — | Bare hostname for the upstream CDN (e.g. `fm.dynatracelabs.com`). |
| `PORT` | — | `8080` | TCP port the proxy listens on. Range: 1–65535. |
| `LOG_LEVEL` | — | `INFO` | Structured log level: `DEBUG`, `INFO`, `WARN`, `ERROR`. |
| `SHUTDOWN_TIMEOUT` | — | `30s` | Grace period for draining in-flight HTTP requests on shutdown. |
| `BILLING_FLUSH_INTERVAL` | — | `60s` | How often usage counters are flushed to the billing endpoint. Minimum `10s`. |
| `BILLING_RETRY_BUFFER_SIZE` | — | `1000` | Max failed billing samples held for retry. Range: 1–100000. |
| `PROXY_INSTANCE_ID` | — | `os.Hostname()` | Identifies this replica in billing records. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | — | Enables OTel metrics export via OTLP/gRPC when set. |

See [docs/configuration.md](docs/configuration.md) for the full reference, including OTel environment variables and tuning guidance.

## Development

```bash
make test           # unit tests
make test-int       # integration tests (spins up in-process mocks)
make lint           # golangci-lint
make vet            # go vet
```

## Documentation

- [docs/cache.md](docs/cache.md) — In-memory cache, TTL/ETag revalidation, flagd config passthrough, SSE URL rewrite
- [docs/sse.md](docs/sse.md) — SSE fan-out endpoint and upstream SSE client
- [docs/telemetry.md](docs/telemetry.md) — OTel metrics reference, per-token counter, batch-and-flush pipeline
- [docs/configuration.md](docs/configuration.md) — Full configuration reference
- [docs/shutdown.md](docs/shutdown.md) — Shutdown sequence and always-exit-0 policy
