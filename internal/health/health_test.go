package health_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dynatrace/sdk-proxy/internal/health"
)

var discardLogger = slog.New(slog.DiscardHandler)

func newHandlers(ready bool) *health.Handlers {
	return health.New(func() bool { return ready }, discardLogger)
}

func TestLiveHandler_AlwaysOK(t *testing.T) {
	for _, ready := range []bool{true, false} {
		h := newHandlers(ready)
		r := httptest.NewRequest(http.MethodGet, "/live", nil)
		w := httptest.NewRecorder()
		h.Live(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("ready=%v: got %d, want 200", ready, w.Code)
		}
	}
}

func TestReadyHandler_TogglesWithReadyFunc(t *testing.T) {
	h := newHandlers(false)
	r := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()
	h.Ready(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
	}

	h = newHandlers(true)
	r = httptest.NewRequest(http.MethodGet, "/ready", nil)
	w = httptest.NewRecorder()
	h.Ready(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}

func TestHealthHandler_AggregatesLiveAndReady(t *testing.T) {
	h := newHandlers(false)
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	h.Health(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", w.Code)
	}

	h = newHandlers(true)
	r = httptest.NewRequest(http.MethodGet, "/health", nil)
	w = httptest.NewRecorder()
	h.Health(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}

func TestProbeHandlers_MethodNotAllowed(t *testing.T) {
	h := newHandlers(true)
	handlers := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"live", h.Live},
		{"ready", h.Ready},
		{"health", h.Health},
	}
	for _, tc := range handlers {
		r := httptest.NewRequest(http.MethodPost, "/"+tc.name, nil)
		w := httptest.NewRecorder()
		tc.handler(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want 405", tc.name, w.Code)
		}
	}
}

func TestReadyHandler_LogsTransitionOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h := health.New(func() bool { return true }, logger)

	for range 3 {
		r := httptest.NewRequest(http.MethodGet, "/ready", nil)
		w := httptest.NewRecorder()
		h.Ready(w, r)
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
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		w := httptest.NewRecorder()
		h.Health(w, r)
	}
	if got := strings.Count(buf.String(), "proxy is ready"); got != 1 {
		t.Errorf("expected exactly 1 'proxy is ready' log entry via /health, got %d\nlog output:\n%s", got, buf.String())
	}
}
