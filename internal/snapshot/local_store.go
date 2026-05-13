package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dynatrace/sdk-proxy/internal/cache"
)

// LocalStore implements Store against a local directory.
// Files are stored as <dir>/<keyID>.json with atomic writes via a temp file
// and rename (POSIX-atomic within the same directory).
// Safe for concurrent use.
type LocalStore struct {
	dir string
}

// NewLocalStore creates a LocalStore rooted at dir.
// The directory is created (with permissions 0o700) if it does not exist.
func NewLocalStore(dir string) (*LocalStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshot: local: create dir %s: %w", dir, err)
	}
	return &LocalStore{dir: dir}, nil
}

// Put writes body to <dir>/<keyID>.json atomically via a unique temp file + rename.
func (s *LocalStore) Put(_ context.Context, keyID string, body []byte) error {
	if !cache.IsValidID(keyID) {
		return fmt.Errorf("snapshot: local: invalid key ID %q", keyID)
	}
	dst := filepath.Join(s.dir, keyID+".json")
	tmp, err := os.CreateTemp(s.dir, keyID+".json.*")
	if err != nil {
		return fmt.Errorf("snapshot: local: create temp %s: %w", keyID, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("snapshot: local: write %s: %w", keyID, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("snapshot: local: chmod %s: %w", keyID, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("snapshot: local: close %s: %w", keyID, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("snapshot: local: rename %s: %w", keyID, err)
	}
	return nil
}

// Get reads the snapshot body for the given key ID.
func (s *LocalStore) Get(_ context.Context, keyID string) ([]byte, error) {
	if !cache.IsValidID(keyID) {
		return nil, fmt.Errorf("snapshot: local: invalid key ID %q", keyID)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, keyID+".json"))
	if err != nil {
		return nil, fmt.Errorf("snapshot: local: get %s: %w", keyID, err)
	}
	return data, nil
}

// List returns the key IDs of all stored snapshots.
// Only regular files whose name is exactly <keyID>.json are included;
// temp files, subdirectories, and non-matching filenames are ignored.
func (s *LocalStore) List(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("snapshot: local: list: %w", err)
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		stem := strings.TrimSuffix(name, ".json")
		if !cache.IsValidID(stem) {
			continue
		}
		keys = append(keys, stem)
	}
	return keys, nil
}

// Delete removes the snapshot file for the given key ID.
// Returns nil when the file does not exist (idempotent).
func (s *LocalStore) Delete(_ context.Context, keyID string) error {
	if !cache.IsValidID(keyID) {
		return fmt.Errorf("snapshot: local: invalid key ID %q", keyID)
	}
	err := os.Remove(filepath.Join(s.dir, keyID+".json"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("snapshot: local: delete %s: %w", keyID, err)
	}
	return nil
}

// ensure LocalStore implements Store at compile time.
var _ Store = (*LocalStore)(nil)
