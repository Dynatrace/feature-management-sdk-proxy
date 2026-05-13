package snapshot

import "context"

// Store is the backend-agnostic persistence interface for snapshot objects.
// Implementations must be safe for concurrent use.
//
// keyID is the SDK key public identifier (cache.PublicID(sdk_key)).
// Raw SDK keys must never appear here.
type Store interface {
	// Put writes the snapshot body for the given key ID, overwriting any
	// existing object.
	Put(ctx context.Context, keyID string, body []byte) error

	// Get retrieves the snapshot body for the given key ID.
	// Returns an error when the object does not exist.
	Get(ctx context.Context, keyID string) ([]byte, error)

	// List returns the key IDs of all stored snapshots.
	List(ctx context.Context) ([]string, error)

	// Delete removes the snapshot for the given key ID.
	// Returns nil when the object does not exist (idempotent).
	Delete(ctx context.Context, keyID string) error
}
