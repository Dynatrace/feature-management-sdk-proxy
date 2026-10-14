# SSE Fan-Out

> See also: [operations.md](operations.md) for a narrative walkthrough of SSE behavior in production.

The proxy maintains exactly one upstream SSE connection per SDK key and fans events out to all connected SDK clients
sharing that key. This is the core multiplexing guarantee: N SDK client instances → 1 upstream connection, never N.

## Client Endpoint: `GET /sse`

SDK clients connect to `GET /sse?stream_id={stream_id}`. The `stream_id` is the SDK key public identifier:
`cache.PublicID(sdkKey)`, i.e. `{token}.{platform}_{geo}_{publicRandom}` (e.g. `dt01.server_us_ab3xy9kp2q`). It is
embedded in the flagd config's `eventStreamEndpoint` URL by the proxy rewrite step, so clients receive it automatically.

**Prerequisites before a connection is accepted:**

1. `SSE_DISABLED` must not be `true`. When SSE is disabled, the handler returns **501 Not Implemented** before any other
   validation. SDK clients must not retry; they should rely exclusively on polling.
2. `stream_id` must be present and a valid SDK key public identifier (
   `^dt[0-9]{2}\.(web|mobile|server)_[a-z]{2}_[a-zA-Z0-9]{10}$`). Invalid format returns 400.
3. A flagd config entry for that `stream_id` must already be cached. If the config has not been fetched yet, the handler
   returns **412 Precondition Failed** with a message directing the client to fetch `/server/{sdk_key}.json` first.

4. Per-key isolation limits must not be exceeded (fan-out cap, subscribe rate). Requests rejected by these limits return
   **429 Too Many Requests** with a `Retry-After: 1` header.

Only connections that pass all gates are accepted and counted toward billing.

**Authentication:** No Authorization header is required or accepted. The `stream_id` query parameter is the only
identifier needed: it encodes which SDK key's events the client wants without exposing the private key segment.

### Cold-start behavior

Before any SDK client has fetched the flagd config for a given SDK key, `GET /sse` returns **412 Precondition Failed**
with a body directing the client to fetch the config first:

```
config not cached; fetch /server/{sdk_key}.json first
```

This is intentional. The 412 signals a precondition failure, not a server error: the proxy is healthy, it simply hasn't
seen this key yet. Once a client fetches the config via `GET /server/{sdk_key}.json`, the proxy caches the entry and
opens an upstream SSE connection. Subsequent `GET /sse` requests for that key succeed.

**Expected provider behavior:**

A provider that receives 412 at cold start should retry the SSE connection after fetching the config. Providers with
`configOrigin` pointing at the proxy retrieve the config (which includes the rewritten SSE URL) before opening the SSE
connection, so they naturally follow the correct sequence. A provider that opens the SSE connection before the config
fetch will receive 412 and should back off briefly before retrying; the proxy will be ready as soon as one flag
evaluation triggers a config fetch.

**Response headers** set on acceptance:

```
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive
X-Accel-Buffering: no
```

### Heartbeats

The handler sends an SSE comment (`: ping`) every **15 seconds** on idle connections to keep proxies and load balancers
from closing the connection.

### Slow Subscriber Handling

Events are dispatched to subscriber channels (buffered at 16). If a client's buffer is full at dispatch time, the event
is dropped and a structured warning is logged. The metric `proxy.sse.upstream.events{result="dropped_slow_subscriber"}`
is incremented.

## Two-Leg Connection Model

The proxy separates the SSE connection into two independent legs.

**Leg 1: Provider to Proxy** (`GET /sse?stream_id={public_id}`)

SDK providers connect to the proxy's `/sse` endpoint using the `stream_id` embedded in the flagd config they received
from `GET /server/{sdk_key}.json`. The proxy rewrites the `eventStreamEndpoint` URL in the flagd config payload to point
at itself, replacing the original upstream URL with `{proxy_base}/sse?stream_id={public_id}`. Providers with
`configOrigin` pointing at the proxy follow this URL automatically.

**Leg 2: Proxy to Upstream** (one connection per SDK key)

The proxy opens exactly one upstream SSE connection per distinct SDK key. It uses the original `eventStreamEndpoint` URL
from the flagd config (scheme, path, and query parameters preserved verbatim). The raw SDK key is passed as-is in
requests to the upstream service for authentication; it is never exposed to providers or embedded in client-facing URLs.

**How `stream_id` maps to the upstream connection**

The `stream_id` is `cache.PublicID(sdkKey)`, i.e. the first two dot-segments of the SDK key (
`{token}.{platform}_{geo}_{publicRandom}`). The proxy uses this as its cache key: when a client subscribes with a given
`stream_id`, the proxy looks up the matching upstream connection by that same key and routes events from it to the
subscriber. The private segment of the SDK key never appears in URLs, logs, or responses.

## Upstream SSE Client

The `Manager` owns all upstream connections. A connection is started (or confirmed) for a given SDK key in three
situations:

1. **Cold fetch:** the proxy fetches config from CDN for the first time for this key.
2. **Demand revalidation:** a snapshot-loaded entry is confirmed or refreshed on the first SDK request after startup (
   see [cache.md](cache.md#demand-revalidation-of-snapshot-loaded-entries)). SSE opens at this point for snapshot-loaded
   keys.
3. **Background refresh:** the background TTL refresher receives a new payload and detects an SSE URL change.

The proxy never opens an upstream SSE connection during snapshot loading; SSE starts only after the proxy has confirmed
upstream reachability via a live HTTP response. If multiple requests trigger the same key simultaneously, the operation
is idempotent.

### Connection Lifecycle

Each upstream connection runs in a dedicated goroutine until its context is cancelled. The connection:

1. Sends `Accept: text/event-stream` and `Cache-Control: no-cache` to upstream.
2. Includes `Last-Event-ID` on reconnects if a previous event ID was received.
3. Reads SSE frames line by line and dispatches complete events to all active subscribers.

### Inactivity Watchdog

Each connection has an **inactivity deadline** read from the `inactivityDelaySec` field in the flagd config's
`eventStreams` entry (defaulting to 120 seconds). If no bytes arrive within that window, the connection body is closed
and the connection enters the reconnect path.

SSE comment lines (`: ...`) count as activity and reset the deadline.

### Reconnect Backoff

On any disconnect (EOF, IO error, inactivity), the client reconnects with **exponential backoff**: starting at 1 second,
doubling each attempt up to a maximum of 60 seconds. The backoff resets to 1 second when a connection stays stable for
more than 30 seconds.

**Reconnect reasons** (tracked by `proxy.sse.upstream.reconnects{reason=...}`):

| Reason       | Trigger                                                       |
|--------------|---------------------------------------------------------------|
| `eof`        | Upstream closed the connection cleanly                        |
| `io_error`   | Network or read error                                         |
| `inactivity` | No bytes received within the inactivity window                |
| `url_change` | The SSE URL in the flagd config changed on background refresh |

### Streaming State and Cache Polling

While the upstream read loop is actively receiving events (after the initial 200 response, before any disconnect), the
connection is considered **streaming**. The cache refresher consults this state per key to choose its polling interval:

- **Streaming** → poll every 10 minutes (SSE covers change notifications)
- **Not streaming** (backoff, reconnecting, never connected) → poll every 10 seconds

This means a healthy SSE connection reduces background CDN load by 60x. During reconnect backoff, the proxy falls back
to aggressive polling so flag changes are still delivered promptly.

### Change Notification Events

Upstream sends `refetchConfig` on server channels (`refetchEvaluation` on web and mobile channels), delivered as an Ably
message whose `data` is the notification as a JSON string. `lastModified` is in Unix milliseconds:

```text
event: message
data: {"id":"...","name":"refetchConfig","data":"{\"type\":\"refetchConfig\",\"etag\":\"\\\"abc123\\\"\",\"lastModified\":1771622898000}"}
```

The proxy also accepts the notification unwrapped in `data`, or as a named `refetchConfig` event.

### Refreshing the Cache on Upstream Notifications

Before forwarding a `refetchConfig` (or `refetchEvaluation`), the proxy revalidates that key's cached config against
the CDN, so SDK clients that refetch in response get the new config rather than the copy cached until the next poll
(10 minutes while connected). The refresh follows the background refresher's rules: a 200 updates the cache and
snapshot, a 304 extends the TTL, and an evictable 4xx evicts the key, in which case the event is not forwarded. On a
failure the event is forwarded anyway.

- If the event's `etag` already matches the cached ETag, it is forwarded without a CDN request.
- The refresh is a plain conditional GET (`If-None-Match`) of the stored config URL; no query parameters are added.
- Events that arrive while a refresh for the key is running are coalesced: one more refresh runs afterwards and only
  the latest event is forwarded.
- Refreshes of one key (polled or event-driven) never run concurrently, so an older response can't overwrite a newer
  one.

Other event types are forwarded unchanged.

### Change Notifications From Polling

When polling retrieves new content (a background refresh or a demand revalidation returning 200) while the key's upstream
connection is not streaming, the proxy publishes a change notification to that key's subscribers itself, so connected
clients don't wait for the upstream connection to come back. It uses the type upstream would send for the key's
platform (`refetchConfig` for server keys) with the new revision's ETag and upstream `Last-Modified` (Unix
milliseconds), in the unwrapped form:

```text
event: message
data: {"type":"refetchConfig","etag":"\"abc123\"","lastModified":1771622898000}
```

No event is published while the upstream connection is streaming (upstream announces changes itself), on a 304, or when
the key has no subscribers. These events are counted as `proxy.sse.upstream.events{result="synthesized"}`.

When `SSE_DISABLED=true`, `GET /sse` returns **501 Not Implemented** and the `eventStreamEndpoint` URL is not rewritten
in the flagd config: SDK clients receive the upstream URL verbatim (or no URL if the upstream omits it). Clients must
rely exclusively on polling; they must not connect to `/sse`.

### SSE URL Changes

When the background cache refresher fetches a new 200 payload, it calls `OnRefreshed`. If the SSE URL in the new payload
differs from the current one, the old upstream connection is cancelled and a new one is started. The brief overlap while
the old goroutine exits is acceptable.

## Per-Flag-Set Isolation

A single misbehaving SDK key (one that opens hundreds of connections or reconnects at high frequency) can consume
resources that other flag sets depend on. To prevent this, the proxy enforces independent admission limits per SDK key
public ID. No flag set's behavior affects another's.

All limits are evaluated before a connection is accepted. Rejected requests are not billed: only connections that
complete setup and begin streaming count toward your usage. This means a misconfigured or aggressive client hitting
the rate limit won't inflate your SDK usage costs.

| Limit           | Env var                      | Default | Description                                                                                                                                                     |
|-----------------|------------------------------|---------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Fan-out cap     | `SSE_FANOUT_CAP_PER_KEY`     | 1000    | Maximum concurrent downstream SSE connections per key. 0 disables the cap.                                                                                      |
| Subscribe rate  | `SSE_SUBSCRIBE_RATE_PER_SEC` | 20      | Sustained rate (per second) for new subscribe admits per key.                                                                                                   |
| Subscribe burst | `SSE_SUBSCRIBE_BURST`        | 100     | Token-bucket burst capacity. Covers startup spikes before the sustained rate applies. See [configuration.md](configuration.md#sse_subscribe_burst) for details. |

Requests rejected by either limit return **429 Too Many Requests** with `Retry-After: 1`. The rejection reason
(`rate_limit` or `fanout_cap`) is logged with `sdk_key_public_id` and recorded in
`proxy.sse.subscribe.rejected_total`. The per-key label is intentionally absent from that metric: adding it would
create unbounded cardinality.

### Metrics

| Metric                               | Type          | Labels                                    | Description                                                                                                          |
|--------------------------------------|---------------|-------------------------------------------|----------------------------------------------------------------------------------------------------------------------|
| `proxy.sse.connections`              | UpDownCounter | `sdk_key_public_id`, `direction`, `state` | Active SSE connections. `direction=upstream`: one per SDK key; `direction=downstream`: one per connected SDK client. |
| `proxy.sse.upstream.events`          | Counter       | `sdk_key_public_id`, `result`             | Upstream events received. `result`: `forwarded`                                                                      |
| `proxy.sse.subscribe.rejected_total` | Counter       | `reason`                                  | Subscribe requests rejected by per-key limits. `reason`: `rate_limit`, `fanout_cap`                                  |
