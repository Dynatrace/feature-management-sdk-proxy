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

const testSDKKey = "dt_server_us_abc123"

func newTestHandlerWithCache(t *testing.T, sub Subscriber) (*Handler, *cache.Cache) {
	t.Helper()
	c := cache.New()
	h := NewHandler(c, sub, newTestInstruments(t), slog.Default(), 15*time.Second)
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
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/sse?sdk_key="+testSDKKey, nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: got %d, want 405", method, rr.Code)
		}
	}
}

func TestHandler_MissingSDKKey_Returns400(t *testing.T) {
	h, _ := newTestHandlerWithCache(t, &mockSubscriber{})
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rr.Code)
	}
}

func TestHandler_InvalidKeyFormat_Returns400(t *testing.T) {
	h, _ := newTestHandlerWithCache(t, &mockSubscriber{})
	for _, key := range []string{"bogus", "dt_client_us_abc123", "dt_server__abc123", "dt_server_US_abc123"} {
		t.Run(key, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/sse?sdk_key="+key, nil)
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
	req := httptest.NewRequest(http.MethodGet, "/sse?sdk_key="+testSDKKey, nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPreconditionFailed {
		t.Errorf("got %d, want 412", rr.Code)
	}
}

// --- streaming tests ---

func TestHandler_HappyPath_StreamsEvent(t *testing.T) {
	sub := &mockSubscriber{}
	h, c := newTestHandlerWithCache(t, sub)
	c.Set(cache.Key(testSDKKey), cache.Entry{Payload: []byte(`{}`)})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?sdk_key="+testSDKKey, nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rr, req)
	}()

	if !waitForSubs(sub, 1) {
		t.Fatal("timed out waiting for subscriber to register")
	}

	sub.dispatch(Event{EventType: "message", Data: []byte(`{"type":"refetchEvaluation"}`)})

	// Poll until the handler writes the event rather than sleeping a fixed duration.
	const want = `data: {"type":"refetchEvaluation"}`
	for range 100 {
		if strings.Contains(rr.Body.String(), want) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

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
	c.Set(cache.Key(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, sub, newTestInstruments(t), slog.Default(), time.Minute)

	cancels := make([]context.CancelFunc, n)
	rrs := make([]*httptest.ResponseRecorder, n)
	done := make([]chan struct{}, n)

	for i := range n {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		rrs[i] = httptest.NewRecorder()
		done[i] = make(chan struct{})
		req := httptest.NewRequest(http.MethodGet, "/sse?sdk_key="+testSDKKey, nil).WithContext(ctx)
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
	c.Set(cache.Key(testSDKKey), cache.Entry{Payload: []byte(`{}`)})

	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?sdk_key="+testSDKKey, nil).WithContext(ctx)

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
	c.Set(cache.Key(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, sub, newTestInstruments(t), slog.Default(), 50*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?sdk_key="+testSDKKey, nil).WithContext(ctx)

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
	c.Set(cache.Key(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, sub, newTestInstruments(t), logger, 15*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?sdk_key="+testSDKKey, nil).WithContext(ctx)

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
	if !strings.Contains(logs, cache.Key(testSDKKey)) {
		t.Errorf("log output missing key hash %q: %s", cache.Key(testSDKKey), logs)
	}
}
