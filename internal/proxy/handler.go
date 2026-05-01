package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dynatrace/sdk-proxy/internal/cache"
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

// Handler serves flagd config requests from an in-memory cache, falling back
// to the upstream CDN on a miss. The SDK key is never logged or stored; only
// its SHA-256 hash is used as a stable identifier.
type Handler struct {
	upstream *url.URL
	client   *http.Client
	cache    *cache.Cache
	logger   *slog.Logger
}

// New constructs a Handler. upstreamBase must be an absolute URL with no
// trailing path components; path-based routing is appended from each request.
func New(upstreamBase string, client *http.Client, c *cache.Cache, logger *slog.Logger) (*Handler, error) {
	u, err := url.Parse(upstreamBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("proxy: invalid upstream URL %q", upstreamBase)
	}
	return &Handler{upstream: u, client: client, cache: c, logger: logger}, nil
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

	key := cache.Key(sdkKey)

	if entry, ok := h.cache.Get(key); ok {
		h.serveFromCache(w, r, entry)
		return
	}

	h.serveMiss(w, r, key)
}

// serveFromCache writes a response from a cached entry. If the client's
// If-None-Match matches the cached ETag, a 304 is returned with no body.
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, entry cache.Entry) {
	if entry.ETag != "" && r.Header.Get("If-None-Match") == entry.ETag {
		w.Header().Set("ETag", entry.ETag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if entry.ETag != "" {
		w.Header().Set("ETag", entry.ETag)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(entry.Payload)
	}
}

// serveMiss fetches from the upstream CDN on a cache miss. A successful 200
// populates the cache and is then served via serveFromCache for header
// consistency with the cache-hit path. 4xx responses are passed through
// unchanged. 5xx and network errors return 503 — an empty response must
// never reach the SDK.
func (h *Handler) serveMiss(w http.ResponseWriter, r *http.Request, key string) {
	target := h.buildTarget(r.URL)

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		h.logger.Error("proxy: build upstream request failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.Warn("proxy: upstream request failed",
			slog.String("key", key),
			slog.String("error", sanitizeErr(err).Error()),
		)
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		h.logger.Error("proxy: read upstream response failed", slog.String("error", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// Upstream returned 304 on a cold fetch (no If-None-Match was sent).
		// This is unexpected; refuse to return nothing to the SDK.
		h.logger.Error("proxy: upstream returned 304 on cold fetch",
			slog.String("key", key),
		)
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		return

	case resp.StatusCode >= 500:
		h.logger.Warn("proxy: upstream server error",
			slog.String("key", key),
			slog.Int("upstream_status", resp.StatusCode),
		)
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
		return

	case resp.StatusCode == http.StatusOK:
		entry := cache.Entry{
			Payload:   body,
			ETag:      resp.Header.Get("ETag"),
			FetchedAt: time.Now(),
			FetchURL:  target,
		}
		h.cache.Set(key, entry)
		// Serve from the cached entry so headers match the cache-hit path.
		h.serveFromCache(w, r, entry)
		return
	}

	// Pass through 4xx and non-200 2xx responses without caching.
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// Fetch implements cache.Fetcher for use by the background Refresher.
// It sends an If-None-Match revalidation request and interprets the result.
// Network errors are sanitized before returning to prevent SDK key leakage
// via *url.Error.Error() in log messages.
func (h *Handler) Fetch(ctx context.Context, fetchURL, etag string) (cache.FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return cache.FetchResult{}, fmt.Errorf("proxy: build refresh request: %w", err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return cache.FetchResult{}, sanitizeErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return cache.FetchResult{NotModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return cache.FetchResult{}, fmt.Errorf("proxy: refresh got status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return cache.FetchResult{}, fmt.Errorf("proxy: read refresh response: %w", err)
	}

	return cache.FetchResult{
		Entry: &cache.Entry{
			Payload:   body,
			ETag:      resp.Header.Get("ETag"),
			FetchedAt: time.Now(),
			// FetchURL is set by the Refresher to preserve the original URL.
		},
	}, nil
}

// buildTarget constructs the upstream URL for a given inbound request URL.
func (h *Handler) buildTarget(reqURL *url.URL) string {
	target := *h.upstream
	target.Path = strings.TrimRight(h.upstream.Path, "/") + reqURL.Path
	target.RawQuery = reqURL.RawQuery
	return target.String()
}

// sanitizeErr strips request URL values from *url.Error to prevent raw SDK
// keys (embedded in upstream paths) from appearing in log output.
func sanitizeErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s upstream: %w", ue.Op, ue.Err)
	}
	return err
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

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		if !hopByHopHeaders[k] {
			for _, v := range vv {
				dst.Add(k, v)
			}
		}
	}
}
