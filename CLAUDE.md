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
```

Or directly via Go toolchain:

```bash
go build ./...
go test ./...
go vet ./...
```

## Architecture

SDK Proxy is a single Go binary with four core components:

| Component | Responsibility |
|---|---|
| **HTTP proxy + cache** | Serves flagd config from in-memory cache; fetches upstream on miss; keyed by `SHA-256(sdk_key)`. See [`docs/cache.md`](docs/cache.md). |
| **SSE fan-out** | Maintains one upstream SSE connection per SDK key; re-broadcasts `refetchEvaluation` events (OFREP ADR-0008) to all connected clients via `GET /sse`. See [`docs/sse.md`](docs/sse.md). |
| **Usage telemetry** | Increments a per-SDK-token counter on every request; batch-flushes to the billing endpoint on a configurable interval. See [`docs/telemetry.md`](docs/telemetry.md). |
| **Snapshot persistence** | Optional. Writes cache entries to S3-compatible object storage (or local file) asynchronously after each update; loaded on startup to survive restarts. Skipped entirely if not configured. |

**Data flow:** SDK → proxy (HTTP config + SSE) → Cloudflare CDN (config fetch on miss) / upstream SSE service (one connection per SDK key) → billing endpoint (periodic flush).

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

- **Token passthrough**: the proxy holds no credentials. Every upstream request uses the SDK token from the originating request.
- **Cache key**: `SHA-256(sdk_key)` truncated to 16 hex chars. Raw SDK keys must never appear in files, object storage, or metric labels — the hash is used as a safe persistent identifier. In-memory use of the raw key is fine (it's retained for upstream requests anyway). When SDK keys gain a structured public identifier, replace the hash with that.
- **ETag / revalidation**: cache entries store the upstream CDN `ETag`. TTL refresh uses `If-None-Match`; a `304` resets the TTL only — no snapshot write, no fan-out.
- **Payload opaque**: the proxy never parses the flagd config JSON. Treat it as bytes.
- **Snapshot writes are async**: a failed snapshot write must never fail an SDK request. Log + metric only. Snapshots are skipped entirely if persistence is not configured.
- **SSE fan-out**: N SDK client instances for the same SDK key → exactly 1 upstream SSE connection. Never N.
- **SSE after config**: upstream SSE connections are only opened after the config for that SDK key is cached. Never open an upstream SSE connection for a key whose config has never been fetched.
- **Proxy SSE endpoint**: `GET /sse`, SDK key in the Authorization header. SDKs with `proxyUrl` configured use this endpoint — the SSE URL embedded in the flagd config JSON is ignored.
- **503 on uncached miss during outage**: do not return an empty config. An empty response is indistinguishable from a legitimately empty flag set.

### Observability

- Emit OTel metrics (not Prometheus-native). All metric names are defined in `internal/metrics` — add new metrics there, not inline.
- Every dropped record (billing buffer overflow, cache miss during outage, malformed request) must produce a structured log entry with enough context to diagnose the cause.
- Full metrics reference (names, labels, instrument types): [`docs/telemetry.md`](docs/telemetry.md).

### Shutdown

- On SIGTERM, `run()` always exits 0 — signal-driven shutdown is not an error. See [`docs/shutdown.md`](docs/shutdown.md) for the full shutdown sequence and rationale.
