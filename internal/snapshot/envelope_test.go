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

	lastModified := time.Date(2026, 5, 3, 11, 59, 0, 0, time.UTC)

	data, err := EncodeEnvelope(Record{KeyID: keyHash, ETag: etag, LastModified: lastModified, CapturedAt: capturedAt, Payload: payload})
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}

	got, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if got.KeyID != keyHash {
		t.Errorf("keyHash: got %q, want %q", got.KeyID, keyHash)
	}
	if got.ETag != etag {
		t.Errorf("etag: got %q, want %q", got.ETag, etag)
	}
	if !got.LastModified.Equal(lastModified) {
		t.Errorf("lastModified: got %v, want %v", got.LastModified, lastModified)
	}
	if !got.CapturedAt.Equal(capturedAt) {
		t.Errorf("capturedAt: got %v, want %v", got.CapturedAt, capturedAt)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("payload mismatch: got %q, want %q", got.Payload, payload)
	}
}

func TestEnvelope_LastModifiedOmittedWhenZero(t *testing.T) {
	data, err := EncodeEnvelope(Record{KeyID: "k", CapturedAt: time.Now()})
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	if bytes.Contains(data, []byte("last_modified")) {
		t.Errorf("zero LastModified should be omitted: %s", data)
	}
}

func TestRecord_Supersedes(t *testing.T) {
	t0 := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	cases := []struct {
		name        string
		next, prior Record
		want        bool
	}{
		{"same etag", Record{ETag: "a", LastModified: t1, CapturedAt: t1}, Record{ETag: "a", LastModified: t0, CapturedAt: t0}, false},
		{"newer last-modified", Record{ETag: "b", LastModified: t1, CapturedAt: t0}, Record{ETag: "a", LastModified: t0, CapturedAt: t1}, true},
		{"older last-modified", Record{ETag: "b", LastModified: t0, CapturedAt: t1}, Record{ETag: "a", LastModified: t1, CapturedAt: t0}, false},
		{"equal last-modified, newer capture", Record{ETag: "b", LastModified: t0, CapturedAt: t1}, Record{ETag: "a", LastModified: t0, CapturedAt: t0}, true},
		{"missing last-modified, newer capture", Record{ETag: "b", CapturedAt: t1}, Record{ETag: "a", LastModified: t0, CapturedAt: t0}, true},
		{"missing last-modified, older capture", Record{ETag: "b", CapturedAt: t0}, Record{ETag: "a", CapturedAt: t1}, false},
		{"empty etags", Record{CapturedAt: t1}, Record{CapturedAt: t0}, true},
	}
	for _, tc := range cases {
		if got := tc.next.Supersedes(tc.prior); got != tc.want {
			t.Errorf("%s: Supersedes = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEnvelope_PayloadByteFidelity(t *testing.T) {
	// Arbitrary bytes including null, high bytes — must survive base64 round-trip.
	payload := []byte{0x00, 0x01, 0xFF, 0xFE, 0x7B, 0x22, 0x7D}
	data, err := EncodeEnvelope(Record{KeyID: "k", CapturedAt: time.Now(), Payload: payload})
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	got, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("byte-fidelity failed: got %v, want %v", got.Payload, payload)
	}
}

func TestEnvelope_UnknownVersionRejected(t *testing.T) {
	// Manually craft an envelope with a higher version number.
	raw := []byte(`{"snapshot_version":999,"sdk_key_id":"k","etag":"","captured_at":"2026-01-01T00:00:00Z","payload":""}`)
	_, err := DecodeEnvelope(raw)
	if !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("expected ErrUnknownVersion, got %v", err)
	}
}

func TestEnvelope_MalformedJSON(t *testing.T) {
	_, err := DecodeEnvelope([]byte(`not json`))
	if err == nil {
		t.Error("expected error for malformed JSON, got nil")
	}
}

func TestEnvelope_ForwardCompatUnknownFields(t *testing.T) {
	// A known version with extra unknown fields should still decode without error.
	raw := []byte(`{"snapshot_version":1,"sdk_key_id":"k","etag":"e","captured_at":"2026-01-01T00:00:00Z","payload":"aGVsbG8=","future_field":"ignored"}`)
	got, err := DecodeEnvelope(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.KeyID != "k" {
		t.Errorf("keyID: got %q", got.KeyID)
	}
	if got.ETag != "e" {
		t.Errorf("etag: got %q", got.ETag)
	}
	if string(got.Payload) != "hello" {
		t.Errorf("payload: got %q", got.Payload)
	}
}
