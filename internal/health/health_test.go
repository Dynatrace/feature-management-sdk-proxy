package health_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/health"
)

func init() {
	gin.SetMode(gin.TestMode)
}

var discardLogger = slog.New(slog.DiscardHandler)

func newHandlers(ready bool) *health.Handlers {
	return health.New(func() bool { return ready }, discardLogger)
}

// ginContext creates a *gin.Context backed by the given recorder and request.
func ginContext(w *httptest.ResponseRecorder, r *http.Request) *gin.Context {
	c, _ := gin.CreateTestContext(w)
	c.Request = r
	return c
}

func TestLiveHandler_AlwaysOK(t *testing.T) {
	for _, ready := range []bool{true, false} {
		h := newHandlers(ready)
		r := httptest.NewRequest(http.MethodGet, "/livez", nil)
		w := httptest.NewRecorder()
		h.Live(ginContext(w, r))
		if w.Code != http.StatusOK {
			t.Errorf("ready=%v: got %d, want 200", ready, w.Code)
		}
	}
}

func TestReadyHandler_TogglesWithReadyFunc(t *testing.T) {
	h := newHandlers(false)
	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	h.Ready(ginContext(w, r))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
	}

	h = newHandlers(true)
	r = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w = httptest.NewRecorder()
	h.Ready(ginContext(w, r))
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}

func TestHealthHandler_AggregatesLiveAndReady(t *testing.T) {
	h := newHandlers(false)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.Health(ginContext(w, r))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
	}

	h = newHandlers(true)
	r = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w = httptest.NewRecorder()
	h.Health(ginContext(w, r))
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}

func TestReadyHandler_LogsTransitionOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h := health.New(func() bool { return true }, logger)

	for range 3 {
		r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		h.Ready(ginContext(w, r))
	}
	if got := strings.Count(buf.String(), "proxy is ready"); got != 1 {
		t.Errorf("expected exactly 1 'proxy is ready' log entry, got %d\nlog output:\n%s", got, buf.String())
	}
}

func TestHealthHandler_LogsTransitionOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h := health.New(func() bool { return true }, logger)

	for range 3 {
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		h.Health(ginContext(w, r))
	}
	if got := strings.Count(buf.String(), "proxy is ready"); got != 1 {
		t.Errorf("expected exactly 1 'proxy is ready' log entry via /healthz, got %d\nlog output:\n%s", got, buf.String())
	}
}
