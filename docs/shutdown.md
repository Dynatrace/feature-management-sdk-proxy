# Shutdown Behavior

> See also: [operations.md](operations.md) for the full runtime context around shutdown.

The proxy handles `SIGTERM` and `SIGINT` gracefully. On either signal, it drains in-flight requests, flushes pending
data, and exits cleanly.

## Shutdown Sequence

1. **HTTP server drains:** waits for in-flight requests to complete, up to `SHUTDOWN_TIMEOUT` (default 30s). New
   connections are rejected immediately.
2. **SSE manager shuts down:** all upstream SSE connections are closed and subscriber channels are drained. A 5-second
   deadline applies.
3. **Snapshot writer drains:** if snapshot persistence is configured, the async write queue is drained. Pending writes
   are given a best-effort opportunity to complete; writes that cannot finish in time are logged and dropped. A failed
   snapshot write never fails the shutdown.
4. **Final usage flush:** pending usage records are flushed. A 10-second deadline applies.

Usage counts are incremented during request handling (step 1), so the final flush (step 4) is guaranteed to capture all
counts from requests that completed before the server stopped.

## Always Exit 0 on Signal-Driven Shutdown

The proxy always exits 0 when shutdown is triggered by a signal, even if non-critical teardown steps log errors. Process
supervisors and Kubernetes should not treat a SIGTERM-driven shutdown as a failure.

## What Still Exits Non-Zero

The process exits 1 in two cases:

- **Startup failure:** config validation, metrics registration, or server construction fails.
- **Listener error before any signal:** the HTTP server dies unexpectedly before a SIGTERM is received.
