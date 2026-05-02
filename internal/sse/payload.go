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
