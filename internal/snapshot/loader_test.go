package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

func noopInst(t *testing.T) *metrics.Instruments {
	t.Helper()
	inst, err := metrics.Register(noop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("metrics.Register: %v", err)
	}
	return inst
}

func noopLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fakeStore is an in-memory Store for tests.
type fakeStore struct {
	objects map[string][]byte
	listErr error
	getErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: make(map[string][]byte)}
}

func (f *fakeStore) Put(_ context.Context, keyHash string, body []byte) error {
	f.objects[keyHash] = body
	return nil
}

func (f *fakeStore) Get(_ context.Context, keyHash string) ([]byte, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	b, ok := f.objects[keyHash]
	if !ok {
		return nil, errors.New("not found")
	}
	return b, nil
}

func (f *fakeStore) List(_ context.Context) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]string, 0, len(f.objects))
	for k := range f.objects {
		out = append(out, k)
	}
	return out, nil
}

func TestLoadAll_HappyPath(t *testing.T) {
	store := newFakeStore()
	payload := []byte(`{"flags":{}}`)
	capturedAt := time.Now().Add(-1 * time.Minute)
	body, err := EncodeEnvelope("abc123def456789a", `"v1"`, capturedAt, payload)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	store.objects["abc123def456789a"] = body

	c := cache.New()
	n, err := LoadAll(context.Background(), store, c, noopInst(t), noopLogger())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if n != 1 {
		t.Errorf("loaded count: got %d, want 1", n)
	}

	entry, ok := c.Get("abc123def456789a")
	if !ok {
		t.Fatal("expected cache hit after load")
	}
	if string(entry.Payload) != string(payload) {
		t.Errorf("payload mismatch: %q", entry.Payload)
	}
	if entry.ETag != `"v1"` {
		t.Errorf("etag mismatch: %q", entry.ETag)
	}
	if entry.FetchURL != "" {
		t.Errorf("FetchURL must be empty after load, got %q", entry.FetchURL)
	}
}

func TestLoadAll_EmptyStore(t *testing.T) {
	n, err := LoadAll(context.Background(), newFakeStore(), cache.New(), noopInst(t), noopLogger())
	if err != nil {
		t.Fatalf("LoadAll on empty store: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 loaded, got %d", n)
	}
}

func TestLoadAll_SkipsUnknownVersion(t *testing.T) {
	store := newFakeStore()
	store.objects["k1"] = []byte(`{"snapshot_version":999,"sdk_key_hash":"k1","etag":"","captured_at":"2026-01-01T00:00:00Z","payload":""}`)

	c := cache.New()
	n, err := LoadAll(context.Background(), store, c, noopInst(t), noopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 loaded, got %d", n)
	}
	if _, ok := c.Get("k1"); ok {
		t.Error("unknown-version snapshot should not populate cache")
	}
}

func TestLoadAll_SkipsCorruptEnvelope(t *testing.T) {
	store := newFakeStore()
	store.objects["k1"] = []byte(`not valid json`)

	c := cache.New()
	n, err := LoadAll(context.Background(), store, c, noopInst(t), noopLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 loaded, got %d", n)
	}
}

func TestLoadAll_ListError(t *testing.T) {
	store := newFakeStore()
	store.listErr = errors.New("bucket not found")

	_, err := LoadAll(context.Background(), store, cache.New(), noopInst(t), noopLogger())
	if err == nil {
		t.Error("expected error from List, got nil")
	}
}

func TestLoadAll_GetError_SkipsContinues(t *testing.T) {
	store := newFakeStore()
	// Put one good entry and one bad entry.
	payload := []byte(`{}`)
	body, _ := EncodeEnvelope("good", "", time.Now(), payload)
	store.objects["good"] = body
	store.objects["bad"] = []byte("data") // listed but Get will error

	// selectiveErrorStore fails Get only for the "bad" key.
	wrapped := &selectiveErrorStore{inner: store, failKey: "bad"}
	c := cache.New()
	n, err := LoadAll(context.Background(), wrapped, c, noopInst(t), noopLogger())
	if err != nil {
		t.Fatalf("LoadAll should not fail when individual gets error: %v", err)
	}
	// "good" should load; "bad" should be skipped.
	if n != 1 {
		t.Errorf("expected 1 loaded, got %d", n)
	}
}

type selectiveErrorStore struct {
	inner   Store
	failKey string
}

func (s *selectiveErrorStore) Put(ctx context.Context, k string, b []byte) error {
	return s.inner.Put(ctx, k, b)
}
func (s *selectiveErrorStore) Get(ctx context.Context, k string) ([]byte, error) {
	if k == s.failKey {
		return nil, errors.New("transient error")
	}
	return s.inner.Get(ctx, k)
}
func (s *selectiveErrorStore) List(ctx context.Context) ([]string, error) {
	return s.inner.List(ctx)
}
