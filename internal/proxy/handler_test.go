package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dynatrace/sdk-proxy/internal/cache"
)

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
	h, err := New("fm.test", newRedirectingClient(upstream), cache.New(), nil, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func newTestHandlerWithCache(t *testing.T, upstream *httptest.Server, c *cache.Cache) *Handler {
	t.Helper()
	h, err := New("fm.test", newRedirectingClient(upstream), c, nil, slog.Default())
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
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	h.ServeHTTP(rec, req)

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
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	h.ServeHTTP(rec, req)

	key := cache.Key("dt_server_us_abc123")
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
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	h.ServeHTTP(rec, req)

	if gotPath != "/server/dt_server_us_abc123.json" {
		t.Errorf("upstream received path %q, want /server/dt_server_us_abc123.json", gotPath)
	}
}

func TestHandler_CacheMiss_Upstream304_Returns503(t *testing.T) {
	// A 304 on a cold fetch is unexpected and must not return empty config to the SDK.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHandler_CacheMiss_UpstreamServerError_Returns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHandler_CacheMiss_NetworkError_Returns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	client := newRedirectingClient(upstream)
	upstream.Close() // close after capturing the address in the client

	h, err := New("fm.test", client, cache.New(), nil, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	h.ServeHTTP(rec, req)

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
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	h.ServeHTTP(rec, req)

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
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	h.ServeHTTP(rec, req)

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
	key := cache.Key("dt_server_us_abc123")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{"foo":true}}`),
		ETag:      `"v1"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt_server_us_abc123.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	h.ServeHTTP(rec, req)

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
	key := cache.Key("dt_server_us_abc123")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      `"abc123"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt_server_us_abc123.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	h.ServeHTTP(rec, req)

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
	key := cache.Key("dt_server_us_abc123")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{"updated":true}}`),
		ETag:      `"v2"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt_server_us_abc123.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	req.Header.Set("If-None-Match", `"v1"`) // stale client version
	h.ServeHTTP(rec, req)

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
	key := cache.Key("dt_server_us_abc123")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      "", // no ETag stored
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt_server_us_abc123.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123.json", nil)
	req.Header.Set("If-None-Match", `"anything"`)
	h.ServeHTTP(rec, req)

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
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/config/dt_server_us_abc123.json", nil)
		h.ServeHTTP(rec, req)
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
	req := httptest.NewRequest(http.MethodGet, "/v1/config/dt_server_us_abc123", nil)
	h.ServeHTTP(rec, req)

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
	h.ServeHTTP(rec, req)

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
		"/v1/config/bogus.json",
		"/v1/config/my-sdk-key.json",
		"/v1/config/dt_client_us_abc.json",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rec, req)
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
	key := cache.Key("dt_server_us_abc123")
	c.Set(key, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      `"v1"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://cdn.us.fm.test/server/dt_server_us_abc123.json",
	})

	h := newTestHandlerWithCache(t, upstream, c)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/v1/config/dt_server_us_abc123.json", nil)
	h.ServeHTTP(rec, req)

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
		_, err := New(tc, http.DefaultClient, cache.New(), nil, slog.Default())
		if err == nil {
			t.Errorf("New(%q): expected error, got nil", tc)
		}
	}
}

func TestExtractSDKKey(t *testing.T) {
	cases := []struct {
		path    string
		wantKey string
		wantOK  bool
	}{
		{"/v1/config/abc123.json", "abc123", true},
		{"/abc123.json", "abc123", true},
		{"noslash.json", "noslash", true},
		{"/.json", "", false},
		{"/config/noextension", "", false},
		{"/config/", "", false},
		{"/", "", false},
	}
	for _, tc := range cases {
		key, ok := extractSDKKey(tc.path)
		if ok != tc.wantOK || key != tc.wantKey {
			t.Errorf("extractSDKKey(%q) = (%q, %v), want (%q, %v)", tc.path, key, ok, tc.wantKey, tc.wantOK)
		}
	}
}

func TestExtractRegion(t *testing.T) {
	cases := []struct {
		sdkKey     string
		wantRegion string
		wantOK     bool
	}{
		{"dt_server_us_abc123", "us", true},
		{"dt_server_eu_abc123", "eu", true},
		{"dt_server_eu_tok_with_underscores", "eu", true},   // token may contain underscores
		{"dt_client_us_abc", "", false},                    // wrong second segment
		{"dt_server__abc", "", false},                      // empty region
		{"dt_server_us_", "", false},                       // empty token
		{"dt_server_us", "", false},                        // too few parts
		{"dt_server_US_abc", "", false},                    // uppercase region rejected
		{"dt_server_us.east_abc", "", false},               // dot in region rejected (SSRF guard)
		{"dt_server_evil.com:8080_abc", "", false},         // port in region rejected (SSRF guard)
		{"bogus", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		region, ok := extractRegion(tc.sdkKey)
		if ok != tc.wantOK || region != tc.wantRegion {
			t.Errorf("extractRegion(%q) = (%q, %v), want (%q, %v)", tc.sdkKey, region, ok, tc.wantRegion, tc.wantOK)
		}
	}
}

func TestBuildTarget_ConstructsGeoBoundURL(t *testing.T) {
	h := &Handler{cdnBaseDomain: "fm.dynatracelabs.com"}
	got, ok := h.buildTarget("dt_server_us_abc123")
	if !ok {
		t.Fatal("buildTarget returned ok=false for valid SDK key")
	}
	want := "https://cdn.us.fm.dynatracelabs.com/server/dt_server_us_abc123.json"
	if got != want {
		t.Errorf("buildTarget = %q, want %q", got, want)
	}
}
