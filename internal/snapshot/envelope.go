package snapshot

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CurrentSnapshotVersion is the version this binary writes and understands.
// Bump this when the envelope format changes in a backward-incompatible way.
const CurrentSnapshotVersion = 1

// ErrUnknownVersion is returned when a snapshot object carries a
// snapshot_version the reader does not recognise.
var ErrUnknownVersion = errors.New("snapshot: unknown version")

// Envelope is the on-disk format for a persisted cache entry.
type Envelope struct {
	Version      int       `json:"snapshot_version"`
	KeyID        string    `json:"sdk_key_id"`
	ETag         string    `json:"etag"`
	LastModified time.Time `json:"last_modified,omitzero"`
	CapturedAt   time.Time `json:"captured_at"`
	// Payload is the raw flagd config bytes, base64-encoded so they survive
	// JSON round-trips without any parsing or escaping.
	Payload string `json:"payload"`
}

// Record is a decoded snapshot. ETag and LastModified are the upstream CDN
// values; CapturedAt is when this proxy fetched the payload.
type Record struct {
	KeyID        string
	ETag         string
	LastModified time.Time
	CapturedAt   time.Time
	Payload      []byte
}

// Supersedes reports whether r should replace stored: never for the same
// upstream ETag, otherwise the later upstream Last-Modified wins, falling back
// to CapturedAt when either is missing or they are equal.
func (r Record) Supersedes(stored Record) bool {
	if r.ETag != "" && r.ETag == stored.ETag {
		return false
	}
	if !r.LastModified.IsZero() && !stored.LastModified.IsZero() && !r.LastModified.Equal(stored.LastModified) {
		return r.LastModified.After(stored.LastModified)
	}
	return r.CapturedAt.After(stored.CapturedAt)
}

// EncodeEnvelope serialises a record into a JSON envelope.
func EncodeEnvelope(r Record) ([]byte, error) {
	env := Envelope{
		Version:      CurrentSnapshotVersion,
		KeyID:        r.KeyID,
		ETag:         r.ETag,
		LastModified: r.LastModified.UTC(),
		CapturedAt:   r.CapturedAt.UTC(),
		Payload:      base64.StdEncoding.EncodeToString(r.Payload),
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("snapshot: encode envelope: %w", err)
	}
	return b, nil
}

// DecodeEnvelope parses a JSON envelope. Returns ErrUnknownVersion when the
// snapshot_version is higher than CurrentSnapshotVersion; callers must skip
// such objects rather than error.
func DecodeEnvelope(data []byte) (Record, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Record{}, fmt.Errorf("snapshot: decode envelope: %w", err)
	}
	if env.Version > CurrentSnapshotVersion {
		return Record{}, ErrUnknownVersion
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return Record{}, fmt.Errorf("snapshot: decode payload: %w", err)
	}
	return Record{
		KeyID:        env.KeyID,
		ETag:         env.ETag,
		LastModified: env.LastModified,
		CapturedAt:   env.CapturedAt,
		Payload:      payload,
	}, nil
}
