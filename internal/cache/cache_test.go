package cache

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func noopLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestKey_Deterministic(t *testing.T) {
	k1 := Key("abc")
	k2 := Key("abc")
	if k1 != k2 {
		t.Error("Key should be deterministic")
	}
}

func TestKey_DifferentInputs(t *testing.T) {
	if Key("abc") == Key("def") {
		t.Error("different inputs should produce different keys")
	}
}

func TestKey_Length(t *testing.T) {
	k := Key("anything")
	if len(k) != 16 {
		t.Errorf("expected 16 hex chars, got %d: %q", len(k), k)
	}
}

func TestCache_GetMiss(t *testing.T) {
	c := New()
	_, ok := c.Get("missing")
	if ok {
		t.Error("expected miss, got hit")
	}
}

func TestCache_SetAndGet(t *testing.T) {
	c := New()
	entry := Entry{
		Payload:   []byte(`{"flags":{}}`),
		ETag:      `"v1"`,
		FetchedAt: time.Now(),
		FetchURL:  "https://example.com/config/key.json",
	}
	c.Set("k1", entry)

	got, ok := c.Get("k1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if string(got.Payload) != string(entry.Payload) {
		t.Errorf("payload mismatch: %q", string(got.Payload))
	}
	if got.ETag != entry.ETag {
		t.Errorf("ETag mismatch: %q", got.ETag)
	}
}

func TestCache_SetOverwrites(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v1")})
	c.Set("k1", Entry{Payload: []byte("v2")})

	got, _ := c.Get("k1")
	if string(got.Payload) != "v2" {
		t.Errorf("expected v2, got %q", string(got.Payload))
	}
}

func TestCache_ResetTTL_UpdatesFetchedAt(t *testing.T) {
	c := New()
	original := time.Now().Add(-10 * time.Minute)
	c.Set("k1", Entry{
		Payload:   []byte("x"),
		ETag:      `"etag"`,
		FetchedAt: original,
	})

	now := time.Now()
	c.ResetTTL("k1", now)

	got, _ := c.Get("k1")
	if !got.FetchedAt.Equal(now) {
		t.Errorf("expected FetchedAt to be updated, got %v", got.FetchedAt)
	}
	if got.ETag != `"etag"` {
		t.Errorf("ETag should be unchanged after ResetTTL, got %q", got.ETag)
	}
}

func TestCache_ResetTTL_NoopOnMissingKey(t *testing.T) {
	c := New()
	// should not panic
	c.ResetTTL("missing", time.Now())
}

func TestEntry_IsStale(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		fetchedAt time.Time
		wantStale bool
	}{
		{"fresh", now.Add(-1 * time.Minute), false},
		{"exactly at TTL", now.Add(-TTL), true},
		{"past TTL", now.Add(-TTL - time.Second), true},
		{"just under TTL", now.Add(-TTL + time.Second), false},
	}
	for _, tc := range cases {
		e := Entry{FetchedAt: tc.fetchedAt}
		if got := e.IsStale(now); got != tc.wantStale {
			t.Errorf("%s: IsStale=%v, want %v", tc.name, got, tc.wantStale)
		}
	}
}

func TestCache_Snapshot_IsACopy(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("original")})

	snap := c.Snapshot()
	snap["k1"] = Entry{Payload: []byte("mutated")}

	got, _ := c.Get("k1")
	if string(got.Payload) != "original" {
		t.Error("Snapshot mutation should not affect the cache")
	}
}

func TestCache_ConcurrentAccess(t *testing.T) {
	c := New()
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Set("k", Entry{Payload: []byte("v")})
		}()
		go func() {
			defer wg.Done()
			c.Get("k")
		}()
	}
	wg.Wait()
}

// --- Refresher tests ---

type mockFetcher struct {
	calls   []string
	results map[string]FetchResult
	err     error
}

func (m *mockFetcher) Fetch(_ context.Context, fetchURL, _ string) (FetchResult, error) {
	m.calls = append(m.calls, fetchURL)
	if m.err != nil {
		return FetchResult{}, m.err
	}
	if r, ok := m.results[fetchURL]; ok {
		return r, nil
	}
	return FetchResult{NotModified: true}, nil
}

func TestRefresher_SkipsFreshEntries(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("data"),
		FetchedAt: time.Now(), // fresh
		FetchURL:  "https://example.com/k1.json",
	})

	fetcher := &mockFetcher{}
	r := NewRefresher(c, fetcher, noopLogger())
	r.refresh(context.Background())

	if len(fetcher.calls) != 0 {
		t.Errorf("expected no fetches for fresh entries, got %d", len(fetcher.calls))
	}
}

func TestRefresher_RefreshesStaleEntries_304(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("data"),
		ETag:      `"v1"`,
		FetchedAt: time.Now().Add(-10 * time.Minute), // stale
		FetchURL:  "https://example.com/k1.json",
	})

	fetcher := &mockFetcher{
		results: map[string]FetchResult{
			"https://example.com/k1.json": {NotModified: true},
		},
	}
	r := NewRefresher(c, fetcher, noopLogger())
	beforeRefresh := time.Now()
	r.refresh(context.Background())

	entry, _ := c.Get("k1")
	if entry.FetchedAt.Before(beforeRefresh) {
		t.Error("expected FetchedAt to be updated after 304 refresh")
	}
	if string(entry.Payload) != "data" {
		t.Error("payload should be unchanged after 304 refresh")
	}
}

func TestRefresher_RefreshesStaleEntries_200(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("old"),
		ETag:      `"v1"`,
		FetchedAt: time.Now().Add(-10 * time.Minute), // stale
		FetchURL:  "https://example.com/k1.json",
	})

	newEntry := Entry{Payload: []byte("new"), ETag: `"v2"`, FetchedAt: time.Now()}
	fetcher := &mockFetcher{
		results: map[string]FetchResult{
			"https://example.com/k1.json": {Entry: &newEntry},
		},
	}
	r := NewRefresher(c, fetcher, noopLogger())
	r.refresh(context.Background())

	entry, _ := c.Get("k1")
	if string(entry.Payload) != "new" {
		t.Errorf("expected updated payload, got %q", string(entry.Payload))
	}
	if entry.ETag != `"v2"` {
		t.Errorf("expected updated ETag, got %q", entry.ETag)
	}
	// FetchURL must be preserved from the original entry.
	if entry.FetchURL != "https://example.com/k1.json" {
		t.Errorf("FetchURL should be preserved, got %q", entry.FetchURL)
	}
}

func TestRefresher_FetchError_PreservesExistingEntry(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("original"),
		ETag:      `"v1"`,
		FetchedAt: time.Now().Add(-10 * time.Minute),
		FetchURL:  "https://example.com/k1.json",
	})

	fetcher := &mockFetcher{err: errors.New("network error")}
	r := NewRefresher(c, fetcher, noopLogger())
	r.refresh(context.Background())

	entry, ok := c.Get("k1")
	if !ok {
		t.Fatal("entry should still exist after refresh error")
	}
	if string(entry.Payload) != "original" {
		t.Error("entry should be unchanged after refresh error")
	}
}
