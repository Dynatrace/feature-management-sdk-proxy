package cache

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// RefreshIntervalConnected is the polling interval when an upstream SSE
// connection is active. SSE delivers change notifications, so polling is
// only a safety net.
const RefreshIntervalConnected = 10 * time.Minute

// RefreshIntervalDisconnected is the polling interval when SSE is unavailable
// or disabled. Polling is the only update mechanism, so the interval is short.
const RefreshIntervalDisconnected = 10 * time.Second

// RefreshIntervalProvider returns the appropriate polling interval for a given
// cache key. Implementations are expected to consult SSE connection state.
// Defined here (consumer's package) to avoid a cache → sse import cycle.
type RefreshIntervalProvider interface {
	RefreshInterval(keyHash string) time.Duration
}

// Entry holds a cached flagd config payload and metadata for revalidation.
// FetchURL stores the full upstream URL used to revalidate this entry; it
// may contain the raw SDK key in the path, which is acceptable for in-memory use.
type Entry struct {
	Payload          []byte
	ETag             string
	FetchedAt        time.Time
	FetchURL         string // full upstream URL for background TTL revalidation
	OriginatingFetch any    // opaque span context set by the proxy layer; interpreted only by tracing code
}

// IsStale reports whether the entry has exceeded the given refresh interval.
func (e Entry) IsStale(now time.Time, interval time.Duration) bool {
	return now.Sub(e.FetchedAt) >= interval
}

// Cache is a thread-safe in-memory store keyed by SHA-256(sdk_key)[:16].
//
// Last-access tracking: each entry carries a last-access timestamp stored as an
// atomic.Int64 (Unix nanoseconds). The pointer into lastAccess is read under
// mu.RLock (safe map lookup), but the int64 value itself is updated atomically
// so that Get can touch the timestamp on every cache hit without upgrading to a
// write lock. This keeps Get lock-free after the initial pointer lookup.
type Cache struct {
	mu         sync.RWMutex
	entries    map[string]Entry
	lastAccess map[string]*atomic.Int64
}

// New returns an empty Cache.
func New() *Cache {
	return &Cache{
		entries:    make(map[string]Entry),
		lastAccess: make(map[string]*atomic.Int64),
	}
}

// Key returns the safe opaque cache key for the given SDK key.
// The raw SDK key is hashed so it never appears in logs, metrics, or storage.
func Key(sdkKey string) string {
	h := sha256.Sum256([]byte(sdkKey))
	return fmt.Sprintf("%x", h[:8])
}

// Get retrieves an entry and refreshes its last-access timestamp. Returns false when the key is absent.
func (c *Cache) Get(key string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if ok {
		if la, exists := c.lastAccess[key]; exists {
			la.Store(time.Now().UnixNano())
		}
	}
	return e, ok
}

// Set stores or replaces an entry and initializes its last-access timestamp.
func (c *Cache) Set(key string, e Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = e
	if la, exists := c.lastAccess[key]; exists {
		la.Store(time.Now().UnixNano())
	} else {
		la := &atomic.Int64{}
		la.Store(time.Now().UnixNano())
		c.lastAccess[key] = la
	}
}

// Delete removes the entry for key. It is a no-op when the key is absent.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
	delete(c.lastAccess, key)
}

// LastAccess returns the last time the entry for key was accessed via Get or Set.
// Returns the zero time when key is not present.
func (c *Cache) LastAccess(key string) time.Time {
	c.mu.RLock()
	la, exists := c.lastAccess[key]
	c.mu.RUnlock()
	if !exists {
		return time.Time{}
	}
	return time.Unix(0, la.Load())
}

// DeleteIfInactiveSince atomically deletes the entry for key only if its
// last-access time is strictly before cutoff. Returns true if the entry was
// deleted. Returns false when the key is absent, was recently accessed, or
// has an active subscriber that bumped its timestamp between the caller's
// check and this call.
func (c *Cache) DeleteIfInactiveSince(key string, cutoff time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	la, exists := c.lastAccess[key]
	if !exists {
		return false
	}
	if !time.Unix(0, la.Load()).Before(cutoff) {
		return false
	}
	delete(c.entries, key)
	delete(c.lastAccess, key)
	return true
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
	maps.Copy(out, c.entries)
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
// keyHash is cache.Key(sdkKey) — used for telemetry only, never the raw key.
// fetchURL is the full upstream URL (including path and query); etag is the
// previously cached ETag value, which should be sent as If-None-Match.
type Fetcher interface {
	Fetch(ctx context.Context, keyHash, fetchURL, etag string) (FetchResult, error)
}

// RefreshObserver is notified when the background refresher fetches a new
// payload from upstream (200 response). It is never called for 304 responses
// or fetch errors. keyHash is the safe SHA-256 identifier for the SDK key.
type RefreshObserver interface {
	OnRefreshed(keyHash string, payload []byte)
}

// Refresher drives background TTL refresh using a Fetcher.
type Refresher struct {
	cache            *Cache
	fetcher          Fetcher
	observers        []RefreshObserver
	intervalProvider RefreshIntervalProvider // nil → always use RefreshIntervalDisconnected
	logger           *slog.Logger
	tracer           trace.Tracer
}

// NewRefresher constructs a Refresher. Call Run to start the background loop.
func NewRefresher(c *Cache, f Fetcher, logger *slog.Logger) *Refresher {
	return &Refresher{cache: c, fetcher: f, logger: logger, tracer: tracenoop.NewTracerProvider().Tracer("cache")}
}

// NewRefresherWithObservers constructs a Refresher that notifies all provided
// observers after each successful 200 refresh. Nil observers are ignored.
func NewRefresherWithObservers(c *Cache, f Fetcher, logger *slog.Logger, observers ...RefreshObserver) *Refresher {
	var filtered []RefreshObserver
	for _, o := range observers {
		if o != nil {
			filtered = append(filtered, o)
		}
	}
	return &Refresher{cache: c, fetcher: f, observers: filtered, logger: logger, tracer: tracenoop.NewTracerProvider().Tracer("cache")}
}

// SetTracer wires a TracerProvider's tracer into the Refresher so background
// fetches emit proxy.refresh_fetch spans. Must be called before Run.
func (r *Refresher) SetTracer(t trace.Tracer) {
	r.tracer = t
}

// NewRefresherWithObserver constructs a Refresher that notifies a single observer
// after each successful 200 refresh. Deprecated: prefer NewRefresherWithObservers.
func NewRefresherWithObserver(c *Cache, f Fetcher, observer RefreshObserver, logger *slog.Logger) *Refresher {
	return NewRefresherWithObservers(c, f, logger, observer)
}

// SetIntervalProvider wires a RefreshIntervalProvider so the Refresher can
// choose a per-key polling interval based on SSE connection state.
// Must be called before Run.
func (r *Refresher) SetIntervalProvider(p RefreshIntervalProvider) {
	r.intervalProvider = p
}

// Run ticks at RefreshIntervalDisconnected (the shortest possible interval)
// so no key is ever checked less often than needed. Per-key staleness is
// evaluated against the interval returned by intervalProvider.
// It blocks until ctx is cancelled.
func (r *Refresher) Run(ctx context.Context) {
	ticker := time.NewTicker(RefreshIntervalDisconnected)
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
		interval := RefreshIntervalDisconnected
		if r.intervalProvider != nil {
			interval = r.intervalProvider.RefreshInterval(key)
		}
		if !entry.IsStale(now, interval) {
			continue
		}
		// FetchURL is empty for snapshot-restored entries until the first SDK
		// request repairs it. Skip quietly; the handler will hydrate it on hit.
		if entry.FetchURL == "" {
			r.logger.Debug("cache: skipping stale snapshot-restored entry (no FetchURL yet)", slog.String("key", key))
			continue
		}
		fetchCtx, span := r.tracer.Start(ctx, "proxy.refresh_fetch",
			trace.WithAttributes(
				attribute.String("sdk_key_hash", key),
				attribute.Bool("if_none_match", entry.ETag != ""),
			))
		result, err := r.fetcher.Fetch(fetchCtx, key, entry.FetchURL, entry.ETag)
		if err != nil {
			span.RecordError(err)
			span.SetAttributes(attribute.String("result", "error"))
			span.End()
			r.logger.Warn("cache: background refresh failed",
				slog.String("key", key),
				slog.String("error", err.Error()),
			)
			continue
		}
		if result.NotModified {
			span.SetAttributes(attribute.String("result", "not_modified"))
			span.End()
			r.cache.ResetTTL(key, time.Now())
			r.logger.Debug("cache: background refresh — upstream unchanged (304), TTL extended", slog.String("key", key))
			continue
		}
		if result.Entry != nil {
			result.Entry.FetchURL = entry.FetchURL // preserve the fetch URL
			result.Entry.OriginatingFetch = span.SpanContext()
			span.SetAttributes(attribute.String("result", "ok"))
			span.End()
			r.cache.Set(key, *result.Entry)
			r.logger.Debug("cache: background refresh — new content received (200), cache updated", slog.String("key", key))
			for _, obs := range r.observers {
				obs.OnRefreshed(key, result.Entry.Payload)
			}
		}
	}
}
