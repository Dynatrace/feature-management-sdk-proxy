package sse

import (
	"bytes"
	"encoding/json"
	"net/url"
)

type streamEntry struct {
	Type               string `json:"type"`
	URL                string `json:"url"`
	InactivityDelaySec int    `json:"inactivityDelaySec"`
}

type configPayload struct {
	EventStreams []streamEntry `json:"eventStreams"`
}

// ExtractSSEURL returns the URL and inactivity delay from the first
// eventStreams entry with type=="sse". Returns ("", 0, false) if absent or
// the payload cannot be parsed.
//
// Narrow parse exception: only eventStreams[*].url and .inactivityDelaySec
// are read; the rest of the flagd config JSON is treated as opaque bytes.
func ExtractSSEURL(payload []byte) (sseURL string, inactivityDelaySec int, ok bool) {
	var cfg configPayload
	if err := json.Unmarshal(payload, &cfg); err != nil {
		return "", 0, false
	}
	for _, e := range cfg.EventStreams {
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
	// RewriteResultNotFoundInBody indicates the upstream URL was not found as an exact byte
	// sequence (e.g. due to non-standard JSON encoding); payload returned unchanged.
	RewriteResultNotFoundInBody
)

// RewriteSSEURL rewrites the SSE URL(s) in payload, replacing the upstream
// SSE URL with proxyURL. All bytes outside the replaced URL value are preserved
// exactly as received.
//
// Extends the narrow parse exception in doc.go: ExtractSSEURL locates the
// upstream URL, then bytes.ReplaceAll substitutes its JSON-encoded form with
// the JSON-encoded form of proxyURL — no round-trip re-encoding of the payload.
//
// Returns the original payload if no type=="sse" entry exists (RewriteResultNoEntry)
// or if the upstream URL cannot be located as an exact byte sequence, e.g. due
// to non-standard JSON encoding of forward slashes (RewriteResultNotFoundInBody).
func RewriteSSEURL(payload []byte, proxyURL string) ([]byte, RewriteResult) {
	upstreamURL, _, ok := ExtractSSEURL(payload)
	if !ok {
		return payload, RewriteResultNoEntry
	}

	oldBytes, err := json.Marshal(upstreamURL)
	if err != nil {
		return payload, RewriteResultNotFoundInBody
	}
	newBytes, err := json.Marshal(proxyURL)
	if err != nil {
		return payload, RewriteResultNotFoundInBody
	}

	if !bytes.Contains(payload, oldBytes) {
		return payload, RewriteResultNotFoundInBody
	}

	return bytes.Replace(payload, oldBytes, newBytes, 1), RewriteResultRewritten
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
