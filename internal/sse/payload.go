package sse

import (
	"encoding/json"
	"net/url"
	"strings"
)

// streamEntry is the narrow view of one eventStreams entry (OFREP ADR-8). An
// entry carries its connection URL in one of two forms:
//
//   - endpoint: {"origin": "https://host", "requestUri": "/path?query"}. The
//     URL is origin + requestUri. origin is optional; when absent, clients use
//     the origin they fetched the config from.
//   - url: an absolute URL.
//
// endpoint takes precedence over url when both are present.
type streamEntry struct {
	Type               string          `json:"type"`
	URL                string          `json:"url"`
	Endpoint           *streamEndpoint `json:"endpoint"`
	InactivityDelaySec int             `json:"inactivityDelaySec"`
}

type streamEndpoint struct {
	Origin     string `json:"origin"`
	RequestURI string `json:"requestUri"`
}

// resolve returns the entry's upstream SSE URL. fallbackOrigin is used when
// the endpoint form omits origin. Returns ("", false) when the entry has no
// usable URL.
func (e streamEntry) resolve(fallbackOrigin string) (string, bool) {
	if e.Endpoint != nil && validRequestURI(e.Endpoint.RequestURI) {
		origin := e.Endpoint.Origin
		if origin == "" {
			origin = fallbackOrigin
		}
		if u := strings.TrimSuffix(origin, "/") + e.Endpoint.RequestURI; isValidOrigin(origin) && isValidSSEURL(u) {
			return u, true
		}
	}
	if isValidSSEURL(e.URL) {
		return e.URL, true
	}
	return "", false
}

// ExtractSSEURL returns the upstream URL and inactivity delay from the first
// eventStreams entry with type=="sse" that has a usable URL. The endpoint form
// (origin + requestUri) is preferred over url; fallbackOrigin (scheme and host
// of the URL the config was fetched from) resolves an endpoint without origin.
// Returns ("", 0, false) if absent or the payload cannot be parsed.
//
// Narrow parse exception: only eventStreams[*].type, .url, .endpoint and
// .inactivityDelaySec are read; the rest of the config JSON is treated as opaque.
func ExtractSSEURL(payload []byte, fallbackOrigin string) (sseURL string, inactivityDelaySec int, ok bool) {
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
		if e.Type != "sse" {
			continue
		}
		if u, ok := e.resolve(fallbackOrigin); ok {
			return u, e.InactivityDelaySec, true
		}
	}
	return "", 0, false
}

// OriginOf returns the scheme and host of rawURL ("https://host[:port]"), or ""
// if rawURL has no scheme or host.
func OriginOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// RewriteResult describes the outcome of RewriteSSEURL.
type RewriteResult int

const (
	// RewriteResultRewritten indicates the SSE entries were pointed at the proxy.
	RewriteResultRewritten RewriteResult = iota
	// RewriteResultNoEntry indicates nothing was rewritten; payload returned unchanged.
	RewriteResultNoEntry
	// RewriteResultNotFoundInBody is retained for backward compatibility but is no longer
	// returned by RewriteSSEURL. The JSON parse approach handles all valid encodings.
	RewriteResultNotFoundInBody
)

// RewriteSSEURL points every type=="sse" eventStreams entry at proxyURL (an
// absolute URL) and re-serializes the payload.
//
// All entries are rewritten, not only the one ExtractSSEURL picks: an SDK may
// choose an entry the proxy skipped and bypass it. Entries the proxy can't
// connect to still work, since polled changes are published to /sse.
//
// Each entry keeps its forms: an endpoint gets origin and requestUri from
// proxyURL, and a non-empty url becomes proxyURL. No form is added, and other
// fields are kept.
//
// Returns the payload unchanged (RewriteResultNoEntry) if the JSON is
// malformed, proxyURL is not absolute, or no type=="sse" entry has an endpoint
// or url.
func RewriteSSEURL(payload []byte, proxyURL string) ([]byte, RewriteResult) {
	pu, err := url.Parse(proxyURL)
	if err != nil || pu.Scheme == "" || pu.Host == "" {
		return payload, RewriteResultNoEntry
	}
	endpoint, err := json.Marshal(streamEndpoint{
		Origin:     pu.Scheme + "://" + pu.Host,
		RequestURI: pu.RequestURI(),
	})
	if err != nil {
		return payload, RewriteResultNoEntry
	}
	encodedURL, err := json.Marshal(proxyURL)
	if err != nil {
		return payload, RewriteResultNoEntry
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return payload, RewriteResultNoEntry
	}

	rawStreams, exists := top["eventStreams"]
	if !exists {
		return payload, RewriteResultNoEntry
	}

	// Decode each entry twice: typed to decide, raw to rewrite while keeping
	// fields the proxy doesn't know about.
	var streams []streamEntry
	if err := json.Unmarshal(rawStreams, &streams); err != nil {
		return payload, RewriteResultNoEntry
	}
	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(rawStreams, &rawEntries); err != nil {
		return payload, RewriteResultNoEntry
	}

	rewritten := false
	for i, e := range streams {
		if e.Type != "sse" || rawEntries[i] == nil {
			continue
		}
		if e.Endpoint != nil {
			rawEntries[i]["endpoint"] = endpoint
			rewritten = true
		}
		if e.URL != "" {
			rawEntries[i]["url"] = encodedURL
			rewritten = true
		}
	}
	if !rewritten {
		return payload, RewriteResultNoEntry
	}

	newStreams, err := json.Marshal(rawEntries)
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

// isValidOrigin accepts only "https://host[:port]" with no userinfo, path,
// query, or fragment, so origin + requestUri can't land on a different host
// than origin names.
func isValidOrigin(origin string) bool {
	u, err := url.Parse(strings.TrimSuffix(origin, "/"))
	if err != nil {
		return false
	}
	return u.Scheme == "https" && u.Host != "" && u.User == nil &&
		u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery
}

// validRequestURI requires an origin-relative path: a leading "/" but not "//",
// which would read as a network-path reference to another host.
func validRequestURI(requestURI string) bool {
	return strings.HasPrefix(requestURI, "/") && !strings.HasPrefix(requestURI, "//")
}
