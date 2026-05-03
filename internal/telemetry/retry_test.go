package telemetry

import "testing"

// Compile-time assertion: noopRetryBuffer satisfies RetryBuffer.
var _ RetryBuffer = (*noopRetryBuffer)(nil)

func TestNoopRetryBuffer_EnqueueAndDrain(t *testing.T) {
	buf := NewNoopRetryBuffer()

	buf.Enqueue(Sample{KeyHash: "aabbccddeeff0011", Count: 1})
	buf.Enqueue(Sample{KeyHash: "1122334455667788", Count: 2})

	if got := buf.Drain(); len(got) != 0 {
		t.Errorf("noopRetryBuffer.Drain() = %d samples, want 0", len(got))
	}
}
