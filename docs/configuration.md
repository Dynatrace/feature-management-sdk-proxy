# Configuration Reference

All configuration is loaded from environment variables at startup and have safe defaults.

The proxy validates all values at startup and exits 1 with a descriptive error if any variable is invalid.

## Proxy Variables

### `CDN_BASE_DOMAIN`

**Optional.** Default: `fm.dynatrace.com`.

The bare hostname for the upstream Dynatrace Feature Management CDN. The proxy derives the full upstream URL from the
SDK key's region segment:

```
https://cdn.{region}.{CDN_BASE_DOMAIN}/server/{sdk_key}.json
```

Valid value: letters, digits, dots, and hyphens only (no scheme (`https://`), no path, no port).

Override this to target a custom CDN environment.

```bash
CDN_BASE_DOMAIN=cdn.example.com
```

---

### `PORT`

**Optional.** Default: `8080`. Range: 1–65535.

The TCP port the proxy listens on.

```bash
PORT=8080
```

---

### `LOG_LEVEL`

**Optional.** Default: `INFO`.

Structured log verbosity. All logs are JSON to stdout.

| Value   | Use                                                  |
|---------|------------------------------------------------------|
| `DEBUG` | Per-request cache decisions, SSE connection events   |
| `INFO`  | Server start/stop, cache refresh outcomes            |
| `WARN`  | Upstream errors, SSE disconnects, retryable failures |
| `ERROR` | Internal errors that should never occur              |

```bash
LOG_LEVEL=INFO
```

---

### `SHUTDOWN_TIMEOUT`

**Optional.** Default: `30s`.

Grace period for draining in-flight HTTP requests when a `SIGTERM` or `SIGINT` is received. After this timeout, the
server stops accepting and closes active connections. Must be a positive Go duration string (e.g. `30s`, `1m`).

The proxy always exits 0 on a signal-driven shutdown, regardless of whether this timeout is exceeded.
See [shutdown.md](shutdown.md).

```bash
SHUTDOWN_TIMEOUT=30s
```

---

### `PROXY_INSTANCE_ID`

**Optional.** Default: `os.Hostname()`, or `unknown` if hostname lookup fails.

An opaque identifier for this proxy replica. Used in usage records to distinguish replicas in a fleet. In Kubernetes,
set this to the pod name via the Downward API.

```bash
PROXY_INSTANCE_ID=pod-abc-123
```

**Kubernetes example:**

```yaml
env:
  - name: PROXY_INSTANCE_ID
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
```

---

### `LIFECYCLE_KEYS_CAP`

**Optional.** Default: `10000`. Range: 1–100000.

Maximum combined number of `sdkKeyHashes` + `sdkKeys` entries accepted in a single lifecycle SSE event (`tokenRotated`,
`stageDeleted`, `projectDeleted`). Events exceeding this cap are rejected before any SHA-256 hashing work is performed
and counted under `proxy.lifecycle.events_total{result="malformed"}`.

The default of 10,000 is well above any realistic rotation or deletion batch. Lower this value in environments where the
upstream is untrusted or resource-constrained; raise it only if legitimate lifecycle events routinely exceed the cap.

```bash
LIFECYCLE_KEYS_CAP=10000
```

---

---

### `SSE_DISABLED`

**Optional.** Default: `false`.

When set to `true`, the proxy does not open any upstream SSE connections. All SDK keys poll for config updates at the
disconnected interval (every 10 seconds).

`GET /sse` returns **501 Not Implemented** immediately, so SDK clients fail fast rather than holding a dead connection.
The `eventStreamEndpoint` URL in the flagd config is also not rewritten: clients receive the upstream URL verbatim (or
no URL if the upstream omits it) rather than a pointer to the non-functional `/sse` endpoint.

Use this if your network or firewall policy blocks long-lived HTTP connections to the upstream SSE service.

Accepted values: `true`, `false`, `1`, `0`.

```bash
SSE_DISABLED=false
```

---

## Billing Variables

### `BILLING_FLUSH_INTERVAL`

**Optional.** Default: `60s`. Minimum: `10s`.

How often the proxy flushes SDK usage counters to the Dynatrace billing endpoint. Each flush sends a batch record of
per-token request counts accumulated since the last flush. Must be a Go duration string of at least `10s`.

Reduce this value to shorten the window during which usage is buffered in memory (relevant if you restart proxies
frequently). The default of 60 seconds is suitable for most deployments.

```bash
BILLING_FLUSH_INTERVAL=60s
```

---

### `BILLING_RETRY_BUFFER_SIZE`

**Optional.** Default: `1000`. Range: 1–100000.

Maximum number of failed billing samples held in the in-memory retry buffer. When the flush to the billing endpoint
fails, the sample is placed in the retry buffer and re-attempted on the next flush cycle. When the buffer is full, the
oldest sample is dropped.

Under normal conditions this counter should be zero. Increase it if your billing endpoint is occasionally unavailable
for longer than a few flush cycles.

```bash
BILLING_RETRY_BUFFER_SIZE=1000
```

---

## Snapshot Persistence Variables

Snapshot persistence is **optional**. When disabled (the default), the proxy starts cold on every restart. When enabled,
cache entries are written asynchronously to the configured backend and loaded back on startup to reduce cold-start
latency.

Two backends are supported: `s3` (any S3-compatible object store) and `local` (a directory on the local filesystem).

See [snapshot.md](snapshot.md) for startup behavior and deployment guidance.

### `SNAPSHOT_BACKEND`

**Optional.** Default: unset (persistence disabled).

Selects the persistence backend.

| Value   | Backend                                                                        |
|---------|--------------------------------------------------------------------------------|
| `s3`    | Any S3-compatible object store (AWS S3, MinIO, Ceph, GCS S3-compat)            |
| `local` | Local filesystem directory, suitable for single-node or air-gapped deployments |

```bash
SNAPSHOT_BACKEND=local
```

---

### `SNAPSHOT_MAX_AGE`

**Optional.** Default: `24h`.

Maximum age of a snapshot before it is considered stale at startup. Stale snapshots are still loaded and served; the
proxy schedules a background refresh to replace them. Must be a positive Go duration string.

Setting this too short defeats the purpose of persistence; setting it too long means the proxy may serve stale configs
after an extended outage. The default of 24 hours is suitable for most deployments.

```bash
SNAPSHOT_MAX_AGE=24h
```

> ⚠️ `SNAPSHOT_MAX_AGE` has no effect unless `SNAPSHOT_BACKEND` is also set.

---

### `CACHE_INACTIVITY_EVICT_AFTER`

**Optional.** Default: `48h`.

How long an SDK key must be idle before the inactivity sweeper evicts it. A key is considered idle when it has had no
HTTP config requests **and** no active SSE subscribers for longer than this window. Eviction removes the in-memory
entry, closes the upstream SSE connection, and deletes the persisted snapshot, identical to a lifecycle invalidation
event.

Set to `0` to disable the sweeper entirely. The sweeper checks all cached keys every hour.

```bash
CACHE_INACTIVITY_EVICT_AFTER=48h  # default
CACHE_INACTIVITY_EVICT_AFTER=0    # disable
```

Must be `0` (disabled) or a Go duration string of at least `1h` (e.g. `24h`, `72h`). Values shorter than `1h` are
rejected at startup: the sweeper runs every hour, so shorter values would never fire.

---

### `UPSTREAM_MAX_RESPONSE_BYTES`

**Optional.** Default: `10485760` (10 MiB).

Maximum number of bytes the proxy will read from a single upstream CDN response. If the response body exceeds this
limit, the request is rejected with a `502 Bad Gateway` and the entry is not cached, preventing unbounded memory growth
from a misbehaving or malicious upstream.

```bash
UPSTREAM_MAX_RESPONSE_BYTES=10485760   # 10 MiB (default)
UPSTREAM_MAX_RESPONSE_BYTES=52428800   # 50 MiB
```

Must be a positive integer. Values below `1` are rejected at startup.

---

### S3 Backend Variables

Used when `SNAPSHOT_BACKEND=s3`.

#### `SNAPSHOT_S3_BUCKET`

**Required when `SNAPSHOT_BACKEND=s3`.**

The name of the S3 bucket that holds snapshot objects.

```bash
SNAPSHOT_S3_BUCKET=my-proxy-snapshots
```

---

#### `SNAPSHOT_S3_REGION`

**Required when `SNAPSHOT_BACKEND=s3`.**

The AWS region for the bucket. For self-hosted S3-compatible services, use any non-empty placeholder (e.g. `us-east-1`).

```bash
SNAPSHOT_S3_REGION=us-east-1
```

---

#### `SNAPSHOT_S3_ENDPOINT`

**Optional.**

Override the S3 endpoint URL. Leave unset to use the AWS regional endpoint derived from `SNAPSHOT_S3_REGION`. Set this
for self-hosted S3-compatible services (MinIO, Ceph, etc.).

```bash
SNAPSHOT_S3_ENDPOINT=http://minio:9000
```

---

#### `SNAPSHOT_S3_PREFIX`

**Optional.** Default: `""` (objects stored at the bucket root).

A key prefix applied to all snapshot objects. Useful for sharing a bucket across multiple proxy deployments.

```bash
SNAPSHOT_S3_PREFIX=prod/sdk-proxy
```

---

#### `SNAPSHOT_S3_ACCESS_KEY` / `SNAPSHOT_S3_SECRET_KEY`

**Optional.** Default: AWS SDK default credential chain (env vars, IRSA, instance roles, etc.).

Explicit static credentials. When set, both must be provided. Prefer IRSA or instance roles in production.

```bash
SNAPSHOT_S3_ACCESS_KEY=AKIAIOSFODNN7EXAMPLE
SNAPSHOT_S3_SECRET_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY
```

---

### Local Backend Variables

Used when `SNAPSHOT_BACKEND=local`.

#### `SNAPSHOT_LOCAL_DIR`

**Required when `SNAPSHOT_BACKEND=local`.**

Absolute path to the directory where snapshot files are stored. The directory is created on startup if it does not
exist. Must be an absolute path (not relative).

```bash
SNAPSHOT_LOCAL_DIR=/var/lib/sdk-proxy/snapshots
```

**Kubernetes volume example:**

```yaml
env:
  - name: SNAPSHOT_BACKEND
    value: local
  - name: SNAPSHOT_LOCAL_DIR
    value: /snapshots
volumeMounts:
  - name: snapshots
    mountPath: /snapshots
volumes:
  - name: snapshots
    persistentVolumeClaim:
      claimName: sdk-proxy-snapshots
```

---

### `SNAPSHOT_WRITE_QUEUE_SIZE`

**Optional.** Default: `256`. Range: 1–10000.

Maximum number of pending async snapshot writes queued in memory. Writes are enqueued after each cache update and
drained by a background goroutine. When the queue is full, the pending write is dropped and
`proxy.snapshot.write_queue.dropped_total` is incremented.

```bash
SNAPSHOT_WRITE_QUEUE_SIZE=256
```

---

## OTel Variables

Metrics and traces export is disabled by default. Set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable both; all other OTel
variables are optional. See [telemetry.md](telemetry.md) for the full reference.

### `OTEL_EXPORTER_OTLP_ENDPOINT`

**Optional.** Default: unset (metrics and traces sent to a no-op provider).

The OTLP/HTTP endpoint for an OpenTelemetry collector. When set, the proxy pushes metrics every 30 seconds and traces
continuously using HTTP/protobuf transport (required by Dynatrace; use port 4318 for OTLP/HTTP).

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
```

---

### `OTEL_SERVICE_NAME`

**Optional.** Default: `sdk-proxy`.

Overrides the service name attached to all exported metrics and traces.

```bash
OTEL_SERVICE_NAME=sdk-proxy
```

---

### `OTEL_RESOURCE_ATTRIBUTES`

**Optional.**

Comma-separated `key=value` pairs added as resource attributes to all exported metrics and traces. Useful for adding
environment, region, or cluster labels.

```bash
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=production,k8s.cluster.name=us-east-1
```

---

### `OTEL_TRACES_SAMPLER`

**Optional.** Default: `parentbased_always_on`.

Sampling strategy for distributed traces. Honors the standard OTel SDK environment variable. Set to `traceidratio` with
`OTEL_TRACES_SAMPLER_ARG=0.1` to sample 10% of traces in high-traffic deployments.

---

## Full Example

Minimal production configuration:

```bash
PORT=8080
LOG_LEVEL=INFO
SHUTDOWN_TIMEOUT=30s
PROXY_INSTANCE_ID=pod-abc-123
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=production
```

With snapshot persistence:

```bash
SNAPSHOT_BACKEND=s3
SNAPSHOT_S3_BUCKET=my-proxy-snapshots
SNAPSHOT_S3_REGION=us-east-1
SNAPSHOT_MAX_AGE=24h
```

## Kubernetes Deployment Example

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sdk-proxy-config
data:
  PORT: "8080"
  LOG_LEVEL: INFO
  SHUTDOWN_TIMEOUT: 30s
  OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.monitoring:4318
  OTEL_RESOURCE_ATTRIBUTES: deployment.environment=production
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: sdk-proxy
spec:
  replicas: 2
  selector:
    matchLabels:
      app: sdk-proxy
  template:
    metadata:
      labels:
        app: sdk-proxy
    spec:
      containers:
        - name: sdk-proxy
          image: sdk-proxy:latest
          ports:
            - containerPort: 8080
          envFrom:
            - configMapRef:
                name: sdk-proxy-config
          env:
            - name: PROXY_INSTANCE_ID
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
          resources:
            requests:
              cpu: 100m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 256Mi
          livenessProbe:
            httpGet:
              path: /livez
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /readyz
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
```
