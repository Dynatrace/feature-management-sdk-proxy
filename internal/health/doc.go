// Package health implements Kubernetes probe endpoints for the sdk-proxy.
// It exposes /livez, /readyz, and /healthz handlers that can be mounted on
// an HTTP mux alongside the proxy's regular routes.
package health
