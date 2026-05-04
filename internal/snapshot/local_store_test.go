package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const (
	hashA = "abc0000000000000"
	hashB = "0000000000000001"
	hashC = "0000000000000002"
	hashD = "00000000000000aa"
	hashE = "000000000000bbbb"
)

func TestLocalStore_PutAndGet(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	body := []byte(`{"snapshot_version":1,"sdk_key_hash":"abc0000000000000","etag":"v1","captured_at":"2026-01-01T00:00:00Z","payload":"dGVzdA=="}`)
	if err := store.Put(context.Background(), hashA, body); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(context.Background(), hashA)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}
}

func TestLocalStore_List(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	for _, k := range []string{hashB, hashC} {
		if err := store.Put(context.Background(), k, []byte(`{}`)); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	if !found[hashB] || !found[hashC] {
		t.Errorf("expected %s and %s in list, got %v", hashB, hashC, keys)
	}
}

func TestLocalStore_ListSkipsNonJSON(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalStore(dir)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	if err := store.Put(context.Background(), hashD, []byte(`{}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Stray .tmp file left from a prior crashed write — must be ignored.
	if err := os.WriteFile(filepath.Join(dir, hashD+".json.tmp"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	// Subdirectory named like a JSON file — must be ignored.
	if err := os.Mkdir(filepath.Join(dir, "subdir.json"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Non-hex filename — must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write README.json: %v", err)
	}
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 1 || keys[0] != hashD {
		t.Errorf("expected [%s], got %v", hashD, keys)
	}
}

func TestLocalStore_GetMissing(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	_, err = store.Get(context.Background(), hashA)
	if err == nil {
		t.Error("expected error for missing key, got nil")
	}
}

func TestLocalStore_ListEmpty(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected empty list, got %v", keys)
	}
}

func TestLocalStore_CreatesDirIfMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "snapshots")
	store, err := NewLocalStore(dir)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	if err := store.Put(context.Background(), hashB, []byte(`{}`)); err != nil {
		t.Fatalf("Put after auto-create: %v", err)
	}
}

func TestLocalStore_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalStore(dir)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	if err := store.Put(context.Background(), hashE, []byte(`{}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, hashE+".json"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %04o, want 0600", mode)
	}
}

func TestLocalStore_TmpOrphanNotListed(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalStore(dir)
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, hashA+".json.tmp"), []byte(`not json`), 0o600); err != nil {
		t.Fatalf("write stray tmp: %v", err)
	}
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("expected empty list, got %v", keys)
	}
}

func TestLocalStore_InvalidKeyHash(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	for _, bad := range []string{"abc", "../escape", "ABCDEFGHIJKLMNOP", "gggggggggggggggg"} {
		if err := store.Put(context.Background(), bad, []byte(`{}`)); err == nil {
			t.Errorf("Put(%q): expected error, got nil", bad)
		}
		if _, err := store.Get(context.Background(), bad); err == nil {
			t.Errorf("Get(%q): expected error, got nil", bad)
		}
	}
}

func TestLocalStore_ConcurrentPuts(t *testing.T) {
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("a1b2c3d4e5f6%04x", i)
			if err := store.Put(context.Background(), key, []byte(`{}`)); err != nil {
				t.Errorf("Put %s: %v", key, err)
			}
		}(i)
	}
	wg.Wait()
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != n {
		t.Errorf("expected %d keys, got %d", n, len(keys))
	}
}
