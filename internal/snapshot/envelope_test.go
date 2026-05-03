package snapshot

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestEnvelope_RoundTrip(t *testing.T) {
	payload := []byte(`{"flags":{"myFlag":{"state":"ENABLED"}}}`)
	keyHash := "abc123def456789a"
	etag := `"v42"`
	capturedAt := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	data, err := EncodeEnvelope(keyHash, etag, capturedAt, payload)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}

	gotKey, gotETag, gotAt, gotPayload, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if gotKey != keyHash {
		t.Errorf("keyHash: got %q, want %q", gotKey, keyHash)
	}
	if gotETag != etag {
		t.Errorf("etag: got %q, want %q", gotETag, etag)
	}
	if !gotAt.Equal(capturedAt) {
		t.Errorf("capturedAt: got %v, want %v", gotAt, capturedAt)
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("payload mismatch: got %q, want %q", gotPayload, payload)
	}
}

func TestEnvelope_PayloadByteFidelity(t *testing.T) {
	// Arbitrary bytes including null, high bytes — must survive base64 round-trip.
	payload := []byte{0x00, 0x01, 0xFF, 0xFE, 0x7B, 0x22, 0x7D}
	data, err := EncodeEnvelope("k", "", time.Now(), payload)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	_, _, _, got, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("byte-fidelity failed: got %v, want %v", got, payload)
	}
}

func TestEnvelope_UnknownVersionRejected(t *testing.T) {
	// Manually craft an envelope with a higher version number.
	raw := []byte(`{"snapshot_version":999,"sdk_key_hash":"k","etag":"","captured_at":"2026-01-01T00:00:00Z","payload":""}`)
	_, _, _, _, err := DecodeEnvelope(raw)
	if !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("expected ErrUnknownVersion, got %v", err)
	}
}

func TestEnvelope_MalformedJSON(t *testing.T) {
	_, _, _, _, err := DecodeEnvelope([]byte(`not json`))
	if err == nil {
		t.Error("expected error for malformed JSON, got nil")
	}
}

func TestEnvelope_ForwardCompatUnknownFields(t *testing.T) {
	// A known version with extra unknown fields should still decode without error.
	raw := []byte(`{"snapshot_version":1,"sdk_key_hash":"k","etag":"e","captured_at":"2026-01-01T00:00:00Z","payload":"aGVsbG8=","future_field":"ignored"}`)
	keyHash, etag, _, payload, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if keyHash != "k" {
		t.Errorf("keyHash: got %q", keyHash)
	}
	if etag != "e" {
		t.Errorf("etag: got %q", etag)
	}
	if string(payload) != "hello" {
		t.Errorf("payload: got %q", payload)
	}
}
