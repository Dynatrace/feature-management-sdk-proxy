# ADR-0004: Evaluation Metrics Reverse Proxy

**Status:** Accepted
**Date:** 2026-05-21
**Deciders:** Michael Beemer, platform/flag management service team

---

## Context and Problem Statement

Provider SDKs emit evaluation metrics (flag evaluations, variants, reasons, counts) to the upstream metrics ingest
service at `POST /v1/metrics`. SDKs configured with a `proxyUrl` have no path to route these calls through the proxy
— they must call the metrics endpoint directly or drop metrics entirely. Either outcome breaks the single-egress model
that sdk-proxy is designed to provide.

Adding a transparent reverse proxy for `POST /v1/metrics` closes the gap: all upstream traffic from an SDK can flow
through sdk-proxy, with no SDK-side configuration change beyond setting `proxyUrl`.

---

## Decision Drivers

- **Single-egress model:** SDKs should not need to know about multiple upstream endpoints. All outbound calls should
  flow through the proxy.
- **Token passthrough invariant:** the proxy holds no credentials. The originating SDK Bearer token must be forwarded
  unchanged; no service credential substitution is permitted.
- **Payload-opaque principle:** the proxy must not parse, validate, or buffer the request body. The upstream metrics
  service owns schema validation; the proxy's job is forwarding.
- **No new configuration:** domain derivation must be expressible from information already available at runtime (the
  SDK key and `CDN_BASE_DOMAIN`). Adding a new env var for something derivable would create an unnecessary operational
  surface.
- **No new billing tracking:** proxying eval metrics is not a billable event in its own right. The proxy does not
  increment usage counters for `POST /v1/metrics` calls.
- **Cardinality constraint:** existing proxy metrics label only by `result`, not by `sdk_key_public_id`, to bound
  cardinality at high SDK-key volumes.

---

## Considered Options

### Option A: Derive upstream metrics host as `metrics.<geo>.<CDN_BASE_DOMAIN>` (accepted)

The SDK key encodes a two-letter geo segment in its second dot-segment: `{token}.{platform}_{geo}_{publicRandom}.{private}`.
The existing `ExtractGeo` helper already parses this segment for CDN URL construction (`cdn.<geo>.<CDN_BASE_DOMAIN>`).
Applying the same pattern with a `metrics.` prefix yields the correct metrics ingest host.

**Pros:**

- No new env var. `CDN_BASE_DOMAIN` already drives CDN host construction; the same value now also drives metrics host
  construction.
- Reuses `ExtractGeo` — same key-format guarantee means the same validation applies.
- Confirmed by the platform team: `metrics.<geo>.<CDN_BASE_DOMAIN>` is the correct target.

**Cons:**

- `CDN_BASE_DOMAIN` now implicitly controls two upstream services. Operators who change the base domain will affect
  both CDN config fetches and metrics ingest. The name `CDN_BASE_DOMAIN` becomes slightly misleading. This is
  acceptable for now; a future rename can address it if the coupling grows.

### Option B: Introduce a separate `METRICS_BASE_DOMAIN` env var

A dedicated env var would make the coupling explicit and allow independent base domains.

**Pros:**

- Explicit; no ambiguity about which env var controls which upstream.

**Cons:**

- Adds operational surface for no functional benefit — the two domains share the same base in every known environment.
- Violates the issue's explicit "no new configuration beyond what is derivable from the SDK key and existing
  CDN_BASE_DOMAIN-style env var pattern" constraint.

**Decision:** Option A.

---

### Option C: New `internal/evalmetrics` package (accepted)

A new package keeps evaluation metrics forwarding separate from the flagd config cache/fetch logic in
`internal/proxy`. The two handlers have different lifecycles, different upstream targets, different response semantics,
and no shared state.

**Pros:**

- SRP: `internal/proxy` is responsible for flagd config (cache, ETag, SSE rewrites, snapshot integration).
  Mixing a stateless reverse proxy into that handler tangles two unrelated lifecycles.
- Easier to test independently; no cache or SSE dependencies in the test setup.

**Cons:**

- Small code duplication: `hopByHopHeaders`, `copyHeader`, and `sanitizeErr` appear in both packages. At five to
  eight lines each, exporting them from a shared location would be premature abstraction. Three similar lines beat
  a premature helper.

**Decision:** New `internal/evalmetrics` package. Duplication accepted.

---

### Option D: Reuse `MAX_BODY_BYTES` for inbound body cap (accepted)

A transparent pass-through with no body size limit is a DoS vector: a multi-GiB POST would exhaust memory. The
issue says "no new configuration," but a cap is operationally necessary.

`MAX_BODY_BYTES` (default 10 MiB, set via env) already governs how much the proxy reads from CDN
responses. Reusing the same limit for the inbound eval-metrics body is consistent and requires no new env var.

**Decision:** Reuse `MAX_BODY_BYTES` via `io.LimitReader`. Oversized bodies return `413 Payload Too
Large` and no upstream call is made.

---

### Option E: Result-only metric labels (accepted)

The issue description asked for metrics keyed on `sdk_key_public_id`. Existing proxy metrics (`proxy.requests.total`)
label only by `result` to bound cardinality at high SDK-key volumes. A per-key counter that grows unboundedly as
new SDK tokens are issued is an operational liability.

`sdk_key_public_id` is still observable via span attributes (`sdk_key_public_id` on every `POST /v1/metrics` server
span) and structured log fields. The metric labels `result` only.

**Decision:** `result`-only metric labels for `proxy.metrics.requests.total` and `proxy.metrics.forward.latency_seconds`.
Per-key correlation is available via OTel spans.

---

## Decision

Implement `POST /v1/metrics` as a transparent reverse proxy in `internal/evalmetrics`:

1. Extract the Bearer token from the `Authorization` header.
2. Derive the upstream host as `metrics.<geo>.<CDN_BASE_DOMAIN>` using `proxy.ExtractGeo`.
3. Cap the inbound body at `MAX_BODY_BYTES` via `io.LimitReader`.
4. Forward the body and Bearer token to the upstream unchanged.
5. Return the upstream response status and body verbatim.
6. Emit `proxy.metrics.requests.total` (counter, label: `result`) and
   `proxy.metrics.forward.latency_seconds` (histogram).
7. Do not increment usage counters — eval metrics POSTs are not billable events.

---

## Consequences

- SDKs with `proxyUrl` configured can route `POST /v1/metrics` through the proxy with no additional configuration.
- `CDN_BASE_DOMAIN` now controls two upstream services. The env var name is slightly misleading; a future rename may
  be warranted if additional services are added.
- The inbound body size limit is shared with the CDN response cap. If the two need to diverge, a new env var can be
  introduced at that time.
- No billing impact: eval metrics calls are not tracked for usage reporting.
