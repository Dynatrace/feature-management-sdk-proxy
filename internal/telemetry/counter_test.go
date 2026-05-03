package telemetry

import (
	"sync"
	"testing"

	"github.com/dynatrace/sdk-proxy/internal/cache"
)

func TestCounter_ZeroState_DrainReturnsNil(t *testing.T) {
	c := NewCounter()
	samples := c.Drain()
	if len(samples) != 0 {
		t.Errorf("fresh counter: Drain returned %d samples, want 0", len(samples))
	}
}

func TestCounter_SingleIncrement(t *testing.T) {
	c := NewCounter()
	const key = "dt_server_us_abc123"
	c.Increment(key)

	samples := c.Drain()
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	s := samples[0]
	if s.Count != 1 {
		t.Errorf("Count = %d, want 1", s.Count)
	}
	if s.SDKKey != key {
		t.Errorf("SDKKey = %q, want %q", s.SDKKey, key)
	}
	if s.KeyHash != cache.Key(key) {
		t.Errorf("KeyHash = %q, want %q", s.KeyHash, cache.Key(key))
	}
}

func TestCounter_MultipleIncrements_Accumulate(t *testing.T) {
	c := NewCounter()
	const key = "dt_server_eu_tok1"
	for range 5 {
		c.Increment(key)
	}

	samples := c.Drain()
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	if samples[0].Count != 5 {
		t.Errorf("Count = %d, want 5", samples[0].Count)
	}
}

func TestCounter_MultipleKeys(t *testing.T) {
	c := NewCounter()
	keys := []string{
		"dt_server_us_tok1",
		"dt_server_eu_tok2",
		"dt_server_ap_tok3",
	}
	for i, k := range keys {
		for range i + 1 {
			c.Increment(k)
		}
	}

	samples := c.Drain()
	if len(samples) != len(keys) {
		t.Fatalf("want %d samples, got %d", len(keys), len(samples))
	}

	counts := make(map[string]int64, len(samples))
	for _, s := range samples {
		counts[s.SDKKey] = s.Count
	}
	for i, k := range keys {
		want := int64(i + 1)
		if counts[k] != want {
			t.Errorf("key %q: Count = %d, want %d", k, counts[k], want)
		}
	}
}

func TestCounter_DrainResetsCount(t *testing.T) {
	c := NewCounter()
	const key = "dt_server_us_abc123"
	c.Increment(key)
	c.Increment(key)

	first := c.Drain()
	if len(first) != 1 || first[0].Count != 2 {
		t.Fatalf("first Drain: want [{Count:2}], got %v", first)
	}

	second := c.Drain()
	if len(second) != 0 {
		t.Errorf("second Drain after reset: want 0 samples, got %d", len(second))
	}
}

func TestCounter_DrainEmpty_AfterZeroIncrements(t *testing.T) {
	c := NewCounter()
	const key = "dt_server_us_abc123"
	c.Increment(key)
	c.Drain()

	// No new increments — Drain must return empty.
	samples := c.Drain()
	if len(samples) != 0 {
		t.Errorf("want 0 samples after idle drain, got %d", len(samples))
	}
}

func TestCounter_KeyHash_MatchesCacheKey(t *testing.T) {
	c := NewCounter()
	const key = "dt_server_us_mytoken"
	c.Increment(key)

	samples := c.Drain()
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	want := cache.Key(key)
	if samples[0].KeyHash != want {
		t.Errorf("KeyHash = %q, want cache.Key(%q) = %q", samples[0].KeyHash, key, want)
	}
}

func TestCounter_RawKeyRoundTrip(t *testing.T) {
	c := NewCounter()
	const key = "dt_server_us_raw_sdk_key"
	c.Increment(key)

	samples := c.Drain()
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	if samples[0].SDKKey != key {
		t.Errorf("SDKKey = %q, want %q", samples[0].SDKKey, key)
	}
}

func TestCounter_ConcurrentIncrements_Race(t *testing.T) {
	c := NewCounter()
	const key = "dt_server_us_concurrent"
	const goroutines = 50
	const incPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range incPerGoroutine {
				c.Increment(key)
			}
		}()
	}
	wg.Wait()

	samples := c.Drain()
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	want := int64(goroutines * incPerGoroutine)
	if samples[0].Count != want {
		t.Errorf("Count = %d, want %d", samples[0].Count, want)
	}
}

func TestCounter_ConcurrentIncrementAndDrain_NoLostCounts(t *testing.T) {
	c := NewCounter()
	const key = "dt_server_us_drainconcurrent"
	const goroutines = 20
	const incPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range incPerGoroutine {
				c.Increment(key)
			}
		}()
	}

	// Drain repeatedly while increments are in-flight; collect totals.
	var drainTotal int64
	var drainMu sync.Mutex
	stop := make(chan struct{})
	draindone := make(chan struct{})
	go func() {
		defer close(draindone)
		for {
			select {
			case <-stop:
				return
			default:
				for _, s := range c.Drain() {
					drainMu.Lock()
					drainTotal += s.Count
					drainMu.Unlock()
				}
			}
		}
	}()

	wg.Wait()
	close(stop)
	<-draindone

	// Sweep up anything that landed after the last Drain.
	for _, s := range c.Drain() {
		drainTotal += s.Count
	}

	want := int64(goroutines * incPerGoroutine)
	if drainTotal != want {
		t.Errorf("total drained = %d, want %d — increments must not be lost", drainTotal, want)
	}
}
