package sse

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
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

	sdkKey := r.URL.Query().Get("sdk_key")
	if sdkKey == "" {
		http.Error(w, "missing sdk_key query parameter", http.StatusBadRequest)
		return
	}

	if _, ok := validateSDKKeyRegion(sdkKey); !ok {
		h.logger.Warn("sse: unrecognized SDK key format", slog.String("key", cache.Key(sdkKey)))
		http.Error(w, "unrecognized SDK key format: must be dt_server_{region}_{token}", http.StatusBadRequest)
		return
	}

	keyHash := cache.Key(sdkKey)

	if _, ok := h.cache.Get(keyHash); !ok {
		h.logger.Warn("sse: client connected before config cached; returning 412",
			slog.String("key", keyHash),
		)
		http.Error(w, "config not cached; fetch "+h.configPath+"{sdk_key}.json first", http.StatusPreconditionFailed)
		return
	}

	// Count only accepted SSE subscription requests (after key and cache gate).
	// Rejected requests (412 no config, invalid key format) are not counted —
	// they never establish a connection and consume no ongoing proxy resources.
	if h.counter != nil {
		h.counter.Increment(sdkKey)
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
		case evt := <-ch:
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

// validateSDKKeyRegion parses the region segment from an SDK key in the format
// dt_server_{region}_{token}. Returns ("", false) for any other format.
// Region is restricted to [a-z0-9-] to prevent SSRF via crafted CDN hostnames.
//
// This is a local copy of proxy.extractRegion. Both should be consolidated into
// internal/sdkkey once this package is stable.
func validateSDKKeyRegion(sdkKey string) (string, bool) {
	parts := strings.SplitN(sdkKey, "_", 4)
	if len(parts) != 4 || parts[0] != "dt" || parts[1] != "server" || parts[2] == "" || parts[3] == "" {
		return "", false
	}
	region := parts[2]
	for _, c := range region {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return "", false
		}
	}
	return region, true
}
