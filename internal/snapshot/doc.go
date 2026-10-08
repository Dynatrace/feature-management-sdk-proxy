// Package snapshot implements optional async persistence of cache entries to
// object storage. Snapshots allow a proxy restart during an upstream outage to
// restore the cache from the last-written state rather than starting cold.
//
// Key types:
//   - Store: backend-agnostic interface (Put/Get/List) implemented by BlobStore.
//   - Envelope: versioned JSON wrapper for a single cache entry.
//   - Writer: async observer that persists entries after each upstream 200.
//   - LoadAll: startup function that reads all snapshots and populates the cache.
//
// Snapshot writes are never on the hot path. A failed Put logs a warning and
// increments proxy.snapshot.writes.total{result="failure"} but never fails an
// SDK request. Persistence is disabled entirely when SNAPSHOT_URL is unset.
package snapshot
