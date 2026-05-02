// Package sse manages upstream SSE connections and fan-out to SDK clients.
//
// Manager maintains exactly one upstream SSE connection per SDK key hash.
// It reads the upstream SSE URL from the flagd config JSON eventStreams array
// (narrow parse exception: only eventStreams[*].url and .inactivityDelaySec
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
// Handler serves GET /sse for SDK clients. The SDK key is supplied via the
// sdk_key query parameter. The handler gates on cache presence (412 if the
// config has not been fetched yet), then subscribes to the Manager and
// re-broadcasts refetchEvaluation events to the client. It never opens an
// upstream SSE connection; that is handled exclusively by Manager.
package sse
