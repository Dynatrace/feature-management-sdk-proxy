package cache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const TTL = 5 * time.Minute

// Entry holds a cached flagd config payload and metadata for revalidation.
// FetchURL stores the full upstream URL used to revalidate this entry; it
// may contain the raw SDK key in the path, which is acceptable for in-memory use.
type Entry struct {
	Payload   []byte
	ETag      string
	FetchedAt time.Time
	FetchURL  string // full upstream URL for background TTL revalidation
}

// IsStale reports whether the entry has exceeded the TTL.
func (e Entry) IsStale(now time.Time) bool {
	return now.Sub(e.FetchedAt) >= TTL
}

// Cache is a thread-safe in-memory store keyed by SHA-256(sdk_key)[:16].
type Cache struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// New returns an empty Cache.
func New() *Cache {
	return &Cache{entries: make(map[string]Entry)}
}

// Key returns the safe opaque cache key for the given SDK key.
// The raw SDK key is hashed so it never appears in logs, metrics, or storage.
func Key(sdkKey string) string {
	h := sha256.Sum256([]byte(sdkKey))
	return fmt.Sprintf("%x", h[:8])
}

// Get retrieves an entry. Returns false when the key is absent.
func (c *Cache) Get(key string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	return e, ok
}

// Set stores or replaces an entry.
func (c *Cache) Set(key string, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = e
}

// ResetTTL updates the FetchedAt timestamp for an existing entry without
// changing the payload or ETag (used when upstream returns 304 Not Modified).
func (c *Cache) ResetTTL(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		e.FetchedAt = now
		c.entries[key] = e
	}
}

// Snapshot returns a stable copy of all current entries for iteration.
func (c *Cache) Snapshot() map[string]Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]Entry, len(c.entries))
	for k, v := range c.entries {
		out[k] = v
	}
	return out
}

// FetchResult is the outcome of a single upstream revalidation attempt.
type FetchResult struct {
	// NotModified is true when the upstream returned 304.
	NotModified bool
	// Entry is populated when the upstream returned a fresh 200 payload.
	Entry *Entry
}

// Fetcher is satisfied by anything that can revalidate a cache entry upstream.
// fetchURL is the full upstream URL (including path and query); etag is the
// previously cached ETag value, which should be sent as If-None-Match.
type Fetcher interface {
	Fetch(ctx context.Context, fetchURL, etag string) (FetchResult, error)
}

// RefreshObserver is notified when the background refresher fetches a new
// payload from upstream (200 response). It is never called for 304 responses
// or fetch errors. keyHash is the safe SHA-256 identifier for the SDK key.
type RefreshObserver interface {
	OnRefreshed(keyHash string, payload []byte)
}

// Refresher drives background TTL refresh using a Fetcher.
type Refresher struct {
	cache    *Cache
	fetcher  Fetcher
	observer RefreshObserver // optional; nil is safe
	logger   *slog.Logger
}

// NewRefresher constructs a Refresher. Call Run to start the background loop.
func NewRefresher(c *Cache, f Fetcher, logger *slog.Logger) *Refresher {
	return &Refresher{cache: c, fetcher: f, logger: logger}
}

// NewRefresherWithObserver constructs a Refresher that notifies observer after
// each successful 200 refresh.
func NewRefresherWithObserver(c *Cache, f Fetcher, observer RefreshObserver, logger *slog.Logger) *Refresher {
	return &Refresher{cache: c, fetcher: f, observer: observer, logger: logger}
}

// Run ticks at half the TTL so stale entries are caught promptly.
// It blocks until ctx is cancelled.
func (r *Refresher) Run(ctx context.Context) {
	ticker := time.NewTicker(TTL / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refresh(ctx)
		}
	}
}

func (r *Refresher) refresh(ctx context.Context) {
	now := time.Now()
	for key, entry := range r.cache.Snapshot() {
		if !entry.IsStale(now) {
			continue
		}
		result, err := r.fetcher.Fetch(ctx, entry.FetchURL, entry.ETag)
		if err != nil {
			r.logger.Warn("cache: background refresh failed",
				slog.String("key", key),
				slog.String("error", err.Error()),
			)
			continue
		}
		if result.NotModified {
			r.cache.ResetTTL(key, time.Now())
			r.logger.Debug("cache: TTL reset (304)", slog.String("key", key))
			continue
		}
		if result.Entry != nil {
			result.Entry.FetchURL = entry.FetchURL // preserve the fetch URL
			r.cache.Set(key, *result.Entry)
			r.logger.Debug("cache: entry refreshed (200)", slog.String("key", key))
			if r.observer != nil {
				r.observer.OnRefreshed(key, result.Entry.Payload)
			}
		}
	}
}
