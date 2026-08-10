package staticsource

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/telemetry"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/tracing"
)

// result labels for proxy.requests.total, shared with the CDN proxy handler.
const (
	resultHit         = "hit"
	resultNotModified = "not_modified"
	resultNotFound    = "not_found"
	resultClientError = "client_error"
)

// Handler serves flag configs from the in-memory static-source Store. Unlike
// the CDN proxy handler it never contacts an upstream, performs no SSE URL
// rewrite, and looks up entries by the exact full SDK key (the flagSetId).
type Handler struct {
	store   *Store
	inst    *metrics.Instruments
	counter *telemetry.Counter
	logger  *slog.Logger
	tracer  trace.Tracer
}

// NewHandler constructs a static-source config handler. inst and counter may be
// nil; metrics and usage counting are then silently skipped.
func NewHandler(store *Store, inst *metrics.Instruments, counter *telemetry.Counter, logger *slog.Logger, t trace.Tracer) *Handler {
	return &Handler{store: store, inst: inst, counter: counter, logger: logger, tracer: t}
}

// Handle serves GET/HEAD /server/:file from the static source. The SDK key is
// pre-extracted by sdkkey.FromPathFile middleware.
//
// Route registration:
//
//	router.GET("/server/:file", sdkkey.FromPathFile(), staticHandler.Handle)
//	router.HEAD("/server/:file", sdkkey.FromPathFile(), staticHandler.Handle)
func (h *Handler) Handle(c *gin.Context) {
	k, _ := sdkkey.KeyFromContext(c)
	key := k.PublicID()

	ctx, span := h.tracer.Start(c.Request.Context(), c.Request.Method+" /server/{sdk_key}.json",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.request.method", c.Request.Method),
			attribute.String("http.route", "/server/{sdk_key}.json"),
			tracing.SDKKeyPublicID(key),
		))
	defer span.End()

	if !k.IsFull() {
		h.logger.WarnContext(ctx, "static: unrecognized SDK key format", slog.String("sdk_key_public_id", key))
		h.finish(ctx, span, resultClientError)
		c.String(http.StatusBadRequest, "unrecognized SDK key format: must be {token}.{platform}_{geo}_{publicRandom}.{secret}_{sha1}\n")
		return
	}

	// Count every request with a recognized key, regardless of hit/miss, to
	// mirror the CDN handler's billing behaviour.
	if h.counter != nil {
		h.counter.Increment(k.Raw())
	}

	payload, etag, ok := h.store.Lookup(k.Raw())
	if !ok {
		h.logger.DebugContext(ctx, "static: no flag set for key", slog.String("sdk_key_public_id", key))
		h.finish(ctx, span, resultNotFound)
		c.String(http.StatusNotFound, "no flag set found for the provided SDK key\n")
		return
	}

	if etag != "" && c.GetHeader("If-None-Match") == etag {
		c.Header("ETag", etag)
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		h.finish(ctx, span, resultNotModified)
		return
	}
	if etag != "" {
		c.Header("ETag", etag)
	}
	if c.Request.Method == http.MethodHead {
		c.Header("Content-Type", "application/json")
		c.Status(http.StatusOK)
		c.Writer.WriteHeaderNow()
		h.finish(ctx, span, resultHit)
		return
	}
	c.Data(http.StatusOK, "application/json", payload)
	h.finish(ctx, span, resultHit)
}

// finish records the result label on the span and the RequestsTotal counter.
func (h *Handler) finish(ctx context.Context, span trace.Span, result string) {
	span.SetAttributes(attribute.String("result", result))
	if h.inst == nil {
		return
	}
	h.inst.RequestsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
