package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func noopLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
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
	interval := RefreshIntervalConnected
	now := time.Now()
	cases := []struct {
		name      string
		fetchedAt time.Time
		wantStale bool
	}{
		{"fresh", now.Add(-1 * time.Minute), false},
		{"exactly at interval", now.Add(-interval), true},
		{"past interval", now.Add(-interval - time.Second), true},
		{"just under interval", now.Add(-interval + time.Second), false},
	}
	for _, tc := range cases {
		e := Entry{FetchedAt: tc.fetchedAt}
		if got := e.IsStale(now, interval); got != tc.wantStale {
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

func (m *mockFetcher) Fetch(_ context.Context, _, fetchURL, _ string) (FetchResult, error) {
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

type mockObserver struct {
	mu    sync.Mutex
	calls []struct {
		keyHash string
		payload []byte
	}
}

func (m *mockObserver) OnRefreshed(keyHash string, entry Entry) {
	payload := entry.Payload
	m.mu.Lock()
	m.calls = append(m.calls, struct {
		keyHash string
		payload []byte
	}{keyHash, payload})
	m.mu.Unlock()
}

func TestRefresher_MultiObserver_BothNotified(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("old"),
		ETag:      `"v1"`,
		FetchedAt: time.Now().Add(-10 * time.Minute),
		FetchURL:  "https://example.com/k1.json",
	})

	newEntry := Entry{Payload: []byte("new"), ETag: `"v2"`, FetchedAt: time.Now()}
	fetcher := &mockFetcher{
		results: map[string]FetchResult{
			"https://example.com/k1.json": {Entry: &newEntry},
		},
	}

	obs1 := &mockObserver{}
	obs2 := &mockObserver{}
	r := NewRefresherWithObservers(c, fetcher, noopLogger(), obs1, obs2)
	r.refresh(context.Background())

	obs1.mu.Lock()
	defer obs1.mu.Unlock()
	if len(obs1.calls) != 1 || obs1.calls[0].keyHash != "k1" {
		t.Errorf("observer1: expected 1 call for k1, got %v", obs1.calls)
	}

	obs2.mu.Lock()
	defer obs2.mu.Unlock()
	if len(obs2.calls) != 1 || obs2.calls[0].keyHash != "k1" {
		t.Errorf("observer2: expected 1 call for k1, got %v", obs2.calls)
	}
}

func TestRefresher_SkipsEmptyFetchURL(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("data"),
		FetchedAt: time.Now().Add(-10 * time.Minute), // stale
		FetchURL:  "",                                // snapshot-restored, not yet hydrated
	})

	fetcher := &mockFetcher{}
	r := NewRefresher(c, fetcher, noopLogger())
	r.refresh(context.Background())

	if len(fetcher.calls) != 0 {
		t.Errorf("expected no fetch for empty FetchURL, got %d calls", len(fetcher.calls))
	}
}

func TestCache_Delete_RemovesEntry(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v1")})
	c.Delete("k1")
	_, ok := c.Get("k1")
	if ok {
		t.Error("expected miss after Delete")
	}
}

func TestCache_Delete_MissingKeyIsNoop(t *testing.T) {
	c := New()
	c.Delete("nonexistent") // must not panic
}

func TestCache_Delete_Idempotent(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v1")})
	c.Delete("k1")
	c.Delete("k1") // second delete must not panic
}

func TestRefresher_4xx_Evictable_EvictsEntry(t *testing.T) {
	evictableCodes := []int{401, 403, 404, 410}
	for _, code := range evictableCodes {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			c := New()
			c.Set("k1", Entry{
				Payload:   []byte("original"),
				FetchedAt: time.Now().Add(-10 * time.Minute),
				FetchURL:  "https://example.com/k1.json",
			})

			var evictedKeys []string
			fetcher := &mockFetcher{
				results: map[string]FetchResult{
					"https://example.com/k1.json": {StatusCode: code},
				},
			}
			r := NewRefresher(c, fetcher, noopLogger())
			r.SetEvictor(func(keyHash string) { evictedKeys = append(evictedKeys, keyHash) })
			r.refresh(context.Background())

			if _, ok := c.Get("k1"); ok {
				t.Errorf("status %d: entry should be evicted from cache", code)
			}
			if len(evictedKeys) != 1 || evictedKeys[0] != "k1" {
				t.Errorf("status %d: expected evictor called once with k1, got %v", code, evictedKeys)
			}
		})
	}
}

func TestRefresher_4xx_Evictable_ObserversNotNotified(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("original"),
		FetchedAt: time.Now().Add(-10 * time.Minute),
		FetchURL:  "https://example.com/k1.json",
	})

	obs := &mockObserver{}
	fetcher := &mockFetcher{
		results: map[string]FetchResult{
			"https://example.com/k1.json": {StatusCode: 404},
		},
	}
	r := NewRefresherWithObservers(c, fetcher, noopLogger(), obs)
	r.SetEvictor(func(string) {})
	r.refresh(context.Background())

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.calls) != 0 {
		t.Errorf("observers must not be called on 4xx eviction, got %d calls", len(obs.calls))
	}
}

func TestRefresher_429_PreservesEntry(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("original"),
		FetchedAt: time.Now().Add(-10 * time.Minute),
		FetchURL:  "https://example.com/k1.json",
	})

	var evictorCalled bool
	fetcher := &mockFetcher{
		results: map[string]FetchResult{
			"https://example.com/k1.json": {StatusCode: 429},
		},
	}
	r := NewRefresher(c, fetcher, noopLogger())
	r.SetEvictor(func(string) { evictorCalled = true })
	r.refresh(context.Background())

	if _, ok := c.Get("k1"); !ok {
		t.Error("429: entry should be preserved (rate-limit must not evict)")
	}
	if evictorCalled {
		t.Error("429: evictor must not be called for rate-limit response")
	}
}

func TestRefresher_5xx_PreservesEntry(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("original"),
		FetchedAt: time.Now().Add(-10 * time.Minute),
		FetchURL:  "https://example.com/k1.json",
	})

	var evictorCalled bool
	fetcher := &mockFetcher{
		results: map[string]FetchResult{
			"https://example.com/k1.json": {StatusCode: 500},
		},
	}
	r := NewRefresher(c, fetcher, noopLogger())
	r.SetEvictor(func(string) { evictorCalled = true })
	r.refresh(context.Background())

	if _, ok := c.Get("k1"); !ok {
		t.Error("5xx: entry should be preserved (server error must not evict)")
	}
	if evictorCalled {
		t.Error("5xx: evictor must not be called for server error response")
	}
}

func TestRefresher_NilEvictor_DoesNotPanic(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("original"),
		FetchedAt: time.Now().Add(-10 * time.Minute),
		FetchURL:  "https://example.com/k1.json",
	})

	fetcher := &mockFetcher{
		results: map[string]FetchResult{
			"https://example.com/k1.json": {StatusCode: 404},
		},
	}
	r := NewRefresher(c, fetcher, noopLogger())
	// No SetEvictor — evictor is nil. Must not panic.
	r.refresh(context.Background())

	if _, ok := c.Get("k1"); ok {
		t.Error("entry should be evicted from cache even without an evictor callback")
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

// mockIntervalProvider returns a fixed interval per key for testing.
type mockIntervalProvider struct {
	intervals map[string]time.Duration
}

func (m *mockIntervalProvider) RefreshInterval(key string) time.Duration {
	if d, ok := m.intervals[key]; ok {
		return d
	}
	return RefreshIntervalDisconnected
}

func TestRefresher_ConnectedKey_UsesConnectedInterval(t *testing.T) {
	c := New()
	// Entry older than disconnected interval but younger than connected interval.
	c.Set("k1", Entry{
		Payload:   []byte("data"),
		FetchedAt: time.Now().Add(-30 * time.Second),
		FetchURL:  "https://example.com/k1.json",
	})

	fetcher := &mockFetcher{}
	r := NewRefresher(c, fetcher, noopLogger())
	r.SetIntervalProvider(&mockIntervalProvider{
		intervals: map[string]time.Duration{"k1": RefreshIntervalConnected},
	})
	r.refresh(context.Background())

	if len(fetcher.calls) != 0 {
		t.Errorf("connected key should not be refreshed before connected interval; got %d calls", len(fetcher.calls))
	}
}

func TestRefresher_DisconnectedKey_UsesDisconnectedInterval(t *testing.T) {
	c := New()
	// Entry older than disconnected interval.
	c.Set("k1", Entry{
		Payload:   []byte("data"),
		FetchedAt: time.Now().Add(-15 * time.Second),
		FetchURL:  "https://example.com/k1.json",
	})

	fetcher := &mockFetcher{}
	r := NewRefresher(c, fetcher, noopLogger())
	r.SetIntervalProvider(&mockIntervalProvider{
		intervals: map[string]time.Duration{"k1": RefreshIntervalDisconnected},
	})
	r.refresh(context.Background())

	if len(fetcher.calls) != 1 {
		t.Errorf("disconnected key should be refreshed after RefreshIntervalDisconnected; got %d calls", len(fetcher.calls))
	}
}

func TestRefresher_NilProvider_DefaultsToDisconnected(t *testing.T) {
	c := New()
	// Entry older than disconnected interval — no provider set.
	c.Set("k1", Entry{
		Payload:   []byte("data"),
		FetchedAt: time.Now().Add(-15 * time.Second),
		FetchURL:  "https://example.com/k1.json",
	})

	fetcher := &mockFetcher{}
	r := NewRefresher(c, fetcher, noopLogger()) // no SetIntervalProvider
	r.refresh(context.Background())

	if len(fetcher.calls) != 1 {
		t.Errorf("nil provider should default to disconnected interval; got %d calls", len(fetcher.calls))
	}
}

func TestCache_LastAccess_InitializedOnSet(t *testing.T) {
	before := time.Now()
	c := New()
	c.Set("k1", Entry{Payload: []byte("v")})
	la := c.LastAccess("k1")
	after := time.Now()

	if la.IsZero() {
		t.Fatal("LastAccess should not be zero after Set")
	}
	if la.Before(before) || la.After(after) {
		t.Errorf("LastAccess %v not in expected range [%v, %v]", la, before, after)
	}
}

func TestCache_LastAccess_UpdatedOnGet(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v")})
	laAfterSet := c.LastAccess("k1")

	time.Sleep(2 * time.Millisecond)
	c.Get("k1")
	laAfterGet := c.LastAccess("k1")

	if !laAfterGet.After(laAfterSet) {
		t.Errorf("LastAccess should advance after Get: set=%v get=%v", laAfterSet, laAfterGet)
	}
}

func TestCache_LastAccess_MissingKeyReturnsZero(t *testing.T) {
	c := New()
	la := c.LastAccess("nonexistent")
	if !la.IsZero() {
		t.Errorf("expected zero time for absent key, got %v", la)
	}
}

func TestCache_LastAccess_ClearedOnDelete(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v")})
	c.Delete("k1")
	la := c.LastAccess("k1")
	if !la.IsZero() {
		t.Errorf("expected zero time after Delete, got %v", la)
	}
}

func TestCache_DeleteIfInactiveSince_DeletesStaleEntry(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v")})

	cutoff := time.Now().Add(time.Second) // future cutoff — entry is stale
	deleted := c.DeleteIfInactiveSince("k1", cutoff)
	if !deleted {
		t.Fatal("expected entry to be deleted")
	}
	_, ok := c.Get("k1")
	if ok {
		t.Error("entry should be absent after DeleteIfInactiveSince")
	}
}

func TestCache_DeleteIfInactiveSince_SkipsFreshEntry(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v")})

	cutoff := time.Now().Add(-time.Hour) // past cutoff — entry is fresh
	deleted := c.DeleteIfInactiveSince("k1", cutoff)
	if deleted {
		t.Fatal("should not delete a fresh entry")
	}
	_, ok := c.Get("k1")
	if !ok {
		t.Error("entry should still be present")
	}
}

func TestCache_DeleteIfInactiveSince_NoopForMissingKey(t *testing.T) {
	c := New()
	cutoff := time.Now().Add(time.Second)
	deleted := c.DeleteIfInactiveSince("nonexistent", cutoff)
	if deleted {
		t.Fatal("should not delete absent key")
	}
}

func TestCache_SetIfNewer_ReplacesWhenExpectationMatches(t *testing.T) {
	c := New()
	fetchedAt := time.Now().Add(-5 * time.Minute)
	c.Set("k1", Entry{Payload: []byte("old"), ETag: `"v1"`, FetchedAt: fetchedAt})

	newer := Entry{Payload: []byte("new"), ETag: `"v2"`, FetchedAt: fetchedAt.Add(time.Minute)}
	replaced := c.SetIfNewer("k1", newer, fetchedAt, `"v1"`)
	if !replaced {
		t.Fatal("SetIfNewer should replace when expectation matches")
	}
	got, _ := c.Get("k1")
	if string(got.Payload) != "new" || got.ETag != `"v2"` {
		t.Errorf("unexpected entry after SetIfNewer: payload=%q etag=%q", string(got.Payload), got.ETag)
	}
}

func TestCache_SetIfNewer_NoopWhenFetchedAtMismatch(t *testing.T) {
	c := New()
	fetchedAt := time.Now().Add(-5 * time.Minute)
	c.Set("k1", Entry{Payload: []byte("old"), ETag: `"v1"`, FetchedAt: fetchedAt})

	stale := Entry{Payload: []byte("new"), ETag: `"v2"`}
	replaced := c.SetIfNewer("k1", stale, fetchedAt.Add(time.Second), `"v1"`) // wrong FetchedAt
	if replaced {
		t.Fatal("SetIfNewer should not replace when FetchedAt does not match")
	}
	got, _ := c.Get("k1")
	if string(got.Payload) != "old" {
		t.Error("entry should be unchanged")
	}
}

func TestCache_SetIfNewer_NoopWhenETagMismatch(t *testing.T) {
	c := New()
	fetchedAt := time.Now().Add(-5 * time.Minute)
	c.Set("k1", Entry{Payload: []byte("old"), ETag: `"v1"`, FetchedAt: fetchedAt})

	replaced := c.SetIfNewer("k1", Entry{Payload: []byte("new"), ETag: `"v2"`}, fetchedAt, `"wrong"`)
	if replaced {
		t.Fatal("SetIfNewer should not replace when ETag does not match")
	}
}

func TestCache_SetIfNewer_NoopForMissingKey(t *testing.T) {
	c := New()
	replaced := c.SetIfNewer("absent", Entry{Payload: []byte("x")}, time.Now(), `"v1"`)
	if replaced {
		t.Fatal("SetIfNewer should return false for absent key")
	}
}

// mockFailureObserver records calls to OnRefreshFailure.
type mockFailureObserver struct {
	mu    sync.Mutex
	calls []struct {
		keyID     string
		etag      string
		fetchedAt time.Time
	}
}

func (m *mockFailureObserver) OnRefreshFailure(_ context.Context, keyID, etag string, fetchedAt time.Time) {
	m.mu.Lock()
	m.calls = append(m.calls, struct {
		keyID     string
		etag      string
		fetchedAt time.Time
	}{keyID, etag, fetchedAt})
	m.mu.Unlock()
}

func (m *mockFailureObserver) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func TestRefresher_FailureObserver_NetworkError(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("data"),
		ETag:      `"v1"`,
		FetchedAt: time.Now().Add(-10 * time.Minute),
		FetchURL:  "https://example.com/k1.json",
	})

	obs := &mockFailureObserver{}
	fetcher := &mockFetcher{err: errors.New("network error")}
	r := NewRefresher(c, fetcher, noopLogger())
	r.SetRefreshFailureObserver(obs)
	r.refresh(context.Background())

	// The observer is invoked as a goroutine; give it a moment to run.
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) && obs.callCount() == 0 {
		time.Sleep(time.Millisecond)
	}
	if obs.callCount() != 1 {
		t.Errorf("expected 1 observer call on network error, got %d", obs.callCount())
	}
}

func TestRefresher_FailureObserver_NonEvictableStatus(t *testing.T) {
	for _, code := range []int{429, 500, 503} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			c := New()
			c.Set("k1", Entry{
				Payload:   []byte("data"),
				FetchedAt: time.Now().Add(-10 * time.Minute),
				FetchURL:  "https://example.com/k1.json",
			})

			obs := &mockFailureObserver{}
			fetcher := &mockFetcher{
				results: map[string]FetchResult{
					"https://example.com/k1.json": {StatusCode: code},
				},
			}
			r := NewRefresher(c, fetcher, noopLogger())
			r.SetRefreshFailureObserver(obs)
			r.refresh(context.Background())

			deadline := time.Now().Add(100 * time.Millisecond)
			for time.Now().Before(deadline) && obs.callCount() == 0 {
				time.Sleep(time.Millisecond)
			}
			if obs.callCount() != 1 {
				t.Errorf("status %d: expected 1 observer call, got %d", code, obs.callCount())
			}
		})
	}
}

func TestRefresher_FailureObserver_NotCalledOnSuccess(t *testing.T) {
	c := New()
	fetchedAt := time.Now().Add(-10 * time.Minute)
	c.Set("k1", Entry{
		Payload:   []byte("old"),
		ETag:      `"v1"`,
		FetchedAt: fetchedAt,
		FetchURL:  "https://example.com/k1.json",
	})

	obs := &mockFailureObserver{}
	newEntry := Entry{Payload: []byte("new"), ETag: `"v2"`, FetchedAt: time.Now()}
	fetcher := &mockFetcher{
		results: map[string]FetchResult{
			"https://example.com/k1.json": {Entry: &newEntry},
		},
	}
	r := NewRefresherWithObservers(c, fetcher, noopLogger())
	r.SetRefreshFailureObserver(obs)
	r.refresh(context.Background())

	// Small sleep so any spurious goroutine would have time to fire.
	time.Sleep(10 * time.Millisecond)
	if obs.callCount() != 0 {
		t.Errorf("observer must not be called on successful 200 refresh, got %d calls", obs.callCount())
	}
}

func TestRefresher_FailureObserver_NotCalledOn304(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("data"),
		ETag:      `"v1"`,
		FetchedAt: time.Now().Add(-10 * time.Minute),
		FetchURL:  "https://example.com/k1.json",
	})

	obs := &mockFailureObserver{}
	fetcher := &mockFetcher{
		results: map[string]FetchResult{
			"https://example.com/k1.json": {NotModified: true},
		},
	}
	r := NewRefresher(c, fetcher, noopLogger())
	r.SetRefreshFailureObserver(obs)
	r.refresh(context.Background())

	time.Sleep(10 * time.Millisecond)
	if obs.callCount() != 0 {
		t.Errorf("observer must not be called on 304, got %d calls", obs.callCount())
	}
}

func TestRefresher_FailureObserver_NotCalledOnEvictable4xx(t *testing.T) {
	for _, code := range []int{401, 403, 404, 410} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			c := New()
			c.Set("k1", Entry{
				Payload:   []byte("data"),
				FetchedAt: time.Now().Add(-10 * time.Minute),
				FetchURL:  "https://example.com/k1.json",
			})

			obs := &mockFailureObserver{}
			fetcher := &mockFetcher{
				results: map[string]FetchResult{
					"https://example.com/k1.json": {StatusCode: code},
				},
			}
			r := NewRefresher(c, fetcher, noopLogger())
			r.SetEvictor(func(string) {})
			r.SetRefreshFailureObserver(obs)
			r.refresh(context.Background())

			time.Sleep(10 * time.Millisecond)
			if obs.callCount() != 0 {
				t.Errorf("status %d: observer must not be called on evictable 4xx, got %d calls", code, obs.callCount())
			}
		})
	}
}

func TestRefresher_NilFailureObserver_DoesNotPanic(t *testing.T) {
	c := New()
	c.Set("k1", Entry{
		Payload:   []byte("data"),
		FetchedAt: time.Now().Add(-10 * time.Minute),
		FetchURL:  "https://example.com/k1.json",
	})

	fetcher := &mockFetcher{err: errors.New("network error")}
	r := NewRefresher(c, fetcher, noopLogger())
	// No SetRefreshFailureObserver — must not panic.
	r.refresh(context.Background())
}

func TestCache_LastAccess_ConcurrentGet_Race(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v")})

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			c.Get("k1")
		})
	}
	wg.Wait()
	// Test passes if the race detector doesn't fire.
}
