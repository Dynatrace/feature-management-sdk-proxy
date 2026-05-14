# Monitoring Guide

This guide is organized by operator concern. Each section describes what normal behavior looks like, what to watch for, and which metrics to track. For the full metric reference, see [telemetry.md](telemetry.md).

Configure your OTel exporter to collect metrics and traces. See [telemetry.md](telemetry.md) for setup.

---

## Cache Health

The proxy's primary job is serving flagd config from its in-memory cache. On a cold start, the first request for each SDK key triggers a CDN fetch; subsequent requests are served directly from cache.

**What to watch:**

| Signal | Healthy | Investigate if |
|--------|---------|----------------|
| `proxy.requests.total{result="hit"}` rate | High relative to `miss` | Miss rate is consistently high; cache may be evicting or not warming |
| `proxy.requests.total{result="upstream_error"}` | Near zero | Sustained upstream errors indicate a CDN connectivity issue |
| `proxy.requests.total{result="client_error"}` | Near zero | Malformed requests reaching the proxy |
| `proxy.cache.size` | Stable, proportional to active SDK keys | Sudden drops indicate unexpected evictions |

**CDN outage behavior:** When the CDN is unreachable, the proxy continues serving cached configs. Requests for uncached keys receive a non-empty error response: the CDN's own 5xx code is forwarded when available, or `503` when the CDN is entirely unreachable. An empty response would be indistinguishable from a legitimately empty flag set and could silently disable all flags.

---

## SSE Connections

The proxy maintains exactly one upstream SSE connection per SDK key, regardless of how many SDK client instances are subscribed. Client instances fan out from this single upstream connection.

**What to watch:**

| Signal | Healthy | Investigate if |
|--------|---------|----------------|
| `proxy.sse.connections` | One per active SDK key | Exceeds number of distinct SDK keys (unexpected duplication) |
| `proxy.sse.client.connections` | Grows with connected SDK instances | Drops unexpectedly while traffic is steady |
| `proxy.sse.upstream.reconnects` | Occasional `eof` or `inactivity` reconnects | Sustained high reconnect rate with `io_error` |
| `proxy.sse.upstream.events{result="dropped_slow_subscriber"}` | Zero | Persistent drops indicate a slow client subscriber |

**Connection lifecycle:** An upstream SSE connection is opened after a key's config is first cached. If a client subscribes before the config is cached, the request returns `412 Precondition Failed`. This is normal on cold start; the SDK retries automatically.

---

## Startup

The proxy classifies each startup into one of five paths based on persistence configuration and snapshot state. See [snapshot.md](snapshot.md) for details on each path.

**What to watch:**

| Signal | Healthy | Investigate if |
|--------|---------|----------------|
| `proxy.startup.path{path="cold"}` | Expected when no persistence is configured | Unexpected cold starts in a fleet using persistence; check backend connectivity |
| `proxy.startup.path{path="snapshot"}` | Expected in a steady-state fleet with persistence | Absent when persistence is configured; may indicate snapshot write failures |
| `proxy.startup.path{path="snapshot_stale"}` | Occasional during long outages | Frequent stale starts suggest `SNAPSHOT_MAX_AGE` may be too short or writes are failing |
| `proxy.startup.path{path="empty_persistence_cold"}` | Expected on first deploy or after a store wipe when upstream is reachable | Recurring in a steady-state fleet; snapshots may not be persisting correctly |
| `proxy.startup.path{path="backend_unreachable"}` | Should never occur in production | Backend is unreachable at startup; check connectivity and credentials |
| `proxy.snapshot.load.total{result="loaded"}` | Increases proportionally to active SDK keys | Unexpectedly low; snapshot writes may be failing |
| `proxy.snapshot.load.total{result!="loaded"}` | Near zero | `skipped_corrupt` or `backend_error` results indicate storage issues |

---

## Cache Eviction Triggers

Cache entries (cache entry + upstream SSE connection + persisted snapshot) are removed by three distinct triggers:

| Trigger | Source | Metric |
|---------|--------|--------|
| **Lifecycle events** (`tokenRotated`, `stageDeleted`, `projectDeleted`) | Upstream SSE lifecycle channel | `proxy.lifecycle.invalidated_keys_total` |
| **Inactivity sweeper** | Background goroutine; 1-hour tick | `proxy.cache.inactivity_evictions.total` |
| **4xx on background refresh** | Upstream CDN returns 401/403/404/410 during TTL revalidation | `proxy.cache.refresh_evictions.total` |

All three triggers have identical effect: the cache entry, the upstream SSE connection, and the persisted snapshot are all removed synchronously. The next SDK request for the key triggers a fresh cold-miss CDN fetch.

See [cache.md](cache.md) for the full 4xx eviction policy and which status codes evict vs. preserve the entry.

---

## Lifecycle Events

The upstream SSE service emits lifecycle events when SDK keys are rotated or their associated stage or project is deleted. The proxy reacts by invalidating the affected cache entry and closing the corresponding upstream SSE connection.

**Event types:**

| Event | Proxy behavior |
|-------|----------------|
| `tokenRotated` | Cache entry removed; upstream SSE connection closed |
| `stageDeleted` | Cache entry removed; upstream SSE connection closed |
| `projectDeleted` | Cache entry removed; upstream SSE connection closed |

After invalidation, the next SDK request for that key triggers a fresh CDN fetch. If the key was rotated, the SDK must be reconfigured with the new key; the proxy has no knowledge of the new key until an SDK instance requests it.

**Key array cap:** Lifecycle events with more than `LIFECYCLE_KEYS_CAP` combined `sdkKeyHashes` + `sdkKeys` entries (default 10,000) are rejected as malformed (`result="malformed"`) before any hashing work is performed. This prevents a compromised upstream from triggering unbounded SHA-256 work on the SSE receive goroutine. See [`LIFECYCLE_KEYS_CAP`](configuration.md#lifecycle_keys_cap) in the configuration reference.

**What to watch:**

| Signal | Healthy | Investigate if |
|--------|---------|----------------|
| `proxy.lifecycle.events_total{result="handled"}` | Matches expected rotation/deletion activity | Low compared to known rotation events; lifecycle channel may be disconnected |
| `proxy.lifecycle.events_total{result="malformed"}` | Zero | Non-zero: upstream is sending unrecognized or oversized events (see cap description above) |
| `proxy.lifecycle.invalidated_keys_total` | Proportional to events handled | Zero when events are received; invalidation may be silently failing |

---

## Shutdown

The proxy shuts down gracefully on `SIGTERM` or `SIGINT`. In-flight requests are allowed to complete before connections are closed.

**What to watch:**

- In-flight requests complete before the proxy exits (`proxy.requests.total` count stops incrementing cleanly).
- `proxy.sse.client.connections` drops to zero as SSE clients are disconnected.
- Log line `"health: proxy is ready"` appears on the first successful `/readyz` probe, confirming the proxy transitioned to ready.

**Exit codes:**
- `0`: clean signal-driven shutdown. Expected behavior.
- `1`: startup failure or unexpected listener error. Investigate logs.

---

## Health Probes

The proxy exposes three HTTP endpoints for Kubernetes liveness and readiness probes.

| Endpoint | Returns 200 when | Returns 503 when |
|----------|-----------------|-----------------|
| `GET /livez` | Process is running | Never (liveness is always true) |
| `GET /readyz` | Proxy has a usable data source | Snapshot load is still in progress at startup |
| `GET /healthz` | Same as `/readyz` | Same as `/readyz` |

The readiness check matters most during startup with snapshot persistence enabled: the proxy becomes ready after snapshots are loaded. Without persistence, `/readyz` returns 200 as soon as the HTTP listener is up.

**Recommended Kubernetes probe configuration:**

```yaml
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
  initialDelaySeconds: 2
  periodSeconds: 5
  failureThreshold: 3
```

Do not route SDK traffic to a pod before `/readyz` returns 200. An unready proxy may serve stale or empty responses.
