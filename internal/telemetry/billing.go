package telemetry

import "context"

// BillingReporter sends per-SDK-token usage counts to the Dynatrace billing
// endpoint (a Cloudflare Worker, out-of-scope of this repository). Each call
// sends exactly one Sample — one HTTP POST per SDK token, authenticated with
// that token. Implementations must be safe for concurrent callers.
//
// # HTTP Contract (proposed — final URL and auth header TBD with Worker team)
//
//	Method:  POST
//	URL:     <BILLING_ENDPOINT_BASE>/v1/usage
//	Headers: Authorization: Api-Token <sample.SDKKey>
//	         Content-Type:  application/json
//
// # Request Body
//
//	{
//	  "schema_version": 1,
//	  "sdk_key_id":     "<public prefix of the SDK key; cache.PublicID(sdk_key)>",
//	  "request_count":  <int64; non-zero>,
//	  "window_start":   "<RFC3339Nano UTC>",
//	  "window_end":     "<RFC3339Nano UTC>",
//	  "proxy_instance": "<opaque string; pod/container identity>"
//	}
//
// Field semantics:
//   - schema_version: integer; currently 1. Bump on any breaking change.
//   - sdk_key_id: public prefix of the SDK key (cache.PublicID). Raw SDK key
//     is auth-only and must never appear in the body.
//   - request_count: delta since the last successful flush for this token;
//     always > 0 (zero counts are not reported).
//   - window_start / window_end: half-open interval [start, end); the Worker
//     deduplicates by (sdk_key_id, window_start).
//   - proxy_instance: opaque pod/container identifier provided at reporter
//     construction time; helps correlate fleet-wide counts across replicas.
//
// # Response Handling
//
//   - 2xx              — success; counts considered delivered.
//   - 408, 429, 5xx    — retriable; caller retries at next flush interval.
//   - other 4xx        — permanent; caller drops the record without retrying.
//   - network/I/O error — retriable.
//
// To distinguish permanent failures, implementations should return an error
// wrapping ErrPermanent (use errors.Is to test). All other non-nil errors are
// treated as retriable by the caller.
//
// # Auth
//
// The originating SDK token authenticates the HTTP call via the Authorization
// header. The proxy holds no separate billing credential. The Worker validates
// that the Authorization token matches the sdk_key_id in the body by computing
// cache.PublicID(token) server-side.
//
// # Back-pressure
//
// Report blocks until the upstream POST completes or ctx is cancelled. On
// ctx.Err() Report returns the wrapped context error without retrying; the
// flush loop (sdk-proxy-awa) controls the deadline and retry policy. Report
// must not buffer internally — that is the caller's responsibility.
type BillingReporter interface {
	// Report sends one Sample to the billing endpoint. Returns nil on 2xx.
	// Returns a wrapped context error on cancellation. Returns a wrapped
	// error (testing errors.Is(err, ErrPermanent)) for non-retriable 4xx
	// responses so callers can drop instead of retry. All other errors are
	// retriable.
	Report(ctx context.Context, sample Sample) error
}

// ErrPermanent is returned (wrapped) by BillingReporter implementations when
// the billing endpoint responds with a non-retriable error (e.g. 400, 401,
// 403). Callers test for it with errors.Is and drop the record on match.
var ErrPermanent = permanentError("permanent billing error")

type permanentError string

func (e permanentError) Error() string { return string(e) }
