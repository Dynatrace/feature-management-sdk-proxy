package sse

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/telemetry"
)

// HandlerPath is the proxy's SSE endpoint path.
const HandlerPath = "/sse"

// defaultEventType is the SSE spec default when no event: field is present.
const defaultEventType = "message"

// Subscriber is the narrow interface the Handler needs from the SSE Manager.
type Subscriber interface {
	Subscribe(keyHash string) (<-chan Event, func())
}

// Handler serves GET /sse. It authenticates the caller via the sdk_key query
// parameter, gates on cache presence, and fans out upstream events to all
// connected clients sharing the same SDK key.
type Handler struct {
	cache             *cache.Cache
	manager           Subscriber
	inst              *metrics.Instruments
	counter           *telemetry.Counter
	logger            *slog.Logger
	heartbeatInterval time.Duration
	configPath        string
}

// NewHandler constructs a Handler. heartbeatInterval is the period between
// keepalive comments sent on idle connections (15s is typical for production).
// configPath is the HTTP path prefix at which the proxy serves flagd config
// (e.g. "/server/"); used in the 412 error hint so it stays in sync with the
// mux registration. counter may be nil; counting is silently skipped.
func NewHandler(c *cache.Cache, m Subscriber, inst *metrics.Instruments, counter *telemetry.Counter, logger *slog.Logger, heartbeatInterval time.Duration, configPath string) *Handler {
	return &Handler{
		cache:             c,
		manager:           m,
		inst:              inst,
		counter:           counter,
		logger:            logger,
		heartbeatInterval: heartbeatInterval,
		configPath:        configPath,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	streamID := r.URL.Query().Get("stream_id")
	if streamID == "" {
		http.Error(w, "missing stream_id query parameter", http.StatusBadRequest)
		return
	}

	if !isValidStreamID(streamID) {
		h.logger.Warn("sse: invalid stream_id format",
			slog.Int("len", len(streamID)),
		)
		http.Error(w, "invalid stream_id: must be 16 lowercase hex characters", http.StatusBadRequest)
		return
	}

	keyHash := streamID

	if _, ok := h.cache.Get(keyHash); !ok {
		h.logger.Warn("sse: client connected before config cached; returning 412",
			slog.String("key", keyHash),
		)
		http.Error(w, "config not cached; fetch "+h.configPath+"{sdk_key}.json first", http.StatusPreconditionFailed)
		return
	}

	// Count only accepted SSE subscription requests (after stream_id and cache gate).
	// Rejected requests (412 no config, invalid stream_id) are not counted —
	// they never establish a connection and consume no ongoing proxy resources.
	if h.counter != nil {
		h.counter.IncrementByHash(keyHash)
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	h.inst.SSEClientConnections.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("sdk_key_hash", keyHash),
	))
	defer h.inst.SSEClientConnections.Add(context.Background(), -1, metric.WithAttributes(
		attribute.String("sdk_key_hash", keyHash),
	))

	ch, unsub := h.manager.Subscribe(keyHash)
	defer unsub()

	h.logger.Debug("sse: client connected", slog.String("key", keyHash))

	heartbeat := time.NewTicker(h.heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			h.logger.Debug("sse: client disconnected", slog.String("key", keyHash))
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, evt); err != nil {
				return
			}
			flusher.Flush()
			h.inst.SSEEventsDelivered.Add(context.Background(), 1, metric.WithAttributes(
				attribute.String("sdk_key_hash", keyHash),
			))
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, evt Event) error {
	if evt.ID != "" {
		if _, err := fmt.Fprintf(w, "id: %s\n", evt.ID); err != nil {
			return err
		}
	}
	evtType := evt.EventType
	if evtType == "" {
		evtType = defaultEventType
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", evtType); err != nil {
		return err
	}
	// Upstream may produce multi-line data (conn.go joins lines with \n).
	// Emit one data: line per segment per the SSE spec.
	for line := range bytes.SplitSeq(evt.Data, []byte{'\n'}) {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\n")
	return err
}

// isValidStreamID returns true if s is exactly 16 lowercase hex characters —
// the format produced by cache.Key(sdkKey).
func isValidStreamID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
