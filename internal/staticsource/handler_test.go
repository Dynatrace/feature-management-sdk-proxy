package staticsource

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
)

func init() {
	gin.SetMode(gin.TestMode)
}

var testTracer = tracenoop.NewTracerProvider().Tracer("test")

func storeFromRaw(t *testing.T, raw string) *Store {
	t.Helper()
	res, err := build([]byte(raw))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := New()
	s.install(res.entries)
	return s
}

// ginRequest builds a *gin.Context for GET/HEAD /server/<file> with the
// FromPathFile middleware applied (extracts the SDK key from the :file param).
func ginRequest(w *httptest.ResponseRecorder, method, file string, headers map[string]string) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(method, "/server/"+file, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	c.Request = req
	c.Params = gin.Params{{Key: "file", Value: file}}
	sdkkey.FromPathFile()(c)
	return c
}

func newTestHandler(store *Store) *Handler {
	return NewHandler(store, nil, nil, testLogger(), testTracer)
}

func sampleFile() string {
	return `{
		"metadata": {"flagSetId": "` + testKeyA + `"},
		"flags": {"f": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on"}}
	}`
}

func TestHandler_Hit(t *testing.T) {
	h := newTestHandler(storeFromRaw(t, sampleFile()))
	rec := httptest.NewRecorder()
	h.Handle(ginRequest(rec, http.MethodGet, testKeyA+".json", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Header().Get("ETag") == "" {
		t.Error("expected ETag header")
	}
}

func TestHandler_NotModified(t *testing.T) {
	store := storeFromRaw(t, sampleFile())
	h := newTestHandler(store)

	// First fetch to learn the ETag.
	rec := httptest.NewRecorder()
	h.Handle(ginRequest(rec, http.MethodGet, testKeyA+".json", nil))
	etag := rec.Header().Get("ETag")

	rec2 := httptest.NewRecorder()
	h.Handle(ginRequest(rec2, http.MethodGet, testKeyA+".json", map[string]string{"If-None-Match": etag}))
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 must have empty body, got %q", rec2.Body.String())
	}
}

func TestHandler_Head(t *testing.T) {
	h := newTestHandler(storeFromRaw(t, sampleFile()))
	rec := httptest.NewRecorder()
	h.Handle(ginRequest(rec, http.MethodHead, testKeyA+".json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD must have empty body, got %q", rec.Body.String())
	}
}

func TestHandler_UnknownFlagSet_404(t *testing.T) {
	h := newTestHandler(storeFromRaw(t, sampleFile()))
	rec := httptest.NewRecorder()
	h.Handle(ginRequest(rec, http.MethodGet, testKeyB+".json", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHandler_NotAFullKey_400(t *testing.T) {
	h := newTestHandler(storeFromRaw(t, sampleFile()))
	rec := httptest.NewRecorder()
	// public-id only (missing private segment) is not a full key.
	h.Handle(ginRequest(rec, http.MethodGet, "dt01.server_us_ab3xy9kp2q.json", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
