// Package telemetry handles per-SDK-token request counting and batch billing flush.
//
// Counter is the in-memory store for request counts. It is incremented on every
// incoming SDK HTTP request and drained by the billing reporter (sdk-proxy-awa)
// on each flush interval.
//
// BillingReporter is the interface that abstracts delivery of drained counts to
// the billing endpoint. An in-process mock is provided in the billingtest
// sub-package for use in tests of the flush loop and related components.
package telemetry
