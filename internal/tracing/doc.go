// Package tracing configures the OTel TracerProvider for the SDK Proxy.
// It mirrors the setup in internal/metrics: OTLP/HTTP exporter, resource
// attributes from environment, and an explicit Shutdown discipline.
package tracing
