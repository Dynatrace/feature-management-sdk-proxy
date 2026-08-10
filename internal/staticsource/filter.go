package staticsource

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
)

// flagSetIDKey is the metadata key that carries a flag's flag-set identifier,
// per the flagd flag-set paradigm.
const flagSetIDKey = "flagSetId"

// buildResult is the outcome of splitting a flagd config document into one
// served document per flag set.
type buildResult struct {
	// entries maps each resolved flagSetId to its precomputed served document.
	entries map[string]entry
	// unassigned counts flags that resolved to no flagSetId (skipped, never served).
	unassigned int
}

// build parses a single flagd config document and groups its flags by resolved
// flagSetId, producing one served document per flag set.
//
// flagSetId resolution mirrors flagd: a flag's effective flagSetId is its own
// metadata.flagSetId when present, otherwise the top-level metadata.flagSetId.
// Per-flag metadata is a shallow key-merge over set-level metadata, so a flag
// that only overrides another metadata key still inherits the set-level
// flagSetId — resolution therefore checks specifically for the flagSetId key
// rather than the presence of a per-flag metadata block. Flags that resolve to
// no flagSetId belong to no real set and are skipped.
//
// Each served document copies every top-level key from the source except
// "flags" (replaced with the flag-set subset) and "eventStreams" (omitted —
// offline mode has no SSE). The document's metadata.flagSetId is set to the
// group's id so the per-set document is self-consistent even when the id came
// from a per-flag override.
func build(raw []byte) (buildResult, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return buildResult{}, fmt.Errorf("parse config: %w", err)
	}

	topMeta, defaultSetID, err := parseMetadata(top["metadata"])
	if err != nil {
		return buildResult{}, fmt.Errorf("parse top-level metadata: %w", err)
	}

	rawFlags, ok := top["flags"]
	if !ok {
		return buildResult{}, errors.New("config has no \"flags\" key")
	}
	flagMap, err := normalizeFlags(rawFlags)
	if err != nil {
		return buildResult{}, err
	}

	grouped := make(map[string]map[string]json.RawMessage)
	unassigned := 0
	for flagKey, rawFlag := range flagMap {
		setID, err := resolveFlagSetID(rawFlag, defaultSetID)
		if err != nil {
			return buildResult{}, fmt.Errorf("flag %q: %w", flagKey, err)
		}
		if setID == "" {
			unassigned++
			continue
		}
		g, exists := grouped[setID]
		if !exists {
			g = make(map[string]json.RawMessage)
			grouped[setID] = g
		}
		g[flagKey] = rawFlag
	}

	entries := make(map[string]entry, len(grouped))
	for setID, flagsForSet := range grouped {
		doc, err := buildDocument(top, topMeta, setID, flagsForSet)
		if err != nil {
			return buildResult{}, fmt.Errorf("build served document: %w", err)
		}
		entries[setID] = entry{payload: doc, etag: computeETag(doc)}
	}

	return buildResult{entries: entries, unassigned: unassigned}, nil
}

// parseMetadata unmarshals a metadata object into a raw-value map and extracts
// its flagSetId (empty when absent). A nil/absent metadata block yields a nil
// map and an empty id.
func parseMetadata(raw json.RawMessage) (map[string]json.RawMessage, string, error) {
	if len(raw) == 0 {
		return nil, "", nil
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, "", fmt.Errorf("metadata is not an object: %w", err)
	}
	setID, err := stringValue(meta[flagSetIDKey])
	if err != nil {
		return nil, "", fmt.Errorf("metadata.flagSetId: %w", err)
	}
	return meta, setID, nil
}

// resolveFlagSetID returns a flag's effective flagSetId: its own
// metadata.flagSetId when present, else the provided default.
func resolveFlagSetID(rawFlag json.RawMessage, defaultSetID string) (string, error) {
	var flag struct {
		Metadata map[string]json.RawMessage `json:"metadata"`
	}
	if err := json.Unmarshal(rawFlag, &flag); err != nil {
		return "", fmt.Errorf("parse flag: %w", err)
	}
	if own, ok := flag.Metadata[flagSetIDKey]; ok {
		id, err := stringValue(own)
		if err != nil {
			return "", fmt.Errorf("metadata.flagSetId: %w", err)
		}
		if id != "" {
			return id, nil
		}
	}
	return defaultSetID, nil
}

// normalizeFlags accepts either the object form (map of flagKey -> flag) or the
// flagd array form (each element a flag object with a "key") and returns a map
// of flagKey -> raw flag JSON. Array elements are kept verbatim (including any
// inner "key" field, which flagd tolerates in object form).
func normalizeFlags(raw json.RawMessage) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("\"flags\" is empty")
	}
	switch trimmed[0] {
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return nil, fmt.Errorf("parse flags object: %w", err)
		}
		return m, nil
	case '[':
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return nil, fmt.Errorf("parse flags array: %w", err)
		}
		m := make(map[string]json.RawMessage, len(arr))
		for i, rawFlag := range arr {
			var keyed struct {
				Key string `json:"key"`
			}
			if err := json.Unmarshal(rawFlag, &keyed); err != nil {
				return nil, fmt.Errorf("parse flags[%d]: %w", i, err)
			}
			if keyed.Key == "" {
				return nil, fmt.Errorf("flags[%d] is missing a \"key\" (required in array form)", i)
			}
			m[keyed.Key] = rawFlag
		}
		return m, nil
	default:
		return nil, errors.New("\"flags\" must be an object or an array")
	}
}

// buildDocument assembles the served flagd document for one flag set. It copies
// every top-level key except "flags" and "eventStreams", overrides the served
// metadata.flagSetId with setID, and sets "flags" to the flag-set subset.
func buildDocument(top, topMeta map[string]json.RawMessage, setID string, flagsForSet map[string]json.RawMessage) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(top)+1)
	for k, v := range top {
		if k == "flags" || k == "eventStreams" {
			continue
		}
		out[k] = v
	}

	meta := make(map[string]json.RawMessage, len(topMeta)+1)
	maps.Copy(meta, topMeta)
	idJSON, err := json.Marshal(setID)
	if err != nil {
		return nil, fmt.Errorf("marshal flagSetId: %w", err)
	}
	meta[flagSetIDKey] = idJSON
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}
	out["metadata"] = metaJSON

	flagsJSON, err := json.Marshal(flagsForSet)
	if err != nil {
		return nil, fmt.Errorf("marshal flags: %w", err)
	}
	out["flags"] = flagsJSON

	doc, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal document: %w", err)
	}
	return doc, nil
}

// stringValue decodes a JSON string value. It returns "" for an absent value
// and an error for a present-but-non-string value.
func stringValue(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("expected a string: %w", err)
	}
	return s, nil
}

// computeETag returns a strong, quoted ETag derived from the document bytes, so
// a reload that leaves a flag set byte-identical keeps returning 304. Marshaling
// via map[string]json.RawMessage sorts keys deterministically, so identical
// logical content yields identical bytes and thus a stable ETag.
func computeETag(doc []byte) string {
	sum := sha256.Sum256(doc)
	return "\"" + hex.EncodeToString(sum[:16]) + "\""
}
