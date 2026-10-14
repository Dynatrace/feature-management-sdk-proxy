//go:build integration

// docs: docs/monitoring.md — SSE Connections; docs/operations.md — SSE Fan-Out Lifecycle

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSSE_FanOut_OneUpstreamConnection verifies the core invariant: N clients
// subscribing to the same SDK key results in exactly one upstream SSE connection.
func TestSSE_FanOut_OneUpstreamConnection(t *testing.T) {
	h := MustNew(t, Options{})

	// Warm the cache so SSE subscription is accepted.
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	// Wait for the upstream SSE connection to be established.
	eventually(t,
		func() bool { return h.SSE.ConnCount() == 1 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not established after config cached",
	)

	sseURL := h.URL(h.SSEPath(TestSDKKey))

	// Connect 5 clients.
	const nClients = 5
	cancels := make([]func(), nClients)
	for i := range nClients {
		_, cancel := connectSSE(t, sseURL)
		cancels[i] = cancel
	}
	t.Cleanup(func() {
		for _, cancel := range cancels {
			cancel()
		}
	})

	// Small settle time for connections to be accepted.
	time.Sleep(100 * time.Millisecond)

	// Still exactly 1 upstream connection despite 5 clients.
	if n := h.SSE.ConnCount(); n != 1 {
		t.Errorf("upstream SSE connections = %d after %d clients; want 1", n, nClients)
	}
}

// TestSSE_EventDelivery verifies that an event sent by the upstream SSE mock
// is received by all subscribed clients.
func TestSSE_EventDelivery(t *testing.T) {
	h := MustNew(t, Options{})

	// Warm cache.
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	eventually(t,
		func() bool { return h.SSE.ConnCount() == 1 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not established",
	)

	sseURL := h.URL(h.SSEPath(TestSDKKey))

	ch1, cancel1 := connectSSE(t, sseURL)
	ch2, cancel2 := connectSSE(t, sseURL)
	defer cancel1()
	defer cancel2()

	// Give the handler time to register both subscribers.
	time.Sleep(100 * time.Millisecond)

	// Send a refetchConfig event from upstream.
	h.SSE.SendEvent(FormatEvent("refetchConfig", `{"flags":"all"}`))

	receiveEvent(t, ch1, "refetchConfig", 3*time.Second)
	receiveEvent(t, ch2, "refetchConfig", 3*time.Second)
}

// TestSSE_BeforeConfigCached_Returns412 verifies that a client connecting to
// /sse before the config has been fetched receives 412 Precondition Failed.
func TestSSE_BeforeConfigCached_Returns412(t *testing.T) {
	h := MustNew(t, Options{})

	sseURL := h.URL(h.SSEPath(TestSDKKey))
	req, err := http.NewRequest(http.MethodGet, sseURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusPreconditionFailed)
	drainBody(t, resp)
}

// TestSSE_ColdStart_BootstrapAndReconnect verifies the full cold-start recovery
// sequence: 412 before config is cached, then 200 once the proxy bootstraps
// via a config fetch and upstream SSE is established.
func TestSSE_ColdStart_BootstrapAndReconnect(t *testing.T) {
	h := MustNew(t, Options{})

	sseURL := h.URL(h.SSEPath(TestSDKKey))

	// SSE before config cached → 412.
	req, err := http.NewRequest(http.MethodGet, sseURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	assertStatus(t, resp, http.StatusPreconditionFailed)
	drainBody(t, resp)

	// Fetch config — bootstraps the proxy for this SDK key.
	configResp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, configResp, http.StatusOK)
	drainBody(t, configResp)

	// Wait for upstream SSE connection to be established.
	eventually(t,
		func() bool { return h.SSE.ConnCount() == 1 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not established after config cached",
	)

	// SSE now succeeds.
	ch, cancel := connectSSE(t, sseURL)
	defer cancel()

	// Give the handler time to register the subscriber before sending.
	time.Sleep(100 * time.Millisecond)

	// Event delivered end-to-end.
	h.SSE.SendEvent(FormatEvent("refetchConfig", `{"flags":"all"}`))
	receiveEvent(t, ch, "refetchConfig", 3*time.Second)
}

// TestSSE_MissingSDKKey_Returns400 verifies that a request to /sse without
// an sdk_key query parameter returns 400.
func TestSSE_MissingSDKKey_Returns400(t *testing.T) {
	h := MustNew(t, Options{})

	resp, err := http.Get(h.URL("/sse"))
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusBadRequest)
	drainBody(t, resp)
}

// TestSSE_TwoKeys_IndependentConnections verifies that two distinct SDK keys
// each get their own upstream SSE connection (no cross-key sharing).
func TestSSE_TwoKeys_IndependentConnections(t *testing.T) {
	h := MustNew(t, Options{})

	// Warm both keys.
	for _, key := range []string{TestSDKKey, TestSDKKey2} {
		resp := h.GetConfig(t, key)
		assertStatus(t, resp, http.StatusOK)
		drainBody(t, resp)
	}

	// Each key should have one upstream SSE connection.
	eventually(t,
		func() bool { return h.SSE.ConnCount() == 2 },
		3*time.Second, 50*time.Millisecond,
		"expected 2 upstream SSE connections for 2 distinct SDK keys",
	)
}

// TestSSE_UpstreamRefetch_RefreshesCacheBeforeForwarding verifies that an
// upstream refetchConfig makes the proxy refresh its own cache before
// forwarding, so the client's refetch gets the new config immediately instead
// of the copy cached until the next (10-minute, while connected) poll.
func TestSSE_UpstreamRefetch_RefreshesCacheBeforeForwarding(t *testing.T) {
	h := MustNew(t, Options{})

	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)
	eventually(t,
		func() bool { return h.SSE.ConnCount() == 1 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not established",
	)

	ch, cancel := connectSSE(t, h.URL(h.SSEPath(TestSDKKey)))
	defer cancel()
	time.Sleep(100 * time.Millisecond)

	// Publish a new revision at the CDN, then announce it over upstream SSE.
	h.CDN.SetPayload([]byte(`{"flags":{"rolled-out":{"state":"ENABLED"}}}`))
	h.CDN.SetETag(`"test-etag-v2"`)
	h.CDN.ResetCalls()
	// Ably message framing, as delivered on server channels.
	h.SSE.SendEvent(FormatEvent("message", `{"id":"m1","name":"refetchConfig","data":"{\"type\":\"refetchConfig\",\"etag\":\"\\\"test-etag-v2\\\"\",\"lastModified\":1771622898000}"}`))

	receiveEvent(t, ch, "refetchConfig", 3*time.Second)
	if calls := h.CDN.Calls(); calls != 1 {
		t.Errorf("CDN calls after event = %d, want 1 (one refresh before forwarding)", calls)
	}

	resp = h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	if body := drainBody(t, resp); !strings.Contains(body, "rolled-out") {
		t.Errorf("client refetch after event got stale config: %s", body)
	}
	if etag := resp.Header.Get("ETag"); etag != `"test-etag-v2"` {
		t.Errorf("ETag = %q, want %q", etag, `"test-etag-v2"`)
	}
}

// TestSSE_UpstreamRefetch_SameETagSkipsCDN verifies that an event naming the
// already-cached revision is forwarded without a CDN request.
func TestSSE_UpstreamRefetch_SameETagSkipsCDN(t *testing.T) {
	h := MustNew(t, Options{})

	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)
	eventually(t,
		func() bool { return h.SSE.ConnCount() == 1 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not established",
	)
	ch, cancel := connectSSE(t, h.URL(h.SSEPath(TestSDKKey)))
	defer cancel()
	time.Sleep(100 * time.Millisecond)

	h.CDN.ResetCalls()
	h.SSE.SendEvent(FormatEvent("message", `{"type":"refetchConfig","etag":"\"test-etag-v1\""}`))

	receiveEvent(t, ch, "refetchConfig", 3*time.Second)
	if calls := h.CDN.Calls(); calls != 0 {
		t.Errorf("CDN calls = %d, want 0 for an event naming the cached ETag", calls)
	}
}
