# In-Memory Cache & Flagd Config Passthrough

The proxy caches flagd config payloads in memory, keyed by the SDK key's public prefix. Every incoming SDK request is served from this cache; upstream CDN calls happen only on a miss or during background TTL refresh.

## Cache Key

The cache is keyed by `cache.PublicID(sdk_key)` — the first two dot-segments of the SDK key: `{token}.{platform}_{geo}_{publicRandom}` (e.g. `dt01.server_us_ab3xy9kp2q`). The private segment (`{secret}_{sha1}`) is stripped and must never appear in files, logs, metric labels, or object storage. The raw SDK key is retained in memory only; it is needed for upstream request authentication.

**Canonical redaction rule:** split on `.`, drop the last segment, rejoin on `.`. `cache.PublicID(sdkKey)` implements this rule.


## Cache Entry

Each entry holds:

- **Payload** — the raw flagd config JSON bytes, treated as opaque
- **ETag** — the upstream CDN ETag, used for conditional revalidation
- **FetchedAt** — the timestamp used to determine staleness
- **FetchURL** — the full upstream URL, retained in memory for background refresh

## TTL and Revalidation

The background `Refresher` revalidates stale entries using `If-None-Match`. The polling interval is per-key and depends on SSE connection state:

| SSE state | Polling interval | Rationale |
|---|---|---|
| Connected (read loop active) | **10 minutes** | SSE delivers change notifications; polling is a safety net only |
| Disconnected / reconnecting | **10 seconds** | May be missing notifications; poll aggressively |
| SSE disabled (`SSE_DISABLED=true`) | **10 seconds** | Polling is the only update mechanism |

The `Refresher` ticks at 10 seconds (the shortest interval) and evaluates per-key staleness against the interval returned by `Manager.RefreshInterval`. This means a connected key won't be fetched more than once per 10 minutes even though the ticker fires every 10 seconds.

The background CDN request has a **5-second timeout** — intentionally less than the minimum poll interval to prevent request overlap.

- A **304** response resets `FetchedAt` without changing the payload or writing a snapshot.
- A **200** response replaces the entry and triggers both snapshot persistence (if configured) and SSE URL change detection.

A background refresh error is logged but never fails an SDK request. The stale entry stays in cache and continues to be served.

## Demand Revalidation of Snapshot-Loaded Entries

When the proxy starts from a snapshot, loaded entries may be hours old. The background `Refresher` eventually revalidates them, but that can take up to 10 minutes depending on SSE connection state. For the first SDK request after startup, serving potentially stale config without checking the CDN is the wrong trade-off.

**Demand revalidation** solves this: on the first cache hit for a snapshot-loaded entry that is older than the current refresh interval, the proxy performs a synchronous conditional GET to the CDN before writing the response. The SDK receives confirmed-valid (or updated) config, at the cost of one CDN round-trip. Subsequent requests within the refresh interval are served from cache without a CDN call.

### Behavior by CDN response

| CDN response | Proxy action |
|---|---|
| **304 Not Modified** | Config confirmed valid; served from cache |
| **200 OK** | Fresh config received; cache updated; snapshot written asynchronously |
| **401 / 403 / 404 / 410** | Entry evicted; SSE connection closed; 503 returned to client (next request triggers a fresh CDN fetch) |
| **429 / 5xx** | Stale entry preserved; served from cache |
| **Network error** | Stale entry preserved; served from cache |

### Concurrency

Concurrent requests for the same stale snapshot-loaded key share a single CDN call. Only one round-trip is made regardless of how many requests arrive simultaneously.

### Scope

Demand revalidation fires once per snapshot-loaded entry after startup. Once confirmed or refreshed, the background `Refresher` handles normal TTL maintenance going forward.

Metric: `proxy.snapshot.demand_revalidation.total{result}` — see [telemetry.md](telemetry.md).

## 4xx Eviction Policy

When the background refresher receives a 4xx response from the upstream CDN, the proxy takes one of two actions depending on the status code:

| Status code | Meaning | Proxy behavior |
|---|---|---|
| `401 Unauthorized` | Credentials revoked | Evict entry, close upstream SSE connection, delete snapshot |
| `403 Forbidden` | Credentials revoked | Evict entry, close upstream SSE connection, delete snapshot |
| `404 Not Found` | Flag set deleted | Evict entry, close upstream SSE connection, delete snapshot |
| `410 Gone` | Flag set permanently deleted | Evict entry, close upstream SSE connection, delete snapshot |
| `429 Too Many Requests` | Rate-limited | Preserve stale entry — evicting on rate-limit would amplify outages via cold-miss stampedes |
| All other 4xx | Unexpected client error | Preserve stale entry |

Eviction is immediate and unconditional once the status code matches. The eviction is identical in effect to a lifecycle `tokenRotated` or `stageDeleted` event: the cache entry, upstream SSE connection, and persisted snapshot are all removed. The next SDK request for that key triggers a fresh CDN cold-miss fetch.

Metric: `proxy.cache.refresh_evictions_total{status_code="404"}` (see [telemetry.md](telemetry.md)).

## Inactivity Eviction

Long-running proxy instances can accumulate cache entries for SDK keys that are no longer in use — keys whose projects were deleted, tokens rotated, or clients disconnected without lifecycle events reaching the proxy. The background `Refresher` polls all cached keys unconditionally, so this builds up unnecessary CDN traffic over time.

The inactivity sweeper is the fix. It runs on a 1-hour tick and evicts entries that have not been accessed for longer than `CACHE_INACTIVITY_EVICT_AFTER` (default 48h). Eviction is a full GC: the cache entry, the upstream SSE connection, and the persisted snapshot are all removed — the same operations as a lifecycle `tokenRotated` or `stageDeleted` event.

### Liveness signal

Before evicting, the sweeper checks whether any SDK client has an active SSE subscription for the key. If `sse.Manager.HasSubscribers` returns true, the key is skipped regardless of its last-access age. An active SSE connection is the authoritative signal that a provider still needs the config.

This matters because an SDK may hold an SSE connection open and not issue HTTP config requests for hours. Without the liveness check, a long-running streaming provider would be incorrectly evicted.

### Last-access tracking

Every `Cache.Get` call atomically updates a per-key last-access timestamp (Unix nanoseconds stored in an `atomic.Int64`). Pointer lookup happens under the read lock; the value itself is updated without acquiring the write lock, so `Get` has no added contention on the hot path. `Cache.Set` initializes the timestamp, so snapshot-restored entries start with a fresh `time.Now()` and are not immediately swept on startup.

### Transient race window

There is a narrow window between the sweeper's liveness check and the atomic `DeleteIfInactiveSince` call where a client could subscribe. Worst case: the entry is evicted, the client's SSE channel closes, and the client falls back through the cold-miss path to re-fetch config. This is the same experience as a network interruption and recovers automatically.

### Configuration

| Variable | Default | Description |
|---|---|---|
| `CACHE_INACTIVITY_EVICT_AFTER` | `48h` | Idle window before eviction. Set to `0` to disable the sweeper entirely. |

Setting `CACHE_INACTIVITY_EVICT_AFTER=0` disables the sweeper — no goroutine is started.

## Config Passthrough and Token Auth

The proxy applies **token passthrough** — every upstream request to the CDN uses the SDK token from the originating request. The proxy holds no separate CDN credential.

The flagd config JSON payload is never parsed or modified except for one targeted operation: rewriting the SSE URL (see below). All other bytes pass through unchanged.

## SSE URL Rewrite

Flagd config payloads include an `eventStreams` array that tells the SDK where to connect for SSE events. The upstream URL points to Dynatrace's CDN SSE service. The proxy rewrites it to point to `GET /sse` on the proxy itself, so SDK clients connect to the proxy's fan-out endpoint instead.

The rewrite uses byte-level replacement (`bytes.ReplaceAll`), not JSON re-encoding, to preserve the rest of the payload exactly as received from upstream.

**Rewrite outcomes** (tracked by `proxy.config.sse_rewrite_total`):

| Result | Meaning |
|---|---|
| `rewritten` | SSE URL replaced with the proxy URL |
| `no_sse_entry` | No `type=sse` entry found; payload returned unchanged |
| `not_found_in_body` | Upstream URL was parsed but not found as an exact byte sequence (e.g. non-standard JSON encoding); payload returned unchanged |

When the upstream URL is not found as a literal byte sequence, a structured warning is logged with the cache key. The payload is still served; only the SSE URL is missing.

### Proxy SSE URL Construction

The rewritten URL is derived from the incoming HTTP request — no static `PROXY_PUBLIC_URL` configuration is needed:

```
{derived-scheme}://{r.Host}/sse?sdk_key={url-encoded-sdk-key}
```

The scheme is resolved from `X-Forwarded-Proto`, then `r.TLS`, then defaults to `http`. A malformed `Host` header is replaced with `unknown` and a warning is logged.

## Cold-Miss Deduplication

When multiple SDK instances cold-start simultaneously, concurrent requests for the same uncached SDK key are deduplicated via `singleflight`. Only one upstream CDN call is made; all waiting goroutines share the result. Side effects (`cache.Set`, SSE notification, snapshot write) fire exactly once per dedup batch.

This eliminates the cold-start stampede: N×M upstream requests become 1×M, where M is the number of distinct SDK keys.

## 503 on Uncached Miss During Outage

When a request arrives for an SDK key with no cache entry and the upstream CDN is unreachable, the proxy returns **503**. It never returns an empty payload — an empty response is indistinguishable from a legitimately empty flag set and would disable all flags for connected SDKs.
