package evalmetrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/tracing"
)

const (
	resultForwarded       = "forwarded"
	resultBadRequest      = "bad_request"
	resultPayloadTooLarge = "payload_too_large"
	resultUpstreamError   = "upstream_error"
	resultBadGateway      = "bad_gateway"
	resultInternalError   = "internal_error"
	resultUnauthorized    = "unauthorized"
)

// hopByHopHeaders must not be forwarded between proxy hops (RFC 7230 §6.1).
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Proxy-Authorization": true,
	"Proxy-Authenticate":  true,
	"Te":                  true,
	"Trailer":             true,
}

// Handler transparently proxies POST /v1/metrics to the upstream metrics
// ingest service. The upstream host is derived from the SDK key geo segment:
// metrics.<geo>.<metricsBaseDomain>. The request body and Bearer token are
// forwarded unchanged; the upstream response status and body are passed back
// to the caller verbatim.
type Handler struct {
	metricsBaseDomain string
	client            *http.Client
	inst              *metrics.Instruments
	logger            *slog.Logger
	tracer            trace.Tracer
	maxRequestBytes   int64
}

const defaultMaxRequestBytes = 10 * 1024 * 1024 // 10 MiB

// New constructs a Handler. metricsBaseDomain must be a bare hostname
// (e.g. fm.dynatracelabs.com); the geo prefix is derived per request from the
// SDK key. inst may be nil; metrics are silently skipped when nil.
func New(metricsBaseDomain string, client *http.Client, inst *metrics.Instruments, logger *slog.Logger, t trace.Tracer) (*Handler, error) {
	if metricsBaseDomain == "" || strings.Contains(metricsBaseDomain, "://") || strings.Contains(metricsBaseDomain, "/") {
		return nil, fmt.Errorf("evalmetrics: invalid metrics base domain %q: must be a bare hostname (e.g. fm.dynatrace.com)", metricsBaseDomain)
	}
	return &Handler{
		metricsBaseDomain: metricsBaseDomain,
		client:            client,
		inst:              inst,
		logger:            logger,
		tracer:            t,
		maxRequestBytes:   defaultMaxRequestBytes,
	}, nil
}

// SetMaxRequestBytes caps the inbound request body size. Bodies exceeding the
// limit are rejected with 413 and no upstream call is made.
func (h *Handler) SetMaxRequestBytes(n int64) {
	h.maxRequestBytes = n
}

// Handle is the Gin handler for POST /v1/metrics.
//
// Route registration:
//
//	api.POST("/v1/metrics", sdkkey.FromBearerToken(), evalMetricsHandler.Handle)
func (h *Handler) Handle(c *gin.Context) {
	start := time.Now()

	ctx, span := h.tracer.Start(c.Request.Context(), "POST /v1/metrics",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.request.method", "POST"),
			attribute.String("http.route", "/v1/metrics"),
		))
	defer span.End()

	// key is pre-extracted and injected by FromBearerToken middleware.
	key, _ := sdkkey.KeyFromContext(c)
	span.SetAttributes(tracing.SDKKeyPublicID(key.PublicID()))

	if !key.IsFull() {
		h.logger.WarnContext(ctx, "evalmetrics: unrecognized SDK key format",
			slog.String("sdk_key_public_id", key.PublicID()),
		)
		span.SetAttributes(attribute.String("result", resultBadRequest))
		h.record(ctx, resultBadRequest, start)
		c.String(http.StatusBadRequest, "unrecognized SDK key format\n")
		return
	}

	if key.Platform() != sdkkey.PlatformServer {
		h.logger.WarnContext(ctx, "evalmetrics: non-server SDK key rejected",
			slog.String("sdk_key_public_id", key.PublicID()),
			slog.String("platform", key.Platform()),
		)
		span.SetAttributes(attribute.String("result", resultUnauthorized))
		h.record(ctx, resultUnauthorized, start)
		c.String(http.StatusUnauthorized, "SDK key platform not authorized for /v1/metrics\n")
		return
	}

	if c.Request.ContentLength > h.maxRequestBytes {
		span.SetAttributes(attribute.String("result", resultPayloadTooLarge))
		h.record(ctx, resultPayloadTooLarge, start)
		c.String(http.StatusRequestEntityTooLarge, "request body too large\n")
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, h.maxRequestBytes+1))
	if err != nil {
		h.logger.ErrorContext(ctx, "evalmetrics: read request body failed",
			slog.String("sdk_key_public_id", key.PublicID()),
			slog.String("error", err.Error()),
		)
		span.SetAttributes(attribute.String("result", resultInternalError))
		h.record(ctx, resultInternalError, start)
		c.String(http.StatusInternalServerError, "internal error\n")
		return
	}
	if int64(len(body)) > h.maxRequestBytes {
		span.SetAttributes(attribute.String("result", resultPayloadTooLarge))
		h.record(ctx, resultPayloadTooLarge, start)
		c.String(http.StatusRequestEntityTooLarge, "request body too large\n")
		return
	}

	target := fmt.Sprintf("https://metrics.%s.%s/v1/metrics", key.Geo(), h.metricsBaseDomain)

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		h.logger.ErrorContext(ctx, "evalmetrics: build upstream request failed",
			slog.String("sdk_key_public_id", key.PublicID()),
			slog.String("error", err.Error()),
		)
		span.RecordError(err)
		span.SetAttributes(attribute.String("result", resultInternalError))
		h.record(ctx, resultInternalError, start)
		c.String(http.StatusInternalServerError, "internal error\n")
		return
	}

	upstreamReq.Header.Set("Authorization", "Bearer "+key.Raw())
	if ct := c.GetHeader("Content-Type"); ct != "" {
		upstreamReq.Header.Set("Content-Type", ct)
	}
	copyHeader(upstreamReq.Header, c.Request.Header)

	resp, err := h.client.Do(upstreamReq)
	if err != nil {
		sanitized := sanitizeErr(err)
		h.logger.WarnContext(ctx, "evalmetrics: upstream request failed",
			slog.String("sdk_key_public_id", key.PublicID()),
			slog.String("error", sanitized.Error()),
		)
		span.RecordError(sanitized)
		span.SetAttributes(attribute.String("result", resultBadGateway))
		h.record(ctx, resultBadGateway, start)
		c.String(http.StatusBadGateway, "bad gateway\n")
		return
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("upstream.status_code", resp.StatusCode))

	result := classifyStatus(resp.StatusCode)
	span.SetAttributes(attribute.String("result", result))
	h.record(ctx, result, start)

	copyHeader(c.Writer.Header(), resp.Header)
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(c.Writer, resp.Body)
}

// classifyStatus maps an upstream HTTP status to a result label.
func classifyStatus(code int) string {
	if code >= 400 {
		return resultUpstreamError
	}
	return resultForwarded
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		if hopByHopHeaders[k] {
			continue
		}
		// Authorization is set explicitly; skip to avoid duplication.
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		// Content-Type is set explicitly from the original request.
		if strings.EqualFold(k, "Content-Type") {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// sanitizeErr strips request URL values from *url.Error to prevent raw SDK
// keys (embedded in upstream paths) from appearing in log output.
func sanitizeErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s upstream: %w", ue.Op, ue.Err)
	}
	return err
}

// record increments EvalMetricsRequestsTotal and records EvalMetricsForwardLatency.
func (h *Handler) record(ctx context.Context, result string, start time.Time) {
	if h.inst == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("result", result))
	h.inst.EvalMetricsRequestsTotal.Add(ctx, 1, attrs)
	h.inst.EvalMetricsForwardLatency.Record(ctx, time.Since(start).Seconds(), attrs)
}
