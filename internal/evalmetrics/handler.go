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

	// staticUpstreamURL, when non-empty, switches Handle into offline mode: the
	// request is forwarded to this exact URL, skipping SDK-key geo derivation and
	// the full-key / server-platform checks (an offline flagSetId carries no geo).
	// The incoming Authorization header and body are still forwarded verbatim.
	// Set in CONFIG_SOURCE=static mode.
	staticUpstreamURL string
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

// NewStatic constructs a Handler for CONFIG_SOURCE=static mode. Every POST
// /v1/metrics is forwarded to upstreamURL verbatim (Authorization header and
// body unchanged); no geo derivation or SDK-key format checks are applied,
// because an offline flagSetId carries no geo segment. upstreamURL must be a
// full absolute URL (validated by config.FromEnv).
func NewStatic(upstreamURL string, client *http.Client, inst *metrics.Instruments, logger *slog.Logger, t trace.Tracer) (*Handler, error) {
	if upstreamURL == "" {
		return nil, errors.New("evalmetrics: static upstream URL must not be empty")
	}
	return &Handler{
		client:            client,
		inst:              inst,
		logger:            logger,
		tracer:            t,
		maxRequestBytes:   defaultMaxRequestBytes,
		staticUpstreamURL: upstreamURL,
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

	if h.staticUpstreamURL != "" {
		h.handleStatic(ctx, c, span, start)
		return
	}

	// key is pre-extracted and injected by FromBearerToken middleware.
	key, _ := sdkkey.KeyFromContext(c)
	keyID := key.PublicID()
	span.SetAttributes(tracing.SDKKeyPublicID(keyID))

	if !key.IsFull() {
		h.logger.WarnContext(ctx, "evalmetrics: unrecognized SDK key format",
			slog.String("sdk_key_public_id", keyID),
		)
		span.SetAttributes(attribute.String("result", resultBadRequest))
		h.record(ctx, resultBadRequest, start)
		c.String(http.StatusBadRequest, "unrecognized SDK key format\n")
		return
	}

	if key.Platform() != sdkkey.PlatformServer {
		h.logger.WarnContext(ctx, "evalmetrics: non-server SDK key rejected",
			slog.String("sdk_key_public_id", keyID),
			slog.String("platform", key.Platform()),
		)
		span.SetAttributes(attribute.String("result", resultUnauthorized))
		h.record(ctx, resultUnauthorized, start)
		c.String(http.StatusUnauthorized, "SDK key platform not authorized for /v1/metrics\n")
		return
	}

	target := fmt.Sprintf("https://metrics.%s.%s/v1/metrics", key.Geo(), h.metricsBaseDomain)
	h.forward(ctx, c, span, start, target, "Bearer "+key.Raw(), keyID)
}

// handleStatic serves POST /v1/metrics in CONFIG_SOURCE=static mode. It skips
// all SDK-key parsing and forwards the request to the configured upstream URL
// with the incoming Authorization header verbatim. A missing Authorization
// header is rejected so anonymous traffic is never forwarded upstream.
func (h *Handler) handleStatic(ctx context.Context, c *gin.Context, span trace.Span, start time.Time) {
	authorization := c.GetHeader("Authorization")
	if authorization == "" {
		span.SetAttributes(attribute.String("result", resultUnauthorized))
		h.record(ctx, resultUnauthorized, start)
		c.String(http.StatusUnauthorized, "Authorization header required\n")
		return
	}
	h.forward(ctx, c, span, start, h.staticUpstreamURL, authorization, "")
}

// forward reads the request body (capped at maxRequestBytes), POSTs it to target
// with the given Authorization header, forwards the caller's other headers, and
// writes the upstream response back verbatim. keyIDForLog is the SDK key public
// id for structured log fields, or "" when unavailable (static mode).
func (h *Handler) forward(ctx context.Context, c *gin.Context, span trace.Span, start time.Time, target, authorization, keyIDForLog string) {
	if c.Request.ContentLength > h.maxRequestBytes {
		span.SetAttributes(attribute.String("result", resultPayloadTooLarge))
		h.record(ctx, resultPayloadTooLarge, start)
		c.String(http.StatusRequestEntityTooLarge, "request body too large\n")
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, h.maxRequestBytes+1))
	if err != nil {
		h.logger.ErrorContext(ctx, "evalmetrics: read request body failed",
			slog.String("sdk_key_public_id", keyIDForLog),
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

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		h.logger.ErrorContext(ctx, "evalmetrics: build upstream request failed",
			slog.String("sdk_key_public_id", keyIDForLog),
			slog.String("error", err.Error()),
		)
		span.RecordError(err)
		span.SetAttributes(attribute.String("result", resultInternalError))
		h.record(ctx, resultInternalError, start)
		c.String(http.StatusInternalServerError, "internal error\n")
		return
	}

	upstreamReq.Header.Set("Authorization", authorization)
	if ct := c.GetHeader("Content-Type"); ct != "" {
		upstreamReq.Header.Set("Content-Type", ct)
	}
	copyHeader(upstreamReq.Header, c.Request.Header)

	resp, err := h.client.Do(upstreamReq)
	if err != nil {
		sanitized := sanitizeErr(err)
		h.logger.WarnContext(ctx, "evalmetrics: upstream request failed",
			slog.String("sdk_key_public_id", keyIDForLog),
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
