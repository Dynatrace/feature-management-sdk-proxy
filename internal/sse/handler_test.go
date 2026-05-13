package sse

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/telemetry"
)

// mockSubscriber implements Subscriber for testing.
type mockSubscriber struct {
	mu   sync.Mutex
	subs []*mockSub
}

type mockSub struct {
	ch chan Event
}

func (m *mockSubscriber) Subscribe(_ string) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	s := &mockSub{ch: ch}
	m.mu.Lock()
	m.subs = append(m.subs, s)
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i, sub := range m.subs {
			if sub == s {
				m.subs = append(m.subs[:i], m.subs[i+1:]...)
				return
			}
		}
	}
}

func (m *mockSubscriber) dispatch(evt Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.subs {
		select {
		case s.ch <- evt:
		default:
		}
	}
}

func (m *mockSubscriber) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

const testSDKKey = "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"

func newTestHandlerWithCache(t *testing.T, sub Subscriber) (*Handler, *cache.Cache) {
	t.Helper()
	c := cache.New()
	h := NewHandler(c, sub, newTestInstruments(t), nil, slog.Default(), 15*time.Second, "/server/")
	return h, c
}

// waitForSubs polls until the subscriber count reaches n or the timeout elapses.
func waitForSubs(sub *mockSubscriber, n int) bool {
	for range 100 {
		if sub.count() >= n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// --- simple error cases (ServeHTTP returns before streaming) ---

func TestHandler_NonGet_Returns405(t *testing.T) {
	h, _ := newTestHandlerWithCache(t, &mockSubscriber{})
	streamID := cache.PublicID(testSDKKey)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/sse?stream_id="+streamID, nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: got %d, want 405", method, rr.Code)
		}
	}
}

func TestHandler_MissingStreamID_Returns400(t *testing.T) {
	h, _ := newTestHandlerWithCache(t, &mockSubscriber{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rr.Code)
	}
}

func TestHandler_InvalidStreamID_Returns400(t *testing.T) {
	h, _ := newTestHandlerWithCache(t, &mockSubscriber{})
	// invalid: not a valid public key prefix
	for _, id := range []string{
		"bogus",
		"abc123",
		"ABCDEF0123456789",                    // old-format hash, no longer valid
		"dt01.invalid_us_ab3xy9kp2q",          // invalid platform
		"dt01.server_US_ab3xy9kp2q",           // uppercase geo
		cache.PublicID(testSDKKey) + ".extra", // full key, not public ID
	} {
		t.Run(id, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+id, nil)
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("got %d, want 400", rr.Code)
			}
		})
	}
}

func TestHandler_ConfigNotCached_Returns412(t *testing.T) {
	h, _ := newTestHandlerWithCache(t, &mockSubscriber{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+cache.PublicID(testSDKKey), nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPreconditionFailed {
		t.Errorf("got %d, want 412", rr.Code)
	}
}

// --- streaming tests ---

func TestHandler_HappyPath_StreamsEvent(t *testing.T) {
	sub := &mockSubscriber{}
	h, c := newTestHandlerWithCache(t, sub)
	c.Set(cache.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+cache.PublicID(testSDKKey), nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rr, req)
	}()

	if !waitForSubs(sub, 1) {
		t.Fatal("timed out waiting for subscriber to register")
	}

	sub.dispatch(Event{EventType: "message", Data: []byte(`{"type":"refetchEvaluation"}`)})

	// Give the handler goroutine time to write the event before cancelling.
	// Reading rr.Body while the handler goroutine is still writing is a data race,
	// so we cancel after the sleep and read only after <-done.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	const want = `data: {"type":"refetchEvaluation"}`
	body := rr.Body.String()
	if !strings.Contains(body, "event: message") {
		t.Errorf("body %q: missing 'event: message'", body)
	}
	if !strings.Contains(body, want) {
		t.Errorf("body %q: missing data field", body)
	}
}

func TestHandler_MultipleSubscribers_AllReceiveEvent(t *testing.T) {
	const n = 3
	sub := &mockSubscriber{}
	c := cache.New()
	c.Set(cache.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, sub, newTestInstruments(t), nil, slog.Default(), time.Minute, "/server/")

	cancels := make([]context.CancelFunc, n)
	rrs := make([]*httptest.ResponseRecorder, n)
	done := make([]chan struct{}, n)

	for i := range n {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		rrs[i] = httptest.NewRecorder()
		done[i] = make(chan struct{})
		req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+cache.PublicID(testSDKKey), nil).WithContext(ctx)
		go func(rr *httptest.ResponseRecorder, req *http.Request, d chan struct{}) {
			defer close(d)
			h.ServeHTTP(rr, req)
		}(rrs[i], req, done[i])
	}

	if !waitForSubs(sub, n) {
		t.Fatalf("timed out waiting for %d subscribers; got %d", n, sub.count())
	}

	sub.dispatch(Event{EventType: "message", Data: []byte(`{"type":"refetchEvaluation"}`)})
	time.Sleep(50 * time.Millisecond)

	for i, cancel := range cancels {
		cancel()
		<-done[i]
	}

	for i, rr := range rrs {
		if !strings.Contains(rr.Body.String(), "event: message") {
			t.Errorf("subscriber %d: body %q missing event", i, rr.Body.String())
		}
	}
}

func TestHandler_ClientDisconnect_Unsubscribes(t *testing.T) {
	sub := &mockSubscriber{}
	h, c := newTestHandlerWithCache(t, sub)
	c.Set(cache.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})

	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+cache.PublicID(testSDKKey), nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rr, req)
	}()

	if !waitForSubs(sub, 1) {
		t.Fatal("timed out waiting for subscriber to register")
	}

	cancel()
	<-done

	if sub.count() != 0 {
		t.Errorf("after disconnect: expected 0 subscribers, got %d", sub.count())
	}
}

func TestHandler_Heartbeat_EmitsPing(t *testing.T) {
	sub := &mockSubscriber{}
	c := cache.New()
	c.Set(cache.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, sub, newTestInstruments(t), nil, slog.Default(), 50*time.Millisecond, "/server/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+cache.PublicID(testSDKKey), nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rr, req)
	}()

	time.Sleep(120 * time.Millisecond)
	cancel()
	<-done

	if !strings.Contains(rr.Body.String(), ": ping") {
		t.Errorf("body %q: expected heartbeat ping comment", rr.Body.String())
	}
}

func TestHandler_RawSDKKeyNeverLogged(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sub := &mockSubscriber{}
	c := cache.New()
	streamID := cache.PublicID(testSDKKey)
	c.Set(streamID, cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, sub, newTestInstruments(t), nil, logger, 15*time.Second, "/server/")

	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+streamID, nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rr, req)
	}()

	if !waitForSubs(sub, 1) {
		t.Fatal("timed out waiting for subscriber")
	}
	cancel()
	<-done

	logs := logBuf.String()
	if strings.Contains(logs, testSDKKey) {
		t.Errorf("log output contains raw SDK key %q: %s", testSDKKey, logs)
	}
	if !strings.Contains(logs, streamID) {
		t.Errorf("log output missing key hash %q: %s", streamID, logs)
	}
}

// TestHandler_ChannelClose_HandlerReturnsCleanly verifies that when the manager
// closes the subscriber channel (e.g. after a lifecycle invalidation), the
// handler exits cleanly without busy-looping on zero-value events.
func TestHandler_ChannelClose_HandlerReturnsCleanly(t *testing.T) {
	sub := &mockSubscriber{}
	h, c := newTestHandlerWithCache(t, sub)
	c.Set(cache.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+cache.PublicID(testSDKKey), nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rr, req)
	}()

	if !waitForSubs(sub, 1) {
		t.Fatal("timed out waiting for subscriber to register")
	}

	// Close all subscriber channels (simulates lifecycle invalidation).
	sub.mu.Lock()
	for _, s := range sub.subs {
		close(s.ch)
	}
	sub.mu.Unlock()

	select {
	case <-done:
		// Handler returned cleanly after channel close.
	case <-time.After(time.Second):
		t.Error("handler did not return after subscriber channel was closed")
	}
}

// --- Counter increment tests ---

func TestSSEHandler_Counter_IncrementOnAcceptedSubscription(t *testing.T) {
	sub := &mockSubscriber{}
	c := cache.New()
	streamID := cache.PublicID(testSDKKey)
	c.Set(streamID, cache.Entry{Payload: []byte(`{}`)})

	counter := telemetry.NewCounter()
	h := NewHandler(c, sub, newTestInstruments(t), counter, slog.Default(), 15*time.Second, "/server/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+streamID, nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rr, req)
	}()

	if !waitForSubs(sub, 1) {
		t.Fatal("timed out waiting for subscriber to register")
	}
	cancel()
	<-done

	samples := counter.Drain()
	if len(samples) != 1 || samples[0].Count != 1 {
		t.Errorf("want 1 sample with Count=1 for accepted SSE subscription, got %v", samples)
	}
	// IncrementByHash creates an entry without the raw SDK key — eligible for
	// billing only after a paired HTTP request via Increment(sdkKey) repairs it.
	if samples[0].SDKKey != "" {
		t.Errorf("SSE counter entry must have empty SDKKey (hash-only), got %q", samples[0].SDKKey)
	}
	if samples[0].KeyID != streamID {
		t.Errorf("SSE counter entry KeyID = %q, want %q", samples[0].KeyID, streamID)
	}
}

func TestSSEHandler_Counter_NoIncrementOn405(t *testing.T) {
	sub := &mockSubscriber{}
	counter := telemetry.NewCounter()
	c := cache.New()
	h := NewHandler(c, sub, newTestInstruments(t), counter, slog.Default(), 15*time.Second, "/server/")

	streamID := cache.PublicID(testSDKKey)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/sse?stream_id="+streamID, nil)
		h.ServeHTTP(rr, req)
	}

	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("405 requests must not increment counter, got %v", samples)
	}
}

func TestSSEHandler_Counter_NoIncrementOnMissingStreamID(t *testing.T) {
	sub := &mockSubscriber{}
	counter := telemetry.NewCounter()
	c := cache.New()
	h := NewHandler(c, sub, newTestInstruments(t), counter, slog.Default(), 15*time.Second, "/server/")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("missing stream_id must not increment counter, got %v", samples)
	}
}

func TestSSEHandler_Counter_NoIncrementOn412(t *testing.T) {
	sub := &mockSubscriber{}
	counter := telemetry.NewCounter()
	c := cache.New() // config not cached
	h := NewHandler(c, sub, newTestInstruments(t), counter, slog.Default(), 15*time.Second, "/server/")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+cache.PublicID(testSDKKey), nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", rr.Code)
	}
	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("412 (no cached config) must not increment counter, got %v", samples)
	}
}
