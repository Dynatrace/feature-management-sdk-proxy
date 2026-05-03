package telemetry

import (
	"context"
	"log/slog"
	"sync"

	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

// RetryBuffer is the seam for the bounded retry buffer.
// Enqueue adds a sample to the buffer. Drain returns all buffered samples
// and empties the buffer. Implementations must be safe for concurrent callers.
type RetryBuffer interface {
	Enqueue(Sample)
	Drain() []Sample
}

type noopRetryBuffer struct{}

// NewNoopRetryBuffer returns a RetryBuffer that silently discards all samples.
func NewNoopRetryBuffer() RetryBuffer {
	return noopRetryBuffer{}
}

func (noopRetryBuffer) Enqueue(Sample)  {}
func (noopRetryBuffer) Drain() []Sample { return nil }

type boundedRetryBuffer struct {
	mu      sync.Mutex
	buf     []Sample
	maxSize int
	inst    *metrics.Instruments
	log     *slog.Logger
}

// NewBoundedRetryBuffer returns a RetryBuffer that holds at most size Samples.
// When full, Enqueue evicts the oldest entry, emits a structured warning, and
// increments proxy.billing.retry_buffer.dropped_total.
func NewBoundedRetryBuffer(size int, inst *metrics.Instruments, logger *slog.Logger) RetryBuffer {
	return &boundedRetryBuffer{
		buf:     make([]Sample, 0, size),
		maxSize: size,
		inst:    inst,
		log:     logger,
	}
}

func (b *boundedRetryBuffer) Enqueue(s Sample) {
	var dropped *Sample
	var newLen int

	b.mu.Lock()
	if len(b.buf) >= b.maxSize {
		d := b.buf[0]
		dropped = &d
		copy(b.buf, b.buf[1:])
		b.buf = b.buf[:len(b.buf)-1]
	}
	b.buf = append(b.buf, s)
	newLen = len(b.buf)
	b.mu.Unlock()

	if dropped != nil {
		b.log.Warn("billing: retry buffer full, dropping oldest sample",
			slog.String("key_hash", dropped.KeyHash),
			slog.Int64("count", dropped.Count),
			slog.Time("window_start", dropped.WindowStart),
			slog.Time("window_end", dropped.WindowEnd),
		)
		b.inst.BillingRetryBufferDropped.Add(context.Background(), 1)
	}
	b.inst.BillingBufferSize.Record(context.Background(), int64(newLen))
}

func (b *boundedRetryBuffer) Drain() []Sample {
	b.mu.Lock()
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return nil
	}
	out := make([]Sample, len(b.buf))
	copy(out, b.buf)
	b.buf = b.buf[:0]
	b.mu.Unlock()
	b.inst.BillingBufferSize.Record(context.Background(), 0)
	return out
}
