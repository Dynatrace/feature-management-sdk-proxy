package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

// spyReporter is a thread-safe BillingReporter for flush tests.
// Defined here rather than importing billingtest to avoid an import cycle
// (billingtest imports telemetry, which would be circular from package telemetry).
type spyReporter struct {
	mu    sync.Mutex
	err   error
	calls []Sample
}

func (r *spyReporter) Report(_ context.Context, s Sample) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
	return r.err
}

func (r *spyReporter) SetError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *spyReporter) Calls() []Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Sample, len(r.calls))
	copy(out, r.calls)
	return out
}

// spyRetryBuffer records every enqueued Sample and serves them on Drain.
type spyRetryBuffer struct {
	mu      sync.Mutex
	pending []Sample
	history []Sample
}

func (b *spyRetryBuffer) Enqueue(s Sample) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending = append(b.pending, s)
	b.history = append(b.history, s)
}

func (b *spyRetryBuffer) Drain() []Sample {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.pending
	b.pending = nil
	return out
}

func (b *spyRetryBuffer) History() []Sample {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Sample, len(b.history))
	copy(out, b.history)
	return out
}

func newTestInst(t *testing.T) *metrics.Instruments {
	t.Helper()
	inst, err := metrics.Register(noop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("metrics.Register: %v", err)
	}
	return inst
}

const tickInterval = 20 * time.Millisecond

func newFlusher(counter *Counter, reporter BillingReporter, retry RetryBuffer, inst *metrics.Instruments) *Flusher {
	return NewFlusher(counter, reporter, retry, inst, tickInterval, slog.Default())
}

func runFlusher(t *testing.T, f *Flusher) (ctx context.Context, cancel context.CancelFunc, done chan struct{}) {
	t.Helper()
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()
	return
}

// waitForCalls polls reporter.Calls() until at least n calls are recorded or
// the deadline passes. Returns true if the condition was met.
func waitForCalls(reporter *spyReporter, n int, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if len(reporter.Calls()) >= n {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestFlusher_DrainOnTick(t *testing.T) {
	reporter := &spyReporter{}
	counter := NewCounter()
	counter.Increment("dt_server_us_tok1")

	f := newFlusher(counter, reporter, NewNoopRetryBuffer(), newTestInst(t))
	_, cancel, done := runFlusher(t, f)
	defer func() { cancel(); <-done }()

	if !waitForCalls(reporter, 1, 500*time.Millisecond) {
		t.Fatal("expected at least 1 report call within 500ms")
	}
	cancel()
	<-done

	calls := reporter.Calls()
	if len(calls) == 0 {
		t.Fatal("no calls recorded")
	}
	if calls[0].Count != 1 {
		t.Errorf("Count = %d, want 1", calls[0].Count)
	}
}

func TestFlusher_WindowFieldsPopulated(t *testing.T) {
	reporter := &spyReporter{}
	counter := NewCounter()
	counter.Increment("dt_server_us_tok1")

	before := time.Now().UTC()
	f := newFlusher(counter, reporter, NewNoopRetryBuffer(), newTestInst(t))
	_, cancel, done := runFlusher(t, f)
	defer func() { cancel(); <-done }()

	if !waitForCalls(reporter, 1, 500*time.Millisecond) {
		t.Fatal("expected at least 1 report call within 500ms")
	}
	cancel()
	<-done
	after := time.Now().UTC()

	calls := reporter.Calls()
	if len(calls) == 0 {
		t.Fatal("no calls recorded")
	}
	s := calls[0]
	if s.WindowStart.IsZero() {
		t.Error("WindowStart is zero — flush loop must populate it")
	}
	if s.WindowEnd.IsZero() {
		t.Error("WindowEnd is zero — flush loop must populate it")
	}
	if s.WindowStart.Before(before) || s.WindowStart.After(after) {
		t.Errorf("WindowStart %v outside expected range [%v, %v]", s.WindowStart, before, after)
	}
	if !s.WindowEnd.After(s.WindowStart) && !s.WindowEnd.Equal(s.WindowStart) {
		t.Errorf("WindowEnd %v not >= WindowStart %v", s.WindowEnd, s.WindowStart)
	}
}

func TestFlusher_ZeroSampleTick_NoOp(t *testing.T) {
	reporter := &spyReporter{}
	counter := NewCounter() // no increments

	f := newFlusher(counter, reporter, NewNoopRetryBuffer(), newTestInst(t))
	_, cancel, done := runFlusher(t, f)

	// Wait for several ticks to confirm no calls.
	time.Sleep(5 * tickInterval)
	cancel()
	<-done

	if len(reporter.Calls()) != 0 {
		t.Errorf("expected 0 calls for empty counter, got %d", len(reporter.Calls()))
	}
}

func TestFlusher_PermanentError_Drops(t *testing.T) {
	reporter := &spyReporter{}
	reporter.SetError(ErrPermanent)

	counter := NewCounter()
	counter.Increment("dt_server_us_tok1")

	spy := &spyRetryBuffer{}
	f := newFlusher(counter, reporter, spy, newTestInst(t))
	_, cancel, done := runFlusher(t, f)
	defer func() { cancel(); <-done }()

	if !waitForCalls(reporter, 1, 500*time.Millisecond) {
		t.Fatal("expected at least 1 report attempt within 500ms")
	}
	cancel()
	<-done

	if len(spy.History()) != 0 {
		t.Errorf("permanent error must not enqueue to retry buffer, got %d entries", len(spy.History()))
	}
}

func TestFlusher_RetriableError_Enqueues(t *testing.T) {
	reporter := &spyReporter{}
	retriableErr := errors.New("upstream unavailable")
	reporter.SetError(retriableErr)

	counter := NewCounter()
	counter.Increment("dt_server_us_tok1")

	spy := &spyRetryBuffer{}
	f := newFlusher(counter, reporter, spy, newTestInst(t))
	_, cancel, done := runFlusher(t, f)
	defer func() { cancel(); <-done }()

	if !waitForCalls(reporter, 1, 500*time.Millisecond) {
		t.Fatal("expected at least 1 report attempt within 500ms")
	}
	cancel()
	<-done

	if len(spy.History()) == 0 {
		t.Error("retriable error must enqueue sample to retry buffer")
	}
}

func TestFlusher_RetryBeforeFresh(t *testing.T) {
	// Pre-load the retry buffer with a sample before starting the flusher.
	spy := &spyRetryBuffer{}
	retrySample := Sample{
		KeyHash:     "aabbccddeeff0011",
		SDKKey:      "dt_server_us_retry_key",
		Count:       5,
		WindowStart: time.Now().Add(-2 * time.Minute).UTC(),
		WindowEnd:   time.Now().Add(-1 * time.Minute).UTC(),
	}
	spy.Enqueue(retrySample)

	counter := NewCounter()
	counter.Increment("dt_server_us_fresh_key")

	reporter := &spyReporter{}
	f := newFlusher(counter, reporter, spy, newTestInst(t))
	_, cancel, done := runFlusher(t, f)
	defer func() { cancel(); <-done }()

	// Wait for both samples to be reported.
	if !waitForCalls(reporter, 2, 500*time.Millisecond) {
		t.Fatalf("expected at least 2 calls within 500ms, got %d", len(reporter.Calls()))
	}
	cancel()
	<-done

	calls := reporter.Calls()
	if len(calls) < 2 {
		t.Fatalf("want >= 2 calls, got %d", len(calls))
	}
	// First call must be the retried sample.
	if calls[0].KeyHash != retrySample.KeyHash {
		t.Errorf("first call KeyHash = %q, want retry sample %q", calls[0].KeyHash, retrySample.KeyHash)
	}
}

func TestFlusher_RetriedSample_PreservesWindow(t *testing.T) {
	// Pre-load retry buffer with a sample that has specific timestamps.
	spy := &spyRetryBuffer{}
	originalStart := time.Now().Add(-90 * time.Second).UTC()
	originalEnd := time.Now().Add(-30 * time.Second).UTC()
	preloaded := Sample{
		KeyHash:     "ffeeddccbbaa9988",
		SDKKey:      "dt_server_us_preloaded",
		Count:       7,
		WindowStart: originalStart,
		WindowEnd:   originalEnd,
	}
	spy.Enqueue(preloaded)

	reporter := &spyReporter{}
	f := newFlusher(NewCounter(), reporter, spy, newTestInst(t))
	_, cancel, done := runFlusher(t, f)
	defer func() { cancel(); <-done }()

	if !waitForCalls(reporter, 1, 500*time.Millisecond) {
		t.Fatal("expected at least 1 call within 500ms")
	}
	cancel()
	<-done

	calls := reporter.Calls()
	if len(calls) == 0 {
		t.Fatal("no calls recorded")
	}

	var found bool
	for _, c := range calls {
		if c.KeyHash == preloaded.KeyHash {
			found = true
			if !c.WindowStart.Equal(originalStart) {
				t.Errorf("retried sample WindowStart changed: got %v, want %v", c.WindowStart, originalStart)
			}
			if !c.WindowEnd.Equal(originalEnd) {
				t.Errorf("retried sample WindowEnd changed: got %v, want %v", c.WindowEnd, originalEnd)
			}
		}
	}
	if !found {
		t.Error("retried sample not found in reporter calls")
	}
}

func TestFlusher_CtxCancel_ExitsRun(t *testing.T) {
	reporter := &spyReporter{}
	f := newFlusher(NewCounter(), reporter, NewNoopRetryBuffer(), newTestInst(t))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Run(ctx)
	}()

	cancel()

	select {
	case <-done:
		// expected
	case <-time.After(500 * time.Millisecond):
		t.Error("Run did not exit within 500ms after ctx cancel")
	}
}
