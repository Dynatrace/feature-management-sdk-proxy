# SSE Fan-Out

The proxy maintains exactly one upstream SSE connection per SDK key and fans events out to all connected SDK clients sharing that key. This is the core multiplexing guarantee: N SDK client instances → 1 upstream connection, never N.

## Client Endpoint: `GET /sse`

SDK clients connect to `GET /sse?sdk_key={sdk_key}`. The SDK key is passed as a query parameter.

**Prerequisites before a connection is accepted:**

1. The SDK key must match the `dt_server_{region}_{token}` format. Invalid format returns 400.
2. A flagd config entry for that SDK key must already be cached. If the config has not been fetched yet, the handler returns **412 Precondition Failed** with a message directing the client to fetch `/v1/config/{sdk_key}.json` first.

Only connections that pass both gates are accepted and counted toward billing.

**Response headers** set on acceptance:

```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no
```

### Heartbeats

The handler sends an SSE comment (`: ping`) every **15 seconds** on idle connections to keep proxies and load balancers from closing the connection.

### Slow Subscriber Handling

Events are dispatched to subscriber channels (buffered at 16). If a client's buffer is full at dispatch time, the event is dropped and a structured warning is logged. The metric `proxy.sse.upstream.events{result="dropped_slow_subscriber"}` is incremented.

## Upstream SSE Client

The `Manager` owns all upstream connections. It starts a connection for a given SDK key when `OnConfigCached` is called after a successful cold fetch (200). It never opens an upstream connection from the snapshot load path — SSE is established only after the proxy has confirmed upstream reachability via a live HTTP response.

### Connection Lifecycle

Each upstream connection runs in a dedicated goroutine until its context is cancelled. The connection:

1. Sends `Accept: text/event-stream` and `Cache-Control: no-cache` to upstream.
2. Includes `Last-Event-ID` on reconnects if a previous event ID was received.
3. Reads SSE frames line by line and dispatches complete events to all active subscribers.

### Inactivity Watchdog

Each connection has an **inactivity deadline** read from the `inactivityDelaySec` field in the flagd config's `eventStreams` entry (defaulting to 120 seconds). If no bytes arrive within that window, the connection body is closed and the connection enters the reconnect path.

SSE comment lines (`: ...`) count as activity and reset the deadline.

### Reconnect Backoff

On any disconnect (EOF, IO error, inactivity), the client reconnects with **exponential backoff**: starting at 1 second, doubling each attempt up to a maximum of 60 seconds. The backoff resets to 1 second when a connection stays stable for more than 30 seconds.

**Reconnect reasons** (tracked by `proxy.sse.upstream.reconnects{reason=...}`):

| Reason | Trigger |
|---|---|
| `eof` | Upstream closed the connection cleanly |
| `io_error` | Network or read error |
| `inactivity` | No bytes received within the inactivity window |
| `url_change` | The SSE URL in the flagd config changed on background refresh |

### SSE URL Changes

When the background cache refresher fetches a new 200 payload, it calls `OnRefreshed`. If the SSE URL in the new payload differs from the current one, the old upstream connection is cancelled and a new one is started. The brief overlap while the old goroutine exits is acceptable.

### Metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `proxy.sse.connections` | UpDownCounter | `sdk_key_hash` | Active upstream SSE connections |
| `proxy.sse.client.connections` | UpDownCounter | `sdk_key_hash` | Active SDK client connections to the proxy |
| `proxy.sse.events_delivered_total` | Counter | `sdk_key_hash` | Events successfully written to SDK clients |
| `proxy.sse.upstream.reconnects` | Counter | `sdk_key_hash`, `reason` | Upstream reconnect attempts |
| `proxy.sse.upstream.events` | Counter | `sdk_key_hash`, `result` | Upstream events received, by dispatch result |

The `result` label on `proxy.sse.upstream.events` takes values `forwarded`, `dropped_no_subscribers`, or `dropped_slow_subscriber`.
