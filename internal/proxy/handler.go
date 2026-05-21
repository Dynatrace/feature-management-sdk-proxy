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

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/singleflight"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/sse"
	"github.com/dynatrace/sdk-proxy/internal/telemetry"
	"github.com/dynatrace/sdk-proxy/internal/tracing"
)

const (
	resultHit           = "hit"
	resultMiss          = "miss"
	resultClientError   = "client_error"
	resultUpstreamError = "upstream_error"
	resultInternalError = "internal_error"
	resultBadGateway    = "bad_gateway"
	resultServerError   = "server_error"
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
	OnConfigCached(keyID, etag string, payload []byte)
}

// coldFetchResult carries the shared outcome of a singleflight cold-miss fetch.
// All waiters for the same cache key receive the same value.
type coldFetchResult struct {
	entry       *cache.Entry // non-nil on 200
	resultLabel string       // "miss", "client_error", resultUpstreamError, resultInternalError
	statusCode  int          // set for 4xx pass-through
	headers     http.Header  // set for 4xx pass-through
	body        []byte       // set for 4xx pass-through
}

// Handler serves flagd config requests from an in-memory cache, falling back
// to the upstream CDN on a miss. The SDK key is never logged or stored; only
// its public prefix is used as a stable identifier.
type Handler struct {
	cdnBaseDomain    string
	client           *http.Client
	cache            *cache.Cache
	sseManager       *sse.Manager
	snapshotNotifier SnapshotNotifier // nil when persistence is disabled
	refreshFetcher   cache.Fetcher    // nil disables demand revalidation
	evictor          func(keyID string)
	inst             *metrics.Instruments
	counter          *telemetry.Counter
	logger           *slog.Logger
	tracer           trace.Tracer
	sfGroup          singleflight.Group
	revalGroup       singleflight.Group
	maxResponseBytes int64
	rewriteSSEURLs   bool
}

const defaultMaxResponseBytes = 10 * 1024 * 1024 // 10 MiB

// New constructs a Handler. cdnBaseDomain must be a bare hostname
// (e.g. fm.dynatracelabs.com); the region and path are derived per request.
// sseManager and snapshotNotifier may be nil; when non-nil, they are notified
// after each successful cold fetch. refreshFetcher may be nil; when non-nil,
// snapshot-loaded entries are demand-revalidated on the first stale hit.
// inst and counter may be nil; metrics and counting are silently skipped.
func New(cdnBaseDomain string, client *http.Client, c *cache.Cache, sseManager *sse.Manager, snapshotNotifier SnapshotNotifier, refreshFetcher cache.Fetcher, inst *metrics.Instruments, counter *telemetry.Counter, logger *slog.Logger, t trace.Tracer) (*Handler, error) {
	if cdnBaseDomain == "" || strings.Contains(cdnBaseDomain, "://") || strings.Contains(cdnBaseDomain, "/") {
		return nil, fmt.Errorf("proxy: invalid CDN base domain %q: must be a bare hostname (e.g. fm.dynatracelabs.com)", cdnBaseDomain)
	}
	return &Handler{cdnBaseDomain: cdnBaseDomain, client: client, cache: c, sseManager: sseManager, snapshotNotifier: snapshotNotifier, refreshFetcher: refreshFetcher, inst: inst, counter: counter, logger: logger, tracer: t, maxResponseBytes: defaultMaxResponseBytes, rewriteSSEURLs: true}, nil
}

// SetEvictor wires a callback invoked after a cache entry is deleted due to a
// 4xx response during demand revalidation. The callback should close the upstream
// SSE connection and delete the persisted snapshot for keyID.
func (h *Handler) SetEvictor(evict func(keyID string)) {
	h.evictor = evict
}

// SetMaxResponseBytes caps the body the proxy reads from the upstream CDN per
// cold fetch. Bodies exceeding the limit are rejected with a 503 and not
// cached. Must be called before the handler serves requests.
func (h *Handler) SetMaxResponseBytes(n int64) {
	h.maxResponseBytes = n
}

// SetRewriteSSEURLs controls whether the SSE endpoint URL in the flagd config
// payload is rewritten to point at this proxy on every GET response. Set to
// false when SSE_DISABLED=true so SDK clients receive the upstream URL verbatim
// (or no URL at all) rather than connecting to a non-functional /sse endpoint.
func (h *Handler) SetRewriteSSEURLs(enabled bool) {
	h.rewriteSSEURLs = enabled
}

// Handle is the native Gin handler for the proxy config endpoint.
// It extracts the SDK key from the :file path parameter, serves from cache
// on hit, or fetches from upstream CDN on miss.
//
// Route registration should use:
//
//	router.GET("/server/:file", proxyHandler.Handle)
//	router.HEAD("/server/:file", proxyHandler.Handle)
//
// The TracePropagation middleware must be applied upstream so
// c.Request.Context() already contains the extracted W3C trace context.
func (h *Handler) Handle(c *gin.Context) {
	file := c.Param("file")
	sdkKey, ok := extractSDKKeyFromFile(file)
	if !ok {
		c.String(http.StatusBadRequest, "path must end with /<sdk_key>.json\n")
		return
	}

	target, ok := h.buildTarget(sdkKey)
	if !ok {
		h.logger.WarnContext(c.Request.Context(), "proxy: unrecognized SDK key format", slog.String("sdk_key_public_id", cache.PublicID(sdkKey)))
		c.String(http.StatusBadRequest, "unrecognized SDK key format: must be {token}.{platform}_{geo}_{publicRandom}.{secret}_{sha1}\n")
		return
	}

	key := cache.PublicID(sdkKey)

	// Create the request span. W3C trace context was already extracted by the
	// TracePropagation middleware, so c.Request.Context() has the parent trace.
	ctx, span := h.tracer.Start(c.Request.Context(), c.Request.Method+" /server/{sdk_key}.json",
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.request.method", c.Request.Method),
			attribute.String("http.route", "/server/{sdk_key}.json"),
			tracing.SDKKeyPublicID(key),
		))
	defer span.End()

	// Count every request with a recognized SDK key, regardless of whether it
	// hits the cache or reaches upstream. Cache hits, cache misses, upstream
	// errors (503), and HEAD requests all reflect real SDK usage.
	if h.counter != nil {
		h.counter.Increment(sdkKey)
	}

	if entry, ok := h.cache.Get(key); ok {
		h.logger.DebugContext(ctx, "proxy: cache hit", slog.String("sdk_key_public_id", key))
		result := h.serveCacheHit(ctx, c, entry, sdkKey, key)
		h.recordResult(ctx, result)
		span.SetAttributes(attribute.String("result", result))
		return
	}

	h.logger.DebugContext(ctx, "proxy: cache miss — fetching upstream", slog.String("sdk_key_public_id", key))
	result := h.serveMiss(ctx, c, sdkKey, key, target)
	h.recordResult(ctx, result)
	span.SetAttributes(attribute.String("result", result))
}

// serveCacheHit starts a proxy.serve_cache_hit child span (with a Span Link to
// the originating fetch when available) and delegates to serveFromCache.
// Returns a result label for RequestsTotal ("hit" or "upstream_error").
func (h *Handler) serveCacheHit(ctx context.Context, c *gin.Context, entry cache.Entry, sdkKey, key string) string {
	opts := []trace.SpanStartOption{
		trace.WithAttributes(tracing.SDKKeyPublicID(key)),
	}
	if sc, ok := entry.OriginatingFetch.(trace.SpanContext); ok && sc.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc}))
	}
	_, cacheSpan := h.tracer.Start(ctx, "proxy.serve_cache_hit", opts...)
	defer cacheSpan.End()
	return h.serveFromCache(ctx, c, entry, sdkKey)
}

// serveFromCache writes a response from a cached entry. If the client's
// If-None-Match matches the cached ETag, a 304 is returned with no body.
// For non-HEAD 200 responses, the SSE URL in the payload is rewritten to
// point to this proxy's SSE endpoint (derived from the incoming request).
//
// When entry.FetchURL is empty (the entry was restored from a snapshot at
// startup), it is repaired here so the background refresher can revalidate
// it on the next TTL tick. If the entry is also stale and refreshFetcher is
// wired, a synchronous conditional GET is performed before serving — the
// response is intentionally delayed by one CDN round-trip on that first stale
// hit so the SDK receives fresh (or confirmed-valid) config.
//
// Returns "hit" on success or "upstream_error" if the entry was evicted (503
// already written).
func (h *Handler) serveFromCache(ctx context.Context, c *gin.Context, entry cache.Entry, sdkKey string) string {
	key := cache.PublicID(sdkKey)

	wasSnapshotLoaded := entry.FetchURL == ""
	if wasSnapshotLoaded {
		if target, ok := h.buildTarget(sdkKey); ok {
			entry.FetchURL = target
			h.cache.Set(key, entry)
		}
	}

	// Demand revalidation: snapshot-loaded entries whose FetchURL was just repaired
	// and whose age exceeds the current refresh interval get a synchronous conditional
	// GET before the response is served.
	if wasSnapshotLoaded && entry.FetchURL != "" && h.refreshFetcher != nil {
		interval := cache.RefreshIntervalDisconnected
		if h.sseManager != nil {
			interval = h.sseManager.RefreshInterval(key)
		}
		if entry.IsStale(time.Now(), interval) {
			updated, evictedCode := h.demandRevalidate(ctx, sdkKey, key, entry)
			if evictedCode != 0 {
				// Entry was evicted due to a 4xx. Pass through the upstream status
				// so providers can respond appropriately (e.g. 401 → re-authenticate).
				c.String(evictedCode, http.StatusText(evictedCode)+"\n")
				return resultClientError
			}
			entry = updated
		} else {
			h.recordDemandReval(ctx, "hit")
		}
	}

	if entry.ETag != "" && c.GetHeader("If-None-Match") == entry.ETag {
		c.Header("ETag", entry.ETag)
		c.Status(http.StatusNotModified)
		c.Writer.WriteHeaderNow()
		return resultHit
	}
	if entry.ETag != "" {
		c.Header("ETag", entry.ETag)
	}
	c.Header("Content-Type", "application/json")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		c.Writer.WriteHeaderNow()
		return resultHit
	}
	var payload []byte
	if h.rewriteSSEURLs {
		var rewriteResult sse.RewriteResult
		payload, rewriteResult = sse.RewriteSSEURL(entry.Payload, h.proxySSEURL(ctx, c.Request, key))
		h.recordRewriteResult(ctx, rewriteResult, key)
	} else {
		payload = entry.Payload
	}
	c.Data(http.StatusOK, "application/json", payload)
	return resultHit
}

// demandRevalEntry is the shared result of a singleflight demand revalidation call.
type demandRevalEntry struct {
	entry       cache.Entry
	evictedCode int // non-zero: entry was evicted; pass this status code to the client
}

// demandRevalidate performs a synchronous conditional GET for a snapshot-loaded
// entry whose age exceeds the current refresh interval. It deduplicates concurrent
// requests for the same key via singleflight so at most one CDN call is made.
//
// Returns the (potentially refreshed) entry and evictedCode=0 on success.
// When the entry was evicted due to a 4xx response, returns the zero entry and
// the upstream status code so the caller can pass it through to the client.
// On network errors or 429/5xx, the original stale entry is returned with evictedCode=0
// so the SDK always receives a cached response.
func (h *Handler) demandRevalidate(ctx context.Context, sdkKey, key string, entry cache.Entry) (cache.Entry, int) {
	// Detach cancellation so the fetch survives client disconnect while
	// preserving trace lineage for the demand_revalidate span.
	detachedCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))

	v, _, _ := h.revalGroup.Do(key, func() (any, error) {
		return h.doDemandRevalidate(detachedCtx, sdkKey, key, entry), nil
	})
	res := v.(demandRevalEntry)
	return res.entry, res.evictedCode
}

func (h *Handler) doDemandRevalidate(ctx context.Context, sdkKey, key string, entry cache.Entry) demandRevalEntry {
	ctx, span := h.tracer.Start(ctx, "proxy.demand_revalidate",
		trace.WithAttributes(
			tracing.SDKKeyPublicID(key),
			attribute.Bool("if_none_match", entry.ETag != ""),
		))
	defer span.End()

	result, err := h.refreshFetcher.Fetch(ctx, key, entry.FetchURL, entry.ETag)
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String("result", "error"))
		h.logger.WarnContext(ctx, "proxy: demand revalidation fetch failed, serving stale entry",
			slog.String("sdk_key_public_id", key),
			slog.String("error", err.Error()),
		)
		h.recordDemandReval(ctx, "error")
		return demandRevalEntry{entry: entry}
	}

	if result.NotModified {
		span.SetAttributes(attribute.String("result", "not_modified"))
		now := time.Now()
		h.cache.ResetTTL(key, now)
		entry.FetchedAt = now
		h.logger.DebugContext(ctx, "proxy: demand revalidation — upstream unchanged (304), TTL extended", slog.String("sdk_key_public_id", key))
		if h.sseManager != nil {
			sseCtx := trace.ContextWithSpanContext(context.Background(), span.SpanContext())
			h.sseManager.OnConfigCached(sseCtx, sdkKey, entry.Payload)
		}
		h.recordDemandReval(ctx, "not_modified")
		return demandRevalEntry{entry: entry}
	}

	if result.Entry != nil {
		result.Entry.FetchURL = entry.FetchURL
		result.Entry.OriginatingFetch = span.SpanContext()
		span.SetAttributes(attribute.String("result", "ok"))
		h.cache.Set(key, *result.Entry)
		h.logger.DebugContext(ctx, "proxy: demand revalidation — new content received (200), cache updated", slog.String("sdk_key_public_id", key))
		if h.sseManager != nil {
			sseCtx := trace.ContextWithSpanContext(context.Background(), span.SpanContext())
			h.sseManager.OnConfigCached(sseCtx, sdkKey, result.Entry.Payload)
		}
		if h.snapshotNotifier != nil {
			h.snapshotNotifier.OnConfigCached(key, result.Entry.ETag, result.Entry.Payload)
		}
		h.recordDemandReval(ctx, "refreshed")
		return demandRevalEntry{entry: *result.Entry}
	}

	if result.StatusCode != 0 {
		span.SetAttributes(
			attribute.String("result", "upstream_error"),
			attribute.Int("upstream.status_code", result.StatusCode),
		)
		if cache.ShouldEvict(result.StatusCode) {
			h.cache.Delete(key)
			h.logger.WarnContext(ctx, "proxy: demand revalidation got evictable status, evicting entry",
				slog.String("sdk_key_public_id", key),
				slog.Int("upstream_status", result.StatusCode),
			)
			if h.evictor != nil {
				h.evictor(key)
			}
			h.recordDemandReval(ctx, "evicted")
			return demandRevalEntry{evictedCode: result.StatusCode}
		}
		h.logger.WarnContext(ctx, "proxy: demand revalidation got upstream error, serving stale entry",
			slog.String("sdk_key_public_id", key),
			slog.Int("upstream_status", result.StatusCode),
		)
		h.recordDemandReval(ctx, "preserved_stale")
		return demandRevalEntry{entry: entry}
	}

	span.SetAttributes(attribute.String("result", "unexpected_empty"))
	return demandRevalEntry{entry: entry}
}

// recordDemandReval increments SnapshotDemandRevalidationTotal with the given result label.
func (h *Handler) recordDemandReval(ctx context.Context, result string) {
	if h.inst == nil {
		return
	}
	h.inst.SnapshotDemandRevalidationTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

// serveMiss deduplicates concurrent cold-miss fetches for the same SDK key via
// singleflight. Only one upstream call is made; all waiters share the result.
// The first caller's trace context is used for the cold fetch span; singleflight
// waiters receive the same result and serve from cache without a cold_fetch span.
// Returns a result label for RequestsTotal ("miss", "upstream_error", or "client_error").
func (h *Handler) serveMiss(ctx context.Context, c *gin.Context, sdkKey, key, target string) string {
	// Detach cancellation so the fetch survives the originating client disconnecting,
	// while preserving the trace lineage so the cold_fetch span links back correctly.
	detachedCtx := trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))
	v, _, _ := h.sfGroup.Do(key, func() (any, error) {
		return h.coldFetch(detachedCtx, sdkKey, key, target), nil
	})
	result := v.(coldFetchResult)
	switch result.resultLabel {
	case resultMiss:
		h.serveFromCache(ctx, c, *result.entry, sdkKey)
		return resultMiss
	case resultClientError, resultServerError:
		copyHeader(c.Writer.Header(), result.headers)
		c.Writer.WriteHeader(result.statusCode)
		_, _ = c.Writer.Write(result.body)
		return result.resultLabel
	case resultBadGateway:
		c.String(http.StatusBadGateway, "bad gateway\n")
		return resultBadGateway
	case resultInternalError:
		c.String(http.StatusInternalServerError, "internal error\n")
		return resultUpstreamError
	default: // resultUpstreamError
		c.String(http.StatusServiceUnavailable, "upstream unavailable\n")
		return resultUpstreamError
	}
}

// coldFetch performs the upstream CDN fetch for a cache miss. It is executed
// inside a singleflight group, so at most one call runs per cache key at a time.
// ctx must be a detached context (no cancellation) with the originating request's
// trace context attached so the cold_fetch span links back to the serving span.
// All side effects (cache write, SSE notification, snapshot) fire exactly once per fetch.
func (h *Handler) coldFetch(ctx context.Context, sdkKey, key, target string) coldFetchResult {
	ctx, span := h.tracer.Start(ctx, "proxy.cold_fetch",
		trace.WithAttributes(tracing.SDKKeyPublicID(key)))
	defer span.End()

	// Defensive read: a prior singleflight group for this key may have just
	// completed and populated the cache before we started.
	if entry, ok := h.cache.Get(key); ok {
		// entry.OriginatingFetch was set by the prior fetch leader — preserve it.
		span.SetAttributes(attribute.String("result", "dedup_hit"))
		return coldFetchResult{entry: &entry, resultLabel: resultMiss}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		h.logger.ErrorContext(ctx, "proxy: build upstream request failed", slog.String("error", sanitizeErr(err).Error()))
		span.RecordError(err)
		span.SetAttributes(attribute.String("result", "internal_error"))
		return coldFetchResult{resultLabel: resultInternalError}
	}

	resp, err := h.client.Do(req)
	if err != nil {
		h.logger.WarnContext(ctx, "proxy: upstream request failed",
			slog.String("sdk_key_public_id", key),
			slog.String("error", sanitizeErr(err).Error()),
		)
		span.RecordError(sanitizeErr(err))
		span.SetAttributes(attribute.String("result", "upstream_error"))
		return coldFetchResult{resultLabel: resultUpstreamError}
	}
	defer resp.Body.Close()

	span.SetAttributes(attribute.Int("upstream.status_code", resp.StatusCode))

	body, err := io.ReadAll(io.LimitReader(resp.Body, h.maxResponseBytes+1))
	if err != nil {
		h.logger.ErrorContext(ctx, "proxy: read upstream response failed", slog.String("error", err.Error()))
		span.RecordError(err)
		span.SetAttributes(attribute.String("result", "internal_error"))
		return coldFetchResult{resultLabel: resultInternalError}
	}

	switch {
	case resp.StatusCode == http.StatusNotModified:
		// Upstream returned 304 on a cold fetch (no If-None-Match was sent).
		// This is a bug in the upstream — refuse to return nothing to the SDK.
		h.logger.ErrorContext(ctx, "proxy: upstream returned 304 on cold fetch",
			slog.String("sdk_key_public_id", key),
		)
		span.SetAttributes(attribute.String("result", "unexpected_304"))
		return coldFetchResult{resultLabel: resultBadGateway}

	case resp.StatusCode >= 500:
		h.logger.WarnContext(ctx, "proxy: upstream server error",
			slog.String("sdk_key_public_id", key),
			slog.Int("upstream_status", resp.StatusCode),
		)
		span.SetAttributes(attribute.String("result", resultServerError),
			attribute.Int("upstream.status_code", resp.StatusCode))
		headers := make(http.Header)
		copyHeader(headers, resp.Header)
		return coldFetchResult{resultLabel: resultServerError, statusCode: resp.StatusCode, headers: headers, body: body}

	case resp.StatusCode == http.StatusOK:
		if int64(len(body)) > h.maxResponseBytes {
			h.logger.WarnContext(ctx, "proxy: upstream response exceeds size limit",
				slog.String("sdk_key_public_id", key),
				slog.Int64("limit_bytes", h.maxResponseBytes),
			)
			span.SetAttributes(attribute.String("result", resultBadGateway))
			return coldFetchResult{resultLabel: resultBadGateway}
		}
		entry := cache.Entry{
			Payload:   body,
			ETag:      resp.Header.Get("ETag"),
			FetchedAt: time.Now(),
			// FetchURL contains the raw SDK key — in-memory use is permitted by design
			// (CLAUDE.md invariant). Snapshot code MUST NOT persist this field as-is.
			FetchURL:         target,
			OriginatingFetch: span.SpanContext(),
		}
		span.SetAttributes(attribute.String("result", "ok"))
		h.cache.Set(key, entry)
		if h.sseManager != nil {
			// Preserve trace lineage for the SSE goroutine that starts after this fetch,
			// but detach cancellation so SSE connections outlive the HTTP request.
			sseCtx := trace.ContextWithSpanContext(context.Background(), span.SpanContext())
			h.sseManager.OnConfigCached(sseCtx, sdkKey, body)
		}
		if h.snapshotNotifier != nil {
			h.snapshotNotifier.OnConfigCached(key, entry.ETag, body)
		}
		return coldFetchResult{entry: &entry, resultLabel: resultMiss}
	}

	// Pass through 4xx and non-200 2xx responses without caching.
	headers := make(http.Header)
	copyHeader(headers, resp.Header)
	span.SetAttributes(attribute.String("result", "client_error"))
	return coldFetchResult{
		resultLabel: resultClientError,
		statusCode:  resp.StatusCode,
		headers:     headers,
		body:        body,
	}
}

// Fetcher performs background TTL revalidation requests against the upstream CDN.
// It uses its own http.Client so the background refresh timeout is independent
// of the request-path client (5s vs 30s).
type Fetcher struct {
	client           *http.Client
	logger           *slog.Logger
	maxResponseBytes int64
}

// NewFetcher constructs a Fetcher for use with cache.NewRefresherWithObservers.
// maxResponseBytes caps how much of the upstream response body is read; bodies
// that exceed this limit produce an error and the stale cache entry is retained.
// Pass 0 to use the default (10 MiB).
func NewFetcher(client *http.Client, logger *slog.Logger, maxResponseBytes int64) *Fetcher {
	if maxResponseBytes == 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}
	return &Fetcher{client: client, logger: logger, maxResponseBytes: maxResponseBytes}
}

// Fetch implements cache.Fetcher. It sends an If-None-Match revalidation
// request and interprets the result. Network errors are sanitized before
// returning to prevent SDK key leakage via *url.Error.Error() in log messages.
// The span for this fetch is emitted by cache.Refresher.refresh which owns keyID.
func (f *Fetcher) Fetch(ctx context.Context, _ string, fetchURL, etag string) (cache.FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return cache.FetchResult{}, fmt.Errorf("proxy: build refresh request: %w", err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return cache.FetchResult{}, sanitizeErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return cache.FetchResult{NotModified: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return cache.FetchResult{StatusCode: resp.StatusCode}, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxResponseBytes+1))
	if err != nil {
		return cache.FetchResult{}, fmt.Errorf("proxy: read refresh response: %w", err)
	}
	if int64(len(body)) > f.maxResponseBytes {
		return cache.FetchResult{}, fmt.Errorf("proxy: refresh response body exceeds %d-byte limit", f.maxResponseBytes)
	}

	return cache.FetchResult{
		Entry: &cache.Entry{
			Payload:   body,
			ETag:      resp.Header.Get("ETag"),
			FetchedAt: time.Now(),
			// FetchURL is set by the Refresher to preserve the original URL.
			// OriginatingFetch is set by the Refresher.refresh after Fetch returns.
		},
	}, nil
}

// buildTarget derives the geo-bound CDN URL for an SDK key.
// Returns ("", false) if the SDK key format is not recognized.
func (h *Handler) buildTarget(sdkKey string) (string, bool) {
	geo, ok := extractGeo(sdkKey)
	if !ok {
		return "", false
	}
	return "https://cdn." + geo + "." + h.cdnBaseDomain + "/server/" + sdkKey + ".json", true
}

// extractGeo parses the geo segment from an SDK key in the new format
// {token}.{platform}_{geo}_{publicRandom}.{secret}_{sha1}.
// Returns ("", false) for any other format. The geo is always [a-z]{2} per the
// full-key regex, so it is safe to embed in the derived CDN URL.
func extractGeo(sdkKey string) (string, bool) {
	if !cache.ValidateFullKey(sdkKey) {
		return "", false
	}
	// second dot-segment: {platform}_{geo}_{publicRandom}
	dotParts := strings.SplitN(sdkKey, ".", 3)
	underParts := strings.SplitN(dotParts[1], "_", 3)
	return underParts[1], true
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

// extractSDKKeyFromFile returns the SDK key from a route parameter value
// (e.g. "dt01.server_us_...json" → the key without ".json").
// Returns ("", false) if the parameter is malformed.
func extractSDKKeyFromFile(file string) (string, bool) {
	if !strings.HasSuffix(file, ".json") {
		return "", false
	}
	key := strings.TrimSuffix(file, ".json")
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
// keyID is the SDK key public identifier (cache.PublicID(sdkKey)) — the raw SDK key must
// never appear in the rewritten URL (logs, browser history, reverse proxies).
//
// r.Host is client-controlled, but the URL is returned only to the same client
// that provided the header — there is no SSRF surface here. The host is
// validated to reject values containing whitespace or control characters that
// could malform the resulting URL string.
func (h *Handler) proxySSEURL(ctx context.Context, r *http.Request, keyID string) string {
	host := r.Host
	if strings.ContainsAny(host, " \t\r\n") {
		h.logger.WarnContext(ctx, "proxy: malformed Host header discarded in SSE URL rewrite",
			slog.String("sdk_key_public_id", keyID),
		)
		host = "unknown"
	}
	return derivedScheme(r) + "://" + host + sse.HandlerPath + "?stream_id=" + keyID
}

// recordResult increments RequestsTotal with the given result label.
func (h *Handler) recordResult(ctx context.Context, result string) {
	if h.inst == nil {
		return
	}
	h.inst.RequestsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

// recordRewriteResult increments the ConfigSSERewriteTotal counter and emits
// a warning log when the upstream URL was not found as a byte literal.
func (h *Handler) recordRewriteResult(ctx context.Context, result sse.RewriteResult, keyID string) {
	var label string
	switch result {
	case sse.RewriteResultRewritten:
		label = "rewritten"
	case sse.RewriteResultNoEntry:
		label = "no_sse_entry"
	case sse.RewriteResultNotFoundInBody:
		label = "not_found_in_body"
		h.logger.WarnContext(ctx, "proxy: SSE URL rewrite skipped — upstream URL not found as literal byte sequence",
			slog.String("sdk_key_public_id", keyID),
		)
	default:
		label = "unknown"
		h.logger.WarnContext(ctx, "proxy: unknown SSE rewrite result",
			slog.String("sdk_key_public_id", keyID),
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
