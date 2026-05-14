# SDK Proxy

The SDK proxy reduces infrastructure overhead for teams running many instances of the Dynatrace Feature Management SDK. Without it, each SDK replica maintains its own connection to the Dynatrace CDN and its own upstream SSE subscription. With it, all replicas share one CDN connection and one SSE subscription per SDK key — regardless of fleet size.

## When to Use It

- You run multiple replicas of an application that uses the Feature Management SDK and want to reduce CDN load.
- You want a single point of control for caching and flag update propagation across a deployment.
- You need resilience against CDN outages: the proxy continues serving cached configs even when upstream is unreachable.
- You want faster restarts with [snapshot persistence](#snapshot-persistence) — new replicas come up warm without hitting the CDN.

## How It Works

```
SDK instances (N)         sdk-proxy (1)          Dynatrace CDN / SSE
─────────────────         ─────────────          ───────────────────
GET /server/{key}.json ─▶ cache hit: serve
                           cache miss: ─────────▶ GET /server/{key}.json
GET /sse ◀──────────────  fan-out  ◀──────────── 1 upstream SSE conn
```

The proxy caches flagd config payloads and revalidates them every 5 minutes. SSE events (`refetchEvaluation`) are fanned out from a single upstream connection to all subscribed SDK clients.

## Quick Start

**Requirements:** Go 1.24+, or Docker.

```bash
# Build
make build

# Run
./sdk-proxy
```

The proxy listens on `:8080` by default.

### Docker

```bash
docker build -t sdk-proxy .

docker run --rm \
  -p 8080:8080 \
  sdk-proxy
```

### Kubernetes

```yaml
containers:
  - name: sdk-proxy
    image: sdk-proxy
    ports:
      - containerPort: 8080
    env:
      - name: PROXY_INSTANCE_ID
        valueFrom:
          fieldRef:
            fieldPath: metadata.name
    livenessProbe:
      httpGet:
        path: /livez
        port: 8080
    readinessProbe:
      httpGet:
        path: /readyz
        port: 8080
      initialDelaySeconds: 2
      periodSeconds: 5
```

## SDK Configuration

Point your SDK at the proxy by setting `proxyUrl` in the SDK provider config:

```json
{
  "proxyUrl": "http://sdk-proxy:8080"
}
```

The proxy handles both flagd config requests (`GET /server/{sdk_key}.json`) and SSE subscriptions (`GET /sse`). No other SDK configuration is needed.

## Snapshot Persistence

Snapshot persistence lets the proxy save its cache to durable storage and restore it on startup. New replicas come up warm — no CDN burst, no cold start latency. Snapshots also provide resilience: if the CDN is unreachable at startup, the proxy serves the last saved state.

Two backends are available: `local` (single-node) and `s3` (multi-replica).

```bash
# Local backend
SNAPSHOT_BACKEND=local
SNAPSHOT_LOCAL_DIR=/var/lib/sdk-proxy/snapshots

# S3 backend
SNAPSHOT_BACKEND=s3
SNAPSHOT_S3_BUCKET=my-proxy-snapshots
SNAPSHOT_S3_REGION=us-east-1
```

See [docs/snapshot.md](docs/snapshot.md) for startup behavior, staleness handling, and rolling deploy guidance.

## Configuration

All configuration is via environment variables. All variables have safe defaults.

| Variable | Default | Description |
|---|---|---|
| `CDN_BASE_DOMAIN` | `fm.dynatrace.com` | Bare hostname for the upstream CDN. Override for staging or custom environments. |
| `PORT` | `8080` | TCP port the proxy listens on. |
| `LOG_LEVEL` | `INFO` | Log verbosity: `DEBUG`, `INFO`, `WARN`, `ERROR`. |
| `SHUTDOWN_TIMEOUT` | `30s` | Grace period for draining in-flight requests on shutdown. |
| `PROXY_INSTANCE_ID` | `os.Hostname()` | Identifies this replica. In Kubernetes, set to the pod name. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | Enables OTel metrics export via OTLP/gRPC when set. |
| `SNAPSHOT_BACKEND` | — | Persistence backend: `local` or `s3`. Unset to disable. |

See [docs/configuration.md](docs/configuration.md) for the full reference, including all snapshot and OTel variables.

## Endpoints

| Endpoint | Description |
|---|---|
| `GET /server/{sdk_key}.json` | Serve flagd config for an SDK key (cache hit or CDN fetch). |
| `GET /sse` | SSE fan-out endpoint. SDK clients subscribe here for `refetchEvaluation` events. |
| `GET /livez` | Liveness probe — returns 200 while the process is running. |
| `GET /readyz` | Readiness probe — returns 200 when the proxy has a usable data source. |
| `GET /healthz` | Health check — same semantics as `/readyz`. |

## Documentation

- [docs/configuration.md](docs/configuration.md) — full configuration reference
- [docs/snapshot.md](docs/snapshot.md) — snapshot persistence: setup, startup behavior, multi-replica deployments
- [docs/monitoring.md](docs/monitoring.md) — monitoring guide: metrics, health probes, happy paths and edge cases
- [docs/cache.md](docs/cache.md) — cache behavior, TTL/ETag revalidation, SSE URL rewrite
- [docs/sse.md](docs/sse.md) — SSE fan-out and upstream SSE client
- [docs/telemetry.md](docs/telemetry.md) — OTel metrics reference and exporter configuration
- [docs/shutdown.md](docs/shutdown.md) — shutdown sequence and exit code behavior

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup, test commands, and commit conventions.
