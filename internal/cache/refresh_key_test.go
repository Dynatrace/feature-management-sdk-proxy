package cache

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const refreshKeyURL = "https://cdn.example.com/server/k1.json"

func TestRefreshKey_UpdatesCache(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("old"), ETag: `"v1"`, FetchedAt: time.Now(), FetchURL: refreshKeyURL})

	var gotURL string
	fetcher := fetcherFunc(func(fetchURL, etag string) (FetchResult, error) {
		gotURL = fetchURL
		if etag != `"v1"` {
			t.Errorf("If-None-Match etag = %q, want the cached %q", etag, `"v1"`)
		}
		return FetchResult{Entry: &Entry{Payload: []byte("new"), ETag: `"v2"`, FetchedAt: time.Now()}}, nil
	})
	obs := &mockObserver{}
	r := NewRefresherWithObservers(c, fetcher, noopLogger(), obs)

	// Fresh entry: an event-driven refresh must not wait for the TTL.
	if outcome := r.RefreshKey(context.Background(), "k1", RefetchHint{ETag: `"v2"`}); outcome != RefreshUpdated {
		t.Fatalf("outcome = %v, want RefreshUpdated", outcome)
	}
	if gotURL != refreshKeyURL {
		t.Errorf("fetch URL = %q, want the stored URL %q unchanged", gotURL, refreshKeyURL)
	}
	e, _ := c.Peek("k1")
	if string(e.Payload) != "new" || e.ETag != `"v2"` || e.FetchURL != refreshKeyURL {
		t.Errorf("cache = %q/%q/%q, want new/\"v2\"/%s", e.Payload, e.ETag, e.FetchURL, refreshKeyURL)
	}
	if len(obs.calls) != 1 {
		t.Errorf("observers notified %d times, want 1", len(obs.calls))
	}
}

func TestRefreshKey_NotModified(t *testing.T) {
	c := New()
	c.Set("k1", Entry{ETag: `"v1"`, FetchURL: refreshKeyURL})
	r := NewRefresher(c, fetcherFunc(func(string, string) (FetchResult, error) {
		return FetchResult{NotModified: true}, nil
	}), noopLogger())

	if outcome := r.RefreshKey(context.Background(), "k1", RefetchHint{}); outcome != RefreshNotModified {
		t.Errorf("outcome = %v, want RefreshNotModified", outcome)
	}
}

func TestRefreshKey_SkipsWhenAlreadyAtHintedETag(t *testing.T) {
	c := New()
	c.Set("k1", Entry{ETag: `"v2"`, FetchURL: refreshKeyURL})
	fetcher := &mockFetcher{}
	r := NewRefresher(c, fetcher, noopLogger())

	if outcome := r.RefreshKey(context.Background(), "k1", RefetchHint{ETag: `"v2"`}); outcome != RefreshSkipped {
		t.Errorf("outcome = %v, want RefreshSkipped", outcome)
	}
	if len(fetcher.calls) != 0 {
		t.Errorf("fetcher called %d times, want 0", len(fetcher.calls))
	}
}

func TestRefreshKey_SkipsMissingOrUnhydratedEntries(t *testing.T) {
	c := New()
	c.Set("restored", Entry{ETag: `"v1"`}) // snapshot-restored: no FetchURL yet
	fetcher := &mockFetcher{}
	r := NewRefresher(c, fetcher, noopLogger())

	for _, key := range []string{"absent", "restored"} {
		if outcome := r.RefreshKey(context.Background(), key, RefetchHint{}); outcome != RefreshSkipped {
			t.Errorf("%s: outcome = %v, want RefreshSkipped", key, outcome)
		}
	}
	if len(fetcher.calls) != 0 {
		t.Errorf("fetcher called %d times, want 0", len(fetcher.calls))
	}
}

func TestRefreshKey_EvictsOn4xx(t *testing.T) {
	c := New()
	c.Set("k1", Entry{ETag: `"v1"`, FetchURL: refreshKeyURL})
	r := NewRefresher(c, fetcherFunc(func(string, string) (FetchResult, error) {
		return FetchResult{StatusCode: http.StatusGone}, nil
	}), noopLogger())
	var evicted atomic.Bool
	r.SetEvictor(func(string) { evicted.Store(true) })

	if outcome := r.RefreshKey(context.Background(), "k1", RefetchHint{}); outcome != RefreshEvicted {
		t.Errorf("outcome = %v, want RefreshEvicted", outcome)
	}
	if _, ok := c.Peek("k1"); ok || !evicted.Load() {
		t.Errorf("entry present=%v evictor called=%v; want evicted", ok, evicted.Load())
	}
}

// TestRefreshKey_SerializesPerKey checks that concurrent refreshes of one key
// never overlap, so each uses the ETag left by the previous one.
func TestRefreshKey_SerializesPerKey(t *testing.T) {
	c := New()
	c.Set("k1", Entry{ETag: `"v0"`, FetchURL: refreshKeyURL})
	var inFlight, maxInFlight atomic.Int32
	var version atomic.Int32
	r := NewRefresher(c, fetcherFunc(func(_, etag string) (FetchResult, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		v := version.Add(1)
		return FetchResult{Entry: &Entry{ETag: fmt.Sprintf(`"v%d"`, v), FetchedAt: time.Now()}}, nil
	}), noopLogger())

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { r.RefreshKey(context.Background(), "k1", RefetchHint{}) })
	}
	wg.Wait()
	if m := maxInFlight.Load(); m != 1 {
		t.Errorf("max concurrent fetches for one key = %d, want 1", m)
	}
}

func TestCache_PeekDoesNotTouchLastAccess(t *testing.T) {
	c := New()
	c.Set("k1", Entry{})
	before := c.LastAccess("k1")
	time.Sleep(2 * time.Millisecond)
	c.Peek("k1")
	if after := c.LastAccess("k1"); !after.Equal(before) {
		t.Errorf("Peek changed last access from %v to %v", before, after)
	}
}

type fetcherFunc func(fetchURL, etag string) (FetchResult, error)

func (f fetcherFunc) Fetch(_ context.Context, _, fetchURL, etag string) (FetchResult, error) {
	return f(fetchURL, etag)
}
