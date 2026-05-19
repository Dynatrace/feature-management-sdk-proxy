package sse

import (
	"encoding/json"
	"net/url"
)

type streamEntry struct {
	Type               string `json:"type"`
	URL                string `json:"url"`
	InactivityDelaySec int    `json:"inactivityDelaySec"`
}

// ExtractSSEURL returns the URL and inactivity delay from the first
// eventStreams entry with type=="sse". Returns ("", 0, false) if absent or
// the payload cannot be parsed.
//
// Narrow parse exception: only eventStreams[*].url and .inactivityDelaySec
// are read; the rest of the config JSON is treated as opaque.
func ExtractSSEURL(payload []byte) (sseURL string, inactivityDelaySec int, ok bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return "", 0, false
	}
	rawStreams, exists := top["eventStreams"]
	if !exists {
		return "", 0, false
	}
	var streams []streamEntry
	if err := json.Unmarshal(rawStreams, &streams); err != nil {
		return "", 0, false
	}
	for _, e := range streams {
		if e.Type == "sse" && isValidSSEURL(e.URL) {
			return e.URL, e.InactivityDelaySec, true
		}
	}
	return "", 0, false
}

// RewriteResult describes the outcome of RewriteSSEURL.
type RewriteResult int

const (
	// RewriteResultRewritten indicates the SSE URL was replaced with the proxy URL.
	RewriteResultRewritten RewriteResult = iota
	// RewriteResultNoEntry indicates no type=="sse" entry was found; payload returned unchanged.
	RewriteResultNoEntry
	// RewriteResultNotFoundInBody is retained for backward compatibility but is no longer
	// returned by RewriteSSEURL. The JSON parse approach handles all valid encodings.
	RewriteResultNotFoundInBody
)

// RewriteSSEURL rewrites the SSE URL in payload by parsing the JSON config,
// replacing the URL of the first type=="sse" entry in the eventStreams array
// with proxyURL, and re-serializing the payload.
//
// Returns the original payload unchanged if the JSON is malformed or no
// type=="sse" entry exists (RewriteResultNoEntry).
func RewriteSSEURL(payload []byte, proxyURL string) ([]byte, RewriteResult) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return payload, RewriteResultNoEntry
	}

	rawStreams, exists := top["eventStreams"]
	if !exists {
		return payload, RewriteResultNoEntry
	}

	var streams []streamEntry
	if err := json.Unmarshal(rawStreams, &streams); err != nil {
		return payload, RewriteResultNoEntry
	}

	found := false
	for i, e := range streams {
		if e.Type == "sse" && isValidSSEURL(e.URL) {
			streams[i].URL = proxyURL
			found = true
			break
		}
	}
	if !found {
		return payload, RewriteResultNoEntry
	}

	newStreams, err := json.Marshal(streams)
	if err != nil {
		return payload, RewriteResultNoEntry
	}
	top["eventStreams"] = newStreams

	out, err := json.Marshal(top)
	if err != nil {
		return payload, RewriteResultNoEntry
	}
	return out, RewriteResultRewritten
}

// isValidSSEURL rejects blank, non-HTTPS, and host-less URLs to prevent SSRF
// via a compromised CDN payload redirecting the SSE connection to an internal host.
func isValidSSEURL(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Host != ""
}
