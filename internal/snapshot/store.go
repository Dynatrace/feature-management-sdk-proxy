package snapshot

import "context"

// Store is the backend-agnostic persistence interface for snapshot objects.
// Implementations must be safe for concurrent use.
//
// keyHash is the 16-hex-char SHA-256 identifier for the SDK key
// (see cache.Key). Raw SDK keys must never appear here.
type Store interface {
	// Put writes the snapshot body for the given key hash, overwriting any
	// existing object.
	Put(ctx context.Context, keyHash string, body []byte) error

	// Get retrieves the snapshot body for the given key hash.
	// Returns an error when the object does not exist.
	Get(ctx context.Context, keyHash string) ([]byte, error)

	// List returns the key hashes of all stored snapshots.
	List(ctx context.Context) ([]string, error)
}
