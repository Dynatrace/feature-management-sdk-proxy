package telemetry

// RetryBuffer is the seam for the bounded retry buffer (sdk-proxy-1hj).
// Enqueue adds a sample to the buffer. Drain returns all buffered samples
// and empties the buffer. Implementations must be safe for concurrent callers.
type RetryBuffer interface {
	Enqueue(Sample)
	Drain() []Sample
}

type noopRetryBuffer struct{}

// NewNoopRetryBuffer returns a RetryBuffer that silently discards all samples.
// It is the default until the bounded implementation (sdk-proxy-1hj) is wired.
func NewNoopRetryBuffer() RetryBuffer {
	return noopRetryBuffer{}
}

func (noopRetryBuffer) Enqueue(Sample)  {}
func (noopRetryBuffer) Drain() []Sample { return nil }
