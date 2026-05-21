package sse

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/launchdarkly/eventsource"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/tracing"
)

// conn manages a single upstream SSE connection for one SDK key using the
// LaunchDarkly eventsource client library. Reconnection with exponential
// backoff, Last-Event-ID resumption, and inactivity timeout are all handled
// by the eventsource.Stream internally.
type conn struct {
	sseURL             string
	inactivityDelaySec int
	keyID              string
	client             *http.Client
	logger             *slog.Logger
	inst               *metrics.Instruments
	tracer             trace.Tracer
	onEvent            func(Event)
	// onLifecycle is called for each received event before onEvent. When it
	// returns true the event is a lifecycle event and must not be forwarded
	// to SDK subscribers via onEvent. May be nil.
	onLifecycle func(Event) bool
	// setStreaming is called with true when the read loop begins and false when
	// it ends (including on error or backoff). Used by Manager.RefreshInterval
	// to signal that SSE is actively delivering events. May be nil.
	setStreaming func(bool)

	// streaming tracks whether we are currently in the active read loop.
	// Used to flip setStreaming only on transitions.
	streaming atomic.Bool

	// connectSpanCtx holds the SpanContext from the sse.upstream.connect setup
	// span, captured after the span ends. Used to link sse.event.forward spans
	// back to the originating connection attempt.
	// Written once in run() before the event loop begins; read only from within
	// the same goroutine via the onEvent closure. No concurrent access.
	connectSpanCtx trace.SpanContext

	// currentHealth tracks the health state currently reflected in SSEConnectionHealth.
	// 0=none (not yet started or already exited), 1=unhealthy, 2=healthy.
	currentHealth atomic.Int32
}

const (
	connHealthNone      int32 = 0
	connHealthUnhealthy int32 = 1
	connHealthHealthy   int32 = 2
)

func connHealthLabel(s int32) string {
	if s == connHealthHealthy {
		return "healthy"
	}
	return "unhealthy"
}

// setHealth transitions to the given health state, updating SSEConnections{direction=upstream}.
func (c *conn) setHealth(next int32) {
	prev := c.currentHealth.Swap(next)
	if prev == next {
		return
	}
	bg := context.Background()
	if prev != connHealthNone {
		c.inst.SSEConnections.Add(bg, -1, metric.WithAttributes(
			attribute.String("sdk_key_public_id", c.keyID),
			attribute.String("direction", "upstream"),
			attribute.String("state", connHealthLabel(prev)),
		))
	}
	c.inst.SSEConnections.Add(bg, 1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", c.keyID),
		attribute.String("direction", "upstream"),
		attribute.String("state", connHealthLabel(next)),
	))
}

// clearHealth decrements the current upstream health bucket and resets to none.
// Called on conn.run exit to balance every +1 emitted during the connection lifetime.
func (c *conn) clearHealth() {
	prev := c.currentHealth.Swap(connHealthNone)
	if prev == connHealthNone {
		return
	}
	c.inst.SSEConnections.Add(context.Background(), -1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", c.keyID),
		attribute.String("direction", "upstream"),
		attribute.String("state", connHealthLabel(prev)),
	))
}

// run is the main loop. It creates an eventsource.Stream that handles
// reconnection internally, and reads events until ctx is cancelled.
func (c *conn) run(ctx context.Context) {
	c.setHealth(connHealthUnhealthy)
	defer c.clearHealth()

	_, setupSpan := c.tracer.Start(ctx, "sse.upstream.connect",
		trace.WithAttributes(tracing.SDKKeyPublicID(c.keyID)))

	readTimeout := time.Duration(c.inactivityDelaySec) * time.Second
	if readTimeout <= 0 {
		readTimeout = defaultInactivityTimeout
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sseURL, nil)
	if err != nil {
		c.logger.Error("sse: failed to build request",
			slog.String("sdk_key_public_id", c.keyID),
			slog.String("error", err.Error()),
		)
		setupSpan.RecordError(err)
		setupSpan.End()
		return
	}
	req.Header.Set("Cache-Control", "no-cache")

	stream, err := eventsource.SubscribeWithRequestAndOptions(req,
		eventsource.StreamOptionHTTPClient(c.client),
		eventsource.StreamOptionInitialRetry(time.Second),
		eventsource.StreamOptionUseBackoff(60*time.Second),
		eventsource.StreamOptionRetryResetInterval(30*time.Second),
		eventsource.StreamOptionReadTimeout(readTimeout),
		eventsource.StreamOptionCanRetryFirstConnection(30*time.Second),
		eventsource.StreamOptionErrorHandler(c.makeErrorHandler(ctx)),
	)
	if err != nil {
		setupSpan.SetAttributes(attribute.String("outcome", "subscribe_failed"))
		setupSpan.RecordError(err)
		setupSpan.End()
		c.logger.Warn("sse: upstream subscribe failed",
			slog.String("sdk_key_public_id", c.keyID),
			slog.String("error", err.Error()),
		)
		return
	}

	setupSpan.SetAttributes(attribute.String("outcome", "connected"))
	setupSpan.End()
	c.connectSpanCtx = setupSpan.SpanContext()

	// Close the stream when the context is cancelled.
	go func() {
		<-ctx.Done()
		stream.Close()
	}()

	c.markStreaming(true)

	for ev := range stream.Events {
		// Ensure streaming is marked true after any reconnection.
		if !c.streaming.Load() {
			c.markStreaming(true)
		}

		evt := Event{
			ID:        ev.Id(),
			EventType: ev.Event(),
			Data:      []byte(ev.Data()),
		}
		c.logger.Debug("sse: event received",
			slog.String("key", c.keyID),
			slog.String("event_type", evt.EventType),
			slog.String("id", evt.ID),
			slog.Int("data_len", len(evt.Data)),
		)
		if c.onLifecycle != nil && c.onLifecycle(evt) {
			continue
		}
		c.onEvent(evt)
	}

	c.markStreaming(false)
}

// makeErrorHandler returns a StreamErrorHandler that records reconnect metrics
// and allows the stream to be cancelled via context.
func (c *conn) makeErrorHandler(ctx context.Context) eventsource.StreamErrorHandler {
	return func(err error) eventsource.StreamErrorHandlerResult {
		// If context is done, signal the stream to close permanently.
		if ctx.Err() != nil {
			return eventsource.StreamErrorHandlerResult{CloseNow: true}
		}

		// Mark as not streaming during reconnection backoff.
		c.markStreaming(false)

		c.logger.Warn("sse: upstream connection lost, reconnecting",
			slog.String("sdk_key_public_id", c.keyID),
			slog.String("reason", classifyError(err)),
		)
		return eventsource.StreamErrorHandlerResult{}
	}
}

// markStreaming transitions the streaming state, invokes the callback, and
// updates the SSEConnectionHealth metric. Only fires on actual transitions.
func (c *conn) markStreaming(v bool) {
	if c.streaming.Swap(v) == v {
		return
	}
	if c.setStreaming != nil {
		c.setStreaming(v)
	}
	if v {
		c.setHealth(connHealthHealthy)
	} else {
		c.setHealth(connHealthUnhealthy)
	}
}

// classifyError maps an eventsource error to a metric label.
func classifyError(err error) string {
	if err == nil {
		return "unknown"
	}
	switch {
	case errors.Is(err, eventsource.ErrReadTimeout):
		return "inactivity"
	default:
		var se eventsource.SubscriptionError
		if errors.As(err, &se) {
			if se.Code >= 400 {
				return "http_error"
			}
		}
		return "io_error"
	}
}

const defaultInactivityTimeout = 120 * time.Second
