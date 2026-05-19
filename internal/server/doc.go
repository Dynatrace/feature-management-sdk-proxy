// Package server provides lifecycle management for the SDK Proxy's Gin-based
// HTTP server: TCP listener binding, graceful shutdown with configurable drain
// timeout, and ReadHeaderTimeout protection against slowloris attacks.
package server
