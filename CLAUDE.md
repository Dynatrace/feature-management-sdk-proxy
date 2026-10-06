# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->

## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**

- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds

<!-- END BEADS INTEGRATION -->

## Build & Test

```bash
make build          # compile binary
make test           # unit tests
make test-int       # integration tests (spins up in-process mocks)
make lint           # golangci-lint
make docker         # build OCI image
make helm-lint      # lint + render the Helm chart (charts/sdk-proxy)
```

Or directly via Go toolchain:

```bash
go build ./...
go test ./...
go vet ./...
```

## Architecture

SDK Proxy is a single Go binary with four core components:

| Component                | Responsibility                                                                                                                                                                              |
|--------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **HTTP proxy + cache**   | Serves flagd config from in-memory cache; fetches upstream on miss; keyed by SDK key public prefix (`cache.PublicID(sdk_key)`). See [`docs/cache.md`](docs/cache.md).                       |
| **SSE fan-out**          | Maintains one upstream SSE connection per SDK key; re-broadcasts `refetchEvaluation` events (OFREP ADR-0008) to all connected clients via `GET /sse`. See [`docs/sse.md`](docs/sse.md).     |
| **Usage telemetry**      | Increments a per-SDK-token counter on every request; batch-flushes to the billing endpoint on a configurable interval. See [`docs/telemetry.md`](docs/telemetry.md).                        |
| **Snapshot persistence** | Optional. Writes cache entries to S3-compatible object storage (or local file) asynchronously after each update; loaded on startup to survive restarts. Skipped entirely if not configured. |
| **Evaluation metrics passthrough** | Transparent reverse proxy for `POST /v1/metrics`. Forwards SDK evaluation metrics to `https://metrics.<geo>.<CDN_BASE_DOMAIN>/v1/metrics`. See [`docs/adr/0004-evaluation-metrics-reverse-proxy.md`](docs/adr/0004-evaluation-metrics-reverse-proxy.md). |

**Data flow:** SDK → proxy (HTTP config + SSE + eval metrics) → Cloudflare CDN (config fetch on miss) / upstream SSE service (one
connection per SDK key) → billing endpoint (periodic flush) / metrics ingest service (`POST /v1/metrics`).

> ⚠️ Keep this section current. When you add or change a component, update this table and the data flow description.

## Conventions & Patterns

### Git

- One logical change per commit — small, focused, reviewable. No "big bang" PRs.
- Commit message format: `<type>(<scope>): <description>` — e.g. `feat(cache): add TTL refresh background goroutine`
  - Types: `feat`, `fix`, `refactor`, `test`, `chore`, `ci`, `docs`, `perf`
  - Scope is the affected package or component (e.g. `cache`, `sse`, `telemetry`, `snapshot`, `server`)
- Never commit directly to `main`; all changes go through a PR.

### Go

- `golangci-lint` must pass before merge (run `make lint`)
- Standard Go project layout: `/cmd/proxy` for the entrypoint, `/internal` for all application packages
- No `init()` functions; wire dependencies explicitly in `main`

### Key Design Invariants

These are load-bearing constraints — do not break them without an explicit design discussion:

- **Token passthrough**: the proxy holds no credentials. Every upstream request uses the SDK token from the originating
  request.
- **Cache key**: `cache.PublicID(sdk_key)` — the first two dot-segments of the SDK key:
  `{token}.{platform}_{geo}_{publicRandom}`. The private segment must never appear in files, object storage, logs, or
  metric labels. In-memory use of the full raw key is fine (it's retained for upstream requests). OTel attribute name:
  `sdk_key_public_id`.
- **ETag / revalidation**: cache entries store the upstream CDN `ETag`. TTL refresh uses `If-None-Match`; a `304` resets
  the TTL only — no snapshot write, no fan-out.
- **Payload opaque**: the proxy never parses the flagd config JSON. Treat it as bytes.
- **Snapshot writes are async**: a failed snapshot write must never fail an SDK request. Log + metric only. Snapshots
  are skipped entirely if persistence is not configured.
- **SSE fan-out**: N SDK client instances for the same SDK key → exactly 1 upstream SSE connection. Never N.
- **SSE after config**: upstream SSE connections are only opened after the config for that SDK key is cached. Never open
  an upstream SSE connection for a key whose config has never been fetched.
- **Proxy SSE endpoint**: `GET /sse?stream_id={public_id}`. The `stream_id` is `cache.PublicID(sdkKey)` — the public
  prefix only, never the raw key. No Authorization header is required or accepted. SDKs with `proxyUrl` configured use
  this endpoint — the SSE URL embedded in the flagd config JSON is ignored.
- **Dynamic cache poll interval**: while an upstream SSE connection is actively streaming for a key, the background
  refresher polls at 10 min (`RefreshIntervalConnected`); when disconnected, reconnecting, or SSE is disabled, it polls
  at 10 s (`RefreshIntervalDisconnected`). The per-key interval comes from `sse.Manager.RefreshInterval` wired via
  `RefreshIntervalProvider`. See [`docs/cache.md`](docs/cache.md).
- **Per-flag-set isolation**: each SDK key public ID has a bounded fan-out group size (`SSE_FANOUT_CAP_PER_KEY`) and a
  bounded subscribe rate (`SSE_SUBSCRIBE_RATE_PER_SEC`/`SSE_SUBSCRIBE_BURST`). One flag set's quota never affects
  another's. Rejections surface as 429 with `sdk_key_public_id` in logs; the rejection counter
  (`proxy.sse.subscribe.rejected_total`) carries no per-key label to avoid unbounded cardinality.
- **Non-empty error on uncached miss during outage**: do not return an empty config. An empty response is
  indistinguishable from a legitimately empty flag set. When the CDN returns a 5xx, forward its code to the SDK client.
  When the CDN is unreachable (network error), return 503. Never return an empty body.

### Observability

- Emit OTel metrics and distributed traces (not Prometheus-native). All metric names are defined in `internal/metrics` —
  add new metrics there, not inline. Tracing is wired via `internal/tracing`, which mirrors `internal/metrics` (
  OTLP/HTTP provider, noop fallback).
- Every dropped record (billing buffer overflow, cache miss during outage, malformed request) must produce a structured
  log entry with enough context to diagnose the cause.
- Full metrics and tracing reference (names, labels, span catalogue): [`docs/telemetry.md`](docs/telemetry.md).

### Startup Sequence

Five paths: `cold` (no backend), `snapshot` (fresh), `snapshot_stale` (age > `SNAPSHOT_MAX_AGE`),
`empty_persistence_cold` (backend reachable + no snapshots → starts cold), `backend_unreachable` (backend configured but
unreachable → **Exit 1**). Defined in `internal/snapshot`. See [`docs/snapshot.md`](docs/snapshot.md).

### Lifecycle Event Invalidation

`tokenRotated`, `stageDeleted`, and `projectDeleted` → evict cache entry + close upstream SSE connection for the
affected key. See [`docs/monitoring.md`](docs/monitoring.md).

### 4xx Eviction on Background Refresh

HTTP `401`, `403`, `404`, `410` from the upstream CDN during background TTL revalidation → evict cache entry + close
upstream SSE connection + delete persisted snapshot for the affected key. Identical effect to lifecycle invalidation.
`429` and `5xx` preserve the stale entry to avoid amplifying outages. See [`docs/cache.md`](docs/cache.md).

### Inactivity Eviction

The inactivity sweeper is a GC path that mirrors lifecycle invalidation: **cache entry + upstream SSE connection +
persisted snapshot** are all removed for keys that have been idle for longer than `CACHE_INACTIVITY_EVICT_AFTER` (
default 48h). A key is idle when `Cache.LastAccess` is past the threshold AND `sse.Manager.HasSubscribers` returns
false. Never evict a key with active downstream SSE subscribers — that connection is the liveness signal. See
[`docs/cache.md`](docs/cache.md).

### Health Probes

`/livez` always 200. `/readyz` and `/healthz` return 200 after snapshot load completes (or immediately if no
persistence), 503 otherwise. Never route traffic before `/readyz` returns 200. See
[`docs/monitoring.md`](docs/monitoring.md).

### Shutdown

On SIGTERM, `run()` always exits 0 — signal-driven shutdown is not an error. See [`docs/shutdown.md`](docs/shutdown.md).
