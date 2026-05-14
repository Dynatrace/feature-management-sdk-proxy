package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

// fallbackStoreTimeout caps the S3 Get in checkAndApply so a slow or
// unresponsive backend cannot keep the goroutine alive indefinitely.
const fallbackStoreTimeout = 10 * time.Second

// fallbackDebounceInterval suppresses redundant S3 checks during sustained CDN
// outages. The background poller fires every 10s per disconnected key; 60s
// (six ticks) keeps S3 request rates low while reacting within one minute of a
// peer writing a newer snapshot. See ADR-0003.
const fallbackDebounceInterval = 60 * time.Second

// Fallback checks the shared snapshot store for a newer entry when a background
// CDN refresh fails. It implements cache.RefreshFailureObserver.
//
// On each failure notification it first checks whether enough time has passed
// since the last attempt for this key (debounce). If the debounce passes, it
// launches a goroutine that reads the snapshot store and, when the snapshot is
// newer than the in-memory entry, atomically updates the cache via SetIfNewer.
//
// Fallback never writes to the snapshot store. See ADR-0003.
type Fallback struct {
	store  Store
	cache  *cache.Cache
	inst   *metrics.Instruments
	logger *slog.Logger

	mu          sync.Mutex
	lastAttempt map[string]time.Time
}

// NewFallback constructs a Fallback. store and c must not be nil.
func NewFallback(store Store, c *cache.Cache, inst *metrics.Instruments, logger *slog.Logger) *Fallback {
	return &Fallback{
		store:       store,
		cache:       c,
		inst:        inst,
		logger:      logger,
		lastAttempt: make(map[string]time.Time),
	}
}

// OnRefreshFailure implements cache.RefreshFailureObserver. It is called from
// a goroutine already spawned by the refresher, so checkAndApply runs
// synchronously here — no second goroutine is needed.
func (f *Fallback) OnRefreshFailure(ctx context.Context, keyID, currentETag string, currentFetchedAt time.Time) {
	f.mu.Lock()
	if time.Since(f.lastAttempt[keyID]) < fallbackDebounceInterval {
		f.mu.Unlock()
		f.inst.SnapshotOutageFallbackTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("result", "debounced")))
		return
	}
	// Update the attempt timestamp and prune entries that have been idle for
	// longer than two debounce intervals (keys evicted from the cache stop
	// generating calls, so their entries would otherwise linger).
	now := time.Now()
	for k, t := range f.lastAttempt {
		if now.Sub(t) > 2*fallbackDebounceInterval {
			delete(f.lastAttempt, k)
		}
	}
	f.lastAttempt[keyID] = now
	f.mu.Unlock()

	f.checkAndApply(ctx, keyID, currentETag, currentFetchedAt)
}

func (f *Fallback) checkAndApply(ctx context.Context, keyID, currentETag string, currentFetchedAt time.Time) {
	storeCtx, cancel := context.WithTimeout(ctx, fallbackStoreTimeout)
	defer cancel()

	data, err := f.store.Get(storeCtx, keyID)
	if err != nil {
		f.logger.Warn("snapshot: fallback store read failed",
			slog.String("sdk_key_public_id", keyID),
			slog.String("error", err.Error()),
		)
		f.inst.SnapshotOutageFallbackTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("result", "backend_error")))
		return
	}

	_, snapshotETag, capturedAt, payload, err := DecodeEnvelope(data)
	if err != nil {
		if !errors.Is(err, ErrUnknownVersion) {
			f.logger.Warn("snapshot: fallback envelope decode failed",
				slog.String("sdk_key_public_id", keyID),
				slog.String("error", err.Error()),
			)
		}
		f.inst.SnapshotOutageFallbackTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("result", "decode_error")))
		return
	}

	if snapshotETag == currentETag {
		f.inst.SnapshotOutageFallbackTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("result", "same_etag")))
		return
	}
	if !capturedAt.After(currentFetchedAt) {
		f.inst.SnapshotOutageFallbackTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("result", "no_newer")))
		return
	}

	// Preserve the current in-memory FetchURL: the snapshot envelope does not
	// persist it and clearing it would re-trigger demand revalidation on the
	// next request. If the entry was evicted between the failure notification
	// and now, skip — there is nothing to update.
	cur, ok := f.cache.Get(keyID)
	if !ok {
		return
	}

	newEntry := cache.Entry{
		Payload:   payload,
		ETag:      snapshotETag,
		FetchedAt: capturedAt,
		FetchURL:  cur.FetchURL,
	}

	if !f.cache.SetIfNewer(keyID, newEntry, currentFetchedAt, currentETag) {
		f.inst.SnapshotOutageFallbackTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("result", "superseded")))
		return
	}

	f.logger.Info("snapshot: fallback applied newer snapshot from shared store",
		slog.String("sdk_key_public_id", keyID),
		slog.String("snapshot_etag", snapshotETag),
	)
	f.inst.SnapshotOutageFallbackTotal.Add(ctx, 1,
		metric.WithAttributes(attribute.String("result", "loaded")))
}
