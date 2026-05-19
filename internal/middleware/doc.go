// Package middleware provides shared Gin middleware for the SDK Proxy.
//
// TracePropagation extracts W3C traceparent/tracestate headers from incoming
// requests and injects them into the request context. This middleware is applied
// to the API route group (/server/*, /sse) so that handler spans appear as
// children of the caller's trace. Health probes are excluded from tracing.
package middleware
