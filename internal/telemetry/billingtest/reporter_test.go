package billingtest_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dynatrace/sdk-proxy/internal/telemetry"
	"github.com/dynatrace/sdk-proxy/internal/telemetry/billingtest"
)

// Compile-time assertion: Reporter satisfies the BillingReporter interface.
var _ telemetry.BillingReporter = (*billingtest.Reporter)(nil)

func TestReporter_RecordsCall(t *testing.T) {
	r := &billingtest.Reporter{}
	s := telemetry.Sample{KeyHash: "aabbccddeeff0011", SDKKey: "test-sdk-key-1", Count: 42}

	if err := r.Report(context.Background(), s); err != nil {
		t.Fatalf("Report returned unexpected error: %v", err)
	}

	calls := r.Calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 call recorded, got %d", len(calls))
	}
	if calls[0] != s {
		t.Errorf("recorded sample = %v, want %v", calls[0], s)
	}
}

func TestReporter_CallsReturnsDefensiveCopy(t *testing.T) {
	r := &billingtest.Reporter{}
	s := telemetry.Sample{KeyHash: "1122334455667788", SDKKey: "test-sdk-key-2", Count: 7}
	_ = r.Report(context.Background(), s)

	calls := r.Calls()
	calls[0].Count = 999 // mutate the returned slice

	// Internal state must be unaffected.
	calls2 := r.Calls()
	if calls2[0].Count != 7 {
		t.Errorf("mutation of Calls() result affected internal state: Count = %d, want 7", calls2[0].Count)
	}
}

func TestReporter_InjectedErrorReturned(t *testing.T) {
	r := &billingtest.Reporter{}
	sentinel := errors.New("billing unavailable")
	r.SetError(sentinel)

	s := telemetry.Sample{KeyHash: "ffeeddccbbaa9988", SDKKey: "test-sdk-key-3", Count: 1}
	err := r.Report(context.Background(), s)
	if !errors.Is(err, sentinel) {
		t.Errorf("Report error = %v, want %v", err, sentinel)
	}

	// Call is still recorded even when an error is injected.
	if len(r.Calls()) != 1 {
		t.Errorf("want 1 call recorded, got %d", len(r.Calls()))
	}
}

func TestReporter_Reset_ClearsCallsPreservesError(t *testing.T) {
	r := &billingtest.Reporter{}
	sentinel := errors.New("oops")
	r.SetError(sentinel)

	s := telemetry.Sample{KeyHash: "0102030405060708", SDKKey: "test-sdk-key-4", Count: 3}
	_ = r.Report(context.Background(), s)
	if len(r.Calls()) != 1 {
		t.Fatalf("pre-reset: want 1 call, got %d", len(r.Calls()))
	}

	r.Reset()

	if len(r.Calls()) != 0 {
		t.Errorf("post-reset: want 0 calls, got %d", len(r.Calls()))
	}
	// Injected error must survive Reset.
	if r.Error() == nil {
		t.Error("Reset cleared injected error; expected it to be preserved")
	}
}

func TestReporter_ConcurrentReport_RaceFree(t *testing.T) {
	r := &billingtest.Reporter{}
	const goroutines = 50
	s := telemetry.Sample{KeyHash: "a0b1c2d3e4f56789", SDKKey: "test-sdk-key-5", Count: 1}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			_ = r.Report(context.Background(), s)
		}()
	}
	wg.Wait()

	if len(r.Calls()) != goroutines {
		t.Errorf("want %d calls recorded, got %d", goroutines, len(r.Calls()))
	}
}
