//go:build integration

// docs: docs/operations.md — Usage Reporting (failure conditions, shutdown flush)

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
)

// TestBilling_CacheHit_IncrementsCounter verifies that a cache-hit request
// increments the per-token billing counter.
func TestBilling_CacheHit_IncrementsCounter(t *testing.T) {
	h := MustNew(t, Options{BillingFlushInterval: 50 * time.Millisecond})

	// First request — cold fetch (cache miss, still counted).
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	// Second request — cache hit.
	resp = h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	// Wait for a billing flush to deliver the samples.
	eventually(t,
		func() bool { return len(h.Billing.Calls()) > 0 },
		3*time.Second, 60*time.Millisecond,
		"billing flush did not deliver any samples",
	)

	total := int64(0)
	for _, s := range h.Billing.Calls() {
		total += s.Count
	}
	if total < 2 {
		t.Errorf("billed request count = %d, want >= 2 (hit + miss)", total)
	}
}

// TestBilling_FlushDeliversCorrectSample verifies that the Sample delivered to
// the billing reporter contains the correct KeyHash and SDKKey.
func TestBilling_FlushDeliversCorrectSample(t *testing.T) {
	h := MustNew(t, Options{BillingFlushInterval: 50 * time.Millisecond})

	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	eventually(t,
		func() bool { return len(h.Billing.Calls()) > 0 },
		3*time.Second, 60*time.Millisecond,
		"billing flush did not deliver a sample",
	)

	calls := h.Billing.Calls()
	if len(calls) == 0 {
		t.Fatal("no billing calls recorded")
	}

	found := false
	for _, s := range calls {
		if s.SDKKey == TestSDKKey {
			found = true
			if s.Count <= 0 {
				t.Errorf("sample Count = %d, want > 0", s.Count)
			}
			if s.KeyID == "" {
				t.Error("sample KeyHash is empty")
			}
		}
	}
	if !found {
		t.Errorf("no Sample found for key_id=%s in billing calls", cache.PublicID(TestSDKKey))
	}
}

// TestBilling_RejectedSSE_NotCounted verifies that a 412-rejected SSE connect
// (before config is cached) is not billed.
func TestBilling_RejectedSSE_NotCounted(t *testing.T) {
	// Use a fast flush interval so we can wait for at least one flush to fire,
	// then assert the rejected SSE produced no sample — a signal-driven negative
	// assertion rather than a time-bounded one.
	h := MustNew(t, Options{BillingFlushInterval: 50 * time.Millisecond})

	// SSE connect before config → 412; must not increment counter.
	sseURL := h.URL(h.SSEPath(TestSDKKey))
	req, err := http.NewRequest(http.MethodGet, sseURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusPreconditionFailed)
	drainBody(t, resp)

	// Warm a different key so the flusher has something to flush — this gives us
	// a signal that at least one tick has fired without relying on time.Sleep.
	resp2 := h.GetConfig(t, TestSDKKey2)
	assertStatus(t, resp2, http.StatusOK)
	drainBody(t, resp2)

	eventually(t,
		func() bool {
			for _, s := range h.Billing.Calls() {
				if s.SDKKey == TestSDKKey2 {
					return true // at least one flush completed
				}
			}
			return false
		},
		3*time.Second, 60*time.Millisecond,
		"billing flush did not fire",
	)

	// Now assert the rejected SSE produced no sample for TestSDKKey.
	for _, s := range h.Billing.Calls() {
		if s.SDKKey == TestSDKKey {
			t.Errorf("unexpected billing sample for rejected SSE request: key_id=%s count=%d", s.KeyID, s.Count)
		}
	}
}

// TestBilling_EndpointError_DoesNotFailSDKRequest verifies that a billing
// endpoint error does not surface as an error to SDK clients.
func TestBilling_EndpointError_DoesNotFailSDKRequest(t *testing.T) {
	h := MustNew(t, Options{BillingFlushInterval: 50 * time.Millisecond})
	h.Billing.SetError(context.DeadlineExceeded)
	t.Cleanup(func() { h.Billing.SetError(nil) })

	// Even with billing errors, SDK requests must succeed.
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)
}

// TestBilling_ShutdownFlush verifies that cancelling the harness context
// triggers a final billing flush of any pending counts.
func TestBilling_ShutdownFlush(t *testing.T) {
	h := MustNew(t, Options{BillingFlushInterval: 1 * time.Hour}) // no periodic flush

	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	// Before shutdown, no flush has happened (1-hour interval).
	callsBefore := len(h.Billing.Calls())

	// Close triggers the shutdown flush.
	h.Close(context.Background())

	// After shutdown, billing should have been flushed.
	callsAfter := len(h.Billing.Calls())
	if callsAfter <= callsBefore {
		t.Errorf("billing calls after shutdown = %d, want > %d (shutdown flush must fire)", callsAfter, callsBefore)
	}
}
