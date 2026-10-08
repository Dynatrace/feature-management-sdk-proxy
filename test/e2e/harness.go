//go:build integration

// docs: docs/operations.md — update that doc when adding or changing harness capabilities.

// Package e2e provides an in-process integration test harness for the SDK Proxy.
// Tests use the integration build tag and run via `make test-int`.
package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/health"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/middleware"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/proxy"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/server"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/snapshot"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sse"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/telemetry"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/telemetry/billingtest"
)

const (
	// TestSDKKey is a valid-format SDK key for use in tests.
	TestSDKKey = "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	// TestSDKKey2 is a second valid SDK key for multi-key tests.
	TestSDKKey2 = "dt01.server_eu_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"

	configHandlerPath = "/server/"
)

// Options configures optional harness settings.
type Options struct {
	// SnapshotDir enables local snapshot persistence rooted at this directory.
	// The directory must already exist (use t.TempDir()).
	SnapshotDir string
	// SnapshotMaxAge overrides the freshness threshold for startup classification.
	// Defaults to 24 hours when zero.
	SnapshotMaxAge time.Duration
	// BillingFlushInterval overrides the billing flush ticker interval.
	// Defaults to 1 hour (effectively disabled during tests) when zero.
	BillingFlushInterval time.Duration
	// EnableDemandRevalidation wires the demand refreshFetcher into the proxy
	// handler so snapshot-loaded entries are conditionally revalidated on the
	// first stale hit. Disabled by default to preserve existing test behaviour.
	EnableDemandRevalidation bool
}

// Harness wires all proxy components in-process for integration testing.
// The embedded CDN and SSE mocks are accessible for controlling upstream
// behavior and asserting call counts / event delivery.
type Harness struct {
	CDN     *CDNMock
	SSE     *SSEMock
	Billing *billingtest.Reporter
	Cache   *cache.Cache

	addr string

	srv            *server.Server
	sseManager     *sse.Manager
	flusher        *telemetry.Flusher
	snapshotWriter *snapshot.Writer
	counter        *telemetry.Counter
	inst           *metrics.Instruments

	ctx          context.Context
	cancel       context.CancelFunc
	flusherDone  chan struct{}
	snapshotDone chan struct{}
	srvErrCh     chan error
}

// New creates and starts a Harness. Returns an error only on startup failure
// (e.g. CaseBackendUnreachable: persistence configured but backend unreachable).
// Use MustNew for tests that don't test startup failure paths.
// t.Cleanup is registered to call Close on test completion.
func New(t *testing.T, opts Options) (*Harness, error) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	meter := noop.NewMeterProvider().Meter("test")
	inst, err := metrics.Register(meter)
	if err != nil {
		return nil, fmt.Errorf("metrics.Register: %w", err)
	}

	sseMock := newSSEMock()
	t.Cleanup(sseMock.Close)

	cdnMock := newCDNMock(sseMock.URL())
	t.Cleanup(cdnMock.Close)

	billing := &billingtest.Reporter{}
	counter := telemetry.NewCounter()
	flagCache := cache.New()

	ctx, cancel := context.WithCancel(context.Background())

	maxAge := opts.SnapshotMaxAge
	if maxAge == 0 {
		maxAge = 24 * time.Hour
	}

	var snapshotWriter *snapshot.Writer
	var snapshotDone chan struct{}

	if opts.SnapshotDir != "" {
		store, err := snapshot.OpenBlobStore(ctx, "file://"+opts.SnapshotDir)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("snapshot.OpenBlobStore: %w", err)
		}
		loaded, writer, loadErr := initSnapshotWriter(ctx, store, flagCache, inst, logger)
		snapshotWriter = writer

		oldest, _ := snapshot.OldestCapturedAt(flagCache)
		switch snapshot.Classify(true, loaded, loadErr != nil, oldest, time.Now(), maxAge) {
		case snapshot.CaseColdNoPersistence:
			// unreachable (persistConfigured=true)
		case snapshot.CaseSnapshotFresh, snapshot.CaseSnapshotStale, snapshot.CaseEmptyPersistenceCold:
			// ok — serve from cache or start cold
		case snapshot.CaseBackendUnreachable:
			cancel()
			return nil, fmt.Errorf("startup: snapshot backend unreachable: %w", loadErr)
		}
	}

	sseManager := sse.New(sseMock.Client(), inst, logger, sse.Config{})

	var snapshotInvalidator sse.Invalidator
	if snapshotWriter != nil {
		snapshotInvalidator = sse.InvalidatorFunc(snapshotWriter.OnInvalidated)
	}
	lifecycleHandler := sse.NewLifecycleHandler(inst, logger, 10000,
		sse.InvalidatorFunc(flagCache.Delete),
		sse.InvalidatorFunc(sseManager.Close),
		snapshotInvalidator,
	)
	sseManager.SetLifecycleHandler(lifecycleHandler.HandleEvent)

	// Explicitly convert typed nil to untyped nil to avoid the Go nil-interface
	// trap: a typed nil *snapshot.Writer stored in a SnapshotNotifier interface
	// is non-nil, causing the proxy to call OnConfigCached on a nil receiver.
	var snapNotifier proxy.SnapshotNotifier
	if snapshotWriter != nil {
		snapNotifier = snapshotWriter
	}
	var demandFetcher cache.Fetcher
	if opts.EnableDemandRevalidation {
		demandFetcher = proxy.NewFetcher(cdnMock.redirectingClient(), logger, 0)
	}
	proxyHandler, err := proxy.New(
		"fm.test",
		cdnMock.redirectingClient(),
		flagCache,
		sseManager,
		snapNotifier,
		demandFetcher,
		inst,
		counter,
		logger,
		tracenoop.NewTracerProvider().Tracer("test"),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("proxy.New: %w", err)
	}
	proxyHandler.SetEvictor(func(keyID string) {
		sseManager.Close(keyID)
		if snapshotWriter != nil {
			snapshotWriter.OnInvalidated(keyID)
		}
	})

	sseHandler := sse.NewHandler(flagCache, sseManager.Server(), sseManager, inst, counter, logger, 15*time.Second, configHandlerPath)

	billingInterval := opts.BillingFlushInterval
	if billingInterval == 0 {
		billingInterval = 1 * time.Hour
	}
	flusher := telemetry.NewFlusher(
		counter,
		billing,
		telemetry.NewBoundedRetryBuffer(16, inst, logger),
		inst,
		billingInterval,
		logger,
	)

	if snapshotWriter != nil {
		snapshotDone = make(chan struct{})
		go func() {
			defer close(snapshotDone)
			snapshotWriter.Run(ctx)
		}()
	}

	// Use a dedicated 5s fetcher for background refresh (same reasoning as main.go).
	refreshFetcher := proxy.NewFetcher(&http.Client{Timeout: 5 * time.Second}, logger, 0)
	var refreshObservers []cache.RefreshObserver
	refreshObservers = append(refreshObservers, sseManager)
	if snapshotWriter != nil {
		refreshObservers = append(refreshObservers, snapshotWriter)
	}
	refresher := cache.NewRefresherWithObservers(flagCache, refreshFetcher, logger, refreshObservers...)
	refresher.SetIntervalProvider(sseManager)
	go refresher.Run(ctx)

	probeHandlers := health.New(func() bool { return true }, logger)

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	// Health probes — no tracing.
	router.GET("/livez", probeHandlers.Live)
	router.GET("/readyz", probeHandlers.Ready)
	router.GET("/healthz", probeHandlers.Health)

	// API routes with trace propagation middleware.
	api := router.Group("/", middleware.TracePropagation())
	api.GET(sse.HandlerPath, sdkkey.FromStreamID(), sseHandler.Handle)
	api.GET(configHandlerPath+":file", sdkkey.FromPathFile(), proxyHandler.Handle)
	api.HEAD(configHandlerPath+":file", sdkkey.FromPathFile(), proxyHandler.Handle)
	router.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "404 page not found\n")
	})

	srv, err := server.New(":0", router, 5*time.Second, logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("server.New: %w", err)
	}

	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		flusher.Run(ctx)
	}()

	srvErrCh := make(chan error, 1)
	go func() { srvErrCh <- srv.Start() }()

	h := &Harness{
		CDN:            cdnMock,
		SSE:            sseMock,
		Billing:        billing,
		Cache:          flagCache,
		addr:           srv.Addr(),
		srv:            srv,
		sseManager:     sseManager,
		flusher:        flusher,
		snapshotWriter: snapshotWriter,
		counter:        counter,
		inst:           inst,
		ctx:            ctx,
		cancel:         cancel,
		flusherDone:    flusherDone,
		snapshotDone:   snapshotDone,
		srvErrCh:       srvErrCh,
	}
	t.Cleanup(func() { h.Close(context.Background()) })
	return h, nil
}

// MustNew calls New and fails the test immediately on error.
func MustNew(t *testing.T, opts Options) *Harness {
	t.Helper()
	h, err := New(t, opts)
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	return h
}

// URL returns the full URL for the given path on the proxy server.
func (h *Harness) URL(path string) string {
	return "http://" + h.addr + path
}

// ConfigPath returns the proxy config path for the given SDK key.
func (h *Harness) ConfigPath(sdkKey string) string {
	return configHandlerPath + sdkKey + ".json"
}

// SSEPath returns the proxy SSE path for the given SDK key using stream_id.
func (h *Harness) SSEPath(sdkKey string) string {
	return sse.HandlerPath + "?stream_id=" + sdkkey.PublicID(sdkKey)
}

// GetConfig makes a GET request to the proxy's config endpoint for sdkKey.
// Additional headers may be supplied (e.g. If-None-Match).
func (h *Harness) GetConfig(t *testing.T, sdkKey string, extraHeaders ...http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		h.URL(h.ConfigPath(sdkKey)), nil)
	if err != nil {
		t.Fatalf("GetConfig: new request: %v", err)
	}
	for _, hdr := range extraHeaders {
		for k, vv := range hdr {
			for _, v := range vv {
				req.Header.Add(k, v)
			}
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GetConfig: do request: %v", err)
	}
	return resp
}

// Close initiates an ordered shutdown, mirroring main.go's <-ctx.Done() branch.
// It is idempotent and safe to call multiple times.
func (h *Harness) Close(ctx context.Context) {
	h.cancel()

	if err := h.srv.Shutdown(ctx); err != nil {
		// best effort — log is discarded in tests
		_ = err
	}
	// Drain the Start goroutine.
	select {
	case <-h.srvErrCh:
	default:
	}

	sseCtx, sseCancel := context.WithTimeout(ctx, 5*time.Second)
	defer sseCancel()
	h.sseManager.Shutdown(sseCtx) //nolint:errcheck

	if h.snapshotWriter != nil && h.snapshotDone != nil {
		select {
		case <-h.snapshotDone:
		case <-ctx.Done():
		}
		snapCtx, snapCancel := context.WithTimeout(ctx, 5*time.Second)
		defer snapCancel()
		h.snapshotWriter.Shutdown(snapCtx)
	}

	select {
	case <-h.flusherDone:
	case <-ctx.Done():
	}
	billingCtx, billingCancel := context.WithTimeout(ctx, 5*time.Second)
	defer billingCancel()
	h.flusher.Shutdown(billingCtx)
}

// initSnapshotWriter loads existing snapshots into flagCache and returns a Writer.
// This mirrors the private initSnapshotWriter in cmd/proxy/main.go.
func initSnapshotWriter(
	ctx context.Context,
	store snapshot.Store,
	flagCache *cache.Cache,
	inst *metrics.Instruments,
	logger *slog.Logger,
) (int, *snapshot.Writer, error) {
	loaded, err := snapshot.LoadAll(ctx, store, flagCache, inst, logger)
	// 0 tells NewWriter to use its built-in default queue size (256).
	return loaded, snapshot.NewWriter(store, inst, logger, 0), err
}
