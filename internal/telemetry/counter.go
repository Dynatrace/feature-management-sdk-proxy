package telemetry

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dynatrace/sdk-proxy/internal/cache"
)

type entry struct {
	rawKey string
	count  atomic.Int64
}

// Counter is a thread-safe per-SDK-token request counter. Increment is O(1)
// on the hot path. Drain atomically resets all counts and returns snapshots.
// The raw SDK key is retained in-memory only — it is required for billing
// flush authentication and must never be logged or persisted.
type Counter struct {
	mu      sync.RWMutex
	entries map[string]*entry
}

// Sample is a snapshot of one token's request count returned by Drain.
// WindowStart and WindowEnd are zero when returned by Counter.Drain; the flush
// loop populates them at drain time before dispatching to BillingReporter.
type Sample struct {
	KeyHash     string // safe for logs and metric labels
	SDKKey      string // raw token for billing flush auth — never log this field
	Count       int64
	WindowStart time.Time // flush loop sets at drain time; zero in counter output
	WindowEnd   time.Time // flush loop sets at drain time; zero in counter output
}

// String returns a safe log representation with the raw SDK key redacted.
// Use this when logging or formatting a Sample to avoid leaking the token.
func (s Sample) String() string {
	return fmt.Sprintf("{KeyHash:%s Count:%d WindowStart:%s WindowEnd:%s}",
		s.KeyHash, s.Count,
		s.WindowStart.UTC().Format(time.RFC3339Nano),
		s.WindowEnd.UTC().Format(time.RFC3339Nano),
	)
}

// NewCounter constructs an empty Counter.
func NewCounter() *Counter {
	return &Counter{entries: make(map[string]*entry)}
}

// Increment records one request for sdkKey. It is safe to call concurrently.
// If an entry already exists with an empty rawKey (created by IncrementByHash),
// Increment repairs it so the entry becomes eligible for billing flush.
func (c *Counter) Increment(sdkKey string) {
	keyHash := cache.Key(sdkKey)

	c.mu.RLock()
	e, ok := c.entries[keyHash]
	rawKey := ""
	if ok {
		rawKey = e.rawKey
	}
	c.mu.RUnlock()

	if ok {
		if rawKey == "" {
			// Repair rawKey under write lock (double-checked to handle concurrent repairs).
			// Note: rawKey repair and count.Add are not atomic — Drain may observe the
			// incremented count with an empty rawKey in the same cycle the repair executes.
			// The entry will flush correctly on the following cycle.
			c.mu.Lock()
			if e.rawKey == "" {
				e.rawKey = sdkKey
			}
			c.mu.Unlock()
		}
		e.count.Add(1)
		return
	}

	c.mu.Lock()
	if e, ok = c.entries[keyHash]; !ok {
		e = &entry{rawKey: sdkKey}
		c.entries[keyHash] = e
	} else if e.rawKey == "" {
		e.rawKey = sdkKey
	}
	c.mu.Unlock()
	e.count.Add(1)
}

// IncrementByHash records one request for a key known only by its hash.
// Used by the SSE handler, which receives stream_id (a key hash) rather than
// the raw SDK key. Entries created this way have an empty rawKey and are
// skipped at billing flush until a paired Increment(sdkKey) repairs the entry.
func (c *Counter) IncrementByHash(keyHash string) {
	c.mu.RLock()
	e, ok := c.entries[keyHash]
	c.mu.RUnlock()

	if ok {
		e.count.Add(1)
		return
	}

	c.mu.Lock()
	if e, ok = c.entries[keyHash]; !ok {
		e = &entry{} // rawKey intentionally empty; repaired by Increment when HTTP path sees the key
		c.entries[keyHash] = e
	}
	c.mu.Unlock()
	e.count.Add(1)
}

// Drain resets and returns all non-zero per-token counts. Each entry's count
// is reset via atomic.Swap — safe to call while Increments run concurrently.
// The overall snapshot is not a single transaction across all keys: an
// Increment that races a Swap for its key will appear in the next Drain cycle.
// Entries with zero counts are not removed; they are reused on subsequent
// Increments for the same key without allocating a new map entry.
func (c *Counter) Drain() []Sample {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var out []Sample
	for keyHash, e := range c.entries {
		n := e.count.Swap(0)
		if n > 0 {
			out = append(out, Sample{KeyHash: keyHash, SDKKey: e.rawKey, Count: n})
		}
	}
	return out
}
