# Operations Guide

Understanding the SDK Proxy's runtime behavior separates a fast diagnosis from a long investigation. What follows covers
requests, SSE fan-out, startup paths, and failure conditions: the behaviors that matter most in production.

For metrics reference, see [telemetry.md](telemetry.md). For configuration, see [configuration.md](configuration.md).

---

## Normal Operation

### Request Lifecycle

Every SDK request to `GET /server/{sdk_key}.json` hits one of two paths:

**Cache hit:** the entry for that SDK key is already in memory. The proxy rewrites the `eventStreamEndpoint` URL in the
flagd config payload to point at its own `/sse` endpoint, then responds immediately. No upstream call is made.

**Cache miss:** no entry exists for this key. The proxy fetches `GET /server/{sdk_key}.json` from the upstream CDN,
stores the payload and ETag in memory, then responds to the SDK. The proxy deduplicates concurrent cold-miss requests
for the same key: only one CDN fetch is made regardless of how many SDK requests arrive simultaneously.

The cache key is the SDK key's public prefix (`cache.PublicID(sdkKey)`, i.e. the first two dot-segments:
`{token}.{platform}_{geo}_{publicRandom}`). The private segment never appears in files, object storage, logs, or metric
labels.

### SSE Fan-Out Lifecycle

The proxy multiplexes SSE events from a single upstream connection to all SDK clients sharing an SDK key:

1. **Config fetch triggers SSE.** When a key's config is first cached (from a CDN fetch or demand revalidation of a
   snapshot entry), the proxy opens one upstream SSE connection for that key. It never opens a connection before the
   config is cached.

2. **Clients subscribe.** SDK clients connect to `GET /sse?stream_id={public_id}`. The `stream_id` is the SDK key's
   public prefix, embedded in the `eventStreamEndpoint` of the flagd config the SDK already received. The proxy gates on
   cache presence: if no config is cached for the given `stream_id`, it returns 412 Precondition Failed. On cold start
   this is expected; providers retry after fetching the config.

3. **Events are fanned out.** When the upstream connection receives a `refetchEvaluation` event, the proxy dispatches it
   to all active subscriber channels. Slow clients whose channel buffer is full (16 events) have events dropped; the
   proxy logs a warning and increments a metric.

4. **One connection per key.** Regardless of how many SDK clients subscribe, the proxy maintains exactly one upstream
   SSE connection per SDK key. N clients → 1 upstream connection, never N.

### Dynamic Cache Poll Interval

The background refresher revalidates stale entries against the CDN using `If-None-Match`. The interval is per-key and
depends on SSE connection state:

| SSE state for this key                | Poll interval  | Why                                                            |
|---------------------------------------|----------------|----------------------------------------------------------------|
| Connected (upstream read loop active) | **10 minutes** | SSE delivers change notifications; CDN polling is a safety net |
| Disconnected / reconnecting           | **10 seconds** | May be missing notifications; poll aggressively                |
| SSE disabled (`SSE_DISABLED=true`)    | **10 seconds** | Polling is the only update mechanism                           |

A healthy fleet with SSE connected polls 60x less frequently than one without SSE. The refresher ticks every 10 seconds
but only fetches a key when its per-key interval has elapsed.

A 304 Not Modified response resets the entry's `FetchedAt` timestamp without changing the payload, writing a snapshot,
or notifying SSE. A 200 OK response replaces the entry and triggers both snapshot persistence (if configured) and SSE
URL change detection.

### ETag Revalidation

Every cache entry stores the CDN `ETag` alongside the payload. Background revalidation sends `If-None-Match: {etag}` to
avoid transferring unchanged payloads. The revalidation request has a 5-second timeout, shorter than the minimum poll
interval, to prevent request overlap.

---

## Failure Conditions

### CDN Unreachable at Runtime

When a background TTL refresh fails (network error or 5xx from the CDN), the stale entry stays in cache and continues to
be served. The proxy logs a warning and increments `proxy.requests.total{result="upstream_error"}`.

The proxy never returns an empty body on a miss: an empty response is indistinguishable from a legitimately empty flag
set and would silently disable all flags. For uncached keys the CDN's own 5xx status code is forwarded to the SDK
client. When the CDN is unreachable entirely (network error), the proxy returns 503 Service Unavailable.

When a background refresh fails with a network error or 429/5xx, the proxy also triggers an async snapshot-fallback
check: it reads the shared store for a newer snapshot written by a peer proxy. This path is only active when snapshot
persistence is configured. See [Runtime Cross-Proxy Fallback](snapshot.md#runtime-cross-proxy-fallback).

**4xx eviction:** HTTP 401, 403, 404, or 410 from the CDN during background refresh immediately evicts the cache entry,
closes the upstream SSE connection, and deletes the persisted snapshot. 429 and 5xx responses preserve the stale entry
to avoid amplifying outages. See [4xx Eviction Policy](cache.md#4xx-eviction-policy).

### CDN Unreachable at Startup

Startup behavior depends on snapshot persistence configuration. The proxy classifies each startup into one of five
paths; the `proxy.startup.path` metric records which:

| Path                     | Condition                                                   | Behavior                                                                                                           |
|--------------------------|-------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------|
| `cold`                   | No backend configured                                       | Empty cache; CDN fetch on first request per key                                                                    |
| `snapshot`               | Backend configured, fresh snapshots present                 | Loads snapshots; serves immediately. First request for each key triggers demand revalidation if the entry is stale |
| `snapshot_stale`         | Backend configured, snapshots older than `SNAPSHOT_MAX_AGE` | Loads stale snapshots; background refresh replaces them; demand revalidation on first request                      |
| `empty_persistence_cold` | Backend configured and reachable, store is empty            | Starts cold; warms from CDN; snapshots written after each fetch                                                    |
| `backend_unreachable`    | Backend configured but unreachable                          | Exits with code 1: the backend must be reachable before the proxy can start                                        |

**Demand revalidation:** When a snapshot-loaded entry is older than the current refresh interval, the first SDK request
for that key performs a synchronous conditional GET to the CDN before responding. The SDK receives confirmed-fresh
config at the cost of one round-trip. Subsequent requests within the interval are served from cache.

### SSE Upstream Disconnect

When the upstream SSE connection drops (EOF, network error, or inactivity), the proxy reconnects with exponential
backoff: starting at 1 second, doubling each attempt, capping at 60 seconds. The backoff resets to 1 second when a
connection stays stable for more than 30 seconds.

While reconnecting, the poll interval for that key falls back to 10 seconds, so flag changes are still delivered via CDN
polling. Connected SDK clients keep receiving heartbeats (`: ping` every 15 seconds) and are not disconnected. They
receive the next `refetchEvaluation` once the upstream connection is re-established.

### Snapshot Write Failure

Snapshot writes happen asynchronously after each cache update. A failed write is logged and counted (
`proxy.snapshot.writes.total{result="failure"}`) but never fails an SDK request or delays the response. If the write
queue fills faster than the backend can drain it, excess writes are dropped and counted by
`proxy.snapshot.writes.total{result="dropped"}`. Under normal load both counters should be zero.

### Lifecycle Events

The upstream SSE service emits lifecycle events when SDK keys are rotated or their associated stage or project is
deleted. The proxy evicts the affected key immediately:

- Cache entry removed from memory
- Upstream SSE connection closed
- Persisted snapshot deleted

The next SDK request for the evicted key triggers a fresh CDN cold-miss fetch. If the key was rotated (not deleted), SDK
instances must be reconfigured with the new key; the proxy has no knowledge of the new key until an SDK instance
requests it.

| Event            | Proxy behavior                                           |
|------------------|----------------------------------------------------------|
| `tokenRotated`   | Evict cache entry + close upstream SSE + delete snapshot |
| `stageDeleted`   | Evict cache entry + close upstream SSE + delete snapshot |
| `projectDeleted` | Evict cache entry + close upstream SSE + delete snapshot |

### Inactivity Eviction

Over time, a long-running proxy can accumulate cache entries for SDK keys that are no longer in active use. The
inactivity sweeper handles this: it runs every hour and evicts entries for SDK keys that have been idle for longer than
`CACHE_INACTIVITY_EVICT_AFTER` (default 48h). A key is idle when it has had no HTTP config requests **and** no active
SSE subscribers for longer than the configured window.

The sweeper applies the same eviction path as a lifecycle event: removes the cache entry, closes the upstream SSE
connection, and deletes the persisted snapshot. The next request for that key is a cold miss.

Keys with active SSE subscribers are never evicted, regardless of last HTTP access age. An active SSE subscription is
the authoritative liveness signal for a provider that holds long-lived connections without polling.

---

## Health and Readiness

The proxy exposes three HTTP health endpoints:

| Endpoint       | Returns 200 when               | Returns 503 when                           |
|----------------|--------------------------------|--------------------------------------------|
| `GET /livez`   | Process is running             | Never                                      |
| `GET /readyz`  | Proxy has a usable data source | Snapshot load still in progress at startup |
| `GET /healthz` | Same as `/readyz`              | Same as `/readyz`                          |

`/livez` is always 200 while the process is alive. `/readyz` and `/healthz` return 200 only after snapshot loading
completes, or immediately if no persistence is configured. Don't route SDK traffic to a pod before `/readyz` returns
200; an unready proxy may serve stale or empty responses.

Without snapshot persistence, the proxy becomes ready as soon as the HTTP listener is up. With persistence, readiness is
gated on the snapshot load, which typically takes under a second for a healthy store.

---

## Graceful Shutdown

On `SIGTERM`, `SIGINT`, or `SIGHUP`, the proxy drains in order:

1. **HTTP server drains:** in-flight requests complete; new connections are rejected. Bounded by `SHUTDOWN_TIMEOUT` (
   default 30s).
2. **SSE manager shuts down:** upstream connections closed; subscriber channels drained. 5-second deadline.
3. **Snapshot writer drains:** pending async writes flushed. Failed writes are logged and dropped.
4. **Final usage flush:** pending usage records sent to the billing endpoint. 10-second deadline.

The proxy exits 0 on a normal signal-driven shutdown, even if non-critical teardown steps log errors. Exit code 1 means
a startup failure or an unexpected listener error before any signal arrived. A **second signal received during
graceful shutdown** forces an immediate exit with code `128 + signo` (e.g. `143` for `SIGTERM`); use this to escape a
hung shutdown instead of `kill -9`. `SIGKILL` cannot be caught — the final usage flush and any queued snapshot writes
are lost.

---

## See Also

- [cache.md](cache.md): cache behavior, TTL/ETag revalidation, 4xx eviction policy
- [sse.md](sse.md): SSE fan-out internals, upstream connection lifecycle, reconnect backoff
- [snapshot.md](snapshot.md): snapshot persistence, startup paths, multi-replica deployments
- [monitoring.md](monitoring.md): metrics reference, health probes, what to watch per concern
- [shutdown.md](shutdown.md): shutdown sequence and exit code details
- [configuration.md](configuration.md): full environment variable reference
