# Shutdown Behavior

The proxy handles `SIGTERM` and `SIGINT` gracefully. On either signal, it drains in-flight requests, flushes billing data, and exits cleanly.

## Shutdown Sequence

1. **HTTP server drains** — `srv.Shutdown` waits for in-flight requests to complete, up to `SHUTDOWN_TIMEOUT` (default 30s). New connections are rejected immediately.
2. **SSE manager shuts down** — all upstream SSE connections are cancelled and subscriber channels are closed. A 5-second deadline applies.
3. **Final billing flush** — the flusher performs one last drain of the counter and retry buffer and attempts to deliver all pending samples. A 10-second deadline applies (`billingShutdownGrace`).

Counter increments happen during request handling (step 1), so the billing flush (step 3) is guaranteed to see all counts from requests that completed before the server shut down.

## Always Exit 0 on Signal-Driven Shutdown

`run()` returns `nil` from the signal branch, even when `srv.Shutdown`, the listener, or the SSE manager returns a non-nil error during teardown. Those errors are logged at `WARN` level and the process exits 0.

This is intentional. Process supervisors (systemd, Kubernetes) and CI pipelines that watch exit codes should not treat a clean, signal-driven shutdown as a failure. A non-zero exit from a SIGTERM is typically used to signal that the process crashed or could not start — not that it shut down on request.

> ⚠️ Do not revert this behavior. If error propagation from shutdown paths is added in the future, make sure the signal branch still returns `nil` so that pod restarts and rolling deployments do not generate false failure alerts.

## What Still Exits Non-Zero

`run()` returns an error (and the process exits 1) in two cases:

- **Startup failure** — config validation, metrics registration, or server construction fails.
- **Listener error before any signal** — the HTTP server fails to start or dies unexpectedly before a SIGTERM is received.
