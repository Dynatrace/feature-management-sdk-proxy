# Snapshot Persistence

> See also: [operations.md](operations.md) for a narrative walkthrough of snapshot startup paths and failure behavior.

Without persistence, every proxy restart starts cold: the first request for each SDK key triggers a CDN fetch before the
cache is warm. With persistence enabled, the proxy saves its cache to durable storage and loads it back on startup,
allowing it to serve requests immediately even when the upstream CDN is unreachable.

## Why Use It

- **Faster restarts.** Cached configs are available the moment the process starts.
- **CDN outage resilience.** If the upstream CDN is unreachable at startup, the proxy can still serve traffic from the
  last saved state.
- **Rolling deploys.** New replicas come up ready to serve without a cold-start CDN burst.

## Backends

The backend is selected by the scheme of `SNAPSHOT_URL`, opened with [gocloud.dev/blob](https://gocloud.dev/howto/blob/):

| Scheme      | Use case                                                                                             |
|-------------|------------------------------------------------------------------------------------------------------|
| `file://`   | Single-node deployments or development. Snapshots are written to a local directory on disk.          |
| `s3://`     | Multi-replica deployments on AWS S3 or an S3-compatible store (MinIO, Ceph RGW, Cloudflare R2, ...). |
| `gs://`     | Multi-replica deployments on Google Cloud Storage.                                                   |
| `azblob://` | Multi-replica deployments on Azure Blob Storage.                                                     |

When `SNAPSHOT_URL` is unset, persistence is disabled and the proxy always starts cold. Any other scheme fails startup.
Each snapshot is stored as `<prefix><keyID>.json`, where the optional `prefix` URL parameter scopes a shared bucket.

## Startup Behavior

At startup, the proxy classifies itself into one of five paths. The `proxy.startup.path` metric records which path each
instance took.

| Path                     | Condition                                                   | Behavior                                                                                                                                                                                                                                                                                           |
|--------------------------|-------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `cold`                   | No backend configured                                       | Starts with an empty cache; fetches from CDN on first request per key                                                                                                                                                                                                                              |
| `snapshot`               | Backend configured, fresh snapshots present                 | Loads snapshots into cache; serves immediately. On the first SDK request for each key, if the entry is older than the refresh interval, it is confirmed valid or refreshed against the CDN before responding (see [Demand Revalidation](cache.md#demand-revalidation-of-snapshot-loaded-entries)). |
| `snapshot_stale`         | Backend configured, snapshots older than `SNAPSHOT_MAX_AGE` | Loads stale snapshots. On the first SDK request for each key, the config is confirmed or updated against the CDN before the response is served.                                                                                                                                                    |
| `empty_persistence_cold` | Backend configured and reachable, store is empty            | Starts cold; warms cache from upstream on first request per key; snapshots are written normally after each fetch                                                                                                                                                                                   |
| `backend_unreachable`    | Backend configured but unreachable                          | Exits with code 1. The backend must be reachable before the proxy can start; check connectivity and credentials.                                                                                                                                                                                   |

> ⚠️ The `backend_unreachable` path exits with code 1 when the backend itself cannot be reached. An empty but reachable
> store is now handled gracefully via `empty_persistence_cold`.

## Configuration

See [configuration.md](configuration.md) for the full variable reference. Common setup examples:

**Local directory (single node):**

```bash
SNAPSHOT_URL=file:///var/lib/sdk-proxy/snapshots
```

**Shared bucket (multi-replica):**

```bash
SNAPSHOT_URL='s3://my-proxy-snapshots?region=us-east-1&prefix=prod'
SNAPSHOT_URL='gs://my-proxy-snapshots?prefix=prod'
SNAPSHOT_URL='azblob://my-proxy-snapshots?storage_account=myaccount&prefix=prod'
```

**Staleness threshold:**

```bash
SNAPSHOT_MAX_AGE=24h   # snapshots older than this are considered stale at startup
```

## Deletion Behavior

Snapshots are deleted in two cases:

- **Lifecycle invalidation:** a `tokenRotated`, `stageDeleted`, or `projectDeleted` event arrives via the upstream SSE
  lifecycle stream for a key.
- **Inactivity eviction:** the inactivity sweeper determines that a key has had no HTTP access and no active SSE
  subscribers for longer than `CACHE_INACTIVITY_EVICT_AFTER` (default 48h). See [cache.md](cache.md) for details.

Both paths use the same delete operation: the in-memory cache entry is removed, the upstream SSE connection is closed,
and the persisted snapshot is deleted. A subsequent request for the evicted key is treated as a cold miss: the proxy
fetches fresh config from CDN and writes a new snapshot.

## Write Behavior

Snapshots are written asynchronously after each cache update. A snapshot write failure is logged and counted (
`proxy.snapshot.writes.total{result="failure"}`) but never fails an SDK request or delays the response.

If the write queue fills faster than the backend can drain it, excess writes are dropped and counted by
`proxy.snapshot.writes.total{result="dropped"}`. Under normal load this counter should be zero.

## Multi-Replica Deployments

When multiple replicas share the same bucket:

- Each replica writes independently. Writes are not coordinated: the last write for a key wins.
- Reads at startup are independent. Each replica loads the full set of stored snapshots.
- There is no fencing or locking. A snapshot written by a replica running an older version may briefly coexist with a
  newer version's entry. The snapshot envelope format is versioned; unknown versions are skipped at load time with a
  `skipped_unknown_version` result on `proxy.snapshot.load.total`.

During a rolling deploy, replicas on different versions may read each other's snapshots. Stale but valid snapshots are
loaded and served; the background TTL refresh will replace them within the cache TTL window.

## Runtime Cross-Proxy Fallback

Snapshot reads are not limited to startup. When a background CDN refresh fails for a key (network error or 429/5xx), the
proxy asynchronously checks the shared store for a newer snapshot written by a peer. If the store has a snapshot with a
`captured_at` more recent than the current entry's `FetchedAt` and a different ETag, the proxy applies it to the
in-memory cache. See [ADR-0003](adr/0003-cross-proxy-snapshot-fallback.md) for the full design.

**Key properties:**

- The check is async: the CDN failure response path is never blocked.
- Checks are debounced per key at 60-second intervals to limit object-storage request rates during sustained
  outages.
- A successful load updates `FetchedAt` to the snapshot's `captured_at` but does not write back to the store (only a
  proxy that independently verified with the CDN should write snapshots).
- This path is only active when `SNAPSHOT_URL` is set. Proxies without persistence are unaffected.

**Known limitation:** if all proxies in a cluster lost CDN access before any of them wrote a newer snapshot, there is
nothing newer in the store to fall back to. This is expected and acceptable.

Metric: `proxy.snapshot.outage_fallback.total{result}`. See [telemetry.md](telemetry.md).

## Authentication

The proxy has no storage-credential settings of its own. Each provider uses its native default credential chain, so the
recommendation is to let the environment provide credentials:

- **AWS S3:** EKS IRSA or Pod Identity, ECS task roles, EC2 instance profiles; or static `AWS_ACCESS_KEY_ID` and
  `AWS_SECRET_ACCESS_KEY` (optionally `AWS_SESSION_TOKEN`), or `AWS_PROFILE` with a shared credentials file.
- **Google Cloud Storage:** GKE Workload Identity or the GCE metadata server; or `GOOGLE_APPLICATION_CREDENTIALS`
  pointing at a mounted service-account key file.
- **Azure Blob Storage:** AKS Workload Identity or managed identity (`AZURE_CLIENT_ID` selects a user-assigned
  identity); or `AZURE_STORAGE_KEY`, `AZURE_STORAGE_SAS_TOKEN`, or `AZURE_STORAGE_CONNECTION_STRING`. The account name
  comes from the `storage_account` URL parameter or `AZURE_STORAGE_ACCOUNT`.

S3-compatible stores take the AWS variables plus an endpoint override. For example, Cloudflare R2:

```bash
SNAPSHOT_URL='s3://my-proxy-snapshots?region=auto&endpoint=https://<account_id>.r2.cloudflarestorage.com'
AWS_ACCESS_KEY_ID=...
AWS_SECRET_ACCESS_KEY=...
```

Self-hosted stores such as MinIO usually also need `use_path_style=true`. See
[configuration.md](configuration.md#snapshot_url) for all URL parameters.
