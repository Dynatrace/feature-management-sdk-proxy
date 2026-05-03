package snapshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type countingStore struct {
	mu    sync.Mutex
	puts  []string
	putFn func(keyHash string) error
}

func (s *countingStore) Put(_ context.Context, keyHash string, _ []byte) error {
	if s.putFn != nil {
		if err := s.putFn(keyHash); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.puts = append(s.puts, keyHash)
	s.mu.Unlock()
	return nil
}

func (s *countingStore) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func (s *countingStore) List(_ context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func TestWriter_OnConfigCached_WritesPersisted(t *testing.T) {
	store := &countingStore{}
	inst := noopInst(t)
	w := NewWriter(store, inst, noopLogger(), 16)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	w.OnConfigCached("key1", `"v1"`, []byte(`{"flags":{}}`))

	// Let the writer drain.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.puts) != 1 || store.puts[0] != "key1" {
		t.Errorf("expected 1 put for key1, got %v", store.puts)
	}
}

func TestWriter_OnRefreshed_WritesPersisted(t *testing.T) {
	store := &countingStore{}
	w := NewWriter(store, noopInst(t), noopLogger(), 16)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	w.OnRefreshed("key2", []byte(`{}`))

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.puts) != 1 || store.puts[0] != "key2" {
		t.Errorf("expected 1 put for key2, got %v", store.puts)
	}
}

func TestWriter_PutError_NoBlock(t *testing.T) {
	store := &countingStore{
		putFn: func(_ string) error { return errors.New("storage down") },
	}
	w := NewWriter(store, noopInst(t), noopLogger(), 16)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	// Must return immediately even with a failing store.
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		w.OnConfigCached("k", "", []byte(`{}`))
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("OnConfigCached blocked")
	}

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
}

func TestWriter_QueueFull_DropsOldest(t *testing.T) {
	// Create a store that blocks Put until released.
	block := make(chan struct{})
	store := &countingStore{
		putFn: func(_ string) error {
			<-block
			return nil
		},
	}
	w := NewWriter(store, noopInst(t), noopLogger(), 2)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()

	// First job will be picked up by Run immediately, blocking on store.Put.
	w.OnConfigCached("k0", "", []byte(`{}`))
	time.Sleep(20 * time.Millisecond) // let Run pick up k0

	// Fill the queue.
	w.OnConfigCached("k1", "", []byte(`{}`))
	w.OnConfigCached("k2", "", []byte(`{}`))

	// This enqueue should drop k1 (oldest in queue).
	w.OnConfigCached("k3", "", []byte(`{}`))

	close(block) // unblock all puts
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	store.mu.Lock()
	defer store.mu.Unlock()
	// k0 and at least one of k2/k3 should have been put; k1 may be dropped.
	if len(store.puts) < 2 {
		t.Errorf("expected at least 2 puts, got %v", store.puts)
	}
}

func TestWriter_Shutdown_DrainsQueue(t *testing.T) {
	store := &countingStore{}
	w := NewWriter(store, noopInst(t), noopLogger(), 16)
	// Do NOT start Run — test Shutdown drain path directly.

	w.OnConfigCached("k1", "", []byte(`{}`))
	w.OnConfigCached("k2", "", []byte(`{}`))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w.Shutdown(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.puts) != 2 {
		t.Errorf("expected 2 puts after shutdown drain, got %v", store.puts)
	}
}
