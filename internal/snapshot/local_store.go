package snapshot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LocalStore implements Store against a local directory.
// Files are stored as <dir>/<keyHash>.json with atomic writes via a temp file
// and rename (POSIX-atomic within the same directory).
// Safe for concurrent use.
type LocalStore struct {
	dir string
}

// keyHashRe matches the only valid keyHash format: exactly 16 lowercase hex chars.
var keyHashRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// NewLocalStore creates a LocalStore rooted at dir.
// The directory is created (with permissions 0o700) if it does not exist.
func NewLocalStore(dir string) (*LocalStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshot: local: create dir %s: %w", dir, err)
	}
	return &LocalStore{dir: dir}, nil
}

// Put writes body to <dir>/<keyHash>.json atomically via a unique temp file + rename.
func (s *LocalStore) Put(_ context.Context, keyHash string, body []byte) error {
	if !keyHashRe.MatchString(keyHash) {
		return fmt.Errorf("snapshot: local: invalid key hash %q", keyHash)
	}
	dst := filepath.Join(s.dir, keyHash+".json")
	tmp, err := os.CreateTemp(s.dir, keyHash+".json.*")
	if err != nil {
		return fmt.Errorf("snapshot: local: create temp %s: %w", keyHash, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("snapshot: local: write %s: %w", keyHash, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("snapshot: local: chmod %s: %w", keyHash, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("snapshot: local: close %s: %w", keyHash, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("snapshot: local: rename %s: %w", keyHash, err)
	}
	return nil
}

// Get reads the snapshot body for the given key hash.
func (s *LocalStore) Get(_ context.Context, keyHash string) ([]byte, error) {
	if !keyHashRe.MatchString(keyHash) {
		return nil, fmt.Errorf("snapshot: local: invalid key hash %q", keyHash)
	}
	data, err := os.ReadFile(filepath.Join(s.dir, keyHash+".json"))
	if err != nil {
		return nil, fmt.Errorf("snapshot: local: get %s: %w", keyHash, err)
	}
	return data, nil
}

// List returns the key hashes of all stored snapshots.
// Only regular files whose name is exactly <16-hex-chars>.json are included;
// temp files, subdirectories, and non-hash filenames are ignored.
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
		if !keyHashRe.MatchString(stem) {
			continue
		}
		keys = append(keys, stem)
	}
	return keys, nil
}

// Delete removes the snapshot file for the given key hash.
// Returns nil when the file does not exist (idempotent).
func (s *LocalStore) Delete(_ context.Context, keyHash string) error {
	if !keyHashRe.MatchString(keyHash) {
		return fmt.Errorf("snapshot: local: invalid key hash %q", keyHash)
	}
	err := os.Remove(filepath.Join(s.dir, keyHash+".json"))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("snapshot: local: delete %s: %w", keyHash, err)
	}
	return nil
}

// ensure LocalStore implements Store at compile time.
var _ Store = (*LocalStore)(nil)
