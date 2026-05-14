# ADR-0001: Billing Ingestion Approach

**Status:** Proposed, pending platform team sync
**Date:** 2026-05-03
**Deciders:** Michael Beemer, platform/flag management service team

---

## Context and Problem Statement

The SDK proxy is a caching layer: one upstream GET to the Cloudflare flagd config CDN can serve thousands of downstream SDK evaluation requests before the cache refreshes. Billing charges on downstream evaluations, but Cloudflare only sees the proxy's upstream requests, not the individual evaluations hidden behind the cache.

The proxy's `internal/telemetry` package already accumulates per-SDK-key request counts. The open question is how those counts reach the billing system.

---

## Decision Drivers

- **Token passthrough invariant:** the proxy holds no credentials. Any billing approach must authenticate using the originating SDK token, not a separate service credential. This is a load-bearing constraint.
- **Billing unit clarity:** the proxy can only report proxy-level HTTP requests (one per cache interval per SDK key), not raw SDK flag evaluation calls. The billing system must account for this.
- **Multi-replica behavior:** two proxy instances serving the same SDK key emit counts independently. The billing backend must aggregate by `sdk_key_id = cache.PublicID(sdk_key)` across replicas.
- **Operational simplicity:** avoid introducing a second network transport, retry loop, and back-pressure contract if a simpler path exists.

---

## Considered Options

### Option A: Worker on the CDN Route

Deploy a Cloudflare Worker in front of the existing flagd config CDN path. The proxy adds an `X-Downstream-Request-Count` header on every upstream GET (cache misses and TTL revalidations). The Worker reads the header and writes the count to Cloudflare Analytics Engine.

Workers bound to a CDN route run on **every request, including edge-cached responses**, so the header is visible even when the CDN serves from cache without hitting origin.

**Pros:**
- One transport, one auth surface (SDK token already on the upstream GET).
- Eliminates the separate billing endpoint, flush goroutine, and retry buffer entirely.
- Token passthrough preserved trivially: no new credential surface.
- Billing data flows on a connection that already exists.

**Cons:**
- Billing latency is bounded by the cache refresh interval (~5 minutes with default TTL). Counts accumulate in memory and drain on the next upstream fetch.
- Adds a Worker invocation cost to every CDN request (~$0.30/M after the included quota).
- Requires a platform-side change to deploy the Worker on the CDN route. **This is the key dependency.**
- Counts held in memory since the last upstream fetch are lost on proxy crash (up to ~5-minute loss window).

**Open questions:**
1. Can a Worker be deployed in front of the existing flagd config CDN route? Who owns that deployment?
2. Is ~5-minute worst-case billing latency acceptable?
3. Is up to ~5 minutes of count loss on proxy crash acceptable?
4. Does Cloudflare forward `X-*` headers to the Worker on every request (including edge-cache hits)?

### Option C: Separate Billing Endpoint

Build a Cloudflare Worker at a dedicated billing URL. The proxy POSTs accumulated counts on a configurable flush interval (default 60 seconds, independent of cache TTL). The Worker writes to Analytics Engine.

The proxy-side design is already implemented in `internal/telemetry/billing.go`. The endpoint-side Worker does not yet exist.

**Pros:**
- Decouples billing timing from cache refresh timing. Sub-minute billing latency achievable.
- Works regardless of whether a Worker sits in front of the CDN config route.
- Smaller crash-loss window (bounded by flush interval, not TTL).
- Proxy-side scaffolding already in place.

**Cons:**
- Requires a second HTTP endpoint, retry loop, and back-pressure contract.
- The Worker team must build and operate the billing endpoint.
- Auth: proxy sends `Authorization: Api-Token <sdk_key>`. If the Worker requires a separate service credential, it violates the token passthrough invariant; escalate before accepting.

**Open questions:**
1. Does the billing Worker endpoint exist today, or must it be built? Who owns it?
2. Is `Authorization: Api-Token <sdk_key>` (SDK token passthrough) the acceptable auth mechanism?
3. What are the Worker team's expectations around guaranteed delivery? The proxy has a bounded in-memory retry buffer; counts are dropped after the buffer fills.
4. Is `sdk_key_id = cache.PublicID(sdk_key)` sufficient as the billing identifier, or does the Worker need additional context?

### Option B: Count on SSE Connection

Send counts as custom headers on the ongoing upstream SSE connection. **Ruled out.** SSE is server-to-client; the proxy is the client and cannot push data back through an open SSE stream. A periodic count would require a separate POST, which collapses into Option C.

---

## Decision

**Pending.** Option A is preferred if the platform team can deploy a Worker on the CDN config route. Option C is the default if that is not feasible or the timeline is uncertain.

**Do not implement either option until the approach is agreed.** Do not send `X-Downstream-Request-Count` speculatively: on cache hits (the majority of requests), the header is discarded silently without a Worker reading it.

---

## Shared Questions (Both Options)

- **Downstream consumer:** Where does billing data land: Grail, a dedicated billing store, Analytics Engine? This affects retention, replay, and late-arrival handling.
- **Billing unit:** The proxy reports proxy-level fetches, not individual flag evaluation calls. Does the billing model account for the multiplier (N SDK evaluations per proxy fetch)?
- **Multi-replica aggregation:** Two proxy replicas serving the same SDK key emit independent counts. The billing backend aggregates by `sdk_key_id` across replicas. Confirm this is expected.

---

## Consequences

**If Option A is chosen:**
- Delete `internal/telemetry/billing.go`, `internal/telemetry/logreporter.go`.
- Remove the billing flush goroutine from `cmd/proxy/main.go`.
- Add `Counter.DrainOne(keyID)` to `internal/telemetry/counter.go`.
- Set `X-Downstream-Request-Count` header in `internal/proxy/handler.go` (`serveMiss`, `Fetch`).
- Cancel issues: `sdk-proxy-sim` (billingShutdownGrace config), `sdk-proxy-7fp` (BoundedRetryBuffer ring buffer).

**If Option C is chosen:**
- Build the HTTP `BillingReporter` implementation against the confirmed endpoint contract.
- Add `BILLING_ENDPOINT_BASE` env var to `internal/config`.
- Update `docs/telemetry.md` to replace the "proposed" contract with the confirmed one.
- Proceed with `sdk-proxy-sim` and `sdk-proxy-7fp` as planned.
