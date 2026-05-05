// Package sse manages upstream SSE connections and fan-out to SDK clients.
//
// Manager maintains exactly one upstream SSE connection per SDK key hash.
// It reads the upstream SSE URL from the flagd config JSON eventStreams array
// (narrow parse exception 1: only eventStreams[*].url and .inactivityDelaySec
// are read; the rest of the payload is treated as opaque bytes).
// RewriteSSEURL extends this exception: it locates the upstream URL via
// ExtractSSEURL and replaces it with a byte-precise substitution, leaving
// all other payload bytes identical to what upstream returned.
//
// OnConfigCached must be called after a fresh cold-fetch 200 populates the
// cache. OnRefreshed must be called after each background 200 refresh (not
// 304). The snapshot load path must NOT call either — SSE is established only
// after the proxy confirms upstream reachability via a live HTTP response.
//
// LifecycleHandler handles lifecycle SSE events (tokenRotated, stageDeleted,
// projectDeleted). These events carry a data field with sdkKeyHashes or sdkKeys
// (narrow parse exception 2). For each affected key hash the handler calls
// Invalidate on each registered Invalidator (cache, SSE manager, snapshot
// writer). Lifecycle events are NOT forwarded to SDK subscribers — they are
// intercepted before fan-out. The Manager's Close method removes the upstream
// connection and closes all subscriber channels for the invalidated key.
//
// Handler serves GET /sse for SDK clients. The stream_id query parameter
// carries the SDK key hash (cache.Key(sdkKey)) — the raw SDK key must never
// appear in this URL. The handler gates on cache presence (412 if the config
// has not been fetched yet), then subscribes to the Manager and re-broadcasts
// refetchEvaluation events to the client. It never opens an upstream SSE
// connection; that is handled exclusively by Manager.
package sse
