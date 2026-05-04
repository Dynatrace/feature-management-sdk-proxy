//go:build integration

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
)

// CDNMock is a mock upstream CDN server backed by httptest.Server.
// The proxy's HTTP client is configured to redirect all CDN requests here.
type CDNMock struct {
	srv atomic.Pointer[httptest.Server]

	mu       sync.Mutex
	calls    int
	etag     string
	payload  []byte
	failCode int  // non-zero: return this status on next call
	failOnce bool // if true, reset failCode after one failure
}

func newCDNMock(sseURL string) *CDNMock {
	m := &CDNMock{
		etag:    `"test-etag-v1"`,
		payload: buildCDNPayload(sseURL),
	}
	srv := httptest.NewServer(http.HandlerFunc(m.serveHTTP))
	m.srv.Store(srv)
	return m
}

func (m *CDNMock) Close() {
	if s := m.srv.Load(); s != nil {
		s.Close()
	}
}

// Addr returns the address the CDN mock is listening on.
func (m *CDNMock) Addr() string {
	return m.srv.Load().Listener.Addr().String()
}

// Calls returns the total number of requests received.
func (m *CDNMock) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// ResetCalls zeroes the call counter.
func (m *CDNMock) ResetCalls() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = 0
}

// SetFailCode causes the next call(s) to return the given HTTP status code.
// If once is true, the code is cleared after the first failure.
func (m *CDNMock) SetFailCode(code int, once bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCode = code
	m.failOnce = once
}

// ClearFail removes any injected failure.
func (m *CDNMock) ClearFail() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failCode = 0
}

// SetPayload replaces the payload returned on subsequent requests.
func (m *CDNMock) SetPayload(payload []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.payload = payload
}

// SetETag replaces the ETag returned on subsequent requests.
func (m *CDNMock) SetETag(etag string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.etag = etag
}

func (m *CDNMock) serveHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.calls++
	failCode := m.failCode
	if m.failOnce && failCode != 0 {
		m.failCode = 0
	}
	etag := m.etag
	payload := m.payload
	m.mu.Unlock()

	if failCode != 0 {
		http.Error(w, http.StatusText(failCode), failCode)
		return
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" && inm == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
	w.Write(payload) //nolint:errcheck
}

// redirectingClient returns an *http.Client that routes all requests to
// this mock, rewriting scheme→http and host→mock-addr, regardless of the
// original Host or scheme. This mirrors the pattern in proxy/handler_test.go.
func (m *CDNMock) redirectingClient() *http.Client {
	addr := m.Addr()
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			r2 := r.Clone(r.Context())
			r2.URL.Scheme = "http"
			r2.URL.Host = addr
			return http.DefaultTransport.RoundTrip(r2)
		}),
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// buildCDNPayload builds a minimal flagd config JSON payload that includes the
// given SSE URL. The structure mirrors real Dynatrace flag config payloads.
func buildCDNPayload(sseURL string) []byte {
	encoded, _ := json.Marshal(sseURL)
	return fmt.Appendf(nil,
		`{"flags":{},"eventStreams":[{"type":"sse","url":%s,"inactivityDelaySec":60}]}`,
		encoded,
	)
}
