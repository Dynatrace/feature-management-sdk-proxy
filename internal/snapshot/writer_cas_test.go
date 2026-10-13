package snapshot

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
)

func storedRecord(t *testing.T, store Store, keyID string) Record {
	t.Helper()
	body, err := store.Get(context.Background(), keyID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rec, err := DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	return rec
}

func putJob(keyID, etag string, lastModified, fetchedAt time.Time) writeJob {
	return writeJob{record: recordFromEntry(keyID, cache.Entry{
		ETag:         etag,
		LastModified: lastModified,
		FetchedAt:    fetchedAt,
		Payload:      []byte(etag),
	})}
}

func TestWriter_PersistsUpstreamETagAndLastModified(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			w := NewWriter(store, noopInst(t), noopLogger(), 16)
			lm := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
			fetched := lm.Add(time.Minute)

			w.OnRefreshed(hashA, cache.Entry{ETag: `"v1"`, LastModified: lm, FetchedAt: fetched, Payload: []byte(`{}`)})
			w.Shutdown(context.Background())

			rec := storedRecord(t, store, hashA)
			if rec.ETag != `"v1"` || !rec.LastModified.Equal(lm) || !rec.CapturedAt.Equal(fetched) {
				t.Errorf("stored etag=%q last_modified=%v captured_at=%v; want %q %v %v",
					rec.ETag, rec.LastModified, rec.CapturedAt, `"v1"`, lm, fetched)
			}
		})
	}
}

func TestWriter_DoesNotOverwriteNewerOrSameSnapshot(t *testing.T) {
	fake := newS3Fake()
	store, _ := newTestS3Store(t, fake)
	w := NewWriter(store, noopInst(t), noopLogger(), 16)
	t0 := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	w.persist(putJob(hashA, `"new"`, t1, t1))
	// A replica that fetched the older revision later must not roll it back.
	w.persist(putJob(hashA, `"old"`, t0, t1.Add(time.Hour)))
	// A replica that fetched the same revision must not rewrite it.
	w.persist(putJob(hashA, `"new"`, t1, t1.Add(time.Hour)))

	if rec := storedRecord(t, store, hashA); rec.ETag != `"new"` {
		t.Errorf("stored etag = %q, want %q", rec.ETag, `"new"`)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.writes != 1 {
		t.Errorf("writes = %d, want 1", fake.writes)
	}
}

func TestWriter_OverwritesOlderSnapshot(t *testing.T) {
	store := newFileStore(t, t.TempDir())
	w := NewWriter(store, noopInst(t), noopLogger(), 16)
	t0 := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	w.persist(putJob(hashA, `"old"`, t0, t0))
	w.persist(putJob(hashA, `"new"`, t0.Add(time.Second), t0))

	if rec := storedRecord(t, store, hashA); rec.ETag != `"new"` {
		t.Errorf("stored etag = %q, want %q", rec.ETag, `"new"`)
	}
}

// conflictStore fails the first n conditional writes, as if a peer replica
// wrote in between our read and write.
type conflictStore struct {
	Store
	mu        sync.Mutex
	conflicts int
	gets      int
}

func (s *conflictStore) GetVersioned(ctx context.Context, keyID string) ([]byte, string, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Store.GetVersioned(ctx, keyID)
}

func (s *conflictStore) PutIfMatch(ctx context.Context, keyID string, body []byte, version string) error {
	s.mu.Lock()
	if s.conflicts > 0 {
		s.conflicts--
		s.mu.Unlock()
		return fmt.Errorf("injected: %w", ErrPreconditionFailed)
	}
	s.mu.Unlock()
	return s.Store.PutIfMatch(ctx, keyID, body, version)
}

func TestWriter_RetriesAfterConflict(t *testing.T) {
	store := &conflictStore{Store: newFileStore(t, t.TempDir()), conflicts: 2}
	w := NewWriter(store, noopInst(t), noopLogger(), 16)

	w.persist(putJob(hashA, `"v1"`, time.Now(), time.Now()))

	if rec := storedRecord(t, store, hashA); rec.ETag != `"v1"` {
		t.Errorf("stored etag = %q, want %q", rec.ETag, `"v1"`)
	}
	if store.gets != 3 {
		t.Errorf("GetVersioned calls = %d, want 3 (re-read after each conflict)", store.gets)
	}
}

func TestWriter_GivesUpAfterMaxConflicts(t *testing.T) {
	store := &conflictStore{Store: newFileStore(t, t.TempDir()), conflicts: maxPutAttempts}
	w := NewWriter(store, noopInst(t), noopLogger(), 16)

	w.persist(putJob(hashA, `"v1"`, time.Now(), time.Now()))

	if _, err := store.Get(context.Background(), hashA); err == nil {
		t.Error("expected no object after exhausting retries")
	}
}

// TestWriter_ConcurrentReplicasConverge simulates replicas sharing one bucket,
// each persisting revisions of the same key in random order. Whatever the
// interleaving, the newest upstream revision must end up stored.
func TestWriter_ConcurrentReplicasConverge(t *testing.T) {
	for name, open := range storeBackends(t) {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			const replicas, revisions = 8, 10
			base := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

			var wg sync.WaitGroup
			for range replicas {
				w := NewWriter(store, noopInst(t), noopLogger(), 16)
				order := rand.Perm(revisions)
				wg.Go(func() {
					for _, rev := range order {
						lm := base.Add(time.Duration(rev) * time.Second)
						w.persist(putJob(hashA, fmt.Sprintf(`"rev-%d"`, rev), lm, time.Now()))
					}
				})
			}
			wg.Wait()

			want := fmt.Sprintf(`"rev-%d"`, revisions-1)
			if rec := storedRecord(t, store, hashA); rec.ETag != want {
				t.Errorf("stored etag = %q, want %q", rec.ETag, want)
			}
		})
	}
}
