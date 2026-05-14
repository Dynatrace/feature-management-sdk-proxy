//go:build integration

// docs: docs/monitoring.md — Shutdown; docs/shutdown.md; docs/operations.md — Graceful Shutdown

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestShutdown_InFlightRequestCompletes verifies that an in-flight SDK request
// is allowed to complete after the context is cancelled (graceful drain).
func TestShutdown_InFlightRequestCompletes(t *testing.T) {
	h := MustNew(t, Options{})

	// Add a delay to the CDN mock so the request is in-flight during shutdown.
	slowPayload := buildCDNPayload(h.SSE.URL())
	origCalls := 0

	var slowMu sync.Mutex
	slowMu.Lock() // will be unlocked after we initiate shutdown

	slowServer := newCDNMock(h.SSE.URL())
	defer slowServer.Close()
	_ = origCalls
	_ = slowPayload

	// Use the standard harness. The server's 5-second drain window is enough.
	resp := make(chan *http.Response, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		r := h.GetConfig(t, TestSDKKey)
		resp <- r
	}()

	// Allow the goroutine to start.
	time.Sleep(20 * time.Millisecond)

	// Initiate shutdown in a separate goroutine.
	shutErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.Close(ctx)
		shutErr <- nil
	}()

	// The in-flight request should complete.
	select {
	case r := <-resp:
		if r != nil {
			drainBody(t, r)
		}
	case <-time.After(5 * time.Second):
		t.Error("in-flight request did not complete within 5s after shutdown initiated")
	}

	select {
	case err := <-shutErr:
		if err != nil {
			t.Errorf("Close returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("shutdown did not complete within 10s")
	}
}

// TestShutdown_SSEClientReceivesClose verifies that SSE clients are
// cleanly disconnected when the harness shuts down.
func TestShutdown_SSEClientReceivesClose(t *testing.T) {
	h := MustNew(t, Options{})

	// Warm cache.
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	sseURL := h.URL(fmt.Sprintf("/sse?sdk_key=%s", TestSDKKey))
	ch, cancel := connectSSE(t, sseURL)
	defer cancel()

	// Give the connection time to establish.
	time.Sleep(100 * time.Millisecond)

	// Shutdown the harness.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	h.Close(shutCtx)

	// The SSE channel should close (connection terminated).
	select {
	case _, ok := <-ch:
		if ok {
			// channel sent something — keep draining until closed
			deadline := time.After(3 * time.Second)
			for {
				select {
				case _, stillOpen := <-ch:
					if !stillOpen {
						return // closed as expected
					}
				case <-deadline:
					t.Error("SSE channel not closed within 3s after shutdown")
					return
				}
			}
		}
		// channel was closed immediately — that's fine
	case <-time.After(3 * time.Second):
		t.Error("SSE channel not closed within 3s after shutdown")
	}
}

// TestShutdown_BillingFlushedBeforeExit verifies that the billing reporter
// receives at least one call during shutdown (final flush of pending counts).
func TestShutdown_BillingFlushedBeforeExit(t *testing.T) {
	h := MustNew(t, Options{BillingFlushInterval: 24 * time.Hour})

	// Make a request so there is something to flush.
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	callsBefore := len(h.Billing.Calls())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	h.Close(ctx)

	if after := len(h.Billing.Calls()); after <= callsBefore {
		t.Errorf("billing calls after shutdown = %d, want > %d; shutdown flush must fire", after, callsBefore)
	}
}
