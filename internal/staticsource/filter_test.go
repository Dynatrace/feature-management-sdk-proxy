package staticsource

import (
	"encoding/json"
	"testing"
)

// testKeyA and testKeyB are valid full SDK keys used as flagSetId values.
const (
	testKeyA = "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	testKeyB = "dt01.server_eu_zz9yy8xx7w.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
)

// decodeDoc parses a served document into a top-level map and its flags map.
func decodeDoc(t *testing.T, doc []byte) (map[string]json.RawMessage, map[string]json.RawMessage) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(doc, &top); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	var flags map[string]json.RawMessage
	if err := json.Unmarshal(top["flags"], &flags); err != nil {
		t.Fatalf("decode flags: %v", err)
	}
	return top, flags
}

func metaSetID(t *testing.T, top map[string]json.RawMessage) string {
	t.Helper()
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(top["metadata"], &meta); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	var id string
	if err := json.Unmarshal(meta[flagSetIDKey], &id); err != nil {
		t.Fatalf("decode flagSetId: %v", err)
	}
	return id
}

func TestBuild_TopLevelDefault(t *testing.T) {
	raw := []byte(`{
		"metadata": {"flagSetId": "` + testKeyA + `", "version": "v1"},
		"flags": {
			"flagOne": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on"},
			"flagTwo": {"state": "DISABLED", "variants": {"on": true}, "defaultVariant": "on"}
		}
	}`)
	res, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(res.entries) != 1 {
		t.Fatalf("want 1 flag set, got %d", len(res.entries))
	}
	e, ok := res.entries[testKeyA]
	if !ok {
		t.Fatalf("flag set %q not found", testKeyA)
	}
	top, flags := decodeDoc(t, e.payload)
	if len(flags) != 2 {
		t.Errorf("want 2 flags, got %d", len(flags))
	}
	if got := metaSetID(t, top); got != testKeyA {
		t.Errorf("served metadata.flagSetId = %q, want %q", got, testKeyA)
	}
	// version metadata should be preserved.
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(top["metadata"], &meta)
	if _, ok := meta["version"]; !ok {
		t.Error("expected version metadata to be preserved")
	}
}

func TestBuild_PerFlagOverride(t *testing.T) {
	raw := []byte(`{
		"metadata": {"flagSetId": "` + testKeyA + `"},
		"flags": {
			"inherits": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on"},
			"overrides": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on", "metadata": {"flagSetId": "` + testKeyB + `"}}
		}
	}`)
	res, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(res.entries) != 2 {
		t.Fatalf("want 2 flag sets, got %d", len(res.entries))
	}
	_, flagsA := decodeDoc(t, res.entries[testKeyA].payload)
	if _, ok := flagsA["inherits"]; !ok || len(flagsA) != 1 {
		t.Errorf("set A should contain only 'inherits', got %v", flagsA)
	}
	_, flagsB := decodeDoc(t, res.entries[testKeyB].payload)
	if _, ok := flagsB["overrides"]; !ok || len(flagsB) != 1 {
		t.Errorf("set B should contain only 'overrides', got %v", flagsB)
	}
}

func TestBuild_PerFlagMetadataStillInheritsFlagSetId(t *testing.T) {
	// A flag that only overrides another metadata key still inherits the
	// set-level flagSetId (shallow key-merge semantics).
	raw := []byte(`{
		"metadata": {"flagSetId": "` + testKeyA + `"},
		"flags": {
			"onlyVersion": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on", "metadata": {"version": "v9"}}
		}
	}`)
	res, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(res.entries) != 1 {
		t.Fatalf("want 1 flag set, got %d", len(res.entries))
	}
	if _, ok := res.entries[testKeyA]; !ok {
		t.Errorf("flag with only a version override should inherit set-level flagSetId %q", testKeyA)
	}
}

func TestBuild_FlagsWithoutFlagSetIdSkipped(t *testing.T) {
	raw := []byte(`{
		"flags": {
			"orphan": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on"},
			"assigned": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on", "metadata": {"flagSetId": "` + testKeyA + `"}}
		}
	}`)
	res, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if res.unassigned != 1 {
		t.Errorf("unassigned = %d, want 1", res.unassigned)
	}
	if len(res.entries) != 1 {
		t.Errorf("want 1 flag set, got %d", len(res.entries))
	}
}

func TestBuild_ArrayFlagsForm(t *testing.T) {
	raw := []byte(`{
		"metadata": {"flagSetId": "` + testKeyA + `"},
		"flags": [
			{"key": "arrFlag", "state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on"}
		]
	}`)
	res, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, flags := decodeDoc(t, res.entries[testKeyA].payload)
	if _, ok := flags["arrFlag"]; !ok {
		t.Errorf("array-form flag not keyed correctly: %v", flags)
	}
}

func TestBuild_EventStreamsOmitted(t *testing.T) {
	raw := []byte(`{
		"metadata": {"flagSetId": "` + testKeyA + `"},
		"eventStreams": [{"type": "sse", "url": "https://sse.example.com"}],
		"flags": {"f": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on"}}
	}`)
	res, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	top, _ := decodeDoc(t, res.entries[testKeyA].payload)
	if _, ok := top["eventStreams"]; ok {
		t.Error("eventStreams must be omitted from the served document in static mode")
	}
}

func TestBuild_TopLevelKeysPreserved(t *testing.T) {
	raw := []byte(`{
		"$schema": "https://flagd.dev/schema/v0/flags.json",
		"metadata": {"flagSetId": "` + testKeyA + `"},
		"$evaluators": {"myRule": {"in": ["x"]}},
		"flags": {"f": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on"}}
	}`)
	res, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	top, _ := decodeDoc(t, res.entries[testKeyA].payload)
	if _, ok := top["$schema"]; !ok {
		t.Error("$schema should be preserved")
	}
	if _, ok := top["$evaluators"]; !ok {
		t.Error("$evaluators should be preserved")
	}
}

func TestBuild_ETagStableAcrossRebuilds(t *testing.T) {
	raw := []byte(`{
		"metadata": {"flagSetId": "` + testKeyA + `"},
		"flags": {"f": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on"}}
	}`)
	r1, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	r2, err := build(raw)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if r1.entries[testKeyA].etag == "" {
		t.Fatal("etag should not be empty")
	}
	if r1.entries[testKeyA].etag != r2.entries[testKeyA].etag {
		t.Errorf("etag not stable: %q vs %q", r1.entries[testKeyA].etag, r2.entries[testKeyA].etag)
	}
}

func TestBuild_Errors(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"invalid json", `{not json`},
		{"no flags key", `{"metadata": {"flagSetId": "x"}}`},
		{"flags wrong type", `{"flags": 42}`},
		{"non-string flagSetId", `{"metadata": {"flagSetId": 5}, "flags": {}}`},
		{"array flag missing key", `{"metadata":{"flagSetId":"x"},"flags":[{"state":"ENABLED"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := build([]byte(tc.raw)); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}
