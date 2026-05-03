// Package telemetry handles per-SDK-token request counting and batch billing flush.
//
// Counter is the in-memory store for request counts. It is incremented on every
// incoming SDK HTTP request and drained by the billing reporter (sdk-proxy-awa)
// on each flush interval.
package telemetry
