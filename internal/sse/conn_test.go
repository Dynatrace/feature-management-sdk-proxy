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
)

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

// TestReadLoop_ParsesBasicEvent ensures a simple data-only event is dispatched.
func TestReadLoop_ParsesBasicEvent(t *testing.T) {
	srv := sseServer(t, []string{"data: hello\n\n"})
	defer srv.Close()

	events := collectEvents(t, srv.URL, 1, 3*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if string(events[0].Data) != "hello" {
		t.Errorf("data = %q, want %q", events[0].Data, "hello")
	}
}

// TestReadLoop_ParsesEventTypeAndID ensures id, event, and data fields are parsed.
func TestReadLoop_ParsesEventTypeAndID(t *testing.T) {
	frame := "id: 42\nevent: refetchConfig\ndata: {\"key\":\"val\"}\n\n"
	srv := sseServer(t, []string{frame})
	defer srv.Close()

	events := collectEvents(t, srv.URL, 1, 3*time.Second)
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

// TestReadLoop_MultilineData ensures multi-line data fields are joined with newlines.
func TestReadLoop_MultilineData(t *testing.T) {
	frame := "data: line1\ndata: line2\n\n"
	srv := sseServer(t, []string{frame})
	defer srv.Close()

	events := collectEvents(t, srv.URL, 1, 3*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if string(events[0].Data) != "line1\nline2" {
		t.Errorf("Data = %q, want %q", events[0].Data, "line1\nline2")
	}
}

// TestReadLoop_CommentsIgnored ensures comment lines (: ...) are ignored.
func TestReadLoop_CommentsIgnored(t *testing.T) {
	frames := []string{": keepalive\n", "data: real\n\n"}
	srv := sseServer(t, frames)
	defer srv.Close()

	events := collectEvents(t, srv.URL, 1, 3*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	if string(events[0].Data) != "real" {
		t.Errorf("Data = %q, want %q", events[0].Data, "real")
	}
}

// TestReadLoop_EmptyDataIgnored ensures a blank-line dispatch with no data is a no-op.
func TestReadLoop_EmptyDataIgnored(t *testing.T) {
	// Two dispatches: first has no data (skipped), second has data.
	frames := []string{"\ndata: hello\n\n"}
	srv := sseServer(t, frames)
	defer srv.Close()

	events := collectEvents(t, srv.URL, 1, 3*time.Second)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
}

// TestReadLoop_MultipleEvents ensures multiple events from one connection are all delivered.
func TestReadLoop_MultipleEvents(t *testing.T) {
	frames := []string{
		"data: first\n\n",
		"data: second\n\n",
		"data: third\n\n",
	}
	srv := sseServer(t, frames)
	defer srv.Close()

	events := collectEvents(t, srv.URL, 3, 3*time.Second)
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
// the server closes the connection.
func TestConn_ReconnectsOnEOF(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: ping\n\n")
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var got atomic.Int64
	c := &conn{
		sseURL:             srv.URL,
		inactivityDelaySec: 60,
		keyHash:            "testhash",
		client:             srv.Client(),
		logger:             slog.Default(),
		inst:               newTestInstruments(t),
		onEvent: func(evt Event) {
			got.Add(1)
			if got.Load() >= 2 {
				cancel() // stop after receiving events from 2 reconnects
			}
		},
	}

	c.run(ctx)

	if n := got.Load(); n < 2 {
		t.Errorf("got %d events, want at least 2 (reconnect happened)", n)
	}
	if n := connCount.Load(); n < 2 {
		t.Errorf("server received %d connections, want at least 2", n)
	}
}

// TestConn_LastEventIDSentOnReconnect verifies Last-Event-ID is forwarded.
func TestConn_LastEventIDSentOnReconnect(t *testing.T) {
	var receivedIDs []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		receivedIDs = append(receivedIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "id: 7\ndata: hello\n\n")
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count atomic.Int64
	c := &conn{
		sseURL:             srv.URL,
		inactivityDelaySec: 60,
		keyHash:            "testhash",
		client:             srv.Client(),
		logger:             slog.Default(),
		inst:               newTestInstruments(t),
		onEvent: func(evt Event) {
			if count.Add(1) >= 2 {
				cancel()
			}
		},
	}
	c.run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(receivedIDs) < 2 {
		t.Fatalf("server received %d connections, want at least 2", len(receivedIDs))
	}
	// First connection: no Last-Event-ID
	if receivedIDs[0] != "" {
		t.Errorf("first connect: Last-Event-ID = %q, want empty", receivedIDs[0])
	}
	// Second connection: should carry the ID from the first response
	if receivedIDs[1] != "7" {
		t.Errorf("second connect: Last-Event-ID = %q, want %q", receivedIDs[1], "7")
	}
}

// TestConn_StopsOnContextCancel verifies the conn exits promptly when ctx is done.
func TestConn_StopsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Keep connection open indefinitely
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	c := &conn{
		sseURL:             srv.URL,
		inactivityDelaySec: 60,
		keyHash:            "testhash",
		client:             srv.Client(),
		logger:             slog.Default(),
		inst:               newTestInstruments(t),
		onEvent:            func(Event) {},
	}
	c.run(ctx)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("run took %v, want < 1s after cancel", elapsed)
	}
}

// TestReconnectBackoff verifies the backoff doubles each step and caps at max.
func TestReconnectBackoff(t *testing.T) {
	b := newBackoff()
	steps := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, want := range steps {
		got := b.next()
		if got != want {
			t.Errorf("step %d: next() = %v, want %v", i, got, want)
		}
	}
	// Drive to cap
	for range 10 {
		b.next()
	}
	if got := b.next(); got != 60*time.Second {
		t.Errorf("capped next() = %v, want 60s", got)
	}
	// Reset returns to initial
	b.reset()
	if got := b.next(); got != time.Second {
		t.Errorf("after reset: next() = %v, want 1s", got)
	}
}

// TestConn_InactivityTimeout verifies that timedBody closes the connection and
// conn.run reconnects when no bytes arrive within inactivityDelaySec.
func TestConn_InactivityTimeout(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Send nothing — inactivity timer should fire and close the connection.
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := &conn{
		sseURL:             srv.URL,
		inactivityDelaySec: 1, // 1-second inactivity timeout
		keyHash:            "testhash",
		client:             srv.Client(),
		logger:             slog.Default(),
		inst:               newTestInstruments(t),
		onEvent:            func(Event) {},
	}

	go func() {
		// Cancel once we've seen at least two connections (one reconnect confirms
		// the inactivity path triggered).
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
		keyHash:            "testhash",
		client:             http.DefaultClient,
		logger:             slog.Default(),
		inst:               newTestInstruments(t),
		onEvent: func(evt Event) {
			mu.Lock()
			events = append(events, evt)
			count := len(events)
			mu.Unlock()
			if count >= want {
				cancel()
			}
		},
	}
	c.run(ctx)
	mu.Lock()
	defer mu.Unlock()
	return events
}
