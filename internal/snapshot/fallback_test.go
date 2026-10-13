package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
)

// stubStore is an in-memory snapshot.Store for testing.
type stubStore struct {
	mu   sync.Mutex
	data map[string][]byte
	err  error // when set, Get always returns this error
	gets int
}

func (s *stubStore) Get(_ context.Context, keyID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.err != nil {
		return nil, s.err
	}
	b, ok := s.data[keyID]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}

func (s *stubStore) GetVersioned(ctx context.Context, keyID string) ([]byte, string, error) {
	b, err := s.Get(ctx, keyID)
	return b, "", err
}

func (s *stubStore) PutIfMatch(ctx context.Context, keyID string, body []byte, _ string) error {
	return s.Put(ctx, keyID, body)
}

func (s *stubStore) Put(_ context.Context, keyID string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = make(map[string][]byte)
	}
	s.data[keyID] = body
	return nil
}

func (s *stubStore) List(_ context.Context) ([]string, error) { return nil, nil }
func (s *stubStore) Delete(_ context.Context, _ string) error { return nil }

func (s *stubStore) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
}

func testInst(t *testing.T) *metrics.Instruments {
	t.Helper()
	inst, err := metrics.Register(noop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("metrics.Register: %v", err)
	}
	return inst
}

func noopFallbackLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func makeSnapshot(t *testing.T, keyID, etag string, capturedAt time.Time, payload []byte) []byte {
	t.Helper()
	data, err := EncodeEnvelope(Record{KeyID: keyID, ETag: etag, CapturedAt: capturedAt, Payload: payload})
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	return data
}

// waitForCondition polls f until it returns true or the deadline passes.
func waitForCondition(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Helper()
}

const testKey = "dt01.server_us_ab3xy9kp2q"

func TestFallback_LoadsNewerSnapshot(t *testing.T) {
	currentFetchedAt := time.Now().Add(-10 * time.Minute)
	snapshotCapturedAt := currentFetchedAt.Add(5 * time.Minute) // newer

	snapshotData := makeSnapshot(t, testKey, `"v2"`, snapshotCapturedAt, []byte(`{"flags":{"new":true}}`))
	store := &stubStore{data: map[string][]byte{testKey: snapshotData}}

	c := cache.New()
	c.Set(testKey, cache.Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      `"v1"`,
		FetchedAt: currentFetchedAt,
		FetchURL:  "https://example.com/k1.json",
	})

	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	waitForCondition(t, func() bool {
		e, ok := c.Get(testKey)
		return ok && e.ETag == `"v2"`
	})

	got, _ := c.Get(testKey)
	if got.ETag != `"v2"` {
		t.Errorf("expected updated ETag, got %q", got.ETag)
	}
	if string(got.Payload) != `{"flags":{"new":true}}` {
		t.Errorf("unexpected payload: %q", string(got.Payload))
	}
	if got.FetchURL != "https://example.com/k1.json" {
		t.Errorf("FetchURL should be preserved, got %q", got.FetchURL)
	}
	if !got.FetchedAt.Equal(snapshotCapturedAt) {
		t.Errorf("FetchedAt should be set to snapshot capturedAt, got %v", got.FetchedAt)
	}
}

func TestFallback_SameEtag_Noop(t *testing.T) {
	currentFetchedAt := time.Now().Add(-10 * time.Minute)
	// Snapshot is newer but has the same ETag — content is identical.
	snapshotData := makeSnapshot(t, testKey, `"v1"`, currentFetchedAt.Add(time.Minute), []byte("same"))
	store := &stubStore{data: map[string][]byte{testKey: snapshotData}}

	c := cache.New()
	c.Set(testKey, cache.Entry{
		Payload:   []byte("same"),
		ETag:      `"v1"`,
		FetchedAt: currentFetchedAt,
	})

	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	time.Sleep(20 * time.Millisecond)

	got, _ := c.Get(testKey)
	if got.ETag != `"v1"` {
		t.Errorf("entry should be unchanged for same_etag, got etag=%q", got.ETag)
	}
}

func TestFallback_OlderSnapshot_Noop(t *testing.T) {
	currentFetchedAt := time.Now().Add(-5 * time.Minute)
	olderCapturedAt := currentFetchedAt.Add(-time.Minute) // older than current

	snapshotData := makeSnapshot(t, testKey, `"v0"`, olderCapturedAt, []byte("old"))
	store := &stubStore{data: map[string][]byte{testKey: snapshotData}}

	c := cache.New()
	c.Set(testKey, cache.Entry{
		Payload:   []byte("current"),
		ETag:      `"v1"`,
		FetchedAt: currentFetchedAt,
	})

	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	time.Sleep(20 * time.Millisecond)

	got, _ := c.Get(testKey)
	if got.ETag != `"v1"` {
		t.Errorf("entry should be unchanged for older snapshot, got etag=%q", got.ETag)
	}
}

func TestFallback_StoreError_Noop(t *testing.T) {
	c := cache.New()
	currentFetchedAt := time.Now()
	c.Set(testKey, cache.Entry{
		Payload:   []byte("data"),
		ETag:      `"v1"`,
		FetchedAt: currentFetchedAt,
	})

	store := &stubStore{err: errors.New("s3 unavailable")}
	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	time.Sleep(20 * time.Millisecond)

	got, _ := c.Get(testKey)
	if got.ETag != `"v1"` {
		t.Errorf("entry should be unchanged after store error, got etag=%q", got.ETag)
	}
}

func TestFallback_DecodeError_Noop(t *testing.T) {
	store := &stubStore{data: map[string][]byte{testKey: []byte("not-valid-json")}}
	c := cache.New()
	currentFetchedAt := time.Now()
	c.Set(testKey, cache.Entry{ETag: `"v1"`, FetchedAt: currentFetchedAt})

	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	time.Sleep(20 * time.Millisecond)

	got, _ := c.Get(testKey)
	if got.ETag != `"v1"` {
		t.Errorf("entry should be unchanged after decode error, got etag=%q", got.ETag)
	}
}

func TestFallback_UnknownVersion_Noop(t *testing.T) {
	// Build an envelope with a version higher than CurrentSnapshotVersion.
	unknownVersion := []byte(`{"snapshot_version":999,"sdk_key_id":"k","etag":"\"v2\"","captured_at":"2026-01-01T00:00:00Z","payload":"dGVzdA=="}`)
	store := &stubStore{data: map[string][]byte{testKey: unknownVersion}}
	c := cache.New()
	currentFetchedAt := time.Now()
	c.Set(testKey, cache.Entry{ETag: `"v1"`, FetchedAt: currentFetchedAt})

	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	time.Sleep(20 * time.Millisecond)

	got, _ := c.Get(testKey)
	if got.ETag != `"v1"` {
		t.Errorf("entry should be unchanged for unknown envelope version, got etag=%q", got.ETag)
	}
}

func TestFallback_Debounce_SuppressesRedundantChecks(t *testing.T) {
	currentFetchedAt := time.Now().Add(-10 * time.Minute)
	snapshotData := makeSnapshot(t, testKey, `"v2"`, currentFetchedAt.Add(time.Minute), []byte("new"))
	store := &stubStore{data: map[string][]byte{testKey: snapshotData}}

	c := cache.New()
	c.Set(testKey, cache.Entry{ETag: `"v1"`, FetchedAt: currentFetchedAt})

	// Set lastAttempt to now to simulate a recent check.
	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	fb.mu.Lock()
	fb.lastAttempt[testKey] = time.Now()
	fb.mu.Unlock()

	// First call after the fake recent attempt — should be debounced.
	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	time.Sleep(20 * time.Millisecond)
	if store.getCount() != 0 {
		t.Errorf("store.Get should not be called within debounce interval, got %d calls", store.getCount())
	}
}

func TestFallback_Debounce_AllowsCheckAfterInterval(t *testing.T) {
	currentFetchedAt := time.Now().Add(-10 * time.Minute)
	snapshotCapturedAt := currentFetchedAt.Add(5 * time.Minute)
	snapshotData := makeSnapshot(t, testKey, `"v2"`, snapshotCapturedAt, []byte("new"))
	store := &stubStore{data: map[string][]byte{testKey: snapshotData}}

	c := cache.New()
	c.Set(testKey, cache.Entry{ETag: `"v1"`, FetchedAt: currentFetchedAt, FetchURL: "https://x.com"})

	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	// Set lastAttempt to a time well in the past (past the debounce interval).
	fb.mu.Lock()
	fb.lastAttempt[testKey] = time.Now().Add(-2 * fallbackDebounceInterval)
	fb.mu.Unlock()

	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	waitForCondition(t, func() bool { return store.getCount() > 0 })
	if store.getCount() == 0 {
		t.Error("store.Get should be called after debounce interval has passed")
	}
}

func TestFallback_Superseded_WhenCacheUpdatedConcurrently(t *testing.T) {
	currentFetchedAt := time.Now().Add(-10 * time.Minute)
	snapshotCapturedAt := currentFetchedAt.Add(5 * time.Minute)
	snapshotData := makeSnapshot(t, testKey, `"v2"`, snapshotCapturedAt, []byte("new"))
	store := &stubStore{data: map[string][]byte{testKey: snapshotData}}

	c := cache.New()
	c.Set(testKey, cache.Entry{ETag: `"v1"`, FetchedAt: currentFetchedAt})

	// Simulate the CDN refresher updating the entry before checkAndApply runs SetIfNewer.
	// We pass a currentFetchedAt that no longer matches the cache entry.
	concurrentFetchedAt := currentFetchedAt.Add(time.Second) // different timestamp
	c.Set(testKey, cache.Entry{ETag: `"v3"`, FetchedAt: concurrentFetchedAt, Payload: []byte("concurrent")})

	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	// Pass the *old* currentFetchedAt — SetIfNewer will reject it.
	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	time.Sleep(30 * time.Millisecond)

	// The concurrently-set entry should remain unchanged.
	got, _ := c.Get(testKey)
	if got.ETag != `"v3"` {
		t.Errorf("concurrent update should not be overwritten by fallback; got etag=%q", got.ETag)
	}
}

func TestFallback_NoWriteToStore(t *testing.T) {
	// Verify that a successful fallback load never calls store.Put.
	currentFetchedAt := time.Now().Add(-10 * time.Minute)
	snapshotCapturedAt := currentFetchedAt.Add(5 * time.Minute)
	snapshotData := makeSnapshot(t, testKey, `"v2"`, snapshotCapturedAt, []byte("new"))

	putCount := 0
	store := &trackingStore{
		inner:    &stubStore{data: map[string][]byte{testKey: snapshotData}},
		onPut:    func() { putCount++ },
		onDelete: func() {},
	}

	c := cache.New()
	c.Set(testKey, cache.Entry{ETag: `"v1"`, FetchedAt: currentFetchedAt, FetchURL: "https://x.com"})

	fb := NewFallback(store, c, testInst(t), noopFallbackLogger())
	fb.OnRefreshFailure(context.Background(), testKey, `"v1"`, currentFetchedAt)

	waitForCondition(t, func() bool {
		e, ok := c.Get(testKey)
		return ok && e.ETag == `"v2"`
	})

	if putCount != 0 {
		t.Errorf("fallback must not write to snapshot store; got %d Put calls", putCount)
	}
}

// trackingStore wraps a Store and calls hooks on mutating operations.
type trackingStore struct {
	inner    Store
	onPut    func()
	onDelete func()
}

func (s *trackingStore) Get(ctx context.Context, keyID string) ([]byte, error) {
	return s.inner.Get(ctx, keyID)
}
func (s *trackingStore) GetVersioned(ctx context.Context, keyID string) ([]byte, string, error) {
	return s.inner.GetVersioned(ctx, keyID)
}
func (s *trackingStore) PutIfMatch(ctx context.Context, keyID string, body []byte, version string) error {
	s.onPut()
	return s.inner.PutIfMatch(ctx, keyID, body, version)
}
func (s *trackingStore) List(ctx context.Context) ([]string, error) { return s.inner.List(ctx) }
func (s *trackingStore) Delete(ctx context.Context, keyID string) error {
	s.onDelete()
	return s.inner.Delete(ctx, keyID)
}
