# Monitoring Guide

This guide is organized by operator concern. Each section describes what normal behavior looks like, what to watch for, and which metrics to track. For the full metric reference, see [telemetry.md](telemetry.md).

Configure your OTel exporter to collect metrics — see [telemetry.md](telemetry.md) for setup.

---

## Cache Health

The proxy's primary job is serving flagd config from its in-memory cache. On a cold start, the first request for each SDK key triggers a CDN fetch; subsequent requests are served directly from cache.

**What to watch:**

| Signal | Healthy | Investigate if |
|--------|---------|----------------|
| `proxy.requests.total{result="hit"}` rate | High relative to `miss` | Miss rate is consistently high — cache may be evicting or not warming |
| `proxy.requests.total{result="upstream_error"}` | Near zero | Sustained upstream errors indicate a CDN connectivity issue |
| `proxy.requests.total{result="client_error"}` | Near zero | Malformed requests reaching the proxy |
| `proxy.cache.size` | Stable, proportional to active SDK keys | Sudden drops indicate unexpected evictions |

**CDN outage behavior:** When the CDN is unreachable, the proxy continues serving cached configs. Requests for uncached keys return `503` — an empty response would be indistinguishable from a legitimately empty flag set and could silently disable all flags.

---

## SSE Connections

The proxy maintains exactly one upstream SSE connection per SDK key, regardless of how many SDK client instances are subscribed. Client instances fan out from this single upstream connection.

**What to watch:**

| Signal | Healthy | Investigate if |
|--------|---------|----------------|
| `proxy.sse.connections` | One per active SDK key | Exceeds number of distinct SDK keys — unexpected duplication |
| `proxy.sse.client.connections` | Grows with connected SDK instances | Drops unexpectedly while traffic is steady |
| `proxy.sse.upstream.reconnects` | Occasional `eof` or `inactivity` reconnects | Sustained high reconnect rate with `io_error` |
| `proxy.sse.upstream.events{result="dropped_slow_subscriber"}` | Zero | Persistent drops indicate a slow client subscriber |

**Connection lifecycle:** An upstream SSE connection is opened after a key's config is first cached. If a client subscribes before the config is cached, the request returns `412 Precondition Failed`. This is normal on cold start — the SDK retries automatically.

---

## Startup

The proxy classifies each startup into one of five paths based on persistence configuration and snapshot state. See [snapshot.md](snapshot.md) for details on each path.

**What to watch:**

| Signal | Healthy | Investigate if |
|--------|---------|----------------|
| `proxy.startup.path{path="cold"}` | Expected when no persistence is configured | Unexpected cold starts in a fleet using persistence — check backend connectivity |
| `proxy.startup.path{path="snapshot"}` | Expected in a steady-state fleet with persistence | Absent when persistence is configured — may indicate snapshot write failures |
| `proxy.startup.path{path="snapshot_stale"}` | Occasional during long outages | Frequent stale starts suggest `SNAPSHOT_MAX_AGE` may be too short or writes are failing |
| `proxy.startup.path{path="empty_persistence_cold"}` | Expected on first deploy or after a store wipe when upstream is reachable | Recurring in a steady-state fleet — snapshots may not be persisting correctly |
| `proxy.startup.path{path="backend_unreachable"}` | Should never occur in production | Backend is unreachable at startup — check connectivity and credentials |
| `proxy.snapshot.load.total{result="loaded"}` | Increases proportionally to active SDK keys | Unexpectedly low — snapshot writes may be failing |
| `proxy.snapshot.load.total{result!="loaded"}` | Near zero | `skipped_corrupt` or `backend_error` results indicate storage issues |

---

## Lifecycle Events

The upstream SSE service emits lifecycle events when SDK keys are rotated or their associated stage or project is deleted. The proxy reacts by invalidating the affected cache entry and closing the corresponding upstream SSE connection.

**Event types:**

| Event | Proxy behavior |
|-------|----------------|
| `tokenRotated` | Cache entry removed; upstream SSE connection closed |
| `stageDeleted` | Cache entry removed; upstream SSE connection closed |
| `projectDeleted` | Cache entry removed; upstream SSE connection closed |

After invalidation, the next SDK request for that key triggers a fresh CDN fetch. If the key was rotated, the SDK must be reconfigured with the new key — the proxy has no knowledge of the new key until an SDK instance requests it.

**What to watch:**

| Signal | Healthy | Investigate if |
|--------|---------|----------------|
| `proxy.lifecycle.events_total{result="handled"}` | Matches expected rotation/deletion activity | Low compared to known rotation events — lifecycle channel may be disconnected |
| `proxy.lifecycle.events_total{result="malformed"}` | Zero | Non-zero — upstream is sending unrecognized events |
| `proxy.lifecycle.invalidated_keys_total` | Proportional to events handled | Zero when events are received — invalidation may be silently failing |

---

## Shutdown

The proxy shuts down gracefully on `SIGTERM` or `SIGINT`. In-flight requests are allowed to complete before connections are closed.

**What to watch:**

- In-flight requests complete before the proxy exits (`proxy.requests.total` count stops incrementing cleanly).
- `proxy.sse.client.connections` drops to zero as SSE clients are disconnected.
- Log line `"health: proxy is ready"` appears on the first successful `/ready` probe, confirming the proxy transitioned to ready.

**Exit codes:**
- `0` — clean signal-driven shutdown. Expected behavior.
- `1` — startup failure or unexpected listener error. Investigate logs.

---

## Health Probes

The proxy exposes three HTTP endpoints for Kubernetes liveness and readiness probes.

| Endpoint | Returns 200 when | Returns 503 when |
|----------|-----------------|-----------------|
| `GET /live` | Process is running | Never (liveness is always true) |
| `GET /ready` | Proxy has a usable data source | Snapshot load is still in progress at startup |
| `GET /health` | Same as `/ready` | Same as `/ready` |

The readiness check matters most during startup with snapshot persistence enabled: the proxy becomes ready after snapshots are loaded. Without persistence, `/ready` returns 200 as soon as the HTTP listener is up.

**Recommended Kubernetes probe configuration:**

```yaml
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
  initialDelaySeconds: 2
  periodSeconds: 5
  failureThreshold: 3
```

Do not route SDK traffic to a pod before `/ready` returns 200. An unready proxy may serve stale or empty responses.
