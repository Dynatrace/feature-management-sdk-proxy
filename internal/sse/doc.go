// Package sse manages upstream SSE connections and fan-out to SDK clients.
//
// Manager maintains exactly one upstream SSE connection per SDK key hash.
// It reads the upstream SSE URL from the flagd config JSON eventStreams array
// (narrow parse exception: only eventStreams[*].url and .inactivityDelaySec
// are read; the rest of the payload is treated as opaque bytes).
//
// OnConfigCached must be called after a fresh cold-fetch 200 populates the
// cache. OnRefreshed must be called after each background 200 refresh (not
// 304). The snapshot load path must NOT call either — SSE is established only
// after the proxy confirms upstream reachability via a live HTTP response.
package sse
