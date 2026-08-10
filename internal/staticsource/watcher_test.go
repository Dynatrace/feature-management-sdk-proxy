package staticsource

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

const maxBytes = 10 * 1024 * 1024

func TestWatcher_LoadInitial(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flags.json")
	writeFile(t, path, sampleFile())

	store := New()
	w := NewWatcher(store, path, time.Second, maxBytes, nil, testLogger())
	if err := w.LoadInitial(context.Background()); err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}
	if !store.Loaded() {
		t.Error("store should report Loaded after LoadInitial")
	}
	if _, _, ok := store.Lookup(testKeyA); !ok {
		t.Errorf("flag set %q should be present", testKeyA)
	}
}

func TestWatcher_LoadInitial_MissingFile(t *testing.T) {
	store := New()
	w := NewWatcher(store, filepath.Join(t.TempDir(), "nope.json"), time.Second, maxBytes, nil, testLogger())
	if err := w.LoadInitial(context.Background()); err == nil {
		t.Error("expected error for missing file")
	}
	if store.Loaded() {
		t.Error("store should not be Loaded after a failed initial load")
	}
}

func TestWatcher_LoadInitial_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flags.json")
	writeFile(t, path, `{not valid`)
	store := New()
	w := NewWatcher(store, path, time.Second, maxBytes, nil, testLogger())
	if err := w.LoadInitial(context.Background()); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestWatcher_ReloadOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flags.json")
	writeFile(t, path, sampleFile())

	store := New()
	w := NewWatcher(store, path, time.Second, maxBytes, nil, testLogger())
	if err := w.LoadInitial(context.Background()); err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}
	if store.FlagSetCount() != 1 {
		t.Fatalf("initial flag sets = %d, want 1", store.FlagSetCount())
	}

	// Add a second flag set, then trigger one reload cycle directly.
	writeFile(t, path, `{
		"flags": {
			"a": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on", "metadata": {"flagSetId": "`+testKeyA+`"}},
			"b": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on", "metadata": {"flagSetId": "`+testKeyB+`"}}
		}
	}`)
	w.checkAndReload(context.Background())

	if store.FlagSetCount() != 2 {
		t.Errorf("after reload flag sets = %d, want 2", store.FlagSetCount())
	}
	if _, _, ok := store.Lookup(testKeyB); !ok {
		t.Errorf("new flag set %q should be present after reload", testKeyB)
	}
}

func TestWatcher_ReloadParseError_KeepsLastGood(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flags.json")
	writeFile(t, path, sampleFile())

	store := New()
	w := NewWatcher(store, path, time.Second, maxBytes, nil, testLogger())
	if err := w.LoadInitial(context.Background()); err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}
	before, beforeETag, _ := store.Lookup(testKeyA)

	// Corrupt the file, then attempt a reload.
	writeFile(t, path, `{invalid`)
	w.checkAndReload(context.Background())

	after, afterETag, ok := store.Lookup(testKeyA)
	if !ok {
		t.Fatal("last-good flag set should still be served after a parse error")
	}
	if string(before) != string(after) || beforeETag != afterETag {
		t.Error("served payload/etag should be unchanged after a parse error")
	}
}

func TestWatcher_NoReloadWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flags.json")
	writeFile(t, path, sampleFile())

	store := New()
	w := NewWatcher(store, path, time.Second, maxBytes, nil, testLogger())
	if err := w.LoadInitial(context.Background()); err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}
	_, etagBefore, _ := store.Lookup(testKeyA)

	// A reload cycle with no content change must keep the same ETag.
	w.checkAndReload(context.Background())
	_, etagAfter, _ := store.Lookup(testKeyA)
	if etagBefore != etagAfter {
		t.Errorf("etag changed on unchanged file: %q -> %q", etagBefore, etagAfter)
	}
}

func TestWatcher_Run_CancelStops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flags.json")
	writeFile(t, path, sampleFile())

	store := New()
	w := NewWatcher(store, path, 10*time.Millisecond, maxBytes, nil, testLogger())
	if err := w.LoadInitial(context.Background()); err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
