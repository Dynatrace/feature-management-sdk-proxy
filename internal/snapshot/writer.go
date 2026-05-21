package snapshot

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
)

// writeJob is an item queued for async persistence.
type writeJob struct {
	keyID    string
	etag     string
	payload  []byte
	isDelete bool // when true, Delete is called instead of Put
}

// Writer persists cache entries to a Store asynchronously. It satisfies
// cache.RefreshObserver and is also called directly by the proxy handler
// after a cold fetch.
//
// Writes are never on the hot path. A failed Put logs a warning and increments
// a metric; it never blocks or fails the SDK request. When the queue is full,
// the oldest pending job is dropped (same pattern as the billing retry buffer).
type Writer struct {
	store  Store
	inst   *metrics.Instruments
	logger *slog.Logger
	queue  chan writeJob
	mu     sync.Mutex // serializes concurrent enqueue callers
}

// NewWriter constructs a Writer. queueSize is the maximum number of pending
// write jobs; when full, the oldest is dropped. Call Run to start the drain loop.
func NewWriter(store Store, inst *metrics.Instruments, logger *slog.Logger, queueSize int) *Writer {
	if queueSize <= 0 {
		queueSize = 256
	}
	return &Writer{
		store:  store,
		inst:   inst,
		logger: logger,
		queue:  make(chan writeJob, queueSize),
	}
}

// OnConfigCached is called by the proxy handler after a successful cold fetch (200).
// This implements the hot-path notification; it is non-blocking.
func (w *Writer) OnConfigCached(keyID, etag string, payload []byte) {
	w.enqueue(writeJob{keyID: keyID, etag: etag, payload: payload})
}

// OnRefreshed implements cache.RefreshObserver. It is called by the background
// refresher after a successful upstream 200 (never for 304 responses).
func (w *Writer) OnRefreshed(keyID string, payload []byte) {
	w.enqueue(writeJob{keyID: keyID, payload: payload})
}

// OnInvalidated queues an async delete of the persisted snapshot for keyID.
// It is called by the lifecycle handler when a key is rotated or deleted.
// A failed delete logs a warning and increments a metric; it never blocks.
func (w *Writer) OnInvalidated(keyID string) {
	w.enqueue(writeJob{keyID: keyID, isDelete: true})
}

func (w *Writer) enqueue(job writeJob) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	select {
	case w.queue <- job:
		return
	default:
	}

	// Queue full — drain oldest to make room, then enqueue the new job.
	// With the mutex held, no other producer can refill the slot we free,
	// so the blocking send below is guaranteed to complete immediately.
	var dropped writeJob
	select {
	case dropped = <-w.queue:
		w.logger.Warn("snapshot: write queue full, dropping oldest",
			slog.String("sdk_key_public_id", dropped.keyID),
		)
		w.inst.SnapshotWritesTotal.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("result", "dropped"),
			attribute.String("operation", "put"),
		))
	default:
		// Run drained a slot concurrently; space is already available.
	}
	w.queue <- job
}

// Run drains the write queue until ctx is cancelled, then flushes remaining
// jobs within the remaining deadline. Call this in a dedicated goroutine.
func (w *Writer) Run(ctx context.Context) {
	for {
		select {
		case job := <-w.queue:
			w.persist(job)
		case <-ctx.Done():
			// Drain remaining jobs (best-effort).
			for {
				select {
				case job := <-w.queue:
					w.persist(job)
				default:
					return
				}
			}
		}
	}
}

// Shutdown drains the write queue with the provided deadline context. It is
// intended to be called once after Run has returned, but is safe to call
// concurrently.
func (w *Writer) Shutdown(ctx context.Context) {
	for {
		select {
		case job := <-w.queue:
			w.persist(job)
		case <-ctx.Done():
			if len(w.queue) > 0 {
				w.logger.Warn("snapshot: shutdown timeout; pending writes dropped",
					slog.Int("remaining", len(w.queue)),
				)
			}
			return
		default:
			return
		}
	}
}

func (w *Writer) persist(job writeJob) {
	if job.isDelete {
		w.persistDelete(job.keyID)
		return
	}

	now := time.Now()
	body, err := EncodeEnvelope(job.keyID, job.etag, now, job.payload)
	if err != nil {
		w.logger.Warn("snapshot: encode failed",
			slog.String("sdk_key_public_id", job.keyID),
			slog.String("error", err.Error()),
		)
		w.inst.SnapshotWritesTotal.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("result", "failure"), attribute.String("operation", "put")))
		return
	}

	if err := w.store.Put(context.Background(), job.keyID, body); err != nil {
		w.logger.Warn("snapshot: write failed",
			slog.String("sdk_key_public_id", job.keyID),
			slog.String("error", err.Error()),
		)
		w.inst.SnapshotWritesTotal.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("result", "failure"), attribute.String("operation", "put")))
		return
	}

	w.inst.SnapshotWritesTotal.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("result", "success"), attribute.String("operation", "put")))
	w.inst.SnapshotAgeSeconds.Record(context.Background(), 0,
		metric.WithAttributes(attribute.String("sdk_key_public_id", job.keyID)))
}

func (w *Writer) persistDelete(keyID string) {
	if err := w.store.Delete(context.Background(), keyID); err != nil {
		w.logger.Warn("snapshot: delete failed",
			slog.String("sdk_key_public_id", keyID),
			slog.String("error", err.Error()),
		)
		w.inst.SnapshotWritesTotal.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("result", "failure"), attribute.String("operation", "delete")))
		return
	}
	w.inst.SnapshotWritesTotal.Add(context.Background(), 1,
		metric.WithAttributes(attribute.String("result", "success"), attribute.String("operation", "delete")))
}
