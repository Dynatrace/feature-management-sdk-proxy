package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/telemetry"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ginContext creates a *gin.Context backed by the given recorder and request,
// with the :file route parameter set and FromPathFile middleware applied.
func ginContext(w *httptest.ResponseRecorder, r *http.Request) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = r
	if file, ok := strings.CutPrefix(r.URL.Path, "/server/"); ok {
		c.Params = gin.Params{{Key: "file", Value: file}}
		sdkkey.FromPathFile()(c)
	}
	return c
}

var testTracer = tracenoop.NewTracerProvider().Tracer("test")

// roundTripFunc adapts a function to the http.RoundTripper interface.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newRedirectingClient returns an http.Client that routes all requests to
// target regardless of the request's Host or scheme. This lets tests pass
// "fm.test" as cdnBaseDomain while buildTarget generates https:// URLs that
// are transparently sent to the httptest.Server over plain HTTP.
func newRedirectingClient(target *httptest.Server) *http.Client {
	addr := target.Listener.Addr().String()
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r2 := r.Clone(r.Context())
			r2.URL.Scheme = "http"
			r2.URL.Host = addr
			return http.DefaultTransport.RoundTrip(r2)
		}),
	}
}

func newTestHandler(t *testing.T, upstream *httptest.Server) *Handler {
	t.Helper()
	h, err := New("fm.test", newRedirectingClient(upstream), cache.New(), nil, nil, nil, nil, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func newTestHandlerWithCache(t *testing.T, upstream *httptest.Server, c *cache.Cache) *Handler {
	t.Helper()
	h, err := New("fm.test", newRedirectingClient(upstream), c, nil, nil, nil, nil, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// --- Cache miss (cold fetch) tests ---

func TestHandler_CacheMiss_FetchesUpstream(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"flags":{}}`)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != `{"flags":{}}` {
		t.Errorf("unexpected body: %q", rec.Body.String())
	}
	if upstreamCalled != 1 {
		t.Errorf("expected upstream called once, got %d", upstreamCalled)
	}
}

func TestHandler_CacheMiss_PopulatesCache(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"flags":{}}`)
	}))
	defer upstream.Close()

	c := cache.New()
	h := newTestHandlerWithCache(t, upstream, c)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	entry, ok := c.Get(key)
	if !ok {
		t.Fatal("expected cache to be populated after miss")
	}
	if string(entry.Payload) != `{"flags":{}}` {
		t.Errorf("unexpected cached payload: %q", string(entry.Payload))
	}
	if entry.ETag != `"abc123"` {
		t.Errorf("unexpected cached ETag: %q", entry.ETag)
	}
}

func TestHandler_CacheMiss_PathForwardedToUpstream(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if gotPath != "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json" {
		t.Errorf("upstream received path %q, want /server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", gotPath)
	}
}

func TestHandler_CacheMiss_Upstream304_Returns502(t *testing.T) {
	// A 304 on a cold fetch is a CDN bug — must not return empty config to the SDK.
	// 502 Bad Gateway because the proxy received an invalid response from upstream.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

func TestHandler_CacheMiss_UpstreamServerError_PassedThrough(t *testing.T) {
	// CDN 5xx codes are forwarded to the SDK client on a cold fetch.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (CDN code forwarded), got %d", rec.Code)
	}
}

func TestHandler_CacheMiss_NetworkError_Returns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := newRedirectingClient(upstream)
	upstream.Close() // close after capturing the address in the client

	h, err := New("fm.test", client, cache.New(), nil, nil, nil, nil, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHandler_CacheMiss_UpstreamClientError_PassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "forbidden")
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "forbidden") {
		t.Errorf("expected upstream body forwarded, got %q", rec.Body.String())
	}
}

func TestHandler_CacheMiss_IfNoneMatch_NotForwardedToUpstream(t *testing.T) {
	// On a cache miss, the client's If-None-Match is not forwarded — the cache
	// handles ETag negotiation, not the upstream on a cold fetch.
	var gotIfNoneMatch string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIfNoneMatch = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	h.Handle(ginContext(rec, req))

	if gotIfNoneMatch != "" {
		t.Errorf("upstream should not receive If-None-Match on cold fetch, got %q", gotIfNoneMatch)
	}
}

// --- Cache hit tests ---

func TestHandler_CacheHit_ServesFromCache_NoUpstreamCall(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{"foo":true}}`),
		ETag:      `"v1"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != `{"flags":{"foo":true}}` {
		t.Errorf("unexpected body: %q", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

func TestHandler_CacheHit_ETagMatch_Returns304(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      `"abc123"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", rec.Code)
	}
	if etag := rec.Header().Get("ETag"); etag != `"abc123"` {
		t.Errorf("expected ETag forwarded, got %q", etag)
	}
	if rec.Body.Len() > 0 {
		t.Errorf("304 response must have no body, got %q", rec.Body.String())
	}
}

func TestHandler_CacheHit_ETagMismatch_Returns200WithPayload(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{"updated":true}}`),
		ETag:      `"v2"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Header.Set("If-None-Match", `"v1"`) // stale client version
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != `{"flags":{"updated":true}}` {
		t.Errorf("unexpected body: %q", rec.Body.String())
	}
}

func TestHandler_CacheHit_NoETag_Returns200(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      "", // no ETag stored
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Header.Set("If-None-Match", `"anything"`)
	h.Handle(ginContext(rec, req))

	// Without a cached ETag we cannot confirm the client has the current version.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// --- Input validation tests ---

func TestHandler_NonGetMethod_Returns405(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)

	// Method filtering is a router-level concern. Test via a Gin router with
	// HandleMethodNotAllowed enabled so POST/PUT/DELETE return 405.
	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.GET("/server/:file", h.Handle)
	router.HEAD("/server/:file", h.Handle)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: expected 405, got %d", method, rec.Code)
		}
	}
}

func TestHandler_MissingJsonSuffix_Returns400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_EmptyKeySegment_Returns400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandler_UnrecognizedSDKKeyFormat_Returns400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for unrecognized key format")
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	for _, path := range []string{
		"/server/bogus.json",
		"/server/my-sdk-key.json",
		// public ID only — valid format but missing private segment
		"/server/dt01.server_us_ab3xy9kp2q.json",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.Handle(ginContext(rec, req))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("path %q: expected 400, got %d", path, rec.Code)
		}
	}
}

func TestHandler_HEAD_CacheHit_NoBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      `"v1"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.Len() > 0 {
		t.Errorf("HEAD response must have no body, got %q", rec.Body.String())
	}
	if etag := rec.Header().Get("ETag"); etag != `"v1"` {
		t.Errorf("expected ETag header, got %q", etag)
	}
}

func TestNew_InvalidCDNBaseDomain(t *testing.T) {
	cases := []string{
		"",
		"https://fm.example.com",
		"fm.example.com/path",
		"http://fm.example.com/",
	}
	for _, tc := range cases {
		_, err := New(tc, http.DefaultClient, cache.New(), nil, nil, nil, nil, nil, slog.Default(), testTracer)
		if err == nil {
			t.Errorf("New(%q): expected error, got nil", tc)
		}
	}
}

func TestExtractSDKKeyFromFile(t *testing.T) {
	cases := []struct {
		file    string
		wantKey string
		wantOK  bool
	}{
		{"abc123.json", "abc123", true},
		{"dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", true},
		{".json", "", false},
		{"noextension", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		key, ok := sdkkey.FromFileName(tc.file)
		if ok != tc.wantOK || key != tc.wantKey {
			t.Errorf("sdkkey.FromFileName(%q) = (%q, %v), want (%q, %v)", tc.file, key, ok, tc.wantKey, tc.wantOK)
		}
	}
}

func TestExtractGeo(t *testing.T) {
	cases := []struct {
		sdkKey  string
		wantGeo string
		wantOK  bool
	}{
		{"dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", "us", true},
		{"dt01.server_eu_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", "eu", true},
		{"dt01.server_ap_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", "ap", true},
		{"dt01.server_us_ab3xy9kp2q", "", false}, // public ID only, no private segment
		{"bogus", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		geo, ok := sdkkey.ExtractGeo(tc.sdkKey)
		if ok != tc.wantOK || geo != tc.wantGeo {
			t.Errorf("sdkkey.ExtractGeo(%q) = (%q, %v), want (%q, %v)", tc.sdkKey, geo, ok, tc.wantGeo, tc.wantOK)
		}
	}
}

func TestBuildTarget_ConstructsGeoBoundURL(t *testing.T) {
	h := &Handler{cdnBaseDomain: "fm.dynatracelabs.com"}
	k, ok := sdkkey.ParseFull("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	if !ok {
		t.Fatal("ParseFull failed for valid SDK key")
	}
	got, ok := h.buildTarget(k)
	if !ok {
		t.Fatal("buildTarget returned ok=false for valid SDK key")
	}
	want := "https://cdn.us.fm.dynatracelabs.com/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json"
	if got != want {
		t.Errorf("buildTarget = %q, want %q", got, want)
	}
}

// --- SSE URL rewrite tests ---

const testPayloadWithSSE = `{"flags":{},"eventStreams":[{"type":"sse","url":"https://upstream.sse.example.com/stream?ch=abc","inactivityDelaySec":120}]}`
const testPayloadNoSSE = `{"flags":{"foo":true}}`

func TestHandler_CacheHit_RewritesSSEURL(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(testPayloadWithSSE),
		ETag:      `"v1"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Host = "proxy.example.com"
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "upstream.sse.example.com") {
		t.Errorf("response still contains upstream SSE URL: %q", body)
	}
	wantStreamID := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	if !strings.Contains(body, "proxy.example.com/sse?stream_id="+wantStreamID) {
		t.Errorf("response missing proxy SSE URL with stream_id, got: %q", body)
	}
	if strings.Contains(body, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a") {
		t.Errorf("response contains raw SDK key in URL: %q", body)
	}
}

func TestHandler_CacheMiss_RewritesSSEURLInResponse_StoresRawInCache(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, testPayloadWithSSE)
	}))
	defer upstream.Close()

	c := cache.New()
	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Host = "proxy.example.com"
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Response served to SDK should have the proxy SSE URL with stream_id.
	body := rec.Body.String()
	wantStreamID := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	if strings.Contains(body, "upstream.sse.example.com") {
		t.Errorf("response still contains upstream SSE URL: %q", body)
	}
	if !strings.Contains(body, "proxy.example.com/sse?stream_id="+wantStreamID) {
		t.Errorf("response missing proxy SSE URL with stream_id, got: %q", body)
	}
	if strings.Contains(body, "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a") {
		t.Errorf("response contains raw SDK key in URL: %q", body)
	}

	// Cache must store raw upstream bytes so the SSE manager can dial the real upstream.
	keyHash := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	entry, ok := c.Get(keyHash)
	if !ok {
		t.Fatal("expected cache populated after miss")
	}
	if string(entry.Payload) != testPayloadWithSSE {
		t.Errorf("cache stores rewritten payload, want raw upstream bytes;\ngot:  %q\nwant: %q",
			string(entry.Payload), testPayloadWithSSE)
	}
}

func TestHandler_NoSSEEntry_PayloadServedByteIdentical(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(testPayloadNoSSE),
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Host = "proxy.example.com"
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != testPayloadNoSSE {
		t.Errorf("payload modified unexpectedly: got %q, want %q", rec.Body.String(), testPayloadNoSSE)
	}
}

func TestHandler_HEAD_NoSSERewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(testPayloadWithSSE),
		ETag:      `"v1"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Host = "proxy.example.com"
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.Len() > 0 {
		t.Errorf("HEAD must return no body, got %q", rec.Body.String())
	}
}

func TestHandler_SSEDisabled_NoRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(testPayloadWithSSE),
		ETag:      `"v1"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	h.SetRewriteSSEURLs(false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Host = "proxy.example.com"
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != testPayloadWithSSE {
		t.Errorf("payload rewritten when rewriteSSEURLs=false:\ngot:  %q\nwant: %q", rec.Body.String(), testPayloadWithSSE)
	}
	if strings.Contains(rec.Body.String(), "proxy.example.com") {
		t.Errorf("proxy host must not appear in response when rewriteSSEURLs=false: %q", rec.Body.String())
	}
}

// --- derivedScheme tests ---

func TestDerivedScheme_XForwardedProto(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := derivedScheme(req); got != "https" {
		t.Errorf("derivedScheme = %q, want %q", got, "https")
	}
}

func TestDerivedScheme_XForwardedProto_CommaSeparated(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-Proto", "https, http")
	if got := derivedScheme(req); got != "https" {
		t.Errorf("derivedScheme = %q, want %q", got, "https")
	}
}

func TestDerivedScheme_TLSConnection(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.TLS = &tls.ConnectionState{}
	if got := derivedScheme(req); got != "https" {
		t.Errorf("derivedScheme = %q, want %q", got, "https")
	}
}

func TestDerivedScheme_Plain(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := derivedScheme(req); got != "http" {
		t.Errorf("derivedScheme = %q, want %q", got, "http")
	}
}

func TestDerivedScheme_XForwardedProto_InvalidSchemeDiscarded(t *testing.T) {
	// Non-http(s) schemes must be discarded to prevent injection into the rewritten URL.
	for _, bad := range []string{"javascript", "ftp", "data", "file"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Forwarded-Proto", bad)
		if got := derivedScheme(req); got != "http" {
			t.Errorf("XFP=%q: derivedScheme = %q, want fallback %q", bad, got, "http")
		}
	}
}

func TestHandler_MalformedHostHeader_FallsBackToUnknown(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(testPayloadWithSSE),
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Host = "proxy.example.com\r\nX-Injected: bad"
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	// The malformed host must not appear verbatim; "unknown" is used as fallback.
	if strings.Contains(body, "\r\n") {
		t.Errorf("malformed host header leaked into response body: %q", body)
	}
	if !strings.Contains(body, "unknown/sse?stream_id=") {
		t.Errorf("expected fallback 'unknown' with stream_id in rewritten URL, got: %q", body)
	}
}

func TestHandler_Rewrite_StreamIDFormat(t *testing.T) {
	// The rewritten SSE URL must use stream_id=<hash> — never the raw SDK key.
	// Verify this for a key with a hyphenated region (valid per SSRF guard).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	// Use a different geo (eu) to verify geo-specific routing works.
	c := cache.New()
	sdkKey := "dt01.server_eu_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	key := sdkkey.PublicID(sdkKey)
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{},"eventStreams":[{"type":"sse","url":"https://upstream.sse.example.com/stream","inactivityDelaySec":60}]}`),
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.eu.fm.test/server/" + sdkKey + ".json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/"+sdkKey+".json", nil)
	req.Host = "proxy.example.com"
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "stream_id="+key) {
		t.Errorf("rewritten URL missing stream_id public ID, got: %q", body)
	}
	if strings.Contains(body, sdkKey) {
		t.Errorf("rewritten URL contains raw SDK key %q: %q", sdkKey, body)
	}
}

func TestHandler_Rewrite_HTTPSSchemeFromXForwardedProto(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	key := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	c.Set(key, cache.Entry{
		Payload:   []byte(testPayloadWithSSE),
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	req.Host = "proxy.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	h.Handle(ginContext(rec, req))

	body := rec.Body.String()
	wantStreamID := sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a")
	if !strings.Contains(body, "https://proxy.example.com/sse?stream_id="+wantStreamID) {
		t.Errorf("expected https URL with stream_id in rewritten URL, got: %q", body)
	}
}

// --- Counter increment tests ---

func newTestHandlerWithCounter(t *testing.T, upstream *httptest.Server, counter *telemetry.Counter) *Handler {
	t.Helper()
	h, err := New("fm.test", newRedirectingClient(upstream), cache.New(), nil, nil, nil, nil, counter, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func newTestHandlerWithCacheAndCounter(t *testing.T, upstream *httptest.Server, c *cache.Cache, counter *telemetry.Counter) *Handler {
	t.Helper()
	h, err := New("fm.test", newRedirectingClient(upstream), c, nil, nil, nil, nil, counter, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestHandler_Counter_IncrementOnCacheMiss(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"flags":{}}`)
	}))
	defer upstream.Close()

	counter := telemetry.NewCounter()
	h := newTestHandlerWithCounter(t, upstream, counter)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	samples := counter.Drain()
	if len(samples) != 1 || samples[0].Count != 1 {
		t.Errorf("want 1 sample with Count=1 after cache miss, got %v", samples)
	}
}

func TestHandler_Counter_IncrementOnCacheHit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	c.Set(sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"), cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})
	counter := telemetry.NewCounter()
	h := newTestHandlerWithCacheAndCounter(t, upstream, c, counter)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	samples := counter.Drain()
	if len(samples) != 1 || samples[0].Count != 1 {
		t.Errorf("want 1 sample with Count=1 after cache hit, got %v", samples)
	}
}

func TestHandler_Counter_IncrementOnUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	counter := telemetry.NewCounter()
	h := newTestHandlerWithCounter(t, upstream, counter)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (CDN code forwarded), got %d", rec.Code)
	}
	samples := counter.Drain()
	if len(samples) != 1 || samples[0].Count != 1 {
		t.Errorf("want 1 sample with Count=1 even on upstream error, got %v", samples)
	}
}

func TestHandler_Counter_IncrementOnHEAD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	c.Set(sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"), cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json",
	})
	counter := telemetry.NewCounter()
	h := newTestHandlerWithCacheAndCounter(t, upstream, c, counter)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	samples := counter.Drain()
	if len(samples) != 1 || samples[0].Count != 1 {
		t.Errorf("want 1 sample with Count=1 for HEAD request, got %v", samples)
	}
}

func TestHandler_Counter_NoIncrementOn405(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	counter := telemetry.NewCounter()
	h := newTestHandlerWithCounter(t, upstream, counter)

	// Method filtering is a router-level concern. Test via a Gin router.
	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.GET("/server/:file", h.Handle)
	router.HEAD("/server/:file", h.Handle)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
		router.ServeHTTP(rec, req)
	}

	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("405 requests must not increment counter, got %v", samples)
	}
}

func TestHandler_Counter_NoIncrementOnMalformedPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	counter := telemetry.NewCounter()
	h := newTestHandlerWithCounter(t, upstream, counter)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/notjson", nil)
	h.Handle(ginContext(rec, req))

	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("malformed path must not increment counter, got %v", samples)
	}
}

func TestHandler_Counter_NoIncrementOnUnrecognizedKeyFormat(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	counter := telemetry.NewCounter()
	h := newTestHandlerWithCounter(t, upstream, counter)

	for _, path := range []string{"/server/bogus.json", "/server/dt01.server_us_ab3xy9kp2q.json"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.Handle(ginContext(rec, req))
	}

	if samples := counter.Drain(); len(samples) != 0 {
		t.Errorf("unrecognized key format must not increment counter, got %v", samples)
	}
}

// --- Snapshot integration tests ---

type snapshotCall struct {
	keyHash      string
	etag         string
	lastModified time.Time
	payload      []byte
}

type mockSnapshotNotifier struct {
	mu    sync.Mutex
	calls []snapshotCall
}

func (m *mockSnapshotNotifier) OnConfigCached(keyHash string, entry cache.Entry) {
	m.mu.Lock()
	m.calls = append(m.calls, snapshotCall{keyHash, entry.ETag, entry.LastModified, entry.Payload})
	m.mu.Unlock()
}

func TestHandler_PassesThroughUpstreamLastModified(t *testing.T) {
	const lastModified = "Sun, 03 May 2026 12:00:00 GMT"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Last-Modified", lastModified)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"flags":{}}`)
	}))
	defer upstream.Close()

	notifier := &mockSnapshotNotifier{}
	h, err := New("fm.test", newRedirectingClient(upstream), cache.New(), nil, notifier, nil, nil, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, label := range []string{"miss", "hit"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
		h.Handle(ginContext(rec, req))
		if got := rec.Header().Get("Last-Modified"); got != lastModified {
			t.Errorf("%s: Last-Modified = %q, want %q", label, got, lastModified)
		}
		if got := rec.Header().Get("ETag"); got != `"v1"` {
			t.Errorf("%s: ETag = %q, want %q", label, got, `"v1"`)
		}
	}

	want, _ := http.ParseTime(lastModified)
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.calls) != 1 || !notifier.calls[0].lastModified.Equal(want) {
		t.Errorf("snapshot notifier calls = %+v, want one with lastModified %v", notifier.calls, want)
	}
}

func TestFetcher_CapturesUpstreamLastModified(t *testing.T) {
	cases := map[string]time.Time{
		"Sun, 03 May 2026 12:00:00 GMT": time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC),
		"":                              {},
		"not a date":                    {},
	}
	for header, want := range cases {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if header != "" {
				w.Header().Set("Last-Modified", header)
			}
			w.Header().Set("ETag", `"v2"`)
			_, _ = io.WriteString(w, `{}`)
		}))
		res, err := NewFetcher(upstream.Client(), slog.Default(), 1<<20).Fetch(context.Background(), "k", upstream.URL, "")
		upstream.Close()
		if err != nil || res.Entry == nil {
			t.Fatalf("Last-Modified %q: Fetch: %+v, %v", header, res, err)
		}
		if !res.Entry.LastModified.Equal(want) || res.Entry.ETag != `"v2"` {
			t.Errorf("Last-Modified %q: got etag %q lastModified %v, want %q %v", header, res.Entry.ETag, res.Entry.LastModified, `"v2"`, want)
		}
	}
}

func TestHandler_CacheMiss_InvokesSnapshotNotifier(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"flags":{}}`)
	}))
	defer upstream.Close()

	notifier := &mockSnapshotNotifier{}
	h, err := New("fm.test", newRedirectingClient(upstream), cache.New(), nil, notifier, nil, nil, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.calls) != 1 {
		t.Fatalf("expected 1 snapshot notifier call, got %d", len(notifier.calls))
	}
	if got := notifier.calls[0].keyHash; got != sdkkey.PublicID("dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a") {
		t.Errorf("wrong keyHash: got %q", got)
	}
	if got := notifier.calls[0].etag; got != `"v1"` {
		t.Errorf("wrong etag: got %q", got)
	}
}

func TestHandler_SnapshotRestoredEntry_HydratesFetchURL(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called on cache hit")
	}))
	defer upstream.Close()

	c := cache.New()
	sdkKey := "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	key := sdkkey.PublicID(sdkKey)
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      `"v1"`,
		FetchedAt: time.Now(),
		FetchURL:  "", // snapshot-restored: FetchURL not persisted
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/"+sdkKey+".json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	got, _ := c.Get(key)
	if got.FetchURL == "" {
		t.Error("expected FetchURL to be hydrated on first SDK hit, got empty string")
	}
}

// --- Singleflight deduplication tests ---

// TestHandler_ColdMiss_Singleflight_OneUpstreamCall verifies that N concurrent
// cold-miss requests for the same SDK key result in exactly one upstream call,
// and that every waiter receives an identical 200 response.
func TestHandler_ColdMiss_Singleflight_OneUpstreamCall(t *testing.T) {
	const n = 8
	var upstreamCalls atomic.Int64

	called := make(chan struct{}, 1) // signals when the leader reaches upstream
	unblock := make(chan struct{})   // closed to release the upstream handler

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		select {
		case called <- struct{}{}: // notify test that the leader is in-flight
		default:
		}
		<-unblock
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"flags":{}}`)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)

	responses := make([]*httptest.ResponseRecorder, n)
	for i := range n {
		responses[i] = httptest.NewRecorder()
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		rec := responses[i]
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
			h.Handle(ginContext(rec, req))
		}()
	}

	// Wait for the leader to be in-flight at the upstream, then give followers
	// time to queue behind the singleflight group before releasing.
	<-called
	time.Sleep(50 * time.Millisecond)
	close(unblock)
	wg.Wait()

	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("expected exactly 1 upstream call, got %d", got)
	}
	for i, rec := range responses {
		if rec.Code != http.StatusOK {
			t.Errorf("response[%d]: expected 200, got %d", i, rec.Code)
		}
		if rec.Body.String() != `{"flags":{}}` {
			t.Errorf("response[%d]: unexpected body %q", i, rec.Body.String())
		}
	}
}

// TestHandler_ColdMiss_Singleflight_ErrorShared verifies that when the upstream
// returns 5xx, all concurrent waiters for the same key each receive a 503.
func TestHandler_ColdMiss_Singleflight_ErrorShared(t *testing.T) {
	const n = 4

	called := make(chan struct{}, 1)
	unblock := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		<-unblock
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)

	responses := make([]*httptest.ResponseRecorder, n)
	for i := range n {
		responses[i] = httptest.NewRecorder()
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		rec := responses[i]
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
			h.Handle(ginContext(rec, req))
		}()
	}

	<-called
	time.Sleep(50 * time.Millisecond)
	close(unblock)
	wg.Wait()

	for i, rec := range responses {
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("response[%d]: expected 500 (CDN code forwarded), got %d", i, rec.Code)
		}
	}
}

// TestHandler_ColdMiss_Singleflight_SideEffectsOnce verifies that side effects
// (snapshotNotifier.OnConfigCached) fire exactly once even when N concurrent
// goroutines share a single cold-miss upstream fetch via singleflight.
func TestHandler_ColdMiss_Singleflight_SideEffectsOnce(t *testing.T) {
	const n = 6

	called := make(chan struct{}, 1)
	unblock := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case called <- struct{}{}:
		default:
		}
		<-unblock
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"flags":{}}`)
	}))
	defer upstream.Close()

	notifier := &mockSnapshotNotifier{}
	h, err := New("fm.test", newRedirectingClient(upstream), cache.New(), nil, notifier, nil, nil, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json", nil)
			h.Handle(ginContext(httptest.NewRecorder(), req))
		}()
	}

	<-called
	time.Sleep(50 * time.Millisecond)
	close(unblock)
	wg.Wait()

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.calls) != 1 {
		t.Errorf("expected snapshotNotifier called exactly once, got %d calls", len(notifier.calls))
	}
}

// --- Fetcher unit tests ---

func TestFetcher_200_ReturnsEntry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"flags":{}}`)
	}))
	defer upstream.Close()

	f := NewFetcher(&http.Client{}, slog.Default(), 10*1024*1024)
	result, err := f.Fetch(context.Background(), "keyhash", upstream.URL+"/config.json", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.Entry == nil {
		t.Fatal("expected Entry to be set on 200")
	}
	if string(result.Entry.Payload) != `{"flags":{}}` {
		t.Errorf("unexpected payload: %q", string(result.Entry.Payload))
	}
	if result.StatusCode != 0 {
		t.Errorf("StatusCode should be zero on 200, got %d", result.StatusCode)
	}
}

func TestFetcher_304_ReturnsNotModified(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer upstream.Close()

	f := NewFetcher(&http.Client{}, slog.Default(), 10*1024*1024)
	result, err := f.Fetch(context.Background(), "keyhash", upstream.URL+"/config.json", `"v1"`)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.NotModified {
		t.Error("expected NotModified=true on 304")
	}
	if result.StatusCode != 0 {
		t.Errorf("StatusCode should be zero on 304, got %d", result.StatusCode)
	}
}

func TestFetcher_4xx_ReturnsStatusCode(t *testing.T) {
	for _, code := range []int{401, 403, 404, 410, 429} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer upstream.Close()

			f := NewFetcher(&http.Client{}, slog.Default(), 10*1024*1024)
			result, err := f.Fetch(context.Background(), "keyhash", upstream.URL+"/config.json", "")
			if err != nil {
				t.Fatalf("status %d: expected no error, got %v", code, err)
			}
			if result.StatusCode != code {
				t.Errorf("status %d: expected StatusCode=%d, got %d", code, code, result.StatusCode)
			}
			if result.Entry != nil {
				t.Errorf("status %d: Entry must be nil on non-200", code)
			}
			if result.NotModified {
				t.Errorf("status %d: NotModified must be false on non-304", code)
			}
		})
	}
}

func TestFetcher_5xx_ReturnsStatusCode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	f := NewFetcher(&http.Client{}, slog.Default(), 10*1024*1024)
	result, err := f.Fetch(context.Background(), "keyhash", upstream.URL+"/config.json", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected StatusCode=500, got %d", result.StatusCode)
	}
}

// --- Demand revalidation tests ---

// mockFetcher is a controllable implementation of cache.Fetcher for testing.
type mockFetcher struct {
	mu     sync.Mutex
	calls  int
	result cache.FetchResult
	err    error
	// block is an optional channel; if set, Fetch blocks until it is closed.
	block chan struct{}
}

func (m *mockFetcher) Fetch(_ context.Context, _, _, _ string) (cache.FetchResult, error) {
	if m.block != nil {
		<-m.block
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return m.result, m.err
}

func (m *mockFetcher) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

const testSDKKey = "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
const testKeyID = "dt01.server_us_ab3xy9kp2q"
const testFetchURL = "https://cdn.us.fm.test/server/dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a.json"

// staleSnapshotEntry returns an entry with FetchURL="" and FetchedAt far enough
// in the past to be stale under RefreshIntervalDisconnected (10s).
func staleSnapshotEntry(payload []byte, etag string) cache.Entry {
	return cache.Entry{
		Payload:   payload,
		ETag:      etag,
		FetchedAt: time.Now().Add(-1 * time.Hour),
		FetchURL:  "", // snapshot-loaded marker
	}
}

func newHandlerWithFetcher(t *testing.T, c *cache.Cache, f cache.Fetcher) *Handler {
	t.Helper()
	// Upstream is not expected to be called for snapshot-loaded entries.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call to proxy upstream (demand reval should use refreshFetcher)")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	h, err := New("fm.test", newRedirectingClient(upstream), c, nil, nil, f, nil, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func newHandlerWithFetcherAndNotifier(t *testing.T, c *cache.Cache, f cache.Fetcher, notifier SnapshotNotifier) *Handler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected call to proxy upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	h, err := New("fm.test", newRedirectingClient(upstream), c, nil, notifier, f, nil, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestHandler_DemandReval_StaleSnapshot_304_ServesCachedPayload(t *testing.T) {
	c := cache.New()
	payload := []byte(`{"flags":{}}`)
	c.Set(testKeyID, staleSnapshotEntry(payload, `"etag1"`))

	f := &mockFetcher{result: cache.FetchResult{NotModified: true}}
	h := newHandlerWithFetcher(t, c, f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if f.callCount() != 1 {
		t.Errorf("expected 1 fetcher call, got %d", f.callCount())
	}
	// Verify FetchedAt was reset (entry is no longer stale).
	entry, ok := c.Get(testKeyID)
	if !ok {
		t.Fatal("entry should still be in cache after 304")
	}
	if entry.IsStale(time.Now(), 10*time.Second) {
		t.Error("entry should not be stale after 304 demand reval")
	}
	if entry.FetchURL == "" {
		t.Error("FetchURL should be repaired after first hit")
	}
}

func TestHandler_DemandReval_StaleSnapshot_304_NoSnapshotWrite(t *testing.T) {
	c := cache.New()
	c.Set(testKeyID, staleSnapshotEntry([]byte(`{"flags":{}}`), `"etag1"`))

	f := &mockFetcher{result: cache.FetchResult{NotModified: true}}
	notifier := &mockSnapshotNotifier{}
	h := newHandlerWithFetcherAndNotifier(t, c, f, notifier)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
	h.Handle(ginContext(rec, req))

	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.calls) != 0 {
		t.Errorf("snapshot notifier must NOT be called on 304, got %d calls", len(notifier.calls))
	}
}

func TestHandler_DemandReval_StaleSnapshot_200_ServesFreshPayload(t *testing.T) {
	c := cache.New()
	c.Set(testKeyID, staleSnapshotEntry([]byte(`{"flags":{"old":true}}`), `"etag1"`))

	freshPayload := []byte(`{"flags":{"new":true}}`)
	f := &mockFetcher{result: cache.FetchResult{
		Entry: &cache.Entry{
			Payload:   freshPayload,
			ETag:      `"etag2"`,
			FetchedAt: time.Now(),
		},
	}}
	notifier := &mockSnapshotNotifier{}
	h := newHandlerWithFetcherAndNotifier(t, c, f, notifier)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// Fresh payload is served (SSE URL rewrite may transform it; check for key content).
	if !strings.Contains(rec.Body.String(), `"new":true`) {
		t.Errorf("expected fresh payload in response, got: %q", rec.Body.String())
	}
	// Cache must hold the fresh entry.
	entry, ok := c.Get(testKeyID)
	if !ok {
		t.Fatal("entry missing from cache after 200 demand reval")
	}
	if string(entry.Payload) != string(freshPayload) {
		t.Errorf("cache has stale payload after 200: %q", string(entry.Payload))
	}
	// Snapshot notifier must be called once.
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	if len(notifier.calls) != 1 {
		t.Errorf("expected snapshot notifier called once after 200, got %d", len(notifier.calls))
	}
}

func TestHandler_DemandReval_StaleSnapshot_NetworkError_ServesStale(t *testing.T) {
	c := cache.New()
	c.Set(testKeyID, staleSnapshotEntry([]byte(`{"flags":{}}`), `"etag1"`))

	f := &mockFetcher{err: errors.New("dial tcp: connection refused")}
	h := newHandlerWithFetcher(t, c, f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected stale 200 on fetch error, got %d", rec.Code)
	}
	// Entry must still be present (not evicted).
	if _, ok := c.Get(testKeyID); !ok {
		t.Error("entry must not be evicted on fetch error")
	}
}

func TestHandler_DemandReval_StaleSnapshot_4xx_Evicts(t *testing.T) {
	for _, code := range []int{401, 403, 404, 410} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c := cache.New()
			c.Set(testKeyID, staleSnapshotEntry([]byte(`{"flags":{}}`), `"etag1"`))

			f := &mockFetcher{result: cache.FetchResult{StatusCode: code}}
			evictorCalled := false
			h := newHandlerWithFetcher(t, c, f)
			h.SetEvictor(func(string) { evictorCalled = true })

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
			h.Handle(ginContext(rec, req))

			// The upstream status code is passed through so providers can respond
			// appropriately (e.g. 401 → re-authenticate, 404 → stop polling).
			if rec.Code != code {
				t.Errorf("status %d: expected pass-through status %d, got %d", code, code, rec.Code)
			}
			if _, ok := c.Get(testKeyID); ok {
				t.Errorf("status %d: entry must be evicted from cache", code)
			}
			if !evictorCalled {
				t.Errorf("status %d: evictor callback must be called", code)
			}
		})
	}
}

func TestHandler_DemandReval_StaleSnapshot_429And5xx_PreservesStale(t *testing.T) {
	for _, code := range []int{429, 500, 502, 503, 504} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			c := cache.New()
			c.Set(testKeyID, staleSnapshotEntry([]byte(`{"flags":{}}`), `"etag1"`))

			f := &mockFetcher{result: cache.FetchResult{StatusCode: code}}
			h := newHandlerWithFetcher(t, c, f)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
			h.Handle(ginContext(rec, req))

			if rec.Code != http.StatusOK {
				t.Errorf("status %d: expected stale 200, got %d", code, rec.Code)
			}
			if _, ok := c.Get(testKeyID); !ok {
				t.Errorf("status %d: entry must not be evicted on %d", code, code)
			}
		})
	}
}

func TestHandler_DemandReval_FreshSnapshotEntry_NoDemandReval(t *testing.T) {
	c := cache.New()
	// FetchURL is empty (snapshot-loaded) but FetchedAt is very recent — not stale.
	c.Set(testKeyID, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      `"etag1"`,
		FetchedAt: time.Now(), // fresh
		FetchURL:  "",
	})

	f := &mockFetcher{result: cache.FetchResult{NotModified: true}}
	h := newHandlerWithFetcher(t, c, f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if f.callCount() != 0 {
		t.Errorf("expected no fetcher call for fresh entry, got %d", f.callCount())
	}
}

func TestHandler_DemandReval_NotFiredForNormalCacheEntry(t *testing.T) {
	c := cache.New()
	// FetchURL is set (normal cold-fetched entry, even if stale).
	c.Set(testKeyID, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      `"etag1"`,
		FetchedAt: time.Now().Add(-1 * time.Hour), // stale
		FetchURL:  testFetchURL,                   // already set — NOT snapshot-loaded
	})

	f := &mockFetcher{result: cache.FetchResult{NotModified: true}}
	h := newHandlerWithFetcher(t, c, f)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if f.callCount() != 0 {
		t.Errorf("demand reval must not fire for entries with FetchURL set, got %d calls", f.callCount())
	}
}

func TestHandler_DemandReval_SecondRequest_SkipsDemandReval(t *testing.T) {
	c := cache.New()
	c.Set(testKeyID, staleSnapshotEntry([]byte(`{"flags":{}}`), `"etag1"`))

	f := &mockFetcher{result: cache.FetchResult{NotModified: true}}
	h := newHandlerWithFetcher(t, c, f)

	sendRequest := func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
		h.Handle(ginContext(rec, req))
		if rec.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rec.Code)
		}
	}

	// First request: stale snapshot entry → demand reval fires.
	sendRequest()
	if f.callCount() != 1 {
		t.Fatalf("expected 1 fetcher call on first request, got %d", f.callCount())
	}

	// Second request: FetchURL is now set (repaired) → no demand reval.
	sendRequest()
	if f.callCount() != 1 {
		t.Errorf("expected no additional fetcher calls on second request, got %d total", f.callCount())
	}
}

func TestHandler_DemandReval_ConcurrentRequests_OneUpstreamCall(t *testing.T) {
	c := cache.New()
	c.Set(testKeyID, staleSnapshotEntry([]byte(`{"flags":{}}`), `"etag1"`))

	block := make(chan struct{})
	f := &mockFetcher{
		result: cache.FetchResult{NotModified: true},
		block:  block,
	}
	h := newHandlerWithFetcher(t, c, f)

	const n = 10
	var wg sync.WaitGroup
	codes := make([]int, n)
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
			h.Handle(ginContext(rec, req))
			codes[i] = rec.Code
		}(i)
	}

	// Let all goroutines reach the singleflight gate, then unblock.
	time.Sleep(20 * time.Millisecond)
	close(block)
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("goroutine %d: expected 200, got %d", i, code)
		}
	}
	if f.callCount() != 1 {
		t.Errorf("expected exactly 1 fetcher call across %d concurrent requests, got %d", n, f.callCount())
	}
}

// --- LimitReader / max response size tests ---

func TestHandler_ColdFetch_OversizeResponse_Returns502(t *testing.T) {
	const limit = 100
	oversized := strings.Repeat("x", limit+1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"big"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, oversized)
	}))
	defer upstream.Close()

	c := cache.New()
	h, err := New("fm.test", newRedirectingClient(upstream), c, nil, nil, nil, nil, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.SetMaxResponseBytes(limit)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d", rec.Code)
	}

	key := sdkkey.PublicID(testSDKKey)
	if _, ok := c.Get(key); ok {
		t.Error("oversized response must not be stored in cache")
	}
}

func TestFetcher_OversizeResponse_ReturnsError(t *testing.T) {
	const limit = 50
	oversized := strings.Repeat("y", limit+1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"big"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, oversized)
	}))
	defer upstream.Close()

	f := NewFetcher(&http.Client{Transport: newRedirectingClient(upstream).Transport}, slog.Default(), limit)
	_, err := f.Fetch(context.Background(), "", upstream.URL+"/server/"+testSDKKey+".json", "")
	if err == nil {
		t.Error("expected error for oversized refresh response, got nil")
	}
}
