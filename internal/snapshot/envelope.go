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
	Version    int       `json:"snapshot_version"`
	KeyHash    string    `json:"sdk_key_hash"`
	ETag       string    `json:"etag"`
	CapturedAt time.Time `json:"captured_at"`
	// Payload is the raw flagd config bytes, base64-encoded so they survive
	// JSON round-trips without any parsing or escaping.
	Payload string `json:"payload"`
}

// EncodeEnvelope serialises a cache entry into a JSON envelope.
func EncodeEnvelope(keyHash, etag string, capturedAt time.Time, payload []byte) ([]byte, error) {
	env := Envelope{
		Version:    CurrentSnapshotVersion,
		KeyHash:    keyHash,
		ETag:       etag,
		CapturedAt: capturedAt,
		Payload:    base64.StdEncoding.EncodeToString(payload),
	}
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("snapshot: encode envelope: %w", err)
	}
	return b, nil
}

// DecodeEnvelope parses a JSON envelope and returns the constituent fields.
// Returns ErrUnknownVersion when the snapshot_version is higher than
// CurrentSnapshotVersion; callers must skip such objects rather than error.
func DecodeEnvelope(data []byte) (keyHash, etag string, capturedAt time.Time, payload []byte, err error) {
	var env Envelope
	if err = json.Unmarshal(data, &env); err != nil {
		return "", "", time.Time{}, nil, fmt.Errorf("snapshot: decode envelope: %w", err)
	}
	if env.Version > CurrentSnapshotVersion {
		return "", "", time.Time{}, nil, ErrUnknownVersion
	}
	payload, err = base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return "", "", time.Time{}, nil, fmt.Errorf("snapshot: decode payload: %w", err)
	}
	return env.KeyHash, env.ETag, env.CapturedAt, payload, nil
}
