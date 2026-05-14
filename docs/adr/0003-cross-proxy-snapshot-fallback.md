# ADR-0003: Cross-Proxy Snapshot Fallback on CDN Failure

**Status:** Accepted
**Date:** 2026-05-13
**Deciders:** Michael Beemer

---

## Context and Problem Statement

During a CDN outage, a proxy that missed the last config update before the outage will serve a stale flag set for the entire duration. When multiple proxies share a common snapshot backend (whether S3-compatible object storage or a shared local filesystem), a proxy that fetched and persisted the latest config before the outage wrote a newer snapshot. Its peers just never read it after startup.

Today the snapshot store is read-at-startup-only. A newer snapshot written by one proxy is invisible to its peers until they restart. In a prolonged outage, this means different proxies in the same cluster diverge on the flag set they serve, with no self-healing path short of a restart.

---

## Decision Drivers

- **Payload opacity invariant:** the proxy never parses the flagd config JSON. Freshness comparisons must use envelope metadata only (ETag, `captured_at`). Using the flag set's top-level `version` field is not an option.
- **Response path invariant:** a store read must never block the SDK response path. The check is async, fire-and-forget.
- **No write amplification:** a proxy that loads a peer's snapshot must not immediately write it back to the store. Only a proxy that independently verified the payload against the CDN should write snapshots.
- **Store read cost during outages:** the background poller fires every 10s per key. Without debouncing, each tick triggers a store read during an outage. The debounce interval must keep read frequency low.
- **Simplicity:** no new configuration, no pub/sub infrastructure, no separate coordination service.

---

## Considered Options

### Option A: Passive read on CDN failure (selected)

Extend the snapshot read path from startup-only to on-CDN-failure. When a background refresh fails (network error or non-evictable upstream status), the proxy asynchronously checks the shared store for a newer snapshot for that key.

### Option B: Active publish/subscribe channel

Proxies publish a notification (e.g. to a Redis channel or a storage event) when they write a snapshot. Peers subscribe and pull the snapshot on notification.

**Rejected:** adds a new infrastructure dependency (pub/sub system) for an optimisation path. Option A achieves the same result using the snapshot store that is already required for snapshot persistence: no new dependencies.

### Option C: Periodic cross-proxy sync loop

A background goroutine periodically scans the store for keys with a newer `captured_at` than the in-memory entry.

**Rejected:** fires unconditionally rather than being triggered by an actual outage. During normal operation this adds unnecessary store reads. Option A only activates when a refresh has already failed, which is the exact condition we care about.

---

## Decision

**Option A: passive read on CDN failure.**

Implementation details:

### Trigger condition

The fallback activates when `Refresher.refresh` encounters either of two failure branches for a given key:

1. **Network error:** `fetcher.Fetch` returns a non-nil error.
2. **Non-evictable upstream status:** the upstream returns 429 or 5xx (the entry is preserved, not evicted).

It does NOT activate on 304 (no change), 200 (successful refresh), or evictable 4xx (401/403/404/410: the entry is already being deleted).

### Update condition

The snapshot is applied only when both conditions hold:

- `snapshot.captured_at > current.FetchedAt` (the snapshot is more recent than what this proxy last fetched)
- `snapshot.etag != current.ETag` (the content actually changed; no-op if payload is identical)

The flag set payload is never parsed. ETags and `captured_at` are the sole freshness signals.

### Debounce strategy

The debounce interval is 60 seconds, defined as a package-level constant (`fallbackDebounceInterval`) in `internal/snapshot`. This choice is based on the following reasoning:

- The background poller fires every 10s per disconnected key.
- Without debouncing, a sustained CDN outage triggers a store read every 10s per key per proxy.
- 60s (six poller ticks) keeps store read frequency low while still reacting within one minute of a peer writing a newer snapshot.
- No environment variable: a fixed constant is sufficient. Operators cannot tune this without understanding the interaction with the poller interval, and the 60s default is appropriate for the expected cluster sizes and outage scenarios.

Debounce state is per-key, held in a `map[string]time.Time` protected by a `sync.Mutex` inside the `Fallback` component. Entries are pruned lazily on each allowed check: any entry older than `2×fallbackDebounceInterval` is deleted, bounding map growth for long-running proxies that cycle through many SDK keys.

### FetchedAt reset

On a successful fallback load, `FetchedAt` in the loaded cache entry is set to the snapshot's `captured_at`. This accurately reflects the age of the data from the perspective of the proxy that fetched it and prevents the refresher from immediately re-checking when the outage ends.

### Snapshot write on fallback load

The fallback path does NOT write to the snapshot store after loading. The proxy did not independently verify the payload against the CDN, so it is not authoritative. Writing would cause every proxy in the cluster to re-write the same snapshot in response to the same outage, amplifying store write traffic with no benefit.

### Atomicity

The cache update uses `Cache.SetIfNewer(key, newEntry, expectedFetchedAt, expectedETag)`, a compare-and-set method that holds the write lock and checks that `FetchedAt` and `ETag` still match the values observed at the start of the check. If a concurrent CDN refresh or another fallback goroutine updated the entry first, `SetIfNewer` returns false and the fallback emits a `superseded` metric.

Note: `SetIfNewer` intentionally does not update the `lastAccess` timestamp. The fallback is an internal maintenance operation, not a client request. Updating `lastAccess` would keep an otherwise-idle key alive past the inactivity eviction threshold.

### FetchURL preservation

The snapshot envelope does not persist `FetchURL`. The fallback reads the current cache entry immediately before calling `SetIfNewer` to recover the existing `FetchURL` and copy it into the new entry. If `SetIfNewer` rejects the update (entry changed), the FetchURL from the stale Get is discarded along with the new entry.

### Backend compatibility

The `Fallback` component depends on `snapshot.Store`, the same interface implemented by both the S3-compatible and local-file backends. The fallback activates for either backend when `SNAPSHOT_BACKEND` is set.

**When the fallback is useful:** cross-proxy benefit requires multiple proxies sharing the same store. This is the typical S3 multi-replica deployment. A single-node `local` deployment shares no store with any peer, so the fallback check will always return `no_newer` (the only snapshot present is the one this proxy wrote) and have no effect on cache state.

**Shared local filesystem:** if a `local` backend is configured on a shared network mount with multiple proxies pointing at the same directory, the fallback works the same way as S3. This is an unusual but valid configuration.

### SSE behaviour during outage

The fallback does not trigger an SSE reconnect or fan-out notification. SDK clients that are actively subscribed will receive a `refetchEvaluation` event from the upstream SSE service when the CDN recovers. During the outage, SDK clients poll the proxy's HTTP config endpoint and receive the updated payload on the next GET after the fallback load applies.

---

## Metric

`proxy.snapshot.outage_fallback.total`: `Int64Counter` with attribute `result`:

| Value | Meaning |
|-------|---------|
| `loaded` | Snapshot applied; cache updated with newer content |
| `no_newer` | Snapshot `captured_at` is not after current `FetchedAt`; no-op |
| `same_etag` | Snapshot ETag matches current ETag; no-op |
| `debounced` | Check suppressed by debounce interval; no store read made |
| `backend_error` | `store.Get` returned an error |
| `decode_error` | Envelope decode failed (corrupt or unknown version) |
| `superseded` | `SetIfNewer` rejected the update; entry was concurrently refreshed |

Single-node `local` deployments will emit `no_newer` on every non-debounced check, since the only snapshot present is the one the proxy itself wrote. This is expected and not actionable.

---

## Rolling-Deploy Safety

During a rolling deploy where pods running different binary versions share a snapshot store, the fallback reads whichever snapshot is present. Snapshots written by the current binary version are always readable by older binaries (ADR-0002 governs forward-compatibility). A pod running an older binary that encounters a newer snapshot version skips it (`ErrUnknownVersion`) and emits a `decode_error` on the fallback metric rather than on the startup metric. This is acceptable: the fallback is an optimisation, not a correctness requirement.

---

## Known Limitation

This only helps when at least one proxy in the cluster successfully fetched and persisted a newer snapshot before the CDN became unreachable. If all proxies lost CDN access simultaneously before any wrote an updated snapshot, the store has nothing newer to offer. This is expected and acceptable.

---

## Consequences

- **Positive:** proxies in a cluster converge on the latest known flag set within ~70s (one debounce interval + goroutine scheduling) of a peer persisting a newer snapshot, even during a total CDN outage.
- **Positive:** no new infrastructure dependencies; uses the snapshot store that is already required.
- **Positive:** no new configuration; the debounce constant requires no tuning for typical deployments.
- **Positive:** works with both the S3-compatible and local-file backends.
- **Negative:** adds store read traffic during CDN outages: one read per key per proxy per 60s. At typical cluster sizes (< 10 proxies, < 100 active keys) this is negligible for S3; for local-file deployments the cost is a filesystem stat and read.
- **Negative:** single-node `local` deployments will see `no_newer` on every non-debounced check, which is noise in the metric. The `debounced` result will dominate after the first check, limiting the noise to one `no_newer` per key per 60s during an outage.
- **Negative:** the fallback cannot help when all proxies were cold at outage start. This is a fundamental limitation of passive propagation.
