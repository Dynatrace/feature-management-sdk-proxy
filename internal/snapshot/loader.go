package snapshot

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

// OldestCapturedAt returns the minimum FetchedAt timestamp across all entries
// currently in c. Returns (zero, false) when the cache is empty.
func OldestCapturedAt(c *cache.Cache) (time.Time, bool) {
	var oldest time.Time
	found := false
	for _, entry := range c.Snapshot() {
		if !found || entry.FetchedAt.Before(oldest) {
			oldest = entry.FetchedAt
			found = true
		}
	}
	return oldest, found
}

// LoadAll reads every snapshot from store, decodes the envelope, and populates
// c with the restored entries. Entries with unknown versions or corrupt envelopes
// are skipped with a warning log and a metric increment — they never fail startup.
//
// Restored entries intentionally have FetchURL="" because the raw SDK key is not
// persisted. The proxy handler repairs FetchURL on the first SDK request for that
// key so the background refresher can later revalidate it.
//
// Returns the number of entries successfully loaded.
func LoadAll(ctx context.Context, store Store, c *cache.Cache, inst *metrics.Instruments, logger *slog.Logger) (int, error) {
	keys, err := store.List(ctx)
	if err != nil {
		inst.SnapshotLoadTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "backend_error")))
		return 0, err
	}

	loaded := 0
	for _, keyID := range keys {
		body, err := store.Get(ctx, keyID)
		if err != nil {
			logger.Warn("snapshot: load: failed to get object",
				slog.String("key", keyID),
				slog.String("error", err.Error()),
			)
			inst.SnapshotLoadTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "backend_error")))
			continue
		}

		gotHash, etag, capturedAt, payload, err := DecodeEnvelope(body)
		if err != nil {
			if errors.Is(err, ErrUnknownVersion) {
				logger.Warn("snapshot: load: skipping object with unknown version",
					slog.String("key", keyID),
				)
				inst.SnapshotLoadTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "skipped_unknown_version")))
			} else {
				logger.Warn("snapshot: load: skipping corrupt envelope",
					slog.String("key", keyID),
					slog.String("error", err.Error()),
				)
				inst.SnapshotLoadTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "skipped_corrupt")))
			}
			continue
		}

		// Prefer the key hash from the object key (used for cache.Set) over the
		// one embedded in the envelope — they should match, but the object key is
		// canonical since it was used to write the object.
		_ = gotHash

		c.Set(keyID, cache.Entry{
			Payload:   payload,
			ETag:      etag,
			FetchedAt: capturedAt,
			// FetchURL intentionally empty — repaired on first SDK request.
		})

		if inst.SnapshotAgeSeconds != nil {
			age := time.Since(capturedAt).Seconds()
			inst.SnapshotAgeSeconds.Record(ctx, age,
				metric.WithAttributes(attribute.String("sdk_key_public_id", keyID)),
			)
		}

		inst.SnapshotLoadTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "loaded")))
		loaded++
	}

	return loaded, nil
}
