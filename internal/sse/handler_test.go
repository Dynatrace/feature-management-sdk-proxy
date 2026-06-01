package sse

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/launchdarkly/eventsource"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/telemetry"
)

// mockTracker implements SubscriberTracker for testing. Always admits.
type mockTracker struct {
	count atomic.Int64
}

func (m *mockTracker) AdmitSubscriber(_ string) (bool, string) { m.count.Add(1); return true, "" }
func (m *mockTracker) DecrementSubscribers(_ string)           { m.count.Add(-1) }

// rejectTracker always rejects with a given reason.
type rejectTracker struct {
	reason string
}

func (r *rejectTracker) AdmitSubscriber(_ string) (bool, string) { return false, r.reason }
func (r *rejectTracker) DecrementSubscribers(_ string)           {}

const testSDKKey = "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"

func init() {
	gin.SetMode(gin.TestMode)
}

// ginContext creates a *gin.Context backed by the given recorder and request
// with FromStreamID middleware applied (validates stream_id query param into context).
func ginContext(w *httptest.ResponseRecorder, r *http.Request) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = r
	sdkkey.FromStreamID()(c)
	return c
}

func newTestHandlerWithCache(t *testing.T, esServer *eventsource.Server) (*Handler, *cache.Cache) {
	t.Helper()
	c := cache.New()
	tracker := &mockTracker{}
	h := NewHandler(c, esServer, tracker, newTestInstruments(t), nil, slog.Default(), 15*time.Second, "/server/")
	return h, c
}

// --- simple error cases (Handle returns before streaming) ---

func TestHandler_MissingStreamID_Returns400(t *testing.T) {
	srv := eventsource.NewServer()
	defer srv.Close()
	h, _ := newTestHandlerWithCache(t, srv)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	h.Handle(ginContext(rr, req))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rr.Code)
	}
}

func TestHandler_InvalidStreamID_Returns400(t *testing.T) {
	srv := eventsource.NewServer()
	defer srv.Close()
	h, _ := newTestHandlerWithCache(t, srv)
	// invalid: not a valid public key prefix
	for _, id := range []string{
		"bogus",
		"abc123",
		"ABCDEF0123456789",                     // old-format hash, no longer valid
		"dt01.invalid_us_ab3xy9kp2q",           // invalid platform
		"dt01.server_US_ab3xy9kp2q",            // uppercase geo
		sdkkey.PublicID(testSDKKey) + ".extra", // full key, not public ID
	} {
		t.Run(id, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+id, nil)
			h.Handle(ginContext(rr, req))
			if rr.Code != http.StatusBadRequest {
				t.Errorf("got %d, want 400", rr.Code)
			}
		})
	}
}

func TestHandler_ConfigNotCached_Returns412(t *testing.T) {
	srv := eventsource.NewServer()
	defer srv.Close()
	h, _ := newTestHandlerWithCache(t, srv)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+sdkkey.PublicID(testSDKKey), nil)
	h.Handle(ginContext(rr, req))
	if rr.Code != http.StatusPreconditionFailed {
		t.Errorf("got %d, want 412", rr.Code)
	}
}

// --- streaming tests ---

func TestHandler_HappyPath_StreamsEvent(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	tracker := &mockTracker{}
	c := cache.New()
	c.Set(sdkkey.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, esSrv, tracker, newTestInstruments(t), nil, slog.Default(), 15*time.Second, "/server/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+sdkkey.PublicID(testSDKKey), nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Handle(ginContext(rr, req))
	}()

	// Give the handler time to connect to the eventsource server.
	time.Sleep(100 * time.Millisecond)

	// Publish an event to the channel.
	keyID := sdkkey.PublicID(testSDKKey)
	esSrv.Publish([]string{keyID}, publishableEvent{evt: Event{EventType: "message", Data: []byte(`{"type":"refetchEvaluation"}`)}})

	// Give time for event delivery.
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	const want = `{"type":"refetchEvaluation"}`
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
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	tracker := &mockTracker{}
	c := cache.New()
	c.Set(sdkkey.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, esSrv, tracker, newTestInstruments(t), nil, slog.Default(), time.Minute, "/server/")

	cancels := make([]context.CancelFunc, n)
	rrs := make([]*httptest.ResponseRecorder, n)
	done := make([]chan struct{}, n)

	for i := range n {
		ctx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		rrs[i] = httptest.NewRecorder()
		done[i] = make(chan struct{})
		req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+sdkkey.PublicID(testSDKKey), nil).WithContext(ctx)
		go func(rr *httptest.ResponseRecorder, req *http.Request, d chan struct{}) {
			defer close(d)
			h.Handle(ginContext(rr, req))
		}(rrs[i], req, done[i])
	}

	// Wait for all subscribers to connect.
	time.Sleep(200 * time.Millisecond)

	keyID := sdkkey.PublicID(testSDKKey)
	esSrv.Publish([]string{keyID}, publishableEvent{evt: Event{EventType: "message", Data: []byte(`{"type":"refetchEvaluation"}`)}})
	time.Sleep(100 * time.Millisecond)

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

func TestHandler_ClientDisconnect_Decrements(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	tracker := &mockTracker{}
	c := cache.New()
	c.Set(sdkkey.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, esSrv, tracker, newTestInstruments(t), nil, slog.Default(), 15*time.Second, "/server/")

	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+sdkkey.PublicID(testSDKKey), nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Handle(ginContext(rr, req))
	}()

	time.Sleep(100 * time.Millisecond)
	if tracker.count.Load() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", tracker.count.Load())
	}

	cancel()
	<-done

	if tracker.count.Load() != 0 {
		t.Errorf("after disconnect: expected 0 subscribers, got %d", tracker.count.Load())
	}
}

func TestHandler_Heartbeat_EmitsPing(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	tracker := &mockTracker{}
	c := cache.New()
	c.Set(sdkkey.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, esSrv, tracker, newTestInstruments(t), nil, slog.Default(), 50*time.Millisecond, "/server/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+sdkkey.PublicID(testSDKKey), nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Handle(ginContext(rr, req))
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	body := rr.Body.String()
	// The eventsource.Server formats comments as ":ping\n" (no space after colon).
	if !strings.Contains(body, ":ping") && !strings.Contains(body, ": ping") {
		t.Errorf("body %q: expected heartbeat ping comment", body)
	}
}

func TestHandler_RawSDKKeyNeverLogged(t *testing.T) {
	var logBuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	tracker := &mockTracker{}
	c := cache.New()
	streamID := sdkkey.PublicID(testSDKKey)
	c.Set(streamID, cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, esSrv, tracker, newTestInstruments(t), nil, logger, 15*time.Second, "/server/")

	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+streamID, nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Handle(ginContext(rr, req))
	}()

	time.Sleep(100 * time.Millisecond)
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

// TestHandler_Unregister_HandlerReturnsCleanly verifies that when the manager
// unregisters a channel (e.g. after a lifecycle invalidation), the handler
// exits cleanly.
func TestHandler_Unregister_HandlerReturnsCleanly(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	tracker := &mockTracker{}
	c := cache.New()
	c.Set(sdkkey.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, esSrv, tracker, newTestInstruments(t), nil, slog.Default(), 15*time.Second, "/server/")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+sdkkey.PublicID(testSDKKey), nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Handle(ginContext(rr, req))
	}()

	time.Sleep(100 * time.Millisecond)

	// Unregister forces disconnection (simulates lifecycle invalidation).
	esSrv.Unregister(sdkkey.PublicID(testSDKKey), true)

	select {
	case <-done:
		// Handler returned cleanly after channel unregister.
	case <-time.After(2 * time.Second):
		t.Error("handler did not return after channel was unregistered")
	}
}

// --- Counter increment tests ---

func TestSSEHandler_Counter_IncrementOnAcceptedSubscription(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	tracker := &mockTracker{}
	c := cache.New()
	streamID := sdkkey.PublicID(testSDKKey)
	c.Set(streamID, cache.Entry{Payload: []byte(`{}`)})

	counter := telemetry.NewCounter()
	h := NewHandler(c, esSrv, tracker, newTestInstruments(t), counter, slog.Default(), 15*time.Second, "/server/")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+streamID, nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Handle(ginContext(rr, req))
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	samples := counter.Drain()
	if len(samples) != 1 || samples[0].Count != 1 {
		t.Errorf("want 1 sample with Count=1 for accepted SSE subscription, got %v", samples)
	}
	if samples[0].SDKKey != "" {
		t.Errorf("SSE counter entry must have empty SDKKey (hash-only), got %q", samples[0].SDKKey)
	}
	if samples[0].KeyID != streamID {
		t.Errorf("SSE counter entry KeyID = %q, want %q", samples[0].KeyID, streamID)
	}
}

func TestSSEHandler_Counter_NoIncrementOnMissingStreamID(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	tracker := &mockTracker{}
	counter := telemetry.NewCounter()
	c := cache.New()
	h := NewHandler(c, esSrv, tracker, newTestInstruments(t), counter, slog.Default(), 15*time.Second, "/server/")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse", nil)
	h.Handle(ginContext(rr, req))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("missing stream_id must not increment counter, got %v", samples)
	}
}

func TestSSEHandler_Counter_NoIncrementOn412(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	tracker := &mockTracker{}
	counter := telemetry.NewCounter()
	c := cache.New() // config not cached
	h := NewHandler(c, esSrv, tracker, newTestInstruments(t), counter, slog.Default(), 15*time.Second, "/server/")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+sdkkey.PublicID(testSDKKey), nil)
	h.Handle(ginContext(rr, req))

	if rr.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412, got %d", rr.Code)
	}
	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("412 (no cached config) must not increment counter, got %v", samples)
	}
}

func TestHandler_SSEDisabled_Returns501(t *testing.T) {
	// nil esServer signals SSE is disabled.
	c := cache.New()
	c.Set(sdkkey.PublicID(testSDKKey), cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, nil, nil, newTestInstruments(t), nil, slog.Default(), 15*time.Second, "/server/")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+sdkkey.PublicID(testSDKKey), nil)
	h.Handle(ginContext(rr, req))

	if rr.Code != http.StatusNotImplemented {
		t.Errorf("got %d, want 501 when SSE is disabled", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "SSE is disabled") {
		t.Errorf("body %q: missing 'SSE is disabled'", rr.Body.String())
	}
}

func TestSSEHandler_Counter_NoIncrementOn501(t *testing.T) {
	counter := telemetry.NewCounter()
	c := cache.New()
	streamID := sdkkey.PublicID(testSDKKey)
	c.Set(streamID, cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, nil, nil, newTestInstruments(t), counter, slog.Default(), 15*time.Second, "/server/")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+streamID, nil)
	h.Handle(ginContext(rr, req))

	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d", rr.Code)
	}
	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("501 (SSE disabled) must not increment counter, got %v", samples)
	}
}

// --- Per-key isolation tests ---

func TestHandler_RateLimit_Returns429(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	c := cache.New()
	streamID := sdkkey.PublicID(testSDKKey)
	c.Set(streamID, cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, esSrv, &rejectTracker{reason: AdmitReasonRateLimit}, newTestInstruments(t), nil, slog.Default(), 15*time.Second, "/server/")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+streamID, nil)
	h.Handle(ginContext(rr, req))

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("got %d, want 429 on rate limit", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
}

func TestHandler_FanoutCap_Returns429(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	c := cache.New()
	streamID := sdkkey.PublicID(testSDKKey)
	c.Set(streamID, cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, esSrv, &rejectTracker{reason: AdmitReasonFanoutCap}, newTestInstruments(t), nil, slog.Default(), 15*time.Second, "/server/")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+streamID, nil)
	h.Handle(ginContext(rr, req))

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("got %d, want 429 on fanout cap", rr.Code)
	}
}

func TestHandler_RateLimit_NoCounterIncrement(t *testing.T) {
	esSrv := eventsource.NewServer()
	defer esSrv.Close()

	c := cache.New()
	streamID := sdkkey.PublicID(testSDKKey)
	c.Set(streamID, cache.Entry{Payload: []byte(`{}`)})
	counter := telemetry.NewCounter()
	h := NewHandler(c, esSrv, &rejectTracker{reason: AdmitReasonRateLimit}, newTestInstruments(t), counter, slog.Default(), 15*time.Second, "/server/")

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+streamID, nil)
	h.Handle(ginContext(rr, req))

	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("rejected request must not increment billing counter, got %v", samples)
	}
}
