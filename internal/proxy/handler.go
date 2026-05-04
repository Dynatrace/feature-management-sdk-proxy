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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/sse"
	"github.com/dynatrace/sdk-proxy/internal/telemetry"
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

// SnapshotNotifier is notified after a successful cold fetch (200) so it can
// persist the new cache entry asynchronously. Defined here (rather than
// importing internal/snapshot) to keep the dependency graph acyclic.
type SnapshotNotifier interface {
	OnConfigCached(keyHash, etag string, payload []byte)
}

// Handler serves flagd config requests from an in-memory cache, falling back
// to the upstream CDN on a miss. The SDK key is never logged or stored; only
// its SHA-256 hash is used as a stable identifier.
type Handler struct {
	cdnBaseDomain    string
	client           *http.Client
	cache            *cache.Cache
	sseManager       *sse.Manager
	snapshotNotifier SnapshotNotifier // nil when persistence is disabled
	inst             *metrics.Instruments
	counter          *telemetry.Counter
	logger           *slog.Logger
}

// New constructs a Handler. cdnBaseDomain must be a bare hostname
// (e.g. fm.dynatracelabs.com); the region and path are derived per request.
// sseManager and snapshotNotifier may be nil; when non-nil, they are notified
// after each successful cold fetch. inst and counter may be nil; metrics and
// counting are silently skipped.
func New(cdnBaseDomain string, client *http.Client, c *cache.Cache, sseManager *sse.Manager, snapshotNotifier SnapshotNotifier, inst *metrics.Instruments, counter *telemetry.Counter, logger *slog.Logger) (*Handler, error) {
	if cdnBaseDomain == "" || strings.Contains(cdnBaseDomain, "://") || strings.Contains(cdnBaseDomain, "/") {
		return nil, fmt.Errorf("proxy: invalid CDN base domain %q: must be a bare hostname (e.g. fm.dynatracelabs.com)", cdnBaseDomain)
	}
	return &Handler{cdnBaseDomain: cdnBaseDomain, client: client, cache: c, sseManager: sseManager, snapshotNotifier: snapshotNotifier, inst: inst, counter: counter, logger: logger}, nil
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

	target, ok := h.buildTarget(sdkKey)
	if !ok {
		h.logger.Warn("proxy: unrecognized SDK key format", slog.String("key", cache.Key(sdkKey)))
		http.Error(w, "unrecognized SDK key format: must be dt_server_{region}_{token}", http.StatusBadRequest)
		return
	}

	key := cache.Key(sdkKey)

	// Count every request with a recognized SDK key, regardless of whether it
	// hits the cache or reaches upstream. Cache hits, cache misses, upstream
	// errors (503), and HEAD requests all reflect real SDK usage.
	if h.counter != nil {
		h.counter.Increment(sdkKey)
	}

	if entry, ok := h.cache.Get(key); ok {
		h.logger.Debug("proxy: cache hit", slog.String("key", key))
		h.serveFromCache(w, r, entry, sdkKey)
		return
	}

	h.logger.Debug("proxy: cache miss — fetching upstream", slog.String("key", key))
	h.serveMiss(w, r, sdkKey, key, target)
}

// serveFromCache writes a response from a cached entry. If the client's
// If-None-Match matches the cached ETag, a 304 is returned with no body.
// For non-HEAD 200 responses, the SSE URL in the payload is rewritten to
// point to this proxy's SSE endpoint (derived from the incoming request).
//
// When entry.FetchURL is empty (the entry was restored from a snapshot at
// startup), it is repaired here so the background refresher can revalidate
// it on the next TTL tick. This happens at most once per key per process.
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, entry cache.Entry, sdkKey string) {
	if entry.FetchURL == "" {
		if target, ok := h.buildTarget(sdkKey); ok {
			entry.FetchURL = target
			h.cache.Set(cache.Key(sdkKey), entry)
		}
	}

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
	if r.Method == http.MethodHead {
		return
	}
	payload, result := sse.RewriteSSEURL(entry.Payload, h.proxySSEURL(r, sdkKey))
	h.recordRewriteResult(result, cache.Key(sdkKey))
	_, _ = w.Write(payload)
}

// serveMiss fetches from the upstream CDN on a cache miss. A successful 200
// populates the cache and is then served via serveFromCache for header
// consistency with the cache-hit path. 4xx responses are passed through
// unchanged. 5xx and network errors return 503 — an empty response must
// never reach the SDK.
func (h *Handler) serveMiss(w http.ResponseWriter, r *http.Request, sdkKey, key, target string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		h.logger.Error("proxy: build upstream request failed", slog.String("error", sanitizeErr(err).Error()))
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
			// FetchURL contains the raw SDK key — in-memory use is permitted by design
			// (CLAUDE.md invariant). Snapshot code MUST NOT persist this field as-is.
			FetchURL: target,
		}
		h.cache.Set(key, entry)
		if h.sseManager != nil {
			// Use context.Background() — SSE connections must outlive the HTTP request.
			h.sseManager.OnConfigCached(context.Background(), sdkKey, body)
		}
		if h.snapshotNotifier != nil {
			h.snapshotNotifier.OnConfigCached(key, entry.ETag, body)
		}
		// Serve from the cached entry so headers match the cache-hit path.
		h.serveFromCache(w, r, entry, sdkKey)
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

// buildTarget derives the geo-bound CDN URL for an SDK key.
// Returns ("", false) if the SDK key format is not recognized.
func (h *Handler) buildTarget(sdkKey string) (string, bool) {
	region, ok := extractRegion(sdkKey)
	if !ok {
		return "", false
	}
	return "https://cdn." + region + "." + h.cdnBaseDomain + "/server/" + sdkKey + ".json", true
}

// extractRegion parses the region segment from an SDK key in the format
// dt_server_{region}_{token}. Returns ("", false) for any other format.
// The region is validated to contain only [a-z0-9-] to prevent SSRF via
// crafted region segments in the derived CDN URL.
func extractRegion(sdkKey string) (string, bool) {
	parts := strings.SplitN(sdkKey, "_", 4)
	if len(parts) != 4 || parts[0] != "dt" || parts[1] != "server" || parts[2] == "" || parts[3] == "" {
		return "", false
	}
	region := parts[2]
	for _, c := range region {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return "", false
		}
	}
	return region, true
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

// derivedScheme infers the public-facing scheme from the request.
// It checks X-Forwarded-Proto (set by most ingress controllers and LBs),
// then r.TLS, then defaults to "http". Only "http" and "https" are accepted
// from X-Forwarded-Proto; any other value is discarded to prevent injection.
func derivedScheme(r *http.Request) string {
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		if i := strings.IndexByte(proto, ','); i >= 0 {
			proto = proto[:i]
		}
		proto = strings.TrimSpace(proto)
		if proto == "http" || proto == "https" {
			return proto
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// proxySSEURL builds the rewrite target URL for the SSE entry in the flagd
// config payload. The scheme and host are derived from the incoming request,
// so no static PROXY_PUBLIC_URL configuration is required.
//
// r.Host is client-controlled, but the URL is returned only to the same client
// that provided the header — there is no SSRF surface here. The host is
// validated to reject values containing whitespace or control characters that
// could malform the resulting URL string.
func (h *Handler) proxySSEURL(r *http.Request, sdkKey string) string {
	host := r.Host
	if strings.ContainsAny(host, " \t\r\n") {
		h.logger.Warn("proxy: malformed Host header discarded in SSE URL rewrite",
			slog.String("key", cache.Key(sdkKey)),
		)
		host = "unknown"
	}
	return derivedScheme(r) + "://" + host + sse.HandlerPath + "?sdk_key=" + url.QueryEscape(sdkKey)
}

// recordRewriteResult increments the ConfigSSERewriteTotal counter and emits
// a warning log when the upstream URL was not found as a byte literal.
func (h *Handler) recordRewriteResult(result sse.RewriteResult, keyHash string) {
	var label string
	switch result {
	case sse.RewriteResultRewritten:
		label = "rewritten"
	case sse.RewriteResultNoEntry:
		label = "no_sse_entry"
	case sse.RewriteResultNotFoundInBody:
		label = "not_found_in_body"
		h.logger.Warn("proxy: SSE URL rewrite skipped — upstream URL not found as literal byte sequence",
			slog.String("key", keyHash),
		)
	default:
		label = "unknown"
		h.logger.Warn("proxy: unknown SSE rewrite result",
			slog.String("key", keyHash),
			slog.Int("result", int(result)),
		)
	}
	if h.inst == nil {
		return
	}
	h.inst.ConfigSSERewriteTotal.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("result", label),
		),
	)
}
