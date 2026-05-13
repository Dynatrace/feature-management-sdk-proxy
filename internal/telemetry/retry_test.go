package telemetry

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

// Compile-time assertions.
var _ RetryBuffer = (*noopRetryBuffer)(nil)
var _ RetryBuffer = (*boundedRetryBuffer)(nil)

func newRetryTestInst(t *testing.T) *metrics.Instruments {
	t.Helper()
	inst, err := metrics.Register(noop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("metrics.Register: %v", err)
	}
	return inst
}

func TestNoopRetryBuffer_EnqueueAndDrain(t *testing.T) {
	buf := NewNoopRetryBuffer()

	buf.Enqueue(Sample{KeyID: "aabbccddeeff0011", Count: 1})
	buf.Enqueue(Sample{KeyID: "1122334455667788", Count: 2})

	if got := buf.Drain(); len(got) != 0 {
		t.Errorf("noopRetryBuffer.Drain() = %d samples, want 0", len(got))
	}
}

func TestBoundedRetryBuffer_DrainFIFO(t *testing.T) {
	buf := NewBoundedRetryBuffer(10, newRetryTestInst(t), slog.Default())

	s1 := Sample{KeyID: "aabbccddeeff0011", Count: 1}
	s2 := Sample{KeyID: "1122334455667788", Count: 2}
	s3 := Sample{KeyID: "99aabbccddeeff00", Count: 3}
	buf.Enqueue(s1)
	buf.Enqueue(s2)
	buf.Enqueue(s3)

	got := buf.Drain()
	if len(got) != 3 {
		t.Fatalf("Drain returned %d samples, want 3", len(got))
	}
	if got[0].KeyID != s1.KeyID || got[1].KeyID != s2.KeyID || got[2].KeyID != s3.KeyID {
		t.Errorf("FIFO order not preserved: got key hashes %v, %v, %v",
			got[0].KeyID, got[1].KeyID, got[2].KeyID)
	}
}

func TestBoundedRetryBuffer_DrainEmptiesBuffer(t *testing.T) {
	buf := NewBoundedRetryBuffer(5, newRetryTestInst(t), slog.Default())
	buf.Enqueue(Sample{KeyID: "aabbccddeeff0011", Count: 1})
	buf.Drain()

	if got := buf.Drain(); got != nil {
		t.Errorf("second Drain() = %v, want nil", got)
	}
}

func TestBoundedRetryBuffer_DrainEmpty_ReturnsNil(t *testing.T) {
	buf := NewBoundedRetryBuffer(5, newRetryTestInst(t), slog.Default())
	if got := buf.Drain(); got != nil {
		t.Errorf("Drain on empty buffer = %v, want nil", got)
	}
}

func TestBoundedRetryBuffer_Overflow_EvictsOldest(t *testing.T) {
	const bufSize = 3
	buf := NewBoundedRetryBuffer(bufSize, newRetryTestInst(t), slog.Default())

	oldest := Sample{KeyID: "oldest000000ffff", Count: 1}
	mid := Sample{KeyID: "middle000000ffff", Count: 2}
	newest := Sample{KeyID: "newest000000ffff", Count: 3}
	overflow := Sample{KeyID: "overflow0000ffff", Count: 4}

	buf.Enqueue(oldest)
	buf.Enqueue(mid)
	buf.Enqueue(newest)
	buf.Enqueue(overflow) // evicts oldest

	got := buf.Drain()
	if len(got) != bufSize {
		t.Fatalf("got %d samples after overflow, want %d", len(got), bufSize)
	}
	for _, s := range got {
		if s.KeyID == oldest.KeyID {
			t.Errorf("oldest sample should have been evicted but was found in buffer")
		}
	}
	if got[len(got)-1].KeyID != overflow.KeyID {
		t.Errorf("overflow sample %q not at end, got %q", overflow.KeyID, got[len(got)-1].KeyID)
	}
}

func TestBoundedRetryBuffer_Overflow_LogsDroppedSample(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	windowStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	windowEnd := time.Date(2026, 5, 1, 0, 1, 0, 0, time.UTC)
	dropped := Sample{
		KeyID:       "dropdrop00000000",
		Count:       42,
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
	}

	buf := NewBoundedRetryBuffer(1, newRetryTestInst(t), logger)
	buf.Enqueue(dropped)
	buf.Enqueue(Sample{KeyID: "newentry00000000", Count: 1}) // triggers overflow

	logOutput := logBuf.String()
	if !strings.Contains(logOutput, "retry buffer full") {
		t.Errorf("log missing 'retry buffer full': %s", logOutput)
	}
	if !strings.Contains(logOutput, dropped.KeyID) {
		t.Errorf("log missing key_hash %q: %s", dropped.KeyID, logOutput)
	}
}

func TestBoundedRetryBuffer_Concurrent(t *testing.T) {
	buf := NewBoundedRetryBuffer(100, newRetryTestInst(t), slog.Default())

	const goroutines = 50
	const enqueuesPerG = 10

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for range goroutines {
		go func() {
			defer wg.Done()
			for range enqueuesPerG {
				buf.Enqueue(Sample{KeyID: "aabbccddeeff0011", Count: 1})
			}
		}()
	}
	for range goroutines {
		go func() {
			defer wg.Done()
			buf.Drain()
		}()
	}

	wg.Wait() // no panics, no data races under -race
}
