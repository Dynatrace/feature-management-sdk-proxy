//go:build integration

package e2e

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/snapshot"
)

// TestStartup_ColdNoPersistence verifies that when no snapshot backend is
// configured, the proxy starts cold and serves from upstream on first request.
func TestStartup_ColdNoPersistence(t *testing.T) {
	h := MustNew(t, Options{})

	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	if got := h.CDN.Calls(); got != 1 {
		t.Errorf("CDN calls = %d, want 1 (cold start must fetch upstream)", got)
	}
}

// TestStartup_SnapshotFresh verifies that a proxy started with a fresh
// snapshot backend serves directly from cache without any CDN fetch.
func TestStartup_SnapshotFresh(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate a fresh snapshot (captured 1 minute ago).
	writeTestSnapshot(t, dir, TestSDKKey, buildCDNPayload("https://sse.example.com"), time.Now().Add(-1*time.Minute))

	h := MustNew(t, Options{
		SnapshotDir:    dir,
		SnapshotMaxAge: 24 * time.Hour,
	})

	keyHash := cache.Key(TestSDKKey)
	if _, ok := h.Cache.Get(keyHash); !ok {
		t.Error("cache: entry not found after loading fresh snapshot")
	}

	// Serve from cache — CDN must not be called.
	h.CDN.ResetCalls()
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	if got := h.CDN.Calls(); got != 0 {
		t.Errorf("CDN calls = %d after fresh snapshot; want 0 (should serve from cache)", got)
	}
}

// TestStartup_SnapshotStale verifies that a stale snapshot still allows the
// proxy to start and serve from cache (with a warning emitted internally).
func TestStartup_SnapshotStale(t *testing.T) {
	dir := t.TempDir()

	// Write a snapshot captured 25 hours ago — stale against a 24-hour maxAge.
	writeTestSnapshot(t, dir, TestSDKKey, buildCDNPayload("https://sse.example.com"), time.Now().Add(-25*time.Hour))

	h := MustNew(t, Options{
		SnapshotDir:    dir,
		SnapshotMaxAge: 24 * time.Hour,
	})

	keyHash := cache.Key(TestSDKKey)
	if _, ok := h.Cache.Get(keyHash); !ok {
		t.Error("cache: entry not found after loading stale snapshot")
	}

	// Proxy starts and can still serve.
	resp := h.GetConfig(t, TestSDKKey)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)
}

// TestStartup_EmptyPersistence verifies that when a snapshot backend is
// configured but the store is empty, New returns an error (proxy cannot serve).
func TestStartup_EmptyPersistence(t *testing.T) {
	dir := t.TempDir()

	// Empty dir — no snapshot files written.
	_, err := New(t, Options{
		SnapshotDir:    dir,
		SnapshotMaxAge: 24 * time.Hour,
	})
	if err == nil {
		t.Fatal("New should return an error for CaseEmptyPersistence; got nil")
	}
	t.Logf("got expected startup error: %v", err)
}

// TestStartup_SnapshotWrittenDuringOperation verifies that when persistence is
// configured and snapshots already exist, a cold fetch for a NEW key writes a
// new snapshot to the store. This exercises the async snapshot write path
// without hitting the CaseEmptyPersistence startup guard.
//
// Note: CaseEmptyPersistence prevents a proxy from starting when persistence is
// configured but the store is empty. Pre-seeding the store (as done here with
// writeTestSnapshot) is the intended first-deployment pattern.
func TestStartup_SnapshotWrittenDuringOperation(t *testing.T) {
	dir := t.TempDir()

	// Pre-seed the store so the harness can start (avoids CaseEmptyPersistence).
	writeTestSnapshot(t, dir, TestSDKKey, buildCDNPayload(h2sseURL), time.Now().Add(-1*time.Minute))

	h := MustNew(t, Options{
		SnapshotDir:    dir,
		SnapshotMaxAge: 24 * time.Hour,
	})

	// Fetch a key that is NOT in the snapshot store to trigger a cold fetch and
	// an async snapshot write for that new key.
	resp := h.GetConfig(t, TestSDKKey2)
	assertStatus(t, resp, http.StatusOK)
	drainBody(t, resp)

	keyHash2 := cache.Key(TestSDKKey2)
	store, err := snapshot.NewLocalStore(dir)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}

	eventually(t,
		func() bool {
			keys, err := store.List(context.Background())
			if err != nil {
				return false
			}
			for _, k := range keys {
				if k == keyHash2 {
					return true
				}
			}
			return false
		},
		5*time.Second, 100*time.Millisecond,
		"snapshot not written for new key after cold fetch",
	)
}

// h2sseURL is a placeholder SSE URL used when building snapshot payloads that
// don't need a real upstream SSE connection.
const h2sseURL = "https://sse.example.invalid"

// writeTestSnapshot writes a snapshot envelope to dir for the given key.
// capturedAt controls the FetchedAt timestamp used for freshness classification.
func writeTestSnapshot(t *testing.T, dir, sdkKey string, payload []byte, capturedAt time.Time) {
	t.Helper()
	keyHash := cache.Key(sdkKey)
	envelope, err := snapshot.EncodeEnvelope(keyHash, `"test-etag"`, capturedAt, payload)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	store, err := snapshot.NewLocalStore(dir)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	if err := store.Put(context.Background(), keyHash, envelope); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
}
