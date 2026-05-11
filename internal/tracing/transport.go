package tracing

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Transport is an http.RoundTripper that creates OTel HTTP client spans for each
// outgoing request. url.full and url.path are intentionally omitted — the
// proxy's upstream CDN URLs contain raw SDK keys.
type Transport struct {
	base   http.RoundTripper
	tracer trace.Tracer
}

// NewTransport wraps base with OTel HTTP client instrumentation using
// semconv v1.26 attribute names. Pass nil for base to use http.DefaultTransport.
func NewTransport(base http.RoundTripper, t trace.Tracer) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{base: base, tracer: t}
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, span := t.tracer.Start(req.Context(), req.Method,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", req.Method),
			attribute.String("server.address", req.URL.Host),
		),
	)

	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "http client error")
		span.End()
		return nil, err
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	span.End()
	return resp, nil
}
