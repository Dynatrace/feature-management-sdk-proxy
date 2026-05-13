package sse

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/tracing"
)

const (
	defaultInactivitySec = 120
	maxEventDataBytes    = 1 << 20 // 1 MiB — guards against OOM from unbounded upstream frames
)

// errInactivity is returned by readLoop when the inactivity watchdog fires.
var errInactivity = errors.New("sse: inactivity timeout")

// conn manages a single upstream SSE connection for one SDK key. It reconnects
// with exponential backoff until its context is cancelled.
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
}

// run is the reconnect loop. It blocks until ctx is cancelled.
func (c *conn) run(ctx context.Context) {
	c.inst.SSEConnections.Add(ctx, 1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", c.keyID),
	))
	defer c.inst.SSEConnections.Add(context.Background(), -1, metric.WithAttributes(
		attribute.String("sdk_key_public_id", c.keyID),
	))

	b := newBackoff()
	lastEventID := ""
	connectedAt := time.Now()

	for {
		// One span per connect attempt — bounded by the attempt lifetime, not the key lifetime.
		attemptCtx, attemptSpan := c.tracer.Start(ctx, "sse.upstream.connect",
			trace.WithAttributes(tracing.SDKKeyPublicID(c.keyID)))

		err := c.connect(attemptCtx, lastEventID, func(evt Event) {
			if evt.ID != "" {
				lastEventID = evt.ID
			}
			if c.onLifecycle != nil && c.onLifecycle(evt) {
				return
			}
			c.onEvent(evt)
		})

		if ctx.Err() != nil {
			attemptSpan.SetAttributes(attribute.String("reconnect.reason", "cancelled"))
			attemptSpan.End()
			return
		}

		// Reset backoff when the connection was stable for a while.
		if time.Since(connectedAt) > 30*time.Second {
			b.reset()
		}
		connectedAt = time.Now()

		reason := reconnectReason(err)
		attemptSpan.SetAttributes(
			attribute.String("reconnect.reason", reason),
			attribute.Bool("streaming", c.setStreaming != nil), // true when streaming was active
		)
		if err != nil && !errors.Is(err, io.EOF) {
			attemptSpan.RecordError(err)
		}
		attemptSpan.End()

		c.inst.SSEUpstreamReconnects.Add(ctx, 1, metric.WithAttributes(
			attribute.String("sdk_key_public_id", c.keyID),
			attribute.String("reason", reason),
		))
		c.logger.Warn("sse: upstream connection lost, will reconnect",
			slog.String("key", c.keyID),
			slog.String("reason", reason),
		)

		delay := b.next()
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// connect opens one upstream SSE connection, reads until EOF or error, and
// invokes onEvent for each complete SSE frame.
func (c *conn) connect(ctx context.Context, lastEventID string, onEvent func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sseURL, nil)
	if err != nil {
		return fmt.Errorf("sse: build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("sse: connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sse: upstream status %d", resp.StatusCode)
	}

	if c.setStreaming != nil {
		c.setStreaming(true)
		defer c.setStreaming(false)
	}
	return c.readLoop(ctx, resp.Body, onEvent)
}

// readLoop reads SSE frames from body, dispatching each complete event to
// onEvent. It enforces an inactivity deadline: if no bytes arrive within
// inactivityDelaySec, the body is closed and readLoop returns errInactivity.
func (c *conn) readLoop(ctx context.Context, body io.ReadCloser, onEvent func(Event)) error {
	deadline := time.Duration(c.inactivityDelaySec) * time.Second
	if deadline <= 0 {
		deadline = defaultInactivitySec * time.Second
	}

	tb := newTimedBody(body, deadline)
	defer tb.stop()

	scanner := bufio.NewScanner(tb)

	var (
		eventType string
		data      strings.Builder
		eventID   string
	)

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			if data.Len() > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					onEvent(Event{
						ID:        eventID,
						EventType: eventType,
						Data:      []byte(strings.Clone(data.String())),
					})
				}
			}
			eventType = ""
			data.Reset()
			eventID = ""

		case strings.HasPrefix(line, "id:"):
			eventID = strings.TrimPrefix(strings.TrimPrefix(line, "id:"), " ")

		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " ")

		case strings.HasPrefix(line, "data:"):
			val := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if data.Len()+len(val) > maxEventDataBytes {
				data.Reset()
				return fmt.Errorf("sse: event data exceeds %d-byte limit", maxEventDataBytes)
			}
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(val)

		case strings.HasPrefix(line, ":"):
			// comment — keepalive; timer already reset by timedBody on each read
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if tb.timedOut() {
		return errInactivity
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.EOF
}

// timedBody wraps an io.ReadCloser and closes it after a period of inactivity.
// Each successful Read resets the deadline.
type timedBody struct {
	body  io.ReadCloser
	timer *time.Timer
	timed atomic.Bool
	d     time.Duration
}

func newTimedBody(body io.ReadCloser, d time.Duration) *timedBody {
	tb := &timedBody{body: body, d: d}
	tb.timer = time.AfterFunc(d, func() {
		tb.timed.Store(true)
		body.Close()
	})
	return tb
}

func (tb *timedBody) Read(p []byte) (int, error) {
	n, err := tb.body.Read(p)
	if n > 0 {
		tb.timer.Reset(tb.d)
	}
	return n, err
}

func (tb *timedBody) timedOut() bool { return tb.timed.Load() }

func (tb *timedBody) stop() { tb.timer.Stop() }

// reconnectReason maps a connection error to a metric label.
func reconnectReason(err error) string {
	switch {
	case errors.Is(err, errInactivity):
		return "inactivity"
	case errors.Is(err, io.EOF):
		return "eof"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	default:
		return "io_error"
	}
}

// exponentialBackoff is a simple capped exponential backoff with no external deps.
type exponentialBackoff struct {
	current time.Duration
	max     time.Duration
}

func newBackoff() *exponentialBackoff {
	return &exponentialBackoff{current: time.Second, max: 60 * time.Second}
}

func (b *exponentialBackoff) next() time.Duration {
	d := b.current
	b.current *= 2
	if b.current > b.max {
		b.current = b.max
	}
	return d
}

func (b *exponentialBackoff) reset() {
	b.current = time.Second
}
