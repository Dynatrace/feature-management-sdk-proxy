# Configuration Reference

All configuration is loaded from environment variables at startup. `CDN_BASE_DOMAIN` is the only required variable; everything else has a safe default.

The proxy validates all values at startup and exits 1 with a descriptive error if any variable is invalid.

## Proxy Variables

### `CDN_BASE_DOMAIN`

**Required.**

The bare hostname for the upstream Dynatrace Feature Management CDN. The proxy derives the full upstream URL from the SDK key's region segment:

```
https://cdn.{region}.{CDN_BASE_DOMAIN}/server/{sdk_key}.json
```

Valid value: letters, digits, dots, and hyphens only — no scheme (`https://`), no path, no port.

```bash
CDN_BASE_DOMAIN=fm.dynatracelabs.com
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

| Value | Use |
|---|---|
| `DEBUG` | Per-request cache decisions, SSE connection events |
| `INFO` | Server start/stop, cache refresh outcomes |
| `WARN` | Upstream errors, SSE disconnects, billing retries |
| `ERROR` | Internal errors that should never occur |

```bash
LOG_LEVEL=INFO
```

---

### `SHUTDOWN_TIMEOUT`

**Optional.** Default: `30s`.

Grace period for draining in-flight HTTP requests when a `SIGTERM` or `SIGINT` is received. After this timeout, the server stops accepting and closes active connections. Must be a positive Go duration string (e.g. `30s`, `1m`).

The proxy always exits 0 on a signal-driven shutdown, regardless of whether this timeout is exceeded. See [shutdown.md](shutdown.md).

```bash
SHUTDOWN_TIMEOUT=30s
```

---

### `PROXY_INSTANCE_ID`

**Optional.** Default: `os.Hostname()`, or `unknown` if hostname lookup fails.

An opaque string included in every billing record to identify which proxy replica generated it. Useful for correlating fleet-wide usage counts across replicas. In Kubernetes, set this to the pod name via the Downward API.

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

## Billing Variables

### `BILLING_FLUSH_INTERVAL`

**Optional.** Default: `60s`. Minimum: `10s`.

How often per-SDK-token request counts are flushed to the billing endpoint. Shorter intervals reduce the window of data loss on an unexpected crash but increase billing endpoint traffic. Must be a Go duration string ≥ `10s`.

```bash
BILLING_FLUSH_INTERVAL=60s
```

---

### `BILLING_RETRY_BUFFER_SIZE`

**Optional.** Default: `1000`. Range: 1–100000.

Maximum number of failed billing samples held in the retry buffer for re-attempt on the next flush. When the buffer is full, the oldest sample is evicted and `proxy.billing.retry_buffer.dropped_total` is incremented.

Size the buffer to hold at least `(expected outage duration / flush interval)` samples per active SDK token. At the default 60s flush interval, a 1000-sample buffer covers ~16 hours of retries for a single SDK token, or proportionally less for multiple tokens.

```bash
BILLING_RETRY_BUFFER_SIZE=1000
```

---

## Snapshot Persistence Variables

Snapshot persistence is **optional**. When disabled (the default), the proxy starts cold on every restart. When enabled, cache entries are written asynchronously to the configured backend and loaded back on startup to reduce cold-start latency.

Two backends are supported: `s3` (any S3-compatible object store) and `local` (a directory on the local filesystem).

### `SNAPSHOT_BACKEND`

**Optional.** Default: unset (persistence disabled).

Selects the persistence backend.

| Value | Backend |
|---|---|
| `s3` | Any S3-compatible object store (AWS S3, MinIO, Ceph, GCS S3-compat) |
| `local` | Local filesystem directory — suitable for single-node or air-gapped deployments |

```bash
SNAPSHOT_BACKEND=local
```

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

Override the S3 endpoint URL. Leave unset to use the AWS regional endpoint derived from `SNAPSHOT_S3_REGION`. Set this for self-hosted S3-compatible services (MinIO, Ceph, etc.).

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

Absolute path to the directory where snapshot files are stored. The directory is created on startup if it does not exist. Must be an absolute path (not relative) to prevent ambiguity when the process working directory changes.

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

Maximum number of pending async snapshot writes queued in memory. Writes are enqueued after each cache update and drained by a background goroutine. When the queue is full, the pending write is dropped and `sdk_proxy.snapshot.write_queue.dropped_total` is incremented.

```bash
SNAPSHOT_WRITE_QUEUE_SIZE=256
```

---

## OTel Variables

Metrics export is disabled by default. Set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable it; all other OTel variables are optional.

### `OTEL_EXPORTER_OTLP_ENDPOINT`

**Optional.** Default: unset (metrics exported to a no-op provider).

The gRPC endpoint for an OpenTelemetry collector. When set, the proxy pushes metrics every 30 seconds over gRPC without TLS (suitable for in-cluster collector traffic).

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
```

---

### `OTEL_SERVICE_NAME`

**Optional.** Default: `sdk-proxy`.

Overrides the service name attached to all exported metrics.

```bash
OTEL_SERVICE_NAME=sdk-proxy
```

---

### `OTEL_RESOURCE_ATTRIBUTES`

**Optional.**

Comma-separated `key=value` pairs added as resource attributes to all exported metrics. Useful for adding environment, region, or cluster labels.

```bash
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=production,k8s.cluster.name=us-east-1
```

---

## Full Example

Minimal production configuration:

```bash
CDN_BASE_DOMAIN=fm.dynatracelabs.com
PORT=8080
LOG_LEVEL=INFO
SHUTDOWN_TIMEOUT=30s
BILLING_FLUSH_INTERVAL=60s
BILLING_RETRY_BUFFER_SIZE=1000
PROXY_INSTANCE_ID=pod-abc-123
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=production
```

## Kubernetes ConfigMap Example

Non-secret configuration can be managed in a `ConfigMap`. The SDK token is a secret and should be provided via the request, not the proxy config — the proxy holds no credentials.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sdk-proxy-config
data:
  CDN_BASE_DOMAIN: fm.dynatracelabs.com
  PORT: "8080"
  LOG_LEVEL: INFO
  SHUTDOWN_TIMEOUT: 30s
  BILLING_FLUSH_INTERVAL: 60s
  BILLING_RETRY_BUFFER_SIZE: "1000"
  OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.monitoring:4317
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
              path: /live
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /ready
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 10
```
