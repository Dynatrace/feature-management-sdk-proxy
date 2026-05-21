package evalmetrics

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
)

func init() {
	gin.SetMode(gin.TestMode)
}

const (
	testSDKKeyUS   = "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	testSDKKeyEU   = "dt01.server_eu_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	testSDKKeyAP   = "dt01.server_ap_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	testBaseDomain = "fm.test"
)

var testTracer = tracenoop.NewTracerProvider().Tracer("test")

// roundTripFunc adapts a function to the http.RoundTripper interface.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newRedirectingClient routes all requests to a specific test server, ignoring Host/scheme.
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

// ginContext creates a *gin.Context for POST /v1/metrics.
func ginContext(w *httptest.ResponseRecorder, r *http.Request) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = r
	return c
}

func newHandler(t *testing.T, upstream *httptest.Server) *Handler {
	t.Helper()
	h, err := New(testBaseDomain, newRedirectingClient(upstream), nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// --- Happy path ---

func TestHandle_HappyPath_202Forwarded(t *testing.T) {
	const payload = `{"version":"1.0","sdkType":"server","sdkVersion":"1.0.0","providerName":"test","evaluations":[]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"accepted":true,"recordCount":0}`)
	}))
	defer upstream.Close()

	h := newHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+testSDKKeyUS)
	req.Header.Set("Content-Type", "application/json")
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != `{"accepted":true,"recordCount":0}` {
		t.Errorf("unexpected body: %q", body)
	}
}

// --- Status code pass-through ---

func TestHandle_StatusPassThrough(t *testing.T) {
	cases := []struct {
		name         string
		upstreamCode int
	}{
		{"400", http.StatusBadRequest},
		{"401", http.StatusUnauthorized},
		{"422", http.StatusUnprocessableEntity},
		{"500", http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Custom", "value")
				w.WriteHeader(tc.upstreamCode)
				_, _ = io.WriteString(w, "error body")
			}))
			defer upstream.Close()

			h := newHandler(t, upstream)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+testSDKKeyUS)
			h.Handle(ginContext(rec, req))

			if rec.Code != tc.upstreamCode {
				t.Errorf("expected %d, got %d", tc.upstreamCode, rec.Code)
			}
			if rec.Body.String() != "error body" {
				t.Errorf("expected error body, got %q", rec.Body.String())
			}
			if rec.Header().Get("X-Custom") != "value" {
				t.Errorf("expected X-Custom header to be forwarded")
			}
		})
	}
}

// --- Authorization failures ---

func TestHandle_MissingAuthorization_Returns401(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	h := newHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if upstreamCalled != 0 {
		t.Errorf("expected no upstream calls, got %d", upstreamCalled)
	}
}

func TestHandle_NonBearerScheme_Returns401(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	h := newHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Basic sometoken")
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if upstreamCalled != 0 {
		t.Errorf("expected no upstream calls, got %d", upstreamCalled)
	}
}

// --- Malformed SDK key ---

func TestHandle_MalformedSDKKey_Returns400_NoUpstreamCall(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	h := newHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer bogus-key")
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if upstreamCalled != 0 {
		t.Errorf("expected no upstream calls, got %d", upstreamCalled)
	}
}

// --- Payload too large ---

func TestHandle_OversizedBody_Returns413_NoUpstreamCall(t *testing.T) {
	upstreamCalled := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	h := newHandler(t, upstream)
	h.SetMaxRequestBytes(10)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", bytes.NewReader(make([]byte, 11)))
	req.Header.Set("Authorization", "Bearer "+testSDKKeyUS)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
	if upstreamCalled != 0 {
		t.Errorf("expected no upstream calls, got %d", upstreamCalled)
	}
}

// --- Network failure ---

func TestHandle_NetworkError_Returns502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	client := newRedirectingClient(upstream)
	upstream.Close()

	h, err := New(testBaseDomain, client, nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testSDKKeyUS)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

// --- Hop-by-hop headers stripped ---

func TestHandle_HopByHopHeadersStripped(t *testing.T) {
	var gotHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	h := newHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testSDKKeyUS)
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("X-Custom-Header", "should-pass")
	h.Handle(ginContext(rec, req))

	if gotHeader.Get("Connection") != "" {
		t.Error("Connection hop-by-hop header should have been stripped")
	}
	if gotHeader.Get("Keep-Alive") != "" {
		t.Error("Keep-Alive hop-by-hop header should have been stripped")
	}
	if gotHeader.Get("X-Custom-Header") != "should-pass" {
		t.Error("X-Custom-Header should have been forwarded")
	}
}

// --- Authorization header not duplicated ---

func TestHandle_AuthorizationNotDuplicated(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	h := newHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testSDKKeyUS)
	h.Handle(ginContext(rec, req))

	if gotAuth != "Bearer "+testSDKKeyUS {
		t.Errorf("unexpected Authorization header: %q", gotAuth)
	}
}

// --- Payload opaque: body forwarded byte-identical ---

func TestHandle_BodyForwardedByteIdentical(t *testing.T) {
	const payload = `{"version":"1.0","sdkType":"web","sdkVersion":"2.0.0","providerName":"test","evaluations":[{"flagKey":"f","variant":"on","evaluationReason":"STATIC","timestampSec":1000,"count":5}]}`
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	h := newHandler(t, upstream)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+testSDKKeyUS)
	h.Handle(ginContext(rec, req))

	if string(gotBody) != payload {
		t.Errorf("body not forwarded byte-identical:\ngot:  %q\nwant: %q", gotBody, payload)
	}
}

// --- Geo derivation: correct host per SDK key region ---

func TestHandle_GeoDerivation(t *testing.T) {
	cases := []struct {
		name    string
		sdkKey  string
		wantGeo string
	}{
		{"us", testSDKKeyUS, "us"},
		{"eu", testSDKKeyEU, "eu"},
		{"ap", testSDKKeyAP, "ap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
			}))
			defer upstream.Close()

			addr := upstream.Listener.Addr().String()
			client := &http.Client{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					wantHost := "metrics." + tc.wantGeo + "." + testBaseDomain
					if r.URL.Host != wantHost {
						t.Errorf("upstream URL host = %q, want %q", r.URL.Host, wantHost)
					}
					r2 := r.Clone(r.Context())
					r2.URL.Scheme = "http"
					r2.URL.Host = addr
					return http.DefaultTransport.RoundTrip(r2)
				}),
			}

			h, err := New(testBaseDomain, client, nil, slog.Default(), testTracer)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+tc.sdkKey)
			h.Handle(ginContext(rec, req))
		})
	}
}

// --- Raw SDK key never in logs (public ID used) ---

func TestHandle_RawSDKKeyNotLeakedInLogs(t *testing.T) {
	// Force a network error (502) with a valid SDK key so the warn log is emitted.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	client := newRedirectingClient(upstream)
	upstream.Close()

	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h, err := New(testBaseDomain, client, nil, logger, testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/metrics", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+testSDKKeyUS)
	h.Handle(ginContext(rec, req))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}

	logs := logOutput.String()
	if strings.Contains(logs, testSDKKeyUS) {
		t.Error("raw SDK key appeared in log output")
	}
	keyID := cache.PublicID(testSDKKeyUS)
	if !strings.Contains(logs, keyID) {
		t.Errorf("expected public ID %q in log output, got: %s", keyID, logs)
	}
}

// --- Router-level method enforcement ---

func TestHandle_WrongMethod_Returns405(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	h, err := New(testBaseDomain, newRedirectingClient(upstream), nil, slog.Default(), testTracer)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.POST("/v1/metrics", h.Handle)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v1/metrics", nil)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: expected 405, got %d", method, rec.Code)
		}
	}
}

// --- extractBearer unit tests ---

func TestExtractBearer(t *testing.T) {
	cases := []struct {
		header    string
		wantToken string
		wantOK    bool
	}{
		{"Bearer mytoken", "mytoken", true},
		{"Bearer  spaced  ", "spaced", true},
		{"Basic abc123", "", false},
		{"", "", false},
		{"Bearer ", "", false},
		{"bearer mytoken", "", false}, // case-sensitive per spec
	}
	for _, tc := range cases {
		token, ok := extractBearer(tc.header)
		if ok != tc.wantOK || token != tc.wantToken {
			t.Errorf("extractBearer(%q) = (%q, %v), want (%q, %v)", tc.header, token, ok, tc.wantToken, tc.wantOK)
		}
	}
}
