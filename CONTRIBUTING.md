# Contributing to the SDK Proxy

## Requirements

- Go 1.26+
- [golangci-lint](https://golangci-lint.run/usage/install/) for linting
- Docker (optional, for building the OCI image)
- [Helm](https://helm.sh/docs/intro/install/) 3.x (optional, for linting the chart in `charts/sdk-proxy`)

## Build & Test

```bash
make build       # compile the binary (outputs ./sdk-proxy)
make test        # unit tests
make test-int    # integration tests (requires no external services; mocks are in-process)
make vet         # go vet
make lint        # golangci-lint (must pass before merge)
make docker      # build the OCI image
make helm-lint   # lint and render the Helm chart
```

All integration tests are tagged `//go:build integration` and live under `test/e2e/`. They use in-process mocks for the
CDN, SSE service, and health endpoints; no network access required.

## Commit Conventions

Follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>
```

**Types:** `feat`, `fix`, `refactor`, `test`, `chore`, `ci`, `docs`, `perf`

**Scope:** the affected package or component: `cache`, `sse`, `telemetry`, `snapshot`, `server`, `health`, `config`,
`metrics`

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
- **HTTP framework:** [Gin](https://github.com/gin-gonic/gin) v1.12+. All handlers are native `func(*gin.Context)` —
  never use `gin.WrapH`/`gin.WrapF` to wrap stdlib handlers
- **Routing:** route groups with middleware in `cmd/proxy/main.go`. Health probes (`/livez`, `/readyz`, `/healthz`)
  mount directly on the root router; API routes (`/server/*`, `/sse`) go through the `middleware.TracePropagation()`
  group
- **Middleware:** shared middleware lives in `internal/middleware/`. Per-route behavior belongs in the handler, not a
  new middleware
- **Tests:** use `gin.CreateTestContext` + a `ginContext()` helper pattern; set `gin.SetMode(gin.TestMode)` in test
  `init()` to suppress debug output
- New OTel metrics go in `internal/metrics/metrics.go`, never inline
- SDK keys must never appear in logs, metrics, or storage: use `cache.Key(sdkKey)` (SHA-256 truncated to 16 hex chars)
- Snapshot writes must be async: a failed write must never fail an SDK request
- `golangci-lint` must pass before merge
