// Package evalmetrics provides a transparent reverse proxy for the evaluation
// metrics ingestion endpoint (POST /v1/metrics). It accepts the request body
// and Bearer token from the SDK client, derives the upstream metrics host from
// the SDK key geo segment, and forwards the payload unchanged. The body is
// treated as opaque bytes — no parsing, no validation, no buffering.
package evalmetrics
