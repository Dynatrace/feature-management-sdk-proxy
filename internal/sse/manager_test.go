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

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
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
	return New(client, newTestInstruments(t), slog.Default(), Config{})
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
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	defer m.esServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload := configWithSSEURL(srv.URL)
	for range 5 {
		m.OnConfigCached(ctx, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", payload)
	}

	time.Sleep(500 * time.Millisecond)

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
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	defer m.esServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload := configWithSSEURL(srv.URL)
	m.OnConfigCached(ctx, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", payload)
	time.Sleep(300 * time.Millisecond)
	m.OnConfigCached(ctx, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", payload) // same URL — no-op

	time.Sleep(300 * time.Millisecond)
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
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	defer m.esServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m.OnConfigCached(ctx, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", configWithSSEURL(srv.URL+"/v1"))
	time.Sleep(300 * time.Millisecond)
	m.OnConfigCached(ctx, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", configWithSSEURL(srv.URL+"/v2"))
	time.Sleep(500 * time.Millisecond)

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
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	defer m.esServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload := configWithSSEURL(srv.URL)
	m.OnConfigCached(ctx, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", payload)
	time.Sleep(300 * time.Millisecond)

	keyHash := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	m.OnRefreshed(keyHash, cache.Entry{Payload: payload}) // same URL

	time.Sleep(300 * time.Millisecond)
	if n := connCount.Load(); n != 1 {
		t.Errorf("upstream connections = %d, want 1 (same-URL refresh is no-op)", n)
	}
}

// TestManager_Publish_DeliversEvent verifies that events published via the
// manager's upstream path are delivered to downstream subscribers via the
// eventsource.Server.
func TestManager_Publish_DeliversEvent(t *testing.T) {
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
	defer m.esServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	keyHash := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	// Subscribe a downstream client via the eventsource server.
	downstreamSrv := httptest.NewServer(m.esServer.Handler(keyHash))
	defer downstreamSrv.Close()

	// Start a downstream SSE client connection.
	downstreamResp, err := http.Get(downstreamSrv.URL) //nolint:bodyclose // closed via defer below
	if err != nil {
		t.Fatalf("downstream connect: %v", err)
	}
	defer downstreamResp.Body.Close()

	// Start the upstream connection which will publish to the eventsource server.
	m.OnConfigCached(ctx, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", configWithSSEURL(srv.URL))

	// Read from downstream (with timeout).
	buf := make([]byte, 1024)
	done := make(chan string, 1)
	go func() {
		n, _ := downstreamResp.Body.Read(buf)
		done <- string(buf[:n])
	}()

	select {
	case body := <-done:
		if body == "" {
			t.Error("got empty response from downstream")
		}
	case <-time.After(5 * time.Second):
		t.Error("timed out waiting for downstream event")
	}
}

// TestManager_Shutdown_CancelsAllConns verifies Shutdown cancels connections
// and waits for them to finish.
func TestManager_Shutdown_CancelsAllConns(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
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
	time.Sleep(500 * time.Millisecond)

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
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

	m := newTestManager(t, srv.Client())
	defer m.esServer.Close()
	time.Sleep(200 * time.Millisecond)

	if n := connCount.Load(); n != 0 {
		t.Errorf("connections opened = %d, want 0 (no OnConfigCached called)", n)
	}
}

// TestManager_Close_CancelsUpstreamConn verifies that Close cancels the
// active upstream connection for a key.
func TestManager_Close_CancelsUpstreamConn(t *testing.T) {
	var connCount atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	defer m.esServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	keyHash := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	m.OnConfigCached(ctx, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", configWithSSEURL(srv.URL))
	time.Sleep(500 * time.Millisecond)

	m.Close(keyHash)
	time.Sleep(300 * time.Millisecond)

	// After Close, no new reconnects should happen.
	snapshotCount := connCount.Load()
	time.Sleep(500 * time.Millisecond)
	if connCount.Load() > snapshotCount+1 {
		t.Errorf("connections kept growing after Close: %d -> %d", snapshotCount, connCount.Load())
	}
}

// TestManager_Close_UnknownKeyIsNoop verifies that Close on an unknown key
// does not panic or error.
func TestManager_Close_UnknownKeyIsNoop(t *testing.T) {
	m := newTestManager(t, nil)
	defer m.esServer.Close()
	m.Close("nonexistent0000") // must not panic
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
	defer m.esServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	m.OnConfigCached(ctx, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", []byte(`{"flags":{}}`))
	time.Sleep(200 * time.Millisecond)

	if n := connCount.Load(); n != 0 {
		t.Errorf("connections opened = %d, want 0 (no SSE in config)", n)
	}
}

// TestManager_RefreshInterval_UnknownKey returns disconnected interval for a key
// that has never been seen.
func TestManager_RefreshInterval_UnknownKey(t *testing.T) {
	m := newTestManager(t, nil)
	defer m.esServer.Close()
	d := m.RefreshInterval("unknownkey")
	if d != cache.RefreshIntervalDisconnected {
		t.Errorf("RefreshInterval for unknown key = %v, want %v", d, cache.RefreshIntervalDisconnected)
	}
}

// TestManager_RefreshInterval_Connected returns connected interval while the
// upstream SSE read loop is actively streaming.
func TestManager_RefreshInterval_Connected(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	m := newTestManager(t, srv.Client())
	// Shut down the manager (cancels upstream conns) before srv.Close()
	// to avoid srv.Close() blocking on the active TLS connection.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Shutdown(ctx) //nolint:errcheck
	}()

	keyHash := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	m.OnConfigCached(context.Background(), "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", configWithSSEURL(srv.URL+"/sse"))

	// Wait for the read loop to enter streaming state.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("SSE server never received connection")
	}
	// Give setStreaming(true) time to execute.
	time.Sleep(100 * time.Millisecond)

	d := m.RefreshInterval(keyHash)
	if d != cache.RefreshIntervalConnected {
		t.Errorf("RefreshInterval while streaming = %v, want %v", d, cache.RefreshIntervalConnected)
	}
}

// TestManager_RefreshInterval_AfterClose returns disconnected interval after
// the connection is closed.
func TestManager_RefreshInterval_AfterClose(t *testing.T) {
	m := newTestManager(t, nil)
	defer m.esServer.Close()
	keyHash := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	m.mu.Lock()
	m.conns[keyHash] = &keyState{}
	m.mu.Unlock()

	d := m.RefreshInterval(keyHash)
	if d != cache.RefreshIntervalDisconnected {
		t.Errorf("RefreshInterval for non-streaming state = %v, want %v", d, cache.RefreshIntervalDisconnected)
	}
}

func TestManager_HasSubscribers_NoSubscribers(t *testing.T) {
	m := newTestManager(t, nil)
	defer m.esServer.Close()
	keyHash := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	if m.HasSubscribers(keyHash) {
		t.Error("expected false for key with no subscribers")
	}
}

func TestManager_HasSubscribers_UnknownKey(t *testing.T) {
	m := newTestManager(t, nil)
	defer m.esServer.Close()
	if m.HasSubscribers("completely-unknown") {
		t.Error("expected false for unknown key")
	}
}

func TestManager_HasSubscribers_WithSubscriber(t *testing.T) {
	m := newTestManager(t, nil)
	defer m.esServer.Close()
	keyHash := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	admitted, _ := m.AdmitSubscriber(keyHash)
	if !admitted {
		t.Fatal("expected admission with no limits configured")
	}
	defer m.DecrementSubscribers(keyHash)

	if !m.HasSubscribers(keyHash) {
		t.Error("expected true when subscriber is active")
	}
}

func TestManager_HasSubscribers_AfterDecrement(t *testing.T) {
	m := newTestManager(t, nil)
	defer m.esServer.Close()
	keyHash := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	m.AdmitSubscriber(keyHash)
	m.DecrementSubscribers(keyHash)

	if m.HasSubscribers(keyHash) {
		t.Error("expected false after decrement")
	}
}

// --- AdmitSubscriber tests ---

func TestManager_AdmitSubscriber_NoLimits_AlwaysAdmits(t *testing.T) {
	m := New(nil, newTestInstruments(t), slog.Default(), Config{})
	defer m.esServer.Close()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	for range 50 {
		admitted, reason := m.AdmitSubscriber(key)
		if !admitted {
			t.Fatalf("no-limits config rejected admit with reason %q", reason)
		}
	}
}

func TestManager_AdmitSubscriber_FanoutCap_Enforced(t *testing.T) {
	const maxConn = 3
	m := New(nil, newTestInstruments(t), slog.Default(), Config{FanoutCapPerKey: maxConn})
	defer m.esServer.Close()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	for range maxConn {
		admitted, _ := m.AdmitSubscriber(key)
		if !admitted {
			t.Fatal("expected admission below cap")
		}
	}

	admitted, reason := m.AdmitSubscriber(key)
	if admitted {
		t.Error("expected rejection at cap")
	}
	if reason != AdmitReasonFanoutCap {
		t.Errorf("reason = %q, want %q", reason, AdmitReasonFanoutCap)
	}
}

func TestManager_AdmitSubscriber_FanoutCap_ResetsAfterDecrement(t *testing.T) {
	const maxConn = 2
	m := New(nil, newTestInstruments(t), slog.Default(), Config{FanoutCapPerKey: maxConn})
	defer m.esServer.Close()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	m.AdmitSubscriber(key)
	m.AdmitSubscriber(key)

	// At cap — reject.
	if admitted, _ := m.AdmitSubscriber(key); admitted {
		t.Fatal("expected rejection at cap before decrement")
	}

	m.DecrementSubscribers(key)

	// One slot freed — should admit now.
	if admitted, _ := m.AdmitSubscriber(key); !admitted {
		t.Error("expected admission after decrement freed a slot")
	}
}

func TestManager_AdmitSubscriber_RateLimit_Enforced(t *testing.T) {
	// Burst of 1: first admit passes, second is immediately rejected.
	m := New(nil, newTestInstruments(t), slog.Default(), Config{SubscribeRatePerSec: 1, SubscribeBurst: 1})
	defer m.esServer.Close()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	admitted, _ := m.AdmitSubscriber(key)
	if !admitted {
		t.Fatal("expected first admit to pass with burst=1")
	}

	admitted, reason := m.AdmitSubscriber(key)
	if admitted {
		t.Error("expected second immediate admit to be rate-limited")
	}
	if reason != AdmitReasonRateLimit {
		t.Errorf("reason = %q, want %q", reason, AdmitReasonRateLimit)
	}
}

func TestManager_AdmitSubscriber_IsolatedPerKey(t *testing.T) {
	const maxConn = 1
	m := New(nil, newTestInstruments(t), slog.Default(), Config{FanoutCapPerKey: maxConn})
	defer m.esServer.Close()
	// Two keys with distinct public IDs (different token prefix).
	key1 := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	key2 := sdkkey.PublicID("dt02.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")

	// Fill key1's cap.
	admitted, _ := m.AdmitSubscriber(key1)
	if !admitted {
		t.Fatal("expected key1 admission")
	}

	// key1 at cap — reject.
	if admitted, _ := m.AdmitSubscriber(key1); admitted {
		t.Error("expected key1 to be at cap")
	}

	// key2 should be unaffected.
	if admitted, _ := m.AdmitSubscriber(key2); !admitted {
		t.Error("key2 should not be affected by key1's cap")
	}
}
