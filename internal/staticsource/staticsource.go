package staticsource

import "sync/atomic"

// entry is a precomputed served document for one flag set and its ETag.
type entry struct {
	payload []byte
	etag    string
}

// snapshot is an immutable set of per-flag-set entries. A reload builds a new
// snapshot and swaps it in atomically, so readers never observe a partial state.
type snapshot struct {
	entries map[string]entry
}

// Store holds the current static-source snapshot behind an atomic pointer.
// Reads (Lookup) are lock-free; reloads replace the whole snapshot at once.
//
// The map is keyed by the full SDK key — the exact flagSetId value. In-memory
// use of the raw full key is permitted by design; callers must use
// sdkkey.PublicID for any log, metric, or trace attribute.
type Store struct {
	snap atomic.Pointer[snapshot]
}

// New returns an empty Store. No snapshot is installed until the first load, so
// Loaded reports false and Lookup misses until then.
func New() *Store { return &Store{} }

// Lookup returns the served document and ETag for the flag set identified by
// fullKey, or ok=false when no flag set matches (or nothing is loaded yet).
func (s *Store) Lookup(fullKey string) (payload []byte, etag string, ok bool) {
	snap := s.snap.Load()
	if snap == nil {
		return nil, "", false
	}
	e, found := snap.entries[fullKey]
	if !found {
		return nil, "", false
	}
	return e.payload, e.etag, true
}

// Loaded reports whether an initial snapshot has been installed. It gates the
// readiness probe: the proxy is not ready until the file has loaded once.
func (s *Store) Loaded() bool { return s.snap.Load() != nil }

// FlagSetCount returns the number of flag sets in the current snapshot, or 0
// when nothing is loaded.
func (s *Store) FlagSetCount() int {
	snap := s.snap.Load()
	if snap == nil {
		return 0
	}
	return len(snap.entries)
}

// install atomically swaps in a new snapshot built from the given entries.
func (s *Store) install(entries map[string]entry) {
	s.snap.Store(&snapshot{entries: entries})
}
