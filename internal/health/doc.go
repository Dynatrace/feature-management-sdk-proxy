// Package health implements Kubernetes probe endpoints for the SDK Proxy.
// It exposes /livez, /readyz, and /healthz as native Gin handlers that are
// registered directly on the router without tracing middleware.
package health
