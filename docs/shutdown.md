# Shutdown Behavior

> See also: [operations.md](operations.md) for the full runtime context around shutdown.

The proxy installs handlers for `SIGTERM`, `SIGINT`, and `SIGHUP`. On any of these it drains in-flight requests, flushes
pending data, and exits cleanly. The signal that triggered shutdown is logged so operators can distinguish an
orchestrator stop from a Ctrl-C or a closed terminal.

## Signals

| Signal            | Source                                                       | Behavior                                                                                  |
|-------------------|--------------------------------------------------------------|-------------------------------------------------------------------------------------------|
| `SIGTERM`         | Kubernetes, systemd, `docker stop` — normal orchestrator stop | Graceful shutdown bounded by `SHUTDOWN_TIMEOUT`. Logged as the received signal.           |
| `SIGINT`          | Interactive Ctrl-C in a terminal                              | Same graceful path as `SIGTERM`.                                                          |
| `SIGHUP`          | Controlling terminal closed (e.g. detached SSH session)       | Graceful shutdown. Logged distinctly so it is not mistaken for a deliberate stop. Not used for config reload. |
| `SIGKILL`/`SIGSTOP` | `kill -9`, OOM killer, kernel                               | Cannot be caught. In-flight requests are dropped, the final usage flush is lost, and queued snapshots are not persisted. Always prefer `SIGTERM`. |

A **second signal received during graceful shutdown** forces an immediate exit. This lets an operator escape a hung
shutdown without resorting to `SIGKILL`. The forced exit uses the conventional Unix exit code `128 + signo`
(e.g. `143` for `SIGTERM`, `130` for `SIGINT`, `129` for `SIGHUP`).

## Shutdown Sequence

1. **HTTP server drains:** waits for in-flight requests to complete, bounded by `SHUTDOWN_TIMEOUT` (default 30s). New
   connections are rejected immediately. If the deadline elapses, remaining handlers are abandoned and shutdown
   continues with the next step.
2. **SSE manager shuts down:** all upstream SSE connections are closed and subscriber channels are drained. A 5-second
   deadline applies.
3. **Snapshot writer drains:** if snapshot persistence is configured, the async write queue is drained. Pending writes
   are given a best-effort opportunity to complete; writes that cannot finish in time are logged and dropped. A failed
   snapshot write never fails the shutdown.
4. **Final usage flush:** pending usage records are flushed. A 10-second deadline applies.

Usage counts are incremented during request handling (step 1), so the final flush (step 4) is guaranteed to capture all
counts from requests that completed before the server stopped.

## Orchestrator Grace Period

The worst-case shutdown is `SHUTDOWN_TIMEOUT` + 5s (SSE) + snapshot drain + 10s (usage flush). Give the orchestrator
more time than that before it sends `SIGKILL`:

| Runtime        | Setting                                           | Default | Recommended (with `SHUTDOWN_TIMEOUT=30s`) |
|----------------|---------------------------------------------------|---------|-------------------------------------------|
| Kubernetes     | `terminationGracePeriodSeconds`                   | 30s     | 60s (Helm chart default)                  |
| Docker         | `docker run --stop-timeout` / `docker stop -t`    | 10s     | 60                                        |
| Docker Compose | `stop_grace_period`                               | 10s     | `60s`                                     |

## Always Exit 0 on Signal-Driven Shutdown

The proxy exits 0 on a normal signal-driven shutdown, even if non-critical teardown steps log errors. Process
supervisors and Kubernetes should not treat a `SIGTERM`-driven shutdown as a failure.

## What Still Exits Non-Zero

- **Startup failure (exit 1):** config validation, metrics registration, or server construction fails.
- **Listener error before any signal (exit 1):** the HTTP server dies unexpectedly before a `SIGTERM` is received.
- **Second-signal force exit (`128 + signo`):** an operator sent a second signal during graceful shutdown. The proxy
  aborts the drain immediately. Pending usage records and queued snapshots may be lost — the same caveats as `SIGKILL`,
  but at least the second signal is logged.
