//go:build integration

package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
)

// SSEMock is a mock upstream SSE server backed by httptest.NewTLSServer.
// TLS is required because sse.isValidSSEURL rejects non-https URLs.
type SSEMock struct {
	srv *httptest.Server

	connCount atomic.Int64

	mu   sync.Mutex
	subs []*sseSub
}

type sseSub struct {
	ch chan string
}

func newSSEMock() *SSEMock {
	m := &SSEMock{}
	m.srv = httptest.NewTLSServer(http.HandlerFunc(m.serveHTTP))
	return m
}

// URL returns the https URL for the mock SSE server.
func (m *SSEMock) URL() string { return m.srv.URL }

// Client returns an *http.Client pre-configured with the mock server's TLS certificate.
func (m *SSEMock) Client() *http.Client { return m.srv.Client() }

// ConnCount returns the number of currently active upstream SSE connections.
func (m *SSEMock) ConnCount() int64 { return m.connCount.Load() }

// Close shuts down the mock server.
func (m *SSEMock) Close() { m.srv.Close() }

// SendEvent broadcasts a pre-formatted SSE frame string to all active connections.
// Use FormatEvent to build the frame.
func (m *SSEMock) SendEvent(frame string) {
	m.mu.Lock()
	subs := make([]*sseSub, len(m.subs))
	copy(subs, m.subs)
	m.mu.Unlock()

	for _, s := range subs {
		select {
		case s.ch <- frame:
		default: // drop for slow test consumers
		}
	}
}

// FormatEvent builds a complete SSE frame for a named event and JSON data.
func FormatEvent(eventType, data string) string {
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data)
}

func (m *SSEMock) serveHTTP(w http.ResponseWriter, r *http.Request) {
	m.connCount.Add(1)
	defer m.connCount.Add(-1)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sub := &sseSub{ch: make(chan string, 16)}

	m.mu.Lock()
	m.subs = append(m.subs, sub)
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		for i, s := range m.subs {
			if s == sub {
				m.subs = append(m.subs[:i], m.subs[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-sub.ch:
			if _, err := fmt.Fprint(w, frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
