package proxy

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// hopByHopHeaders must not be forwarded between proxy hops (RFC 7230 §6.1).
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Proxy-Authorization": true,
	"Proxy-Authenticate":  true,
	"Te":                  true,
	"Trailer":             true,
}

// Handler proxies flagd config requests to the upstream CDN.
// The SDK key is the final path segment minus its ".json" suffix.
// No credentials are held by the proxy; the path itself carries auth.
type Handler struct {
	upstream *url.URL
	client   *http.Client
	logger   *slog.Logger
}

// New constructs a Handler. upstreamBase must be an absolute URL with no
// trailing path components; path-based routing is appended from each request.
func New(upstreamBase string, client *http.Client, logger *slog.Logger) (*Handler, error) {
	u, err := url.Parse(upstreamBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("proxy: invalid upstream URL %q", upstreamBase)
	}
	return &Handler{upstream: u, client: client, logger: logger}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sdkKey, ok := extractSDKKey(r.URL.Path)
	if !ok {
		http.Error(w, "path must end with /<sdk_key>.json", http.StatusBadRequest)
		return
	}

	target := *h.upstream
	target.Path = strings.TrimRight(h.upstream.Path, "/") + r.URL.Path
	target.RawQuery = r.URL.RawQuery

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), nil)
	if err != nil {
		h.logger.Error("proxy: build request failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Forward safe inbound headers (e.g. If-None-Match for ETag revalidation).
	for k, vv := range r.Header {
		if !hopByHopHeaders[k] {
			req.Header[k] = vv
		}
	}

	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.Warn("proxy: upstream request failed",
			slog.String("sdk_key_ref", sdkKeyRef(sdkKey)),
			slog.String("error", err.Error()),
		)
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		h.logger.Warn("proxy: upstream returned server error",
			slog.String("sdk_key_ref", sdkKeyRef(sdkKey)),
			slog.Int("upstream_status", resp.StatusCode),
		)
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		return
	}

	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// extractSDKKey returns the SDK key embedded in path as the final segment
// minus its ".json" suffix. Returns ("", false) if the path is malformed.
func extractSDKKey(path string) (string, bool) {
	segment := path[strings.LastIndex(path, "/")+1:]
	if !strings.HasSuffix(segment, ".json") {
		return "", false
	}
	key := strings.TrimSuffix(segment, ".json")
	if key == "" {
		return "", false
	}
	return key, true
}

// sdkKeyRef returns a stable opaque identifier for use in logs and metrics.
// Raw SDK keys must never appear in persistent stores per the cache key invariant.
func sdkKeyRef(sdkKey string) string {
	h := sha256.Sum256([]byte(sdkKey))
	return fmt.Sprintf("%x", h[:8])
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		if !hopByHopHeaders[k] {
			for _, v := range vv {
				dst.Add(k, v)
			}
		}
	}
}
