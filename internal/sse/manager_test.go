package sse

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

func newTestInstruments(t *testing.T) *metrics.Instruments {
	t.Helper()
	inst, err := metrics.Register(noop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("metrics.Register: %v", err)
	}
	return inst
}

func newTestManager(t *testing.T, client *http.Client) *Manager {
	t.Helper()
	return New(client, newTestInstruments(t), slog.Default())
}

func configWithSSEURL(url string) []byte {
	return fmt.Appendf(nil, `{"eventStreams":[{"type":"sse","url":%q,"inactivityDelaySec":60}]}`, url)
}

// TestManager_OnConfigCached_StartsOneConn verifies that N calls with the same
// SDK key and same SSE URL result in exactly one upstream connection.
func TestManager_OnConfigCached_StartsOneConn(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	payload := configWithSSEURL(srv.URL)
	for range 5 {
		m.OnConfigCached(ctx, "dt_server_us_abc123", payload)
	}

	time.Sleep(200 * time.Millisecond)

	if n := connCount.Load(); n != 1 {
		t.Errorf("upstream connections = %d, want 1", n)
	}
}

// TestManager_OnConfigCached_SameURLIsNoop verifies idempotency.
func TestManager_OnConfigCached_SameURLIsNoop(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	payload := configWithSSEURL(srv.URL)
	m.OnConfigCached(ctx, "dt_server_us_abc123", payload)
	time.Sleep(100 * time.Millisecond)
	m.OnConfigCached(ctx, "dt_server_us_abc123", payload) // same URL — no-op

	time.Sleep(200 * time.Millisecond)
	if n := connCount.Load(); n != 1 {
		t.Errorf("upstream connections = %d, want 1 (same URL is no-op)", n)
	}
}

// TestManager_OnConfigCached_URLChangeRestartsConn verifies a different SSE URL
// tears down the old connection and starts a new one.
func TestManager_OnConfigCached_URLChangeRestartsConn(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// First URL: srv.URL + "/v1", second URL: srv.URL + "/v2"
	m.OnConfigCached(ctx, "dt_server_us_abc123", configWithSSEURL(srv.URL+"/v1"))
	time.Sleep(150 * time.Millisecond)
	m.OnConfigCached(ctx, "dt_server_us_abc123", configWithSSEURL(srv.URL+"/v2"))
	time.Sleep(300 * time.Millisecond)

	if n := connCount.Load(); n < 2 {
		t.Errorf("upstream connections = %d, want >= 2 (URL change restarted)", n)
	}
}

// TestManager_OnRefreshed_SameURLIsNoop verifies that a 200 refresh with the
// same SSE URL does not restart the connection.
func TestManager_OnRefreshed_SameURLIsNoop(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	payload := configWithSSEURL(srv.URL)
	m.OnConfigCached(ctx, "dt_server_us_abc123", payload)
	time.Sleep(150 * time.Millisecond)

	keyHash := cache.Key("dt_server_us_abc123")
	m.OnRefreshed(keyHash, payload) // same URL

	time.Sleep(150 * time.Millisecond)
	if n := connCount.Load(); n != 1 {
		t.Errorf("upstream connections = %d, want 1 (same-URL refresh is no-op)", n)
	}
}

// TestManager_Subscribe_ReceivesEvents verifies that a subscriber receives
// events dispatched by the upstream conn goroutine.
func TestManager_Subscribe_ReceivesEvents(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: flagsUpdated\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	keyHash := cache.Key("dt_server_us_abc123")
	ch, unsub := m.Subscribe(keyHash)
	defer unsub()

	m.OnConfigCached(ctx, "dt_server_us_abc123", configWithSSEURL(srv.URL))

	select {
	case evt := <-ch:
		if string(evt.Data) != "flagsUpdated" {
			t.Errorf("evt.Data = %q, want %q", evt.Data, "flagsUpdated")
		}
	case <-ctx.Done():
		t.Error("timed out waiting for event")
	}
}

// TestManager_Unsubscribe_StopsDelivery verifies that after unsubscribing,
// dispatch to that subscriber no longer occurs.
func TestManager_Unsubscribe_StopsDelivery(t *testing.T) {
	m := newTestManager(t, nil)

	keyHash := cache.Key("dt_server_us_abc123")
	ch, unsub := m.Subscribe(keyHash)

	// Dispatch before unsubscribe: channel should be populated
	m.dispatch(keyHash, Event{Data: []byte("before")})
	select {
	case evt := <-ch:
		if string(evt.Data) != "before" {
			t.Errorf("before: Data = %q, want %q", evt.Data, "before")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event before unsub")
	}

	unsub()

	// Dispatch after unsubscribe: channel should remain empty
	m.dispatch(keyHash, Event{Data: []byte("after")})
	select {
	case evt := <-ch:
		t.Errorf("after unsub: got unexpected event %q", evt.Data)
	case <-time.After(100 * time.Millisecond):
		// expected: no event
	}
}

// TestManager_Shutdown_CancelsAllConns verifies Shutdown cancels connections
// and waits for them to finish.
func TestManager_Shutdown_CancelsAllConns(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i := range 3 {
		key := fmt.Sprintf("dt_server_us_key%d", i)
		m.OnConfigCached(ctx, key, configWithSSEURL(srv.URL))
	}
	time.Sleep(200 * time.Millisecond)

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutCancel()

	if err := m.Shutdown(shutCtx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestManager_NeverCachedKeyNeverOpensConn verifies that a key with no config
// cached never triggers an upstream SSE connection.
func TestManager_NeverCachedKeyNeverOpensConn(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
	}))
	defer srv.Close()

	_ = newTestManager(t, srv.Client())
	time.Sleep(200 * time.Millisecond)

	if n := connCount.Load(); n != 0 {
		t.Errorf("connections opened = %d, want 0 (no OnConfigCached called)", n)
	}
}

// TestManager_Close_CancelsUpstreamConn verifies that Close cancels the
// active upstream connection for a key (connCount stops growing).
func TestManager_Close_CancelsUpstreamConn(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	keyHash := cache.Key("dt_server_us_abc123")
	m.OnConfigCached(ctx, "dt_server_us_abc123", configWithSSEURL(srv.URL))
	time.Sleep(150 * time.Millisecond)

	m.Close(keyHash)
	time.Sleep(150 * time.Millisecond)

	// After Close, no new reconnects should happen (context cancelled).
	snapshotCount := connCount.Load()
	time.Sleep(200 * time.Millisecond)
	if connCount.Load() > snapshotCount+1 {
		t.Errorf("connections kept growing after Close: %d -> %d", snapshotCount, connCount.Load())
	}
}

// TestManager_Close_ClosesSubscriberChannels verifies that Close closes all
// channels belonging to subscribers of the key.
func TestManager_Close_ClosesSubscriberChannels(t *testing.T) {
	m := newTestManager(t, nil)
	keyHash := cache.Key("dt_server_us_abc123")

	ch1, unsub1 := m.Subscribe(keyHash)
	ch2, unsub2 := m.Subscribe(keyHash)
	defer unsub1()
	defer unsub2()

	m.Close(keyHash)

	for _, ch := range []<-chan Event{ch1, ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Error("expected closed channel, got open channel with value")
			}
		case <-time.After(time.Second):
			t.Error("timed out waiting for channel to be closed")
		}
	}
}

// TestManager_Close_UnknownKeyIsNoop verifies that Close on an unknown key
// does not panic or error.
func TestManager_Close_UnknownKeyIsNoop(t *testing.T) {
	m := newTestManager(t, nil)
	m.Close("nonexistent0000") // must not panic
}

// TestManager_Close_SubscribeAfterCloseCreatesNewState verifies that a new
// Subscribe call after Close creates fresh state (channel is open).
func TestManager_Close_SubscribeAfterCloseCreatesNewState(t *testing.T) {
	m := newTestManager(t, nil)
	keyHash := cache.Key("dt_server_us_abc123")

	ch1, _ := m.Subscribe(keyHash)
	m.Close(keyHash)

	ch2, unsub2 := m.Subscribe(keyHash)
	defer unsub2()

	// ch1 should be closed; ch2 should be open (not immediately closed).
	select {
	case _, ok := <-ch1:
		if ok {
			t.Error("ch1: expected closed, got open with value")
		}
	case <-time.After(time.Second):
		t.Error("ch1: timed out waiting for close")
	}

	// ch2 should not be closed yet.
	select {
	case <-ch2:
		t.Error("ch2: should be open after fresh Subscribe")
	default:
		// expected
	}
}

// TestManager_NoSSEInConfig_NoConnection verifies that a payload without
// eventStreams does not open an upstream SSE connection.
func TestManager_NoSSEInConfig_NoConnection(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	m.OnConfigCached(ctx, "dt_server_us_abc123", []byte(`{"flags":{}}`))
	time.Sleep(200 * time.Millisecond)

	if n := connCount.Load(); n != 0 {
		t.Errorf("connections opened = %d, want 0 (no SSE in config)", n)
	}
}
