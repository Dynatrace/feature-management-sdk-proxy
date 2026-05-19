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

	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/tracing"
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
}

// run is the main loop. It creates an eventsource.Stream that handles
// reconnection internally, and reads events until ctx is cancelled.
func (c *conn) run(ctx context.Context) {
	c.inst.SSEConnections.Add(ctx, 1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", c.keyID),
	))
	defer c.inst.SSEConnections.Add(context.Background(), -1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", c.keyID),
	))

	_, setupSpan := c.tracer.Start(ctx, "sse.upstream.connect",
		trace.WithAttributes(tracing.SDKKeyPublicID(c.keyID)))

	readTimeout := time.Duration(c.inactivityDelaySec) * time.Second
	if readTimeout <= 0 {
		readTimeout = defaultInactivityTimeout
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sseURL, nil)
	if err != nil {
		c.logger.Error("sse: failed to build request", slog.String("key", c.keyID), slog.String("error", err.Error()))
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
			slog.String("key", c.keyID),
			slog.String("error", err.Error()),
		)
		return
	}

	setupSpan.SetAttributes(attribute.String("outcome", "connected"))
	setupSpan.End()

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

		reason := classifyError(err)
		c.inst.SSEUpstreamReconnects.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("sdk_key_public_id", c.keyID),
			attribute.String("reason", reason),
		))
		c.logger.Warn("sse: upstream connection lost, reconnecting",
			slog.String("key", c.keyID),
			slog.String("reason", reason),
		)
		return eventsource.StreamErrorHandlerResult{}
	}
}

// markStreaming transitions the streaming state and invokes the callback.
func (c *conn) markStreaming(v bool) {
	if c.streaming.Swap(v) != v && c.setStreaming != nil {
		c.setStreaming(v)
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
