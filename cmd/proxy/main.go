// Package main is the entrypoint for the sdk-proxy binary.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/launchdarkly/eventsource"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/config"
	"github.com/dynatrace/sdk-proxy/internal/health"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/middleware"
	"github.com/dynatrace/sdk-proxy/internal/proxy"
	"github.com/dynatrace/sdk-proxy/internal/server"
	"github.com/dynatrace/sdk-proxy/internal/snapshot"
	"github.com/dynatrace/sdk-proxy/internal/sse"
	"github.com/dynatrace/sdk-proxy/internal/telemetry"
	"github.com/dynatrace/sdk-proxy/internal/tracing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	stdoutHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	})
	logger := slog.New(stdoutHandler)
	slog.SetDefault(logger)
	slog.Info("starting sdk-proxy", slog.Any("config", cfg))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Install W3C TraceContext propagator globally so incoming traceparent headers
	// are honoured when extracting context in ServeHTTP handlers.
	// Baggage propagation is intentionally omitted: SDK clients are untrusted and
	// arbitrary baggage headers would bloat every span.
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Set up OTel metrics, traces, and logs. Gate all three on OTEL_EXPORTER_OTLP_ENDPOINT;
	// fall back to no-op so components can record telemetry unconditionally without nil checks.
	// Stdout JSON logging is always active; the OTel log bridge is an additive second sink.
	var meter = noop.NewMeterProvider().Meter("github.com/dynatrace/sdk-proxy")
	var tracer = tracenoop.NewTracerProvider().Tracer("github.com/dynatrace/sdk-proxy")
	if cfg.OTLPEnabled {
		// Build the shared OTel resource once; all three signal providers attach it
		// so Dynatrace correlates metrics, traces, and logs under the same service entity.
		res, err := resource.New(ctx,
			resource.WithAttributes(semconv.ServiceName("sdk-proxy")),
			resource.WithFromEnv(),      // honours OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME
			resource.WithTelemetrySDK(), // adds telemetry.sdk.* attributes
		)
		if err != nil {
			return fmt.Errorf("otel: build resource: %w", err)
		}

		mp, err := metrics.NewProvider(ctx, res)
		if err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := mp.Shutdown(shutCtx); err != nil {
				slog.Error("metrics: shutdown failed", slog.String("error", err.Error()))
			}
		}()
		meter = mp.Meter("github.com/dynatrace/sdk-proxy")

		tp, err := tracing.NewProvider(ctx, res)
		if err != nil {
			return fmt.Errorf("tracing: %w", err)
		}
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := tp.Shutdown(shutCtx); err != nil {
				slog.Error("tracing: shutdown failed", slog.String("error", err.Error()))
			}
		}()
		tracer = tp.Tracer("github.com/dynatrace/sdk-proxy")

		lp, err := telemetry.NewLogProvider(ctx, res)
		if err != nil {
			return fmt.Errorf("logs: %w", err)
		}
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := lp.Shutdown(shutCtx); err != nil {
				slog.Error("logs: shutdown failed", slog.String("error", err.Error()))
			}
		}()
		bridge := otelslog.NewHandler("github.com/dynatrace/feature-management-sdk-proxy",
			otelslog.WithLoggerProvider(lp))
		logger = slog.New(telemetry.NewTeeHandler(stdoutHandler, bridge))
		slog.SetDefault(logger)
		logger.Info("telemetry: enabled, exporting metrics, traces, and logs via OTLP/HTTP")
	}

	inst, err := metrics.Register(meter)
	if err != nil {
		return fmt.Errorf("metrics register: %w", err)
	}

	flagCache := cache.New()

	// --- Snapshot persistence (optional) ---
	//
	// Build the store and load snapshots synchronously before starting the HTTP
	// listener. The /ready probe returns true only after startup classification.
	snapshotReady := false
	var snapshotStore snapshot.Store
	var snapshotWriter *snapshot.Writer
	var snapshotLoaded int
	var snapshotLoadErr error

	switch cfg.SnapshotBackend {
	case "s3":
		store, err := snapshot.NewS3Store(ctx, snapshot.S3Config{
			Endpoint:        cfg.SnapshotS3Endpoint,
			Bucket:          cfg.SnapshotS3Bucket,
			Region:          cfg.SnapshotS3Region,
			Prefix:          cfg.SnapshotS3Prefix,
			AccessKeyID:     cfg.SnapshotS3AccessKey.Unwrap(),
			SecretAccessKey: cfg.SnapshotS3SecretKey.Unwrap(),
		})
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		snapshotStore = store
		snapshotLoaded, snapshotWriter, snapshotLoadErr = initSnapshotWriter(ctx, store, flagCache, inst, logger, cfg.SnapshotWriteQueueSize,
			slog.String("bucket", cfg.SnapshotS3Bucket),
			slog.String("prefix", cfg.SnapshotS3Prefix),
		)
	case "local":
		store, err := snapshot.NewLocalStore(cfg.SnapshotLocalDir)
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		snapshotStore = store
		snapshotLoaded, snapshotWriter, snapshotLoadErr = initSnapshotWriter(ctx, store, flagCache, inst, logger, cfg.SnapshotWriteQueueSize,
			slog.String("dir", cfg.SnapshotLocalDir),
		)
	}

	// Classify the startup path and log it.
	// oldest is the zero time when snapshotLoaded==0; Classify guards on loaded==0 before using it.
	oldest, _ := snapshot.OldestCapturedAt(flagCache)
	switch snapshot.Classify(cfg.SnapshotBackend != "", snapshotLoaded, snapshotLoadErr != nil, oldest, time.Now(), cfg.SnapshotMaxAge) {
	case snapshot.CaseColdNoPersistence:
		logger.Info("startup: no persistence configured, starting cold")
	case snapshot.CaseSnapshotFresh:
		logger.Info("startup: serving from fresh snapshot", slog.Int("count", snapshotLoaded))
	case snapshot.CaseSnapshotStale:
		logger.Warn("startup: serving stale snapshot: oldest entry meets or exceeds SNAPSHOT_MAX_AGE",
			slog.Duration("age", time.Since(oldest)),
			slog.Duration("max_age", cfg.SnapshotMaxAge))
	case snapshot.CaseEmptyPersistenceCold:
		logger.Info("startup: persistence configured but store is empty, starting cold; snapshots will be written after first upstream fetch")
	case snapshot.CaseBackendUnreachable:
		return fmt.Errorf("startup: snapshot backend unreachable and no cached data to serve: %w", snapshotLoadErr)
	}
	snapshotReady = true

	// SSE manager uses a dedicated client with no overall timeout — SSE connections
	// are long-lived. ResponseHeaderTimeout guards the initial handshake.
	var sseManager *sse.Manager
	if !cfg.SSEDisabled {
		sseClient := &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		}
		sseManager = sse.New(sseClient, inst, logger)
		sseManager.SetTracer(tracer)

		// Wire lifecycle invalidation: on tokenRotated/stageDeleted/projectDeleted events,
		// drop the cache entry, close SSE fan-out, and (if persistence is configured)
		// delete the persisted snapshot.
		var snapshotInvalidator sse.Invalidator
		if snapshotWriter != nil {
			snapshotInvalidator = sse.InvalidatorFunc(snapshotWriter.OnInvalidated)
		}
		lifecycleHandler := sse.NewLifecycleHandler(inst, logger, cfg.LifecycleKeysCap,
			sse.InvalidatorFunc(flagCache.Delete),
			sse.InvalidatorFunc(sseManager.Close),
			snapshotInvalidator,
		)
		sseManager.SetLifecycleHandler(lifecycleHandler.HandleEvent)
	}

	counter := telemetry.NewCounter()
	logReporter := telemetry.NewLogReporter(logger, cfg.ProxyInstanceID)
	flusher := telemetry.NewFlusher(counter, logReporter, telemetry.NewBoundedRetryBuffer(cfg.BillingRetryBufferSize, inst, logger), inst, cfg.BillingFlushInterval, logger)

	// snapshotWriter is nil when persistence is disabled; proxy.New accepts nil.
	// Both clients wrap the transport with tracing.NewTransport so CDN HTTP calls
	// appear as child spans of proxy.cold_fetch and proxy.refresh_fetch respectively.
	proxyClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: tracing.NewTransport(nil, tracer),
	}

	// Background and demand refresher uses a separate 5s client — intentionally shorter
	// than the minimum poll interval (10s) to prevent overlapping requests.
	refreshClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: tracing.NewTransport(nil, tracer),
	}
	refreshFetcher := proxy.NewFetcher(refreshClient, logger, cfg.UpstreamMaxResponseBytes)

	proxyHandler, err := proxy.New(cfg.CDNBaseDomain, proxyClient, flagCache, sseManager, snapshotWriter, refreshFetcher, inst, counter, logger, tracer)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	proxyHandler.SetMaxResponseBytes(cfg.UpstreamMaxResponseBytes)
	proxyHandler.SetRewriteSSEURLs(!cfg.SSEDisabled)
	proxyHandler.SetEvictor(func(keyID string) {
		if sseManager != nil {
			sseManager.Close(keyID)
		}
		if snapshotWriter != nil {
			snapshotWriter.OnInvalidated(keyID)
		}
	})

	// Pass the eventsource.Server and Manager to the handler. When SSE is disabled,
	// sseManager is nil and NewHandler treats a nil esServer as disabled (501).
	var esServer *eventsource.Server
	var sseTracker sse.SubscriberTracker
	if sseManager != nil {
		esServer = sseManager.Server()
		sseTracker = sseManager
	}
	sseHandler := sse.NewHandler(flagCache, esServer, sseTracker, inst, counter, logger, 15*time.Second, configHandlerPath)
	sseHandler.SetTracer(tracer)

	// ReadyFunc gates /ready on snapshot load completion.
	probeHandlers := health.New(func() bool { return snapshotReady }, logger)

	// Gin engine with recovery middleware (panic → 500 instead of crash).
	// Release mode suppresses Gin's debug-level route registration logs.
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	// Health/probe endpoints — lightweight, no tracing overhead.
	router.GET("/livez", probeHandlers.Live)
	router.GET("/readyz", probeHandlers.Ready)
	router.GET("/healthz", probeHandlers.Health)

	// API route group with W3C trace context propagation middleware.
	// Handlers in this group receive extracted trace context via c.Request.Context().
	api := router.Group("/", middleware.TracePropagation())

	// SSE streaming endpoint — native Gin handler.
	api.GET(sse.HandlerPath, sseHandler.Handle)

	// Proxy config endpoint — accepts GET and HEAD; uses :file param to match
	// the /server/{sdk_key}.json path structure.
	api.GET(configHandlerPath+":file", proxyHandler.Handle)
	api.HEAD(configHandlerPath+":file", proxyHandler.Handle)

	// Catch-all: return 404 for unmatched routes.
	router.NoRoute(func(c *gin.Context) {
		c.String(http.StatusNotFound, "404 page not found\n")
	})

	srv, err := server.New(
		fmt.Sprintf(":%d", cfg.Port),
		router,
		cfg.ShutdownTimeout,
		logger,
	)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}

	// Build the observer slice explicitly to avoid typed-nil interfaces: a
	// (*sse.Manager)(nil) passed as RefreshObserver would satisfy != nil but panic
	// when OnRefreshed is called.
	var refreshObservers []cache.RefreshObserver
	if sseManager != nil {
		refreshObservers = append(refreshObservers, sseManager)
	}
	if snapshotWriter != nil {
		refreshObservers = append(refreshObservers, snapshotWriter)
	}
	refresher := cache.NewRefresherWithObservers(flagCache, refreshFetcher, logger, refreshObservers...)
	if sseManager != nil {
		refresher.SetIntervalProvider(sseManager)
	}
	refresher.SetTracer(tracer)
	refresher.SetInstruments(inst)
	// sseManager and snapshotWriter are fully initialized before this point;
	// the closure captures are stable for the lifetime of the refresher.
	refresher.SetEvictor(func(keyID string) {
		if sseManager != nil {
			sseManager.Close(keyID)
		}
		if snapshotWriter != nil {
			snapshotWriter.OnInvalidated(keyID)
		}
	})
	if snapshotStore != nil {
		refresher.SetRefreshFailureObserver(snapshot.NewFallback(snapshotStore, flagCache, inst, logger))
	}
	go refresher.Run(ctx)

	// Inactivity sweeper: evict cache entries (cache + SSE + snapshot) that have
	// had no HTTP access and no active SSE subscribers for longer than
	// CacheInactivityEvictAfter. Disabled when CacheInactivityEvictAfter == 0.
	var sweeperDone chan struct{}
	if cfg.CacheInactivityEvictAfter > 0 {
		var liveness cache.Liveness = cache.LivenessFunc(func(string) bool { return false })
		if sseManager != nil {
			liveness = sseManager
		}
		evict := func(keyID string) {
			if sseManager != nil {
				sseManager.Close(keyID)
			}
			if snapshotWriter != nil {
				snapshotWriter.OnInvalidated(keyID)
			}
		}
		sweeper := cache.NewSweeper(flagCache, evict, liveness, cfg.CacheInactivityEvictAfter, inst, logger)
		sweeperDone = make(chan struct{})
		go func() {
			defer close(sweeperDone)
			sweeper.Run(ctx)
		}()
	}

	// Start snapshot writer drain loop if persistence is enabled.
	var snapshotWriterDone chan struct{}
	if snapshotWriter != nil {
		snapshotWriterDone = make(chan struct{})
		go func() {
			defer close(snapshotWriterDone)
			snapshotWriter.Run(ctx)
		}()
	}

	// Track flusher goroutine so we can wait for Run to exit before calling
	// Shutdown — the two must not execute concurrently.
	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		flusher.Run(ctx)
	}()

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		// Drain in-flight HTTP requests first — counter.Increment is called
		// during request handling, so the final billing flush must happen after.
		if err := srv.Shutdown(context.Background()); err != nil {
			slog.Warn("server: shutdown error", slog.String("error", err.Error()))
		}
		if startErr := <-errCh; startErr != nil {
			slog.Warn("server: listen error", slog.String("error", startErr.Error()))
		}

		if sseManager != nil {
			sseShutCtx, sseCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer sseCancel()
			if err := sseManager.Shutdown(sseShutCtx); err != nil {
				slog.Warn("sse: shutdown timed out", slog.String("error", err.Error()))
			}
		}

		if sweeperDone != nil {
			<-sweeperDone
		}

		// Drain the snapshot write queue before billing flush so any final
		// cold-fetch snapshot has a chance to land.
		if snapshotWriter != nil {
			<-snapshotWriterDone
			snapShutCtx, snapCancel := context.WithTimeout(context.Background(), snapshotShutdownGrace)
			defer snapCancel()
			snapshotWriter.Shutdown(snapShutCtx)
		}

		// Wait for the Run loop to exit before calling Shutdown to avoid
		// concurrent access to Flusher internals.
		<-flusherDone
		billingShutCtx, billingCancel := context.WithTimeout(context.Background(), billingShutdownGrace)
		defer billingCancel()
		flusher.Shutdown(billingShutCtx)

		return nil
	}
}

// configHandlerPath mirrors the upstream CDN path structure (cdn…/server/<key>.json).
const configHandlerPath = "/server/"

const billingShutdownGrace = 10 * time.Second
const snapshotShutdownGrace = 5 * time.Second

// initSnapshotWriter loads existing snapshots from store into flagCache, logs
// errors, and returns the loaded count, a Writer ready to accept async writes,
// and any error from LoadAll. The caller uses the error to distinguish a backend
// failure from a genuinely empty store. Startup-path classification and metric
// emission are handled by the caller.
func initSnapshotWriter(
	ctx context.Context,
	store snapshot.Store,
	flagCache *cache.Cache,
	inst *metrics.Instruments,
	logger *slog.Logger,
	queueSize int,
	attrs ...slog.Attr,
) (int, *snapshot.Writer, error) {
	loaded, err := snapshot.LoadAll(ctx, store, flagCache, inst, logger)

	if err != nil {
		extra := make([]any, len(attrs))
		for i, a := range attrs {
			extra[i] = a
		}
		logger.Warn("snapshot: startup load failed",
			append([]any{slog.String("error", err.Error())}, extra...)...)
	}

	return loaded, snapshot.NewWriter(store, inst, logger, queueSize), err
}
