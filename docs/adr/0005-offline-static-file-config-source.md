# ADR-0005: Offline Static Single-File Config Source

**Status:** Accepted
**Date:** 2026-07-29
**Deciders:** Jamie Sinn, platform/flag management service team

---

## Context and Problem Statement

The proxy is CDN-backed: every SDK-key config is fetched on a cache miss from
`https://cdn.<geo>.<CDN_BASE_DOMAIN>/server/<sdk_key>.json`, revalidated on a TTL, and change
notifications arrive over an upstream SSE fan-out. This requires continuous outbound connectivity
to Dynatrace.

Some deployments are air-gapped or otherwise cannot reach the Dynatrace CDN, yet still want the
proxy's single-endpoint model for SDK clients. They need to serve flag config from a single local
file — the sole source of truth — with no upstream connectivity of any kind, and have the proxy
pick up changes when an operator edits the file (or a Kubernetes ConfigMap is re-mounted).

The file follows the flagd "flag set" paradigm: a single flagd config document contains many
flags, each resolvable to a `flagSetId`. An SDK requests one flag set by its identifier and must
receive only that set's flags.

---

## Decision Drivers

- **No upstream connectivity:** the mode must make zero outbound config calls — no CDN fetch, no
  TTL refresh, no upstream SSE.
- **Payload-opaque, except where unavoidable:** the CDN path treats the config JSON as opaque
  bytes. Splitting one file into per-flag-set documents requires structured parsing, but it should
  stay minimal (mirroring the existing narrow `eventStreams` parse in `internal/sse/payload.go`).
- **flagd fidelity:** flag-set membership must resolve exactly as flagd resolves it, including
  metadata inheritance, so the served subset matches what a flagd SDK would compute.
- **Reuse the SDK contract:** SDK clients should not need a new endpoint. The existing
  `GET /server/<key>.json` path is reused; the key in the path is the flag set's identifier.
- **Bounded change latency without a new dependency:** the proxy must notice file changes on its
  own, without adding a file-watching library.
- **Isolation from the CDN path:** the static path must not alter or risk the well-tested CDN
  behaviour.

---

## Considered Options

### flagSetId identity: full SDK key vs public prefix (accepted: full key)

Cache keys elsewhere are the SDK key *public prefix* (`cache.PublicID`). For the static source the
`flagSetId` value in the file is a **full SDK key** and is matched **exactly** against the full key
the SDK sends in the path. This is an exact-match lookup, not a prefix match: the operator writes
the full key into each flag's `metadata.flagSetId`, and only a request carrying that same full key
is served the set.

The full key contains the private segment. Per the token-passthrough invariant, in-memory use of
the raw full key is permitted; it is used only as the in-memory map key. Every log, metric, and
trace attribute still uses `sdkkey.PublicID`. The backing file is operator-authored input secured
by file permissions — the proxy never persists or logs it.

### flagSetId resolution: replicate flagd inheritance (accepted)

flagd resolves a flag's effective `flagSetId` as its own `metadata.flagSetId` when present,
otherwise the top-level (flag-set-level) `metadata.flagSetId`. Per-flag metadata is a shallow
key-merge over set-level metadata, so a flag that overrides only (say) `version` still inherits the
set-level `flagSetId`. The static source replicates this precisely: it checks for the `flagSetId`
key specifically rather than treating the presence of a per-flag `metadata` block as an override.
Flags that resolve to no `flagSetId` belong to no real set (flagd assigns an internal UUID) and are
never served.

### Change detection: polling vs fsnotify (accepted: polling)

**Polling** (stat mtime/size as a cheap pre-check, then a content hash to confirm a real change)
was chosen over `fsnotify`. Polling adds no dependency, mirrors the existing `cache.Refresher.Run`
ticker, and is robust to ConfigMap atomic-rename swaps (which change the inode and can be missed by
naive watches). A content hash means a `touch` or an identical re-mount does not churn ETags. The
interval is configurable (`STATIC_FILE_POLL_INTERVAL`, default 10s).

### Metrics upstream: explicit URL vs geo derivation (accepted: explicit URL)

The CDN path derives the metrics host as `metrics.<geo>.<CDN_BASE_DOMAIN>` from the SDK key's geo
segment (ADR-0004). An offline `flagSetId` carries no usable geo for this purpose, and the whole
point of the mode is to not assume Dynatrace hostnames. So the metrics passthrough target is
configured explicitly via `OFFLINE_METRICS_UPSTREAM_URL` (a full URL). When unset, the passthrough
is disabled and `POST /v1/metrics` returns 501. When set, the request body and `Authorization`
header are forwarded verbatim, skipping the geo-derivation and full-key/server-platform checks.

### SSE: disabled (accepted)

There is no upstream SSE. `GET /sse` returns 501 (same as `SSE_DISABLED=true`), and the SSE URL in
the served payload is not rewritten. SDK clients fall back to polling `GET /server/<key>.json`,
which reflects file changes via ETag revalidation.

### Isolation: separate package and mutually exclusive config (accepted)

The static path lives in a new `internal/staticsource` package and is selected by
`CONFIG_SOURCE=static`. In this mode the proxy constructs no CDN client, snapshot writer, SSE
manager, background refresher, or inactivity sweeper. `SNAPSHOT_BACKEND` is rejected in combination
with static mode (the file is the source of truth; a second persistence layer would conflict).

---

## Decision

Add `CONFIG_SOURCE=static`. On startup the proxy loads `STATIC_FILE_PATH`, splits it into one
served document per flag set (keyed by the exact full-key `flagSetId`, with flagd inheritance),
and serves `GET/HEAD /server/<key>.json` from that in-memory snapshot with ETag/`If-None-Match`
support. A polling watcher rebuilds and atomically swaps the snapshot when the file content
changes; a read or parse error during a reload is fail-safe (the last-good snapshot is retained).
The initial load must succeed or the proxy exits non-zero. `POST /v1/metrics` forwards to
`OFFLINE_METRICS_UPSTREAM_URL` when set, else returns 501. `GET /sse` returns 501.

Each served document copies every top-level key from the source except `flags` (replaced with the
flag-set subset) and `eventStreams` (omitted — no SSE offline); its `metadata.flagSetId` is set to
the group's id so the per-set document is self-consistent.

---

## Consequences

- **Positive:** the proxy runs fully offline from a single file, with no Dynatrace connectivity.
  SDK clients need no change beyond `proxyUrl`. File edits propagate within one poll interval.
- **Positive:** the CDN path is untouched — the static path is a separate package selected by a
  single flag, and the shared HTTP/billing/shutdown scaffolding is reused unchanged.
- **Negative:** the mode introduces the proxy's first structured parse of the flag payload (flag
  metadata). It stays narrow, but flagd format drift could require maintenance here.
- **Negative:** the backing file contains full SDK keys (with private segments) in
  `metadata.flagSetId`. That file is operator-authored and must be secured by the operator; the
  proxy never logs or persists it.
- **Negative:** change latency is bounded by the poll interval rather than event-driven. This is
  acceptable for a file-backed source and avoids a new dependency.
