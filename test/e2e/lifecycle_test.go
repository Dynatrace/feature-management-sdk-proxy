//go:build integration

// docs: docs/monitoring.md — Lifecycle Events; docs/operations.md — Lifecycle Events

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sse"
)

// TestLifecycle_TokenRotated_InvalidatesKey verifies that a tokenRotated event
// for a cached key removes the cache entry and closes the SSE upstream connection.
func TestLifecycle_TokenRotated_InvalidatesKey(t *testing.T) {
	h := MustNew(t, Options{})

	// Warm the cache and establish upstream SSE connection.
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	eventually(t,
		func() bool { return h.SSE.ConnCount() == 1 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not established",
	)

	keyHash := sdkkey.PublicID(TestSDKKey)

	// Send tokenRotated lifecycle event for the key hash.
	payload := lifecyclePayload(keyHash)
	h.SSE.SendEvent(FormatEvent(sse.EventTypeTokenRotated, payload))

	// Cache entry must be removed.
	eventually(t,
		func() bool { _, ok := h.Cache.Get(keyHash); return !ok },
		3*time.Second, 50*time.Millisecond,
		"cache entry not removed after tokenRotated",
	)

	// Upstream SSE connection must be closed (connCount drops to 0).
	eventually(t,
		func() bool { return h.SSE.ConnCount() == 0 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not closed after tokenRotated",
	)
}

// TestLifecycle_StageDeleted_InvalidatesKey verifies that a stageDeleted event
// removes the cache entry and closes the upstream SSE connection.
func TestLifecycle_StageDeleted_InvalidatesKey(t *testing.T) {
	h := MustNew(t, Options{})

	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	eventually(t,
		func() bool { return h.SSE.ConnCount() == 1 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not established",
	)

	keyHash := sdkkey.PublicID(TestSDKKey)
	payload := lifecyclePayload(keyHash)
	h.SSE.SendEvent(FormatEvent(sse.EventTypeStageDeleted, payload))

	eventually(t,
		func() bool { _, ok := h.Cache.Get(keyHash); return !ok },
		3*time.Second, 50*time.Millisecond,
		"cache entry not removed after stageDeleted",
	)
}

// TestLifecycle_ProjectDeleted_InvalidatesKey verifies that a projectDeleted event
// removes the cache entry and closes the upstream SSE connection.
func TestLifecycle_ProjectDeleted_InvalidatesKey(t *testing.T) {
	h := MustNew(t, Options{})

	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	eventually(t,
		func() bool { return h.SSE.ConnCount() == 1 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not established",
	)

	keyHash := sdkkey.PublicID(TestSDKKey)
	payload := lifecyclePayload(keyHash)
	h.SSE.SendEvent(FormatEvent(sse.EventTypeProjectDeleted, payload))

	eventually(t,
		func() bool { _, ok := h.Cache.Get(keyHash); return !ok },
		3*time.Second, 50*time.Millisecond,
		"cache entry not removed after projectDeleted",
	)
}

// TestLifecycle_Invalidation_SiblingUnaffected verifies that invalidating key1
// does not affect key2 (no cross-key contamination).
func TestLifecycle_Invalidation_SiblingUnaffected(t *testing.T) {
	h := MustNew(t, Options{})

	// Warm both keys.
	for _, key := range []string{TestSDKKey, TestSDKKey2} {
		resp := h.GetConfig(t, key)
		assertStatus(t, resp, http.StatusOK)
		drainBody(t, resp)
	}

	// Wait until both upstream SSE connections are established before sending events.
	eventually(t,
		func() bool { return h.SSE.ConnCount() == 2 },
		3*time.Second, 50*time.Millisecond,
		"expected 2 upstream SSE connections before sending lifecycle event",
	)

	keyHash1 := sdkkey.PublicID(TestSDKKey)
	keyHash2 := sdkkey.PublicID(TestSDKKey2)

	// Invalidate only key1.
	payload := lifecyclePayload(keyHash1)
	h.SSE.SendEvent(FormatEvent(sse.EventTypeTokenRotated, payload))

	eventually(t,
		func() bool { _, ok := h.Cache.Get(keyHash1); return !ok },
		3*time.Second, 50*time.Millisecond,
		"key1 cache entry not removed after tokenRotated",
	)

	// key2 must remain in cache.
	if _, ok := h.Cache.Get(keyHash2); !ok {
		t.Error("key2 was unexpectedly removed from cache — sibling should be unaffected")
	}
}

// TestLifecycle_RawSDKKey_AlsoInvalidates verifies that lifecycle events using
// the raw sdkKeys field (instead of pre-hashed sdkKeyHashes) still invalidate correctly.
func TestLifecycle_RawSDKKey_AlsoInvalidates(t *testing.T) {
	h := MustNew(t, Options{})

	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	// Wait for upstream SSE connection before sending the event.
	eventually(t,
		func() bool { return h.SSE.ConnCount() == 1 },
		3*time.Second, 50*time.Millisecond,
		"upstream SSE connection not established",
	)

	keyHash := sdkkey.PublicID(TestSDKKey)
	payload := rawKeyPayload(TestSDKKey)
	h.SSE.SendEvent(FormatEvent(sse.EventTypeTokenRotated, payload))

	eventually(t,
		func() bool { _, ok := h.Cache.Get(keyHash); return !ok },
		3*time.Second, 50*time.Millisecond,
		"cache not invalidated via raw sdkKeys field",
	)
}

// lifecyclePayload builds a lifecycle event data JSON with a pre-hashed sdkKeyHashes field.
func lifecyclePayload(keyHash string) string {
	b, _ := json.Marshal(map[string][]string{"sdkKeyHashes": {keyHash}})
	return string(b)
}

// rawKeyPayload builds a lifecycle event data JSON using the raw sdkKeys field.
func rawKeyPayload(sdkKey string) string {
	b, _ := json.Marshal(map[string][]string{"sdkKeys": {sdkKey}})
	return string(b)
}
