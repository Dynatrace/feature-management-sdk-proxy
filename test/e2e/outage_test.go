//go:build integration

package e2e

import (
	"net/http"
	"testing"
)

// TestOutage_UncachedMiss_Returns503 verifies that when the CDN is unreachable
// and the key has not been cached, the proxy returns 503 (not empty body).
func TestOutage_UncachedMiss_Returns503(t *testing.T) {
	h := MustNew(t, Options{})

	// Simulate CDN outage before any warm-up.
	h.CDN.SetFailCode(http.StatusServiceUnavailable, false)
	t.Cleanup(h.CDN.ClearFail)

	resp := h.GetConfig(t, TestSDKKey)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable && resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 or 503", resp.StatusCode)
	}

	// Body must be non-empty — never return an empty response on a miss.
	body := drainBody(t, resp)
	if len(body) == 0 {
		t.Error("response body is empty on CDN outage miss; should contain an error description")
	}
}

// TestOutage_CachedKey_ServedFromCache verifies that a cached key continues
// to serve 200 responses even when the CDN is unreachable.
func TestOutage_CachedKey_ServedFromCache(t *testing.T) {
	h := MustNew(t, Options{})

	// Warm the cache first.
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	// Now simulate CDN outage.
	h.CDN.SetFailCode(http.StatusServiceUnavailable, false)
	t.Cleanup(h.CDN.ClearFail)

	resp = h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	body := drainBody(t, resp)

	if len(body) == 0 {
		t.Error("served empty body from cache during outage")
	}
}

// TestOutage_CDN500_Returns502 verifies that a CDN 500 error on a cache miss
// causes the proxy to return a 5xx to the client (not a 200 with empty data).
func TestOutage_CDN500_Returns502(t *testing.T) {
	h := MustNew(t, Options{})

	h.CDN.SetFailCode(http.StatusInternalServerError, false)
	t.Cleanup(h.CDN.ClearFail)

	resp := h.GetConfig(t, TestSDKKey)
	defer resp.Body.Close()

	if resp.StatusCode < 500 {
		t.Errorf("status = %d, want a 5xx response on CDN error", resp.StatusCode)
	}
	drainBody(t, resp)
}

// TestOutage_CDN403_PassedThrough verifies that a CDN 4xx response is passed
// through to the client (the SDK should see that the key is not authorized,
// not a generic gateway error).
func TestOutage_CDN403_PassedThrough(t *testing.T) {
	h := MustNew(t, Options{})

	h.CDN.SetFailCode(http.StatusForbidden, false)
	t.Cleanup(h.CDN.ClearFail)

	resp := h.GetConfig(t, TestSDKKey)
	defer resp.Body.Close()

	// 4xx responses from upstream should be forwarded as-is.
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Errorf("status = %d, want 4xx for CDN 403 passthrough", resp.StatusCode)
	}
	drainBody(t, resp)
}
