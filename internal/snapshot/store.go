package snapshot

import (
	"context"
	"errors"
)

// ErrNotFound is returned (wrapped) when a snapshot object does not exist.
var ErrNotFound = errors.New("snapshot: not found")

// ErrPreconditionFailed is returned (wrapped) by PutIfMatch when the stored
// object no longer matches the expected version.
var ErrPreconditionFailed = errors.New("snapshot: precondition failed")

// Store is the backend-agnostic persistence interface for snapshot objects.
// Implementations must be safe for concurrent use.
//
// keyID is the SDK key public identifier (sdkkey.PublicID(sdk_key)).
// Raw SDK keys must never appear here.
type Store interface {
	// Get retrieves the snapshot body for the given key ID.
	// Returns an error wrapping ErrNotFound when the object does not exist.
	Get(ctx context.Context, keyID string) ([]byte, error)

	// GetVersioned is Get plus an opaque version identifying the returned
	// revision, for use with PutIfMatch.
	GetVersioned(ctx context.Context, keyID string) (body []byte, version string, err error)

	// PutIfMatch writes body only if the stored object is still at version,
	// or does not exist when version is "". Returns an error wrapping
	// ErrPreconditionFailed otherwise.
	PutIfMatch(ctx context.Context, keyID string, body []byte, version string) error

	// List returns the key IDs of all stored snapshots.
	List(ctx context.Context) ([]string, error)

	// Delete removes the snapshot for the given key ID.
	// Returns nil when the object does not exist (idempotent).
	Delete(ctx context.Context, keyID string) error
}
