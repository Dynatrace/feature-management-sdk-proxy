# Contributing to the SDK Proxy

## Requirements

- Go 1.24+
- [golangci-lint](https://golangci-lint.run/usage/install/) for linting
- Docker (optional, for building the OCI image)

## Build & Test

```bash
make build       # compile the binary (outputs ./sdk-proxy)
make test        # unit tests
make test-int    # integration tests (requires no external services; mocks are in-process)
make vet         # go vet
make lint        # golangci-lint (must pass before merge)
make docker      # build the OCI image
```

All integration tests are tagged `//go:build integration` and live under `test/e2e/`. They use in-process mocks for the CDN, SSE service, and health endpoints; no network access required.

## Commit Conventions

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>
```

**Types:** `feat`, `fix`, `refactor`, `test`, `chore`, `ci`, `docs`, `perf`

**Scope:** the affected package or component: `cache`, `sse`, `telemetry`, `snapshot`, `server`, `health`, `config`, `metrics`

**Examples:**
```
feat(cache): add background TTL refresh goroutine
fix(sse): handle upstream EOF on reconnect
docs(monitoring): add lifecycle events section
```

One logical change per commit. Keep PRs small and focused.

## Branch & PR Workflow

1. Create a branch from `main`: `<type>/<scope>-<short-description>` (e.g. `feat/cache-ttl-refresh`)
2. Make changes, commit following the conventions above
3. Ensure `make lint`, `make test`, and `make test-int` all pass
4. Open a PR against `main`

Never commit directly to `main`.

## Issue Tracking

This project uses **beads** for issue tracking. Run `bd prime` for the full workflow reference.

```bash
bd ready              # find available work
bd show <id>          # view issue details
bd update <id> --claim  # claim an issue before starting
bd close <id>         # close when done
```

## Code Conventions

- No `init()` functions: wire all dependencies explicitly in `cmd/proxy/main.go`
- New OTel metrics go in `internal/metrics/metrics.go`, never inline
- SDK keys must never appear in logs, metrics, or storage: use `cache.Key(sdkKey)` (SHA-256 truncated to 16 hex chars)
- Snapshot writes must be async: a failed write must never fail an SDK request
- `golangci-lint` must pass before merge
