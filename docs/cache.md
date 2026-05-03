# In-Memory Cache & Flagd Config Passthrough

The proxy caches flagd config payloads in memory, keyed by a hash of the SDK key. Every incoming SDK request is served from this cache; upstream CDN calls happen only on a miss or during background TTL refresh.

## Cache Key

The cache is keyed by `SHA-256(sdk_key)` truncated to 16 hex characters (`cache.Key()`). Raw SDK keys are never written to files, logs, metric labels, or object storage — the hash is the only safe persistent identifier. In-memory retention of the raw key is fine; it's needed for upstream request authentication.

## Cache Entry

Each entry holds:

- **Payload** — the raw flagd config JSON bytes, treated as opaque
- **ETag** — the upstream CDN ETag, used for conditional revalidation
- **FetchedAt** — the timestamp used to determine staleness
- **FetchURL** — the full upstream URL, retained in memory for background refresh

## TTL and Revalidation

Entries expire after **5 minutes** (`cache.TTL`). The background `Refresher` ticks at half the TTL (every 2.5 minutes) and revalidates stale entries using `If-None-Match`.

- A **304** response resets `FetchedAt` without changing the payload or writing a snapshot.
- A **200** response replaces the entry and triggers both snapshot persistence (if configured) and SSE URL change detection.

A background refresh error is logged and counted but never fails an SDK request. The stale entry stays in cache and continues to be served.

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

## 503 on Uncached Miss During Outage

When a request arrives for an SDK key with no cache entry and the upstream CDN is unreachable, the proxy returns **503**. It never returns an empty payload — an empty response is indistinguishable from a legitimately empty flag set and would disable all flags for connected SDKs.
