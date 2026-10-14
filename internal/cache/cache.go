package cache

import (
	"context"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
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
	RefreshInterval(keyID string) time.Duration
}

// Entry holds a cached flagd config payload and metadata for revalidation.
// FetchURL stores the full upstream URL used to revalidate this entry; it
// may contain the raw SDK key in the path, which is acceptable for in-memory use.
type Entry struct {
	Payload          []byte
	ETag             string
	LastModified     time.Time // upstream Last-Modified; zero when absent
	FetchedAt        time.Time
	FetchURL         string // full upstream URL for background TTL revalidation
	OriginatingFetch any    // opaque span context set by the proxy layer; interpreted only by tracing code
}

// IsStale reports whether the entry has exceeded the given refresh interval.
func (e Entry) IsStale(now time.Time, interval time.Duration) bool {
	return now.Sub(e.FetchedAt) >= interval
}

// Cache is a thread-safe in-memory store keyed by SDK key public prefix (sdkkey.PublicID(sdk_key)).
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

// Peek retrieves an entry without refreshing its last-access timestamp, for
// internal maintenance that must not count as client activity.
func (c *Cache) Peek(key string) (Entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
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

// SetIfNewer atomically replaces the entry for key only when the current entry
// still has the given expectedFetchedAt and expectedETag (compare-and-set).
// Returns true when the replacement was applied, false when the entry was absent
// or had already been updated by a concurrent refresh or another fallback call.
func (c *Cache) SetIfNewer(key string, newEntry Entry, expectedFetchedAt time.Time, expectedETag string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.entries[key]
	if !ok {
		return false
	}
	if !cur.FetchedAt.Equal(expectedFetchedAt) || cur.ETag != expectedETag {
		return false
	}
	// Intentionally does NOT update lastAccess: this is an internal maintenance
	// write, not a client request. Updating lastAccess would keep an idle key
	// alive past the inactivity eviction threshold.
	c.entries[key] = newEntry
	return true
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
// Exactly one field is set per call:
//   - NotModified=true → upstream returned 304 (no payload change)
//   - Entry!=nil       → upstream returned 200 with a fresh payload
//   - StatusCode!=0    → upstream returned any other status; Entry and NotModified are both zero
type FetchResult struct {
	// NotModified is true when the upstream returned 304.
	NotModified bool
	// Entry is populated when the upstream returned a fresh 200 payload.
	Entry *Entry
	// StatusCode is set to the HTTP status code when upstream returned a
	// non-200/non-304 response. Zero otherwise. Callers must handle this
	// explicitly — it is not surfaced as an error so policy decisions (evict
	// vs. preserve) can be applied without errors.As plumbing.
	StatusCode int
}

// Fetcher is satisfied by anything that can revalidate a cache entry upstream.
// keyID is sdkkey.PublicID(sdkKey) — used for telemetry only, never the raw key.
// fetchURL is the full upstream URL (including path and query); etag is the
// previously cached ETag value, which should be sent as If-None-Match.
type Fetcher interface {
	Fetch(ctx context.Context, keyID, fetchURL, etag string) (FetchResult, error)
}

// RefreshObserver is notified when the background refresher fetches a new
// payload from upstream (200 response). It is never called for 304 responses
// or fetch errors. keyID is the SDK key public identifier (sdkkey.PublicID(sdkKey)).
type RefreshObserver interface {
	OnRefreshed(keyID string, entry Entry)
}

// RefreshFailureObserver is notified when the background refresher fails to
// fetch a new payload due to a network error or a non-evictable upstream status
// (429, 5xx). It is never called for successful fetches, 304 responses, or
// evictable 4xx responses. keyID is the SDK key public identifier.
type RefreshFailureObserver interface {
	OnRefreshFailure(ctx context.Context, keyID, currentETag string, currentFetchedAt time.Time)
}

// Refresher drives background TTL refresh using a Fetcher.
type Refresher struct {
	cache            *Cache
	fetcher          Fetcher
	observers        []RefreshObserver
	failureObserver  RefreshFailureObserver  // nil → no failure notification
	intervalProvider RefreshIntervalProvider // nil → always use RefreshIntervalDisconnected
	evictor          func(keyID string)      // nil → no side-effect eviction
	inst             *metrics.Instruments    // nil → metrics skipped
	logger           *slog.Logger
	tracer           trace.Tracer
	keyLocks         sync.Map // keyID -> *sync.Mutex; serializes refreshes per key
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

// SetEvictor wires a callback invoked after a cache entry is deleted due to a
// 4xx eviction. The callback should close the upstream SSE connection and delete
// the persisted snapshot for keyID (same contract as Sweeper.evict). Must be
// called before Run.
func (r *Refresher) SetEvictor(evict func(keyID string)) {
	r.evictor = evict
}

// SetInstruments wires OTel instruments into the Refresher so eviction events
// are counted. Must be called before Run.
func (r *Refresher) SetInstruments(inst *metrics.Instruments) {
	r.inst = inst
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

// SetRefreshFailureObserver wires an observer that is notified when a background
// refresh fails with a network error or a non-evictable upstream status (429/5xx).
// Must be called before Run.
func (r *Refresher) SetRefreshFailureObserver(o RefreshFailureObserver) {
	r.failureObserver = o
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
	for keyID, entry := range r.cache.Snapshot() {
		interval := RefreshIntervalDisconnected
		if r.intervalProvider != nil {
			interval = r.intervalProvider.RefreshInterval(keyID)
		}
		if !entry.IsStale(now, interval) {
			continue
		}
		// FetchURL is empty for snapshot-restored entries until the first SDK
		// request repairs it. Skip quietly; the handler will hydrate it on hit.
		if entry.FetchURL == "" {
			r.logger.Debug("cache: skipping stale snapshot-restored entry (no FetchURL yet)", slog.String("sdk_key_public_id", keyID))
			continue
		}
		r.refreshKeyLocked(ctx, keyID, RefetchHint{})
	}
}

// RefetchHint carries the change metadata from an upstream change notification
// (refetchConfig or refetchEvaluation). ETag is optional.
type RefetchHint struct {
	ETag string
}

// RefreshOutcome is the result of a single-key refresh.
type RefreshOutcome int

// RefreshKey outcomes.
const (
	RefreshSkipped     RefreshOutcome = iota // no entry, no FetchURL, or already at the hinted ETag
	RefreshUpdated                           // 200: cache updated, observers notified
	RefreshNotModified                       // 304: TTL extended
	RefreshFailed                            // network error or non-evictable status; stale entry kept
	RefreshEvicted                           // evictable 4xx: entry evicted
)

// RefreshKey revalidates keyID against upstream immediately, outside the
// polling schedule, e.g. when upstream SSE announces a change. It follows the
// same rules as the background refresh (observers, eviction, failure
// notification). When hint.ETag matches the cached ETag the key is already
// current and no request is made. Safe for concurrent use; refreshes of the
// same key are serialized.
func (r *Refresher) RefreshKey(ctx context.Context, keyID string, hint RefetchHint) RefreshOutcome {
	return r.refreshKeyLocked(ctx, keyID, hint)
}

// refreshKeyLocked serializes refreshes per key so a slow poll can't overwrite
// the result of a later event-driven refresh, then re-reads the entry so
// If-None-Match uses the latest ETag.
func (r *Refresher) refreshKeyLocked(ctx context.Context, keyID string, hint RefetchHint) RefreshOutcome {
	mu, _ := r.keyLocks.LoadOrStore(keyID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	entry, ok := r.cache.Peek(keyID)
	if !ok || entry.FetchURL == "" {
		return RefreshSkipped
	}
	if hint.ETag != "" && hint.ETag == entry.ETag {
		return RefreshSkipped
	}
	return r.refreshEntry(ctx, keyID, entry)
}

// refreshEntry performs one upstream revalidation of entry.
func (r *Refresher) refreshEntry(ctx context.Context, keyID string, entry Entry) RefreshOutcome {
	fetchCtx, span := r.tracer.Start(ctx, "proxy.refresh_fetch",
		trace.WithAttributes(
			attribute.String("sdk_key_public_id", keyID),
			attribute.Bool("if_none_match", entry.ETag != ""),
		))
	result, err := r.fetcher.Fetch(fetchCtx, keyID, entry.FetchURL, entry.ETag)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String("result", "error"))
		span.End()
		r.logger.Warn("cache: background refresh failed",
			slog.String("sdk_key_public_id", keyID),
			slog.String("error", err.Error()),
		)
		if r.failureObserver != nil {
			go r.failureObserver.OnRefreshFailure(context.WithoutCancel(ctx), keyID, entry.ETag, entry.FetchedAt)
		}
		return RefreshFailed
	}
	if result.NotModified {
		span.SetAttributes(attribute.String("result", "not_modified"))
		span.End()
		r.cache.ResetTTL(keyID, time.Now())
		r.logger.Debug("cache: background refresh — upstream unchanged (304), TTL extended", slog.String("sdk_key_public_id", keyID))
		return RefreshNotModified
	}
	if result.Entry != nil {
		result.Entry.FetchURL = entry.FetchURL // preserve the fetch URL
		result.Entry.OriginatingFetch = span.SpanContext()
		span.SetAttributes(attribute.String("result", "ok"))
		span.End()
		r.cache.Set(keyID, *result.Entry)
		r.logger.Debug("cache: background refresh — new content received (200), cache updated", slog.String("sdk_key_public_id", keyID))
		for _, obs := range r.observers {
			obs.OnRefreshed(keyID, *result.Entry)
		}
		return RefreshUpdated
	}
	// Non-200/non-304: upstream returned an error status code.
	if result.StatusCode != 0 {
		statusStr := strconv.Itoa(result.StatusCode)
		span.SetAttributes(
			attribute.String("result", "upstream_error"),
			attribute.Int("upstream.status_code", result.StatusCode),
		)
		span.End()
		if ShouldEvict(result.StatusCode) {
			r.cache.Delete(keyID)
			r.logger.Warn("cache: background refresh got evictable status, evicting entry",
				slog.String("sdk_key_public_id", keyID),
				slog.Int("upstream_status", result.StatusCode),
				slog.String("reason", EvictionReason(result.StatusCode)),
			)
			if r.inst != nil {
				r.inst.CacheRefreshTotal.Add(ctx, 1, metric.WithAttributes(
					attribute.String("result", "evicted"),
					attribute.String("status_code", statusStr),
				))
			}
			if r.evictor == nil {
				r.logger.Warn("cache: evictor not set — SSE close and snapshot delete skipped on eviction",
					slog.String("sdk_key_public_id", keyID),
				)
			} else {
				r.evictor(keyID)
			}
			return RefreshEvicted
		}
		r.logger.Warn("cache: background refresh got upstream error, preserving stale entry",
			slog.String("sdk_key_public_id", keyID),
			slog.Int("upstream_status", result.StatusCode),
		)
		if r.inst != nil {
			r.inst.CacheRefreshTotal.Add(ctx, 1, metric.WithAttributes(
				attribute.String("result", "preserved_stale"),
				attribute.String("status_code", statusStr),
			))
		}
		if r.failureObserver != nil {
			go r.failureObserver.OnRefreshFailure(context.WithoutCancel(ctx), keyID, entry.ETag, entry.FetchedAt)
		}
		return RefreshFailed
	}
	// Defensive: Fetcher returned a zero FetchResult — not a valid state.
	// End the span to prevent a leak from a buggy Fetcher implementation.
	span.SetAttributes(attribute.String("result", "unexpected_empty"))
	span.End()
	return RefreshFailed
}

// EvictionReason returns a human-readable label for a status code that warrants
// eviction, for use in structured log fields.
func EvictionReason(statusCode int) string {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "revoked_credentials"
	case http.StatusNotFound, http.StatusGone:
		return "resource_deleted"
	}
	return "unknown"
}

// ShouldEvict reports whether an HTTP status code from the upstream CDN warrants
// immediate cache eviction. 401/403 indicate revoked credentials; 404/410 indicate
// the resource no longer exists. 429 (rate-limit) and 5xx are explicitly excluded
// to avoid amplifying outages with thundering-herd cold fetches.
func ShouldEvict(statusCode int) bool {
	return statusCode == http.StatusUnauthorized ||
		statusCode == http.StatusForbidden ||
		statusCode == http.StatusNotFound ||
		statusCode == http.StatusGone
}
