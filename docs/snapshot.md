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

Two backends are available, selected by `SNAPSHOT_BACKEND`:

| Backend | Use case                                                                                                         |
|---------|------------------------------------------------------------------------------------------------------------------|
| `local` | Single-node deployments or development. Snapshots are written to a local directory on disk.                      |
| `s3`    | Multi-replica or Kubernetes deployments. Snapshots are stored in an S3-compatible bucket shared across replicas. |

When `SNAPSHOT_BACKEND` is unset, persistence is disabled and the proxy always starts cold.

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

**Local backend (single node):**

```bash
SNAPSHOT_BACKEND=local
SNAPSHOT_LOCAL_DIR=/var/lib/sdk-proxy/snapshots
```

**S3 backend (multi-replica):**

```bash
SNAPSHOT_BACKEND=s3
SNAPSHOT_S3_BUCKET=my-proxy-snapshots
SNAPSHOT_S3_REGION=us-east-1
SNAPSHOT_S3_PREFIX=prod/
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
`proxy.snapshot.write_queue.dropped_total`. Under normal load this counter should be zero.

## Multi-Replica Deployments

When multiple replicas share the same S3 backend:

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
- Checks are debounced per key at 60-second intervals to limit S3 request rates during sustained outages.
- A successful load updates `FetchedAt` to the snapshot's `captured_at` but does not write back to the store (only a
  proxy that independently verified with the CDN should write snapshots).
- This path is only active when `SNAPSHOT_BACKEND` is set. Proxies without persistence are unaffected.

**Known limitation:** if all proxies in a cluster lost CDN access before any of them wrote a newer snapshot, there is
nothing newer in the store to fall back to. This is expected and acceptable.

Metric: `proxy.snapshot.outage_fallback.total{result}`. See [telemetry.md](telemetry.md).

## Authentication

By default, the proxy pulls from the AWS S3 client's default credential chain, which includes environment variables,
shared credentials files, and EC2/ECS metadata. The recommendation is to leave it to the environment to provide
credentials when running in AWS.

For overriding the destination bucket or providing static credentials, the following variables are supported:

```bash
SNAPSHOT_S3_BUCKET=my-proxy-snapshots
SNAPSHOT_S3_REGION=us-east-1
SNAPSHOT_S3_PREFIX=prod/            # optional prefix for snapshot keys in the bucket
SNAPSHOT_S3_ACCESS_KEY_ID=AKIA...   # optional static credentials
SNAPSHOT_S3_SECRET_ACCESS_KEY=...   # optional static credentials
```

For compatibility with Cloudflare R2, or other providers, the `SNAPSHOT_S3_ENDPOINT` variable is also supported to
override the S3 endpoint URL:

```bash
SNAPSHOT_S3_ENDPOINT=https://<account_id>.r2.cloudflarestorage.com
```
