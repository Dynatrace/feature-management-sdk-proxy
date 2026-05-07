package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

func noopInstruments(t *testing.T) *metrics.Instruments {
	t.Helper()
	inst, err := metrics.Register(noop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("metrics.Register: %v", err)
	}
	return inst
}

type mockLiveness struct {
	mu   sync.Mutex
	keys map[string]bool
}

func (m *mockLiveness) HasSubscribers(keyHash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keys[keyHash]
}

func newSweeper(c *Cache, evict func(string), liveness Liveness, evictAfter time.Duration, inst *metrics.Instruments) *Sweeper {
	s := NewSweeper(c, evict, liveness, evictAfter, inst, noopLogger())
	return s
}

func TestSweeper_EvidesStaleEntry_NoSubscribers(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v")})

	// Backdate the last-access time by manipulating it directly through the atomic.
	c.mu.RLock()
	la := c.lastAccess["k1"]
	c.mu.RUnlock()
	la.Store(time.Now().Add(-49 * time.Hour).UnixNano())

	evicted := []string{}
	var mu sync.Mutex
	evict := func(key string) {
		mu.Lock()
		evicted = append(evicted, key)
		mu.Unlock()
	}

	liveness := &mockLiveness{keys: map[string]bool{}}
	inst := noopInstruments(t)
	s := newSweeper(c, evict, liveness, 48*time.Hour, inst)
	s.sweep()

	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0] != "k1" {
		t.Errorf("expected [k1] evicted, got %v", evicted)
	}
	_, ok := c.Get("k1")
	if ok {
		t.Error("entry should be absent after sweep")
	}
}

func TestSweeper_SkipsStaleEntry_WithActiveSubscribers(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v")})

	c.mu.RLock()
	la := c.lastAccess["k1"]
	c.mu.RUnlock()
	la.Store(time.Now().Add(-49 * time.Hour).UnixNano())

	evicted := []string{}
	var mu sync.Mutex
	evict := func(key string) {
		mu.Lock()
		evicted = append(evicted, key)
		mu.Unlock()
	}

	liveness := &mockLiveness{keys: map[string]bool{"k1": true}} // has active subscribers
	inst := noopInstruments(t)
	s := newSweeper(c, evict, liveness, 48*time.Hour, inst)
	s.sweep()

	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 0 {
		t.Errorf("expected no evictions for active key, got %v", evicted)
	}
	_, ok := c.Get("k1")
	if !ok {
		t.Error("entry should still be present when subscribers are active")
	}
}

func TestSweeper_SkipsFreshEntry(t *testing.T) {
	c := New()
	c.Set("k1", Entry{Payload: []byte("v")})
	// LastAccess is just set by Set — definitely fresh.

	evicted := []string{}
	var mu sync.Mutex
	evict := func(key string) {
		mu.Lock()
		evicted = append(evicted, key)
		mu.Unlock()
	}

	liveness := &mockLiveness{}
	inst := noopInstruments(t)
	s := newSweeper(c, evict, liveness, 48*time.Hour, inst)
	s.sweep()

	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 0 {
		t.Errorf("expected no evictions for fresh entry, got %v", evicted)
	}
}

func TestSweeper_MultipleEntries_OnlyEvictsStale(t *testing.T) {
	c := New()
	c.Set("stale", Entry{Payload: []byte("old")})
	c.Set("fresh", Entry{Payload: []byte("new")})

	c.mu.RLock()
	c.lastAccess["stale"].Store(time.Now().Add(-49 * time.Hour).UnixNano())
	c.mu.RUnlock()

	evicted := []string{}
	var mu sync.Mutex
	evict := func(key string) {
		mu.Lock()
		evicted = append(evicted, key)
		mu.Unlock()
	}

	liveness := &mockLiveness{}
	inst := noopInstruments(t)
	s := newSweeper(c, evict, liveness, 48*time.Hour, inst)
	s.sweep()

	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0] != "stale" {
		t.Errorf("expected [stale] evicted, got %v", evicted)
	}
	_, ok := c.Get("fresh")
	if !ok {
		t.Error("fresh entry should survive sweep")
	}
}

func TestSweeper_Run_ExitsOnContextCancel(t *testing.T) {
	c := New()
	liveness := &mockLiveness{}
	inst := noopInstruments(t)
	s := newSweeper(c, func(string) {}, liveness, 48*time.Hour, inst)
	s.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Sweeper.Run did not exit after context cancellation")
	}
}

func TestSweeper_EmptyCache_NoEvictions(t *testing.T) {
	c := New()
	evicted := 0
	liveness := &mockLiveness{}
	inst := noopInstruments(t)
	s := newSweeper(c, func(string) { evicted++ }, liveness, 48*time.Hour, inst)
	s.sweep()
	if evicted != 0 {
		t.Errorf("expected 0 evictions on empty cache, got %d", evicted)
	}
}
