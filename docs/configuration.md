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

This value also drives the upstream metrics ingest host for the `POST /v1/metrics` reverse proxy:

```
https://metrics.{region}.{CDN_BASE_DOMAIN}/v1/metrics
```

No separate env var is needed for the metrics endpoint: both CDN and metrics URLs share the same base domain and the
same geo segment from the SDK key.

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

### `SSE_FANOUT_CAP_PER_KEY`

**Optional.** Default: `1000`. Range: 0–100000.

Maximum number of concurrent downstream SSE connections the proxy accepts per SDK key public ID. When this cap is
reached, new subscribe requests for that key return **429 Too Many Requests**. Other flag sets are unaffected.

Set to `0` to disable the cap entirely. Under normal conditions the default is sufficient; lower this value to
protect the proxy from a single flag set opening an unbounded number of connections.

```bash
SSE_FANOUT_CAP_PER_KEY=1000
```

---

### `SSE_SUBSCRIBE_RATE_PER_SEC`

**Optional.** Default: `20`. Range: 1–10000.

Token-bucket rate (per second) at which new SSE subscribe requests are admitted per SDK key public ID. Requests that
exceed the rate return **429 Too Many Requests** with `Retry-After: 1`.

Paired with `SSE_SUBSCRIBE_BURST` to allow brief bursts above the sustained rate.

```bash
SSE_SUBSCRIBE_RATE_PER_SEC=20
```

---

### `SSE_SUBSCRIBE_BURST`

**Optional.** Default: `100`. Range: 1–100000.

Token-bucket burst capacity for the per-key subscribe rate limiter. The bucket starts full and refills at
`SSE_SUBSCRIBE_RATE_PER_SEC` tokens per second up to this maximum.

Burst capacity matters most at startup: when a Kubernetes deployment scales up or recovers from a crash, many SDK
instances may connect simultaneously. With the defaults (`SSE_SUBSCRIBE_RATE_PER_SEC=20`,
`SSE_SUBSCRIBE_BURST=100`), up to 100 connections are admitted instantly, then 20 per second after that. The default
of 100 is sized to absorb a typical rolling deployment without triggering 429s during the reconnect window.

Raise this value if your deployments routinely start more than 100 SDK instances for a single flag set at once.
Lower it if you want to enforce tighter burst limits, accepting more 429s during restarts in exchange for less
instantaneous load.

```bash
SSE_SUBSCRIBE_BURST=100
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

The backend is selected by a single bucket URL, `SNAPSHOT_URL`, opened with
[gocloud.dev/blob](https://gocloud.dev/howto/blob/). AWS S3 (and S3-compatible stores), Google Cloud Storage, Azure
Blob Storage, and the local filesystem are supported.

See [snapshot.md](snapshot.md) for startup behavior and deployment guidance.

### `SNAPSHOT_URL`

**Optional.** Default: unset (persistence disabled).

The bucket that holds snapshot objects. The scheme selects the provider:

| Scheme      | Backend                                                    | Example                                                 |
|-------------|------------------------------------------------------------|---------------------------------------------------------|
| `s3://`     | AWS S3 and S3-compatible stores (MinIO, Ceph RGW, ...)     | `s3://my-proxy-snapshots?region=us-east-1`              |
| `gs://`     | Google Cloud Storage                                       | `gs://my-proxy-snapshots`                               |
| `azblob://` | Azure Blob Storage (the host is the container name)        | `azblob://my-proxy-snapshots?storage_account=myaccount` |
| `file://`   | Local directory, for single-node or air-gapped deployments | `file:///var/lib/sdk-proxy/snapshots`                   |

An unparseable URL or any other scheme fails startup with a descriptive error.

Query parameters:

| Parameter                     | Applies to | Description                                                                                        |
|-------------------------------|------------|----------------------------------------------------------------------------------------------------|
| `prefix`                      | all        | Key prefix for all snapshot objects, for sharing a bucket. A trailing `/` is added if missing.     |
| `region`                      | `s3`       | Bucket region. For self-hosted stores, any non-empty placeholder.                                  |
| `endpoint`                    | `s3`       | Endpoint override for S3-compatible stores, e.g. `http://minio:9000`.                              |
| `use_path_style`              | `s3`       | `true` for path-style addressing. Most self-hosted S3-compatible stores need this with `endpoint`. |
| `storage_account`             | `azblob`   | Storage account name. Alternatively set `AZURE_STORAGE_ACCOUNT`.                                   |
| `create_dir`, `dir_file_mode` | `file`     | Default to creating the directory with mode `0700` (`dir_file_mode` is decimal, so `448`).         |

Other provider parameters are passed through; see the gocloud.dev
[URL reference](https://gocloud.dev/concepts/urls/) for each driver. Values containing `&`, `?` or `=` (such as an
`endpoint` with a query string) must be URL-encoded.

```bash
SNAPSHOT_URL='s3://my-proxy-snapshots?region=us-east-1&prefix=prod/sdk-proxy'
SNAPSHOT_URL='s3://snapshots?region=us-east-1&endpoint=http://minio:9000&use_path_style=true'
SNAPSHOT_URL='gs://my-proxy-snapshots?prefix=prod'
SNAPSHOT_URL='azblob://my-proxy-snapshots?storage_account=myaccount'
SNAPSHOT_URL=file:///var/lib/sdk-proxy/snapshots
```

> **Breaking change:** `SNAPSHOT_URL` replaces `SNAPSHOT_BACKEND`, `SNAPSHOT_LOCAL_DIR`, and `SNAPSHOT_S3_*`, which are
> no longer read. Migrate `SNAPSHOT_BACKEND=s3` + `SNAPSHOT_S3_BUCKET=b` + `SNAPSHOT_S3_REGION=r` +
> `SNAPSHOT_S3_ENDPOINT=e` + `SNAPSHOT_S3_PREFIX=p` to `s3://b?region=r&endpoint=e&use_path_style=true&prefix=p`
> (`use_path_style=true` matches the previous behavior when an endpoint was set), `SNAPSHOT_S3_ACCESS_KEY` /
> `SNAPSHOT_S3_SECRET_KEY` to `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`, and `SNAPSHOT_BACKEND=local` +
> `SNAPSHOT_LOCAL_DIR=/d` to `file:///d`. Existing snapshots keep the same `<prefix>/<keyID>.json` layout and are
> loaded as-is, except under an old `SNAPSHOT_S3_PREFIX` ending in `/`, which was written as `<prefix>//<keyID>.json`.

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

> ⚠️ `SNAPSHOT_MAX_AGE` has no effect unless `SNAPSHOT_URL` is also set; setting it alone fails startup.

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

### `MAX_BODY_BYTES`

**Optional.** Default: `10485760` (10 MiB).

Maximum number of bytes the proxy will read or accept for any body: upstream CDN responses (rejected with `502 Bad
Gateway` if exceeded, entry not cached) and inbound `POST /v1/metrics` request bodies (rejected with `413 Payload Too
Large` if exceeded). Prevents unbounded memory growth from misbehaving upstreams or oversized client payloads.

```bash
MAX_BODY_BYTES=10485760   # 10 MiB (default)
MAX_BODY_BYTES=52428800   # 50 MiB
```

Must be a positive integer. Values below `1` are rejected at startup.

---

### Snapshot Storage Credentials

The proxy has no storage-credential variables of its own. Each provider uses its native credential chain, so workload
identity works with no static secrets:

| Provider | Keyless (recommended)                                                         | Static credentials                                                                                           |
|----------|-------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------|
| AWS S3   | EKS IRSA / Pod Identity, ECS task roles, EC2 instance profiles                | `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` (+ `AWS_SESSION_TOKEN`), or `AWS_PROFILE`                      |
| GCS      | GKE Workload Identity, GCE metadata server                                    | `GOOGLE_APPLICATION_CREDENTIALS` pointing at a mounted service-account key file                              |
| Azure    | AKS Workload Identity, managed identity (`AZURE_CLIENT_ID` for user-assigned) | `AZURE_STORAGE_KEY` (with the account name), `AZURE_STORAGE_SAS_TOKEN`, or `AZURE_STORAGE_CONNECTION_STRING` |

S3-compatible stores (MinIO, Ceph) use the AWS variables. These are object-storage credentials, never SDK keys.

**Kubernetes `file://` volume example:**

```yaml
env:
  - name: SNAPSHOT_URL
    value: file:///snapshots
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
`proxy.snapshot.writes.total{result="dropped", operation="put"}` is incremented.

```bash
SNAPSHOT_WRITE_QUEUE_SIZE=256
```

---

## OTel Variables

Metrics, traces, and logs export is disabled by default. Set `OTEL_EXPORTER_OTLP_ENDPOINT` to enable all three; all
other OTel variables are optional. See [telemetry.md](telemetry.md) for the full reference.

### `OTEL_EXPORTER_OTLP_ENDPOINT`

**Optional.** Default: unset (metrics, traces, and logs sent to no-op providers).

The OTLP/HTTP endpoint for an OpenTelemetry collector. When set, the proxy pushes metrics every 30 seconds and batches
traces and logs continuously.

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
```

---

### `OTEL_SERVICE_NAME`

**Optional.** Default: `sdk-proxy`.

Overrides the service name attached to all exported telemetry.

```bash
OTEL_SERVICE_NAME=sdk-proxy
```

---

### `OTEL_RESOURCE_ATTRIBUTES`

**Optional.**

Comma-separated `key=value` pairs added as resource attributes to all exported metrics, traces, and logs. Useful for
adding environment, region, or cluster labels.

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
SNAPSHOT_URL='s3://my-proxy-snapshots?region=us-east-1'
SNAPSHOT_MAX_AGE=24h
```

## Kubernetes Deployment

Use the Helm chart in [`charts/feature-management-sdk-proxy`](../charts/feature-management-sdk-proxy/README.md). Its
typed values (`logLevel`, `sse`, `otel`, `snapshot`, ...) map onto the variables above, other variables go in
`extraEnv`, and `PORT` and `PROXY_INSTANCE_ID` are set by the chart. For plain manifests, see
[`examples/kubernetes.example.yml`](../examples/kubernetes.example.yml).

Whichever you use, set `terminationGracePeriodSeconds` above `SHUTDOWN_TIMEOUT` plus about 15s (see
[shutdown.md](shutdown.md)). Otherwise Kubernetes kills the pod before the final usage flush.
