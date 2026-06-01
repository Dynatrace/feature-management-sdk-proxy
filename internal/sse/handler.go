package sse

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/launchdarkly/eventsource"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/telemetry"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/tracing"
)

// HandlerPath is the proxy's SSE endpoint path.
const HandlerPath = "/sse"

// SubscriberTracker is the narrow interface the Handler needs from the Manager
// to gate and track downstream subscriber connections.
type SubscriberTracker interface {
	// AdmitSubscriber gates on per-key rate and fan-out cap; increments the
	// subscriber count on success. Returns (true, "") on admission, or
	// (false, reason) where reason is "rate_limit" or "fanout_cap".
	AdmitSubscriber(keyID string) (bool, string)
	// DecrementSubscribers decrements the subscriber count for keyID.
	// Called when a client disconnects.
	DecrementSubscribers(keyID string)
}

// Handler serves GET /sse. It authenticates the caller via the stream_id query
// parameter, gates on cache presence, and delegates to the eventsource.Server
// for downstream SSE streaming. The eventsource.Server handles fan-out of
// upstream events to all connected clients sharing the same SDK key channel.
//
// When constructed with a nil esServer, disabled is set to true and all
// requests return 501 Not Implemented.
type Handler struct {
	cache             *cache.Cache
	esServer          *eventsource.Server
	tracker           SubscriberTracker
	disabled          bool
	inst              *metrics.Instruments
	counter           *telemetry.Counter
	logger            *slog.Logger
	tracer            trace.Tracer
	heartbeatInterval time.Duration
	configPath        string
}

// NewHandler constructs a Handler. heartbeatInterval is the period between
// keepalive comments sent on idle connections (15s is typical for production).
// configPath is the HTTP path prefix at which the proxy serves flagd config
// (e.g. "/server/"); used in the 412 error hint so it stays in sync with the
// router registration. counter may be nil; counting is silently skipped.
func NewHandler(c *cache.Cache, esServer *eventsource.Server, tracker SubscriberTracker, inst *metrics.Instruments, counter *telemetry.Counter, logger *slog.Logger, heartbeatInterval time.Duration, configPath string) *Handler {
	return &Handler{
		cache:             c,
		esServer:          esServer,
		tracker:           tracker,
		disabled:          esServer == nil,
		inst:              inst,
		counter:           counter,
		logger:            logger,
		tracer:            tracenoop.NewTracerProvider().Tracer("sse"),
		heartbeatInterval: heartbeatInterval,
		configPath:        configPath,
	}
}

// SetTracer wires a TracerProvider's tracer into the Handler so client
// subscription setup emits sse.client.subscribe spans. Must be called before
// the first request is served.
func (h *Handler) SetTracer(t trace.Tracer) {
	h.tracer = t
}

// Handle is the native Gin handler for the SSE endpoint. It reads the public
// SDK key ID injected by FromStreamID middleware, gates on cache presence, and
// delegates to the eventsource.Server for downstream SSE streaming.
//
// Route registration should use:
//
//	router.GET(sse.HandlerPath, sdkkey.FromStreamID(), sseHandler.Handle)
func (h *Handler) Handle(c *gin.Context) {
	if h.disabled {
		h.logger.DebugContext(c.Request.Context(), "sse: SSE disabled — rejecting client connection with 501")
		c.String(http.StatusNotImplemented, "SSE is disabled\n")
		return
	}

	// keyID is pre-validated and injected by FromStreamID middleware.
	keyID, _ := sdkkey.PubID(c)

	// Create the setup span. W3C trace context was already extracted by the
	// TracePropagation middleware, so c.Request.Context() has the parent trace.
	ctx, setupSpan := h.tracer.Start(c.Request.Context(), "sse.client.subscribe",
		trace.WithAttributes(tracing.SDKKeyPublicID(keyID)))

	if _, ok := h.cache.Get(keyID); !ok {
		h.logger.WarnContext(ctx, "sse: client connected before config cached; returning 412",
			slog.String("sdk_key_public_id", keyID),
		)
		setupSpan.SetAttributes(attribute.String("outcome", "412_no_config"))
		setupSpan.End()
		c.String(http.StatusPreconditionFailed, "config not cached; fetch "+h.configPath+"{sdk_key}.json first\n")
		return
	}

	// Per-key isolation gate: enforces fan-out cap and subscribe rate limit.
	// Runs before the billing counter so rejected requests are not billed.
	if h.tracker != nil {
		if admitted, reason := h.tracker.AdmitSubscriber(keyID); !admitted {
			h.logger.WarnContext(ctx, "sse: subscriber rejected by per-key limit",
				slog.String("sdk_key_public_id", keyID),
				slog.String("reason", reason),
			)
			h.inst.SSESubscribeRejectedTotal.Add(context.Background(), 1, metric.WithAttributes(
				attribute.String("reason", reason),
			))
			setupSpan.SetAttributes(attribute.String("outcome", "429_"+reason))
			setupSpan.End()
			c.Header("Retry-After", "1")
			c.String(http.StatusTooManyRequests, "subscriber limit exceeded: "+reason+"\n")
			return
		}
		defer h.tracker.DecrementSubscribers(keyID)
	}

	// Count only accepted SSE subscription requests (after stream_id, cache, and isolation gates).
	if h.counter != nil {
		h.counter.IncrementByHash(keyID)
	}

	// Set additional headers before the eventsource handler writes the response.
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// Setup complete — end the span here so it doesn't span the full connection.
	setupSpan.SetAttributes(attribute.String("outcome", "200_connected"))
	setupSpan.End()

	h.inst.SSEConnections.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", keyID),
		attribute.String("direction", "downstream"),
		attribute.String("state", "connected"),
	))
	defer func() {
		h.inst.SSEConnections.Add(context.Background(), -1, metric.WithAttributes(
			attribute.String("sdk_key_public_id", keyID),
			attribute.String("direction", "downstream"),
			attribute.String("state", "connected"),
		))
	}()

	h.logger.Debug("sse: client connected", slog.String("sdk_key_public_id", keyID))

	// Start a heartbeat goroutine that publishes comments to this channel.
	// The eventsource.Server delivers the comment to all subscribers of keyID.
	heartbeatCtx, heartbeatCancel := context.WithCancel(c.Request.Context())
	defer heartbeatCancel()
	go h.heartbeatLoop(heartbeatCtx, keyID)

	// Delegate to the eventsource.Server handler. This blocks until the client
	// disconnects or the channel is unregistered with forceDisconnect.
	h.esServer.Handler(keyID)(c.Writer, c.Request)

	h.logger.Debug("sse: client disconnected", slog.String("sdk_key_public_id", keyID))
}

// heartbeatLoop periodically publishes a comment to the keyID channel to keep
// idle connections alive. Stops when ctx is done.
func (h *Handler) heartbeatLoop(ctx context.Context, keyID string) {
	ticker := time.NewTicker(h.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.esServer.PublishComment([]string{keyID}, "ping")
		}
	}
}
