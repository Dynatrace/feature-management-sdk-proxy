// Package billingtest provides an in-process BillingReporter for use in tests.
// It lives in a sub-package so that packages outside of telemetry (e.g.
// internal/telemetry/flush in sdk-proxy-awa) can import it without a cycle.
package billingtest

import (
	"context"
	"sync"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/telemetry"
)

// Reporter is a thread-safe BillingReporter for tests. It records every Report
// call and can inject a fixed error for all subsequent calls.
type Reporter struct {
	mu    sync.Mutex
	err   error
	calls []telemetry.Sample
}

// Report records the sample and returns the injected error (if any). Safe for concurrent callers.
func (r *Reporter) Report(_ context.Context, s telemetry.Sample) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
	return r.err
}

// Calls returns a defensive copy of all recorded samples in call order.
func (r *Reporter) Calls() []telemetry.Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	out := make([]telemetry.Sample, len(r.calls))
	copy(out, r.calls)
	return out
}

// SetError sets the error returned by all subsequent Report calls.
func (r *Reporter) SetError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// Error returns the currently injected error.
func (r *Reporter) Error() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// Reset clears recorded calls. The injected error is preserved.
func (r *Reporter) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}
