package sse

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

var testConnTracer = tracenoop.NewTracerProvider().Tracer("test")

// sseServer streams the given frames to each connected client and then closes
// the connection. Each frame is a complete SSE message (already formatted).
func sseServer(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			fmt.Fprint(w, f)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

// TestConn_ParsesBasicEvent ensures a simple data-only event is dispatched.
func TestConn_ParsesBasicEvent(t *testing.T) {
	srv := sseServer(t, []string{"data: hello\n\n"})
	defer srv.Close()

	events := collectEvents(t, srv.URL, 1, 5*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if string(events[0].Data) != "hello" {
		t.Errorf("data = %q, want %q", events[0].Data, "hello")
	}
}

// TestConn_ParsesEventTypeAndID ensures id, event, and data fields are parsed.
func TestConn_ParsesEventTypeAndID(t *testing.T) {
	frame := "id: 42\nevent: refetchConfig\ndata: {\"key\":\"val\"}\n\n"
	srv := sseServer(t, []string{frame})
	defer srv.Close()

	events := collectEvents(t, srv.URL, 1, 5*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	evt := events[0]
	if evt.ID != "42" {
		t.Errorf("ID = %q, want %q", evt.ID, "42")
	}
	if evt.EventType != "refetchConfig" {
		t.Errorf("EventType = %q, want %q", evt.EventType, "refetchConfig")
	}
	if string(evt.Data) != `{"key":"val"}` {
		t.Errorf("Data = %q, want %q", evt.Data, `{"key":"val"}`)
	}
}

// TestConn_MultilineData ensures multi-line data fields are joined with newlines.
func TestConn_MultilineData(t *testing.T) {
	frame := "data: line1\ndata: line2\n\n"
	srv := sseServer(t, []string{frame})
	defer srv.Close()

	events := collectEvents(t, srv.URL, 1, 5*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if string(events[0].Data) != "line1\nline2" {
		t.Errorf("Data = %q, want %q", events[0].Data, "line1\nline2")
	}
}

// TestConn_CommentsIgnored ensures comment lines (: ...) are ignored.
func TestConn_CommentsIgnored(t *testing.T) {
	frames := []string{": keepalive\ndata: real\n\n"}
	srv := sseServer(t, frames)
	defer srv.Close()

	events := collectEvents(t, srv.URL, 1, 5*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if string(events[0].Data) != "real" {
		t.Errorf("Data = %q, want %q", events[0].Data, "real")
	}
}

// TestConn_MultipleEvents ensures multiple events from one connection are all delivered.
func TestConn_MultipleEvents(t *testing.T) {
	frames := []string{
		"data: first\n\n",
		"data: second\n\n",
		"data: third\n\n",
	}
	srv := sseServer(t, frames)
	defer srv.Close()

	events := collectEvents(t, srv.URL, 3, 5*time.Second)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, want := range []string{"first", "second", "third"} {
		if string(events[i].Data) != want {
			t.Errorf("event[%d].Data = %q, want %q", i, events[i].Data, want)
		}
	}
}

// TestConn_ReconnectsOnEOF verifies that the conn goroutine reconnects when
// the server closes the connection (via the eventsource library's internal reconnect).
func TestConn_ReconnectsOnEOF(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: ping\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var got atomic.Int64
	c := &conn{
		sseURL:             srv.URL,
		inactivityDelaySec: 60,
		keyID:              "testhash",
		client:             srv.Client(),
		logger:             slog.Default(),
		inst:               newTestInstruments(t),
		tracer:             testConnTracer,
		onEvent: func(evt Event) {
			if got.Add(1) >= 2 {
				cancel() // stop after receiving events from 2 reconnects
			}
		},
		setStreaming: func(bool) {},
	}

	c.run(ctx)

	if n := got.Load(); n < 2 {
		t.Errorf("got %d events, want at least 2 (reconnect happened)", n)
	}
	if n := connCount.Load(); n < 2 {
		t.Errorf("server received %d connections, want at least 2", n)
	}
}

// TestConn_StopsOnContextCancel verifies the conn exits promptly when ctx is done.
func TestConn_StopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Keep connection open indefinitely
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	c := &conn{
		sseURL:             srv.URL,
		inactivityDelaySec: 60,
		keyID:              "testhash",
		client:             srv.Client(),
		logger:             slog.Default(),
		inst:               newTestInstruments(t),
		tracer:             testConnTracer,
		onEvent:            func(Event) {},
		setStreaming:       func(bool) {},
	}
	c.run(ctx)

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("run took %v, want < 3s after cancel", elapsed)
	}
}

// TestConn_InactivityTimeout verifies that the eventsource ReadTimeout fires
// and the stream reconnects when no data arrives.
func TestConn_InactivityTimeout(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Send nothing — read timeout should fire and cause reconnection.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := &conn{
		sseURL:             srv.URL,
		inactivityDelaySec: 1, // 1-second read timeout
		keyID:              "testhash",
		client:             srv.Client(),
		logger:             slog.Default(),
		inst:               newTestInstruments(t),
		tracer:             testConnTracer,
		onEvent:            func(Event) {},
		setStreaming:       func(bool) {},
	}

	go func() {
		// Cancel once we've seen at least two connections (one reconnect).
		for {
			if connCount.Load() >= 2 {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	c.run(ctx)

	if n := connCount.Load(); n < 2 {
		t.Errorf("server received %d connections, want >= 2 (inactivity triggered reconnect)", n)
	}
}

// collectEvents opens a conn to url and collects up to want events within timeout.
func collectEvents(t *testing.T, url string, want int, timeout time.Duration) []Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var (
		events []Event
		mu     sync.Mutex
	)
	c := &conn{
		sseURL:             url,
		inactivityDelaySec: 60,
		keyID:              "testhash",
		client:             http.DefaultClient,
		logger:             slog.Default(),
		inst:               newTestInstruments(t),
		tracer:             testConnTracer,
		onEvent: func(evt Event) {
			mu.Lock()
			events = append(events, evt)
			count := len(events)
			mu.Unlock()
			if count >= want {
				cancel()
			}
		},
		setStreaming: func(bool) {},
	}
	c.run(ctx)
	mu.Lock()
	defer mu.Unlock()
	return events
}
