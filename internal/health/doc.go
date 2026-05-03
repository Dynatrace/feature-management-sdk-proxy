// Package health implements Kubernetes probe endpoints for the sdk-proxy.
// It exposes /live, /ready, and /health handlers that can be mounted on
// an HTTP mux alongside the proxy's regular routes.
package health
