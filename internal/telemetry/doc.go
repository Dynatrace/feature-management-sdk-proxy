// Package telemetry handles per-SDK-token request counting, batch billing flush,
// and OTel observability bootstrapping helpers.
//
// Counter is the in-memory store for request counts. It is incremented on every
// incoming SDK HTTP request and drained by the billing reporter (sdk-proxy-awa)
// on each flush interval.
//
// BillingReporter is the interface that abstracts delivery of drained counts to
// the billing endpoint. An in-process mock is provided in the billingtest
// sub-package for use in tests of the flush loop and related components.
//
// NewLogProvider constructs an OTLP/HTTP LoggerProvider that exports structured
// logs via the same OTEL_EXPORTER_OTLP_ENDPOINT used for metrics and traces.
// TeeHandler fans slog records to multiple handlers so stdout JSON output is
// preserved alongside the OTel log bridge when OTLP export is enabled.
package telemetry
