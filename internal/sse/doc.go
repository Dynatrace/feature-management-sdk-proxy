// Package sse manages upstream SSE connections and fan-out to SDK clients using
// the LaunchDarkly eventsource library for both client (upstream) and server
// (downstream) roles.
//
// Manager maintains exactly one upstream SSE connection per SDK key public identifier
// using eventsource.Stream (which handles reconnection with exponential backoff,
// Last-Event-ID resumption, and read timeouts internally). It reads the upstream
// SSE URL from the flagd config JSON eventStreams array (narrow parse exception 1:
// only eventStreams[*].url and .inactivityDelaySec are read; the rest of the
// payload is treated as opaque bytes).
// RewriteSSEURL extends this exception: it parses the full JSON payload, replaces
// the URL field in the first type=="sse" eventStreams entry, and re-serializes
// the modified payload.
//
// OnConfigCached must be called after a fresh cold-fetch 200 populates the
// cache. OnRefreshed must be called after each background 200 refresh (not
// 304). The snapshot load path must NOT call either — SSE is established only
// after the proxy confirms upstream reachability via a live HTTP response.
//
// Downstream fan-out is provided by an eventsource.Server instance held by the
// Manager. When the upstream eventsource.Stream delivers an event, the Manager
// publishes it to the eventsource.Server channel keyed by the SDK key public ID.
// The eventsource.Server fans the event out to all connected downstream HTTP
// clients subscribed to that channel.
//
// LifecycleHandler handles lifecycle SSE events (tokenRotated, stageDeleted,
// projectDeleted). These events carry a data field with sdkKeyHashes or sdkKeys
// (narrow parse exception 2). For each affected key hash the handler calls
// Invalidate on each registered Invalidator (cache, SSE manager, snapshot
// writer). Lifecycle events are NOT forwarded to SDK subscribers — they are
// intercepted before fan-out. The Manager's Close method cancels the upstream
// connection and force-disconnects all downstream subscribers for the key via
// eventsource.Server.Unregister.
//
// Handler serves GET /sse for SDK clients. The stream_id query parameter
// carries the SDK key public identifier (sdkkey.PublicID(sdkKey)) — the raw SDK key must never
// appear in this URL. The handler gates on cache presence (412 if the config
// has not been fetched yet), then delegates to the eventsource.Server's handler
// for that channel. It never opens an upstream SSE connection; that is handled
// exclusively by Manager.
package sse
