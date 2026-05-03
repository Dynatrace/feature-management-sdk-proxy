package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

// Flusher drains per-token counters on a configurable interval and dispatches
// each Sample to a BillingReporter. Retriable failures are handed to the
// RetryBuffer for re-attempt on the next tick. Flusher.Run blocks until ctx
// is cancelled.
type Flusher struct {
	counter     *Counter
	reporter    BillingReporter
	retry       RetryBuffer
	inst        *metrics.Instruments
	interval    time.Duration
	windowStart time.Time
	logger      *slog.Logger
}

// NewFlusher constructs a Flusher. windowStart is initialised to time.Now().UTC()
// at construction; each tick advances it to the previous windowEnd.
func NewFlusher(
	counter *Counter,
	reporter BillingReporter,
	retry RetryBuffer,
	inst *metrics.Instruments,
	interval time.Duration,
	logger *slog.Logger,
) *Flusher {
	return &Flusher{
		counter:     counter,
		reporter:    reporter,
		retry:       retry,
		inst:        inst,
		interval:    interval,
		windowStart: time.Now().UTC(),
		logger:      logger,
	}
}

// Run ticks at the configured interval until ctx is cancelled.
func (f *Flusher) Run(ctx context.Context) {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.tick(ctx)
		}
	}
}

func (f *Flusher) tick(ctx context.Context) {
	windowEnd := time.Now().UTC()

	// Retry buffered samples first (preserve their original window timestamps).
	// Collect all at once — Drain is destructive, so we re-enqueue the tail if
	// the context is cancelled mid-iteration.
	retrySamples := f.retry.Drain()
	for i, s := range retrySamples {
		if ctx.Err() != nil {
			for _, remaining := range retrySamples[i:] {
				f.enqueueWithLog(remaining)
			}
			f.windowStart = windowEnd
			return
		}
		f.dispatchSample(ctx, s)
	}

	// Fresh samples from the counter — stamp with the current window.
	// Drain is destructive; if cancelled mid-iteration, stamp and enqueue the tail.
	counterSamples := f.counter.Drain()
	for i, s := range counterSamples {
		if ctx.Err() != nil {
			for _, remaining := range counterSamples[i:] {
				remaining.WindowStart = f.windowStart
				remaining.WindowEnd = windowEnd
				f.enqueueWithLog(remaining)
			}
			f.windowStart = windowEnd
			return
		}
		s.WindowStart = f.windowStart
		s.WindowEnd = windowEnd
		f.dispatchSample(ctx, s)
	}

	f.windowStart = windowEnd
}

// enqueueWithLog passes s to the retry buffer and logs the attempt so that
// any subsequent buffer-overflow drop is observable in the structured log.
func (f *Flusher) enqueueWithLog(s Sample) {
	f.logger.Warn("billing: enqueueing sample for retry",
		slog.String("key_hash", s.KeyHash),
		slog.Int64("count", s.Count),
		slog.Time("window_start", s.WindowStart),
		slog.Time("window_end", s.WindowEnd),
	)
	f.retry.Enqueue(s)
}

// Shutdown performs one final drain of the retry buffer and counter and
// dispatches each sample using ctx. Must be called after Run has returned —
// the two must not execute concurrently. ctx should carry a short deadline
// (typically 10s). Samples that cannot be dispatched before ctx expires are
// logged and passed to the retry buffer.
func (f *Flusher) Shutdown(ctx context.Context) {
	f.logger.Info("telemetry: starting final billing flush")
	f.tick(ctx)
	f.logger.Info("telemetry: final billing flush complete")
}

func (f *Flusher) dispatchSample(ctx context.Context, s Sample) {
	timeout := min(2*f.interval, 30*time.Second)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := f.reporter.Report(callCtx, s)
	lag := time.Since(s.WindowStart).Seconds()

	if err == nil {
		f.inst.BillingFlushTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "success")))
		f.inst.BillingFlushLag.Record(ctx, lag)
		return
	}

	if errors.Is(err, ErrPermanent) {
		f.logger.Warn("billing: permanent error, dropping sample",
			slog.String("key_hash", s.KeyHash),
			slog.Int64("count", s.Count),
			slog.Time("window_start", s.WindowStart),
			slog.Time("window_end", s.WindowEnd),
			slog.String("error", err.Error()),
		)
		f.inst.BillingFlushTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "permanent_error")))
		f.inst.BillingFlushLag.Record(ctx, lag)
		return
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		f.inst.BillingFlushTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "ctx_error")))
		f.inst.BillingFlushLag.Record(ctx, lag)
		if ctx.Err() == nil {
			// Per-call timeout only — outer context still live, re-enqueue.
			f.enqueueWithLog(s)
		} else {
			f.logger.Warn("billing: dropping sample on shutdown",
				slog.String("key_hash", s.KeyHash),
				slog.Int64("count", s.Count),
				slog.Time("window_start", s.WindowStart),
				slog.Time("window_end", s.WindowEnd),
			)
		}
		return
	}

	// Any other error is retriable.
	f.enqueueWithLog(s)
	f.inst.BillingFlushTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", "retry")))
	f.inst.BillingFlushLag.Record(ctx, lag)
}
