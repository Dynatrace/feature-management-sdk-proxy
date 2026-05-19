package sse

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/launchdarkly/eventsource"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/telemetry"
	"github.com/dynatrace/sdk-proxy/internal/tracing"
)

// HandlerPath is the proxy's SSE endpoint path.
const HandlerPath = "/sse"

// SubscriberTracker is the narrow interface the Handler needs from the Manager
// to track downstream subscriber counts (for HasSubscribers / cache liveness).
type SubscriberTracker interface {
	IncrementSubscribers(keyID string)
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
// mux registration. counter may be nil; counting is silently skipped.
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

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.disabled {
		h.logger.Debug("sse: SSE disabled — rejecting client connection with 501")
		http.Error(w, "SSE is disabled", http.StatusNotImplemented)
		return
	}

	streamID := r.URL.Query().Get("stream_id")
	if streamID == "" {
		http.Error(w, "missing stream_id query parameter", http.StatusBadRequest)
		return
	}

	if !cache.IsValidID(streamID) {
		h.logger.Warn("sse: invalid stream_id format",
			slog.Int("len", len(streamID)),
		)
		http.Error(w, "invalid stream_id: must be a valid SDK key public prefix", http.StatusBadRequest)
		return
	}

	keyID := streamID

	// Extract W3C trace context and span the setup phase only — SSE connections
	// can last hours and Dynatrace bins spans by completion time. Active connection
	// duration is covered by the SSEClientConnections gauge metric.
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	_, setupSpan := h.tracer.Start(ctx, "sse.client.subscribe",
		trace.WithAttributes(tracing.SDKKeyPublicID(keyID)))

	if _, ok := h.cache.Get(keyID); !ok {
		h.logger.Warn("sse: client connected before config cached; returning 412",
			slog.String("key", keyID),
		)
		setupSpan.SetAttributes(attribute.String("outcome", "412_no_config"))
		setupSpan.End()
		http.Error(w, "config not cached; fetch "+h.configPath+"{sdk_key}.json first", http.StatusPreconditionFailed)
		return
	}

	// Count only accepted SSE subscription requests (after stream_id and cache gate).
	if h.counter != nil {
		h.counter.IncrementByHash(keyID)
	}

	// Set additional headers before the eventsource handler writes the response.
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	// Setup complete — end the span here so it doesn't span the full connection.
	setupSpan.SetAttributes(attribute.String("outcome", "200_connected"))
	setupSpan.End()

	h.inst.SSEClientConnections.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", keyID),
	))
	if h.tracker != nil {
		h.tracker.IncrementSubscribers(keyID)
	}
	defer func() {
		h.inst.SSEClientConnections.Add(context.Background(), -1, metric.WithAttributes(
			attribute.String("sdk_key_public_id", keyID),
		))
		if h.tracker != nil {
			h.tracker.DecrementSubscribers(keyID)
		}
	}()

	h.logger.Debug("sse: client connected", slog.String("key", keyID))

	// Start a heartbeat goroutine that publishes comments to this channel.
	// The eventsource.Server delivers the comment to all subscribers of keyID.
	heartbeatCtx, heartbeatCancel := context.WithCancel(r.Context())
	defer heartbeatCancel()
	go h.heartbeatLoop(heartbeatCtx, keyID)

	// Delegate to the eventsource.Server handler. This blocks until the client
	// disconnects or the channel is unregistered with forceDisconnect.
	h.esServer.Handler(keyID)(w, r)

	h.logger.Debug("sse: client disconnected", slog.String("key", keyID))
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
