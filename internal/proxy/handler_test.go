package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestHandler(t *testing.T, upstream *httptest.Server) *Handler {
	t.Helper()
	h, err := New(upstream.URL, upstream.Client(), slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestHandler_HappyPath(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"flags":{}}`)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/my-sdk-key.json", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
	if etag := rec.Header().Get("ETag"); etag != `"abc123"` {
		t.Errorf("expected ETag forwarded, got %q", etag)
	}
	if body := rec.Body.String(); body != `{"flags":{}}` {
		t.Errorf("unexpected body: %q", body)
	}
}

func TestHandler_PathForwardedToUpstream(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/my-sdk-key.json", nil)
	h.ServeHTTP(rec, req)

	if gotPath != "/v1/config/my-sdk-key.json" {
		t.Errorf("upstream received path %q, want /v1/config/my-sdk-key.json", gotPath)
	}
}

func TestHandler_QueryStringPassthrough(t *testing.T) {
	var gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/my-sdk-key.json?foo=bar", nil)
	h.ServeHTTP(rec, req)

	if gotQuery != "foo=bar" {
		t.Errorf("upstream received query %q, want foo=bar", gotQuery)
	}
}

func TestHandler_IfNoneMatchForwardedToUpstream(t *testing.T) {
	var gotHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/my-sdk-key.json", nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	h.ServeHTTP(rec, req)

	if gotHeader != `"abc123"` {
		t.Errorf("upstream received If-None-Match %q, want \"abc123\"", gotHeader)
	}
}

func TestHandler_Upstream304PassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/my-sdk-key.json", nil)
	req.Header.Set("If-None-Match", `"abc123"`)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("expected 304, got %d", rec.Code)
	}
	if etag := rec.Header().Get("ETag"); etag != `"abc123"` {
		t.Errorf("expected ETag forwarded, got %q", etag)
	}
}

func TestHandler_UpstreamServerError_Returns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/my-sdk-key.json", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHandler_NetworkError_Returns503(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstreamURL := upstream.URL
	upstream.Close() // close before request so the connection is refused

	h, err := New(upstreamURL, http.DefaultClient, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/my-sdk-key.json", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHandler_UpstreamClientError_PassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "forbidden")
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/config/my-sdk-key.json", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "forbidden") {
		t.Errorf("expected upstream body forwarded, got %q", rec.Body.String())
	}
}

func TestHandler_NonGetMethod_Returns405(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()

	h := newTestHandler(t, upstream)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/config/my-sdk-key.json", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/v1/config/my-sdk-key", nil)
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
