// Package tracing configures the OTel TracerProvider for sdk-proxy.
// It mirrors the setup in internal/metrics: OTLP/HTTP exporter, resource
// attributes from environment, and an explicit Shutdown discipline.
package tracing
