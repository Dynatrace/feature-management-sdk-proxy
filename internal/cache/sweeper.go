package cache

import (
	"context"
	"log/slog"
	"time"

	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

const inactivitySweepInterval = 1 * time.Hour

// Liveness reports whether a cache key currently has active downstream clients.
// Defined in the cache package (consumer) to avoid a cache → sse import cycle.
type Liveness interface {
	HasSubscribers(keyHash string) bool
}

// LivenessFunc is a function adapter for Liveness.
type LivenessFunc func(keyHash string) bool

// HasSubscribers implements Liveness by calling the underlying function.
func (f LivenessFunc) HasSubscribers(keyHash string) bool { return f(keyHash) }

// Sweeper evicts cache entries that have been inactive for longer than
// evictAfter and have no active downstream subscribers. Eviction is a full
// GC: cache entry, upstream SSE connection, and persisted snapshot are all
// removed (identical to lifecycle invalidation).
type Sweeper struct {
	cache      *Cache
	evict      func(keyHash string)
	liveness   Liveness
	evictAfter time.Duration
	interval   time.Duration
	inst       *metrics.Instruments
	logger     *slog.Logger
	now        func() time.Time // injectable for testing
}

// NewSweeper constructs a Sweeper. evict is called after a cache entry is
// successfully deleted; it should close the upstream SSE connection and delete
// the persisted snapshot. liveness gates eviction: if HasSubscribers returns
// true the entry is skipped regardless of its last-access time.
func NewSweeper(
	c *Cache,
	evict func(keyHash string),
	liveness Liveness,
	evictAfter time.Duration,
	inst *metrics.Instruments,
	logger *slog.Logger,
) *Sweeper {
	return &Sweeper{
		cache:      c,
		evict:      evict,
		liveness:   liveness,
		evictAfter: evictAfter,
		interval:   inactivitySweepInterval,
		inst:       inst,
		logger:     logger,
		now:        time.Now,
	}
}

// Run ticks at InactivitySweepInterval and evicts inactive entries.
// It blocks until ctx is cancelled.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep()
		}
	}
}

func (s *Sweeper) sweep() {
	now := s.now()
	cutoff := now.Add(-s.evictAfter)

	for keyHash := range s.cache.Snapshot() {
		lastAccess := s.cache.LastAccess(keyHash)
		if lastAccess.IsZero() || !lastAccess.Before(cutoff) {
			continue
		}
		if s.liveness.HasSubscribers(keyHash) {
			s.logger.Debug("cache: inactivity sweeper: skipping key with active subscribers",
				slog.String("key_hash", keyHash),
			)
			continue
		}
		if s.cache.DeleteIfInactiveSince(keyHash, cutoff) {
			idleFor := now.Sub(lastAccess)
			s.logger.Info("cache: evicted inactive entry",
				slog.String("key_hash", keyHash),
				slog.Duration("idle_for", idleFor),
			)
			s.inst.CacheInactivityEvictionsTotal.Add(context.Background(), 1)
			s.evict(keyHash)
		}
	}
}
