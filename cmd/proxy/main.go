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

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/config"
	"github.com/dynatrace/sdk-proxy/internal/health"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/proxy"
	"github.com/dynatrace/sdk-proxy/internal/server"
	"github.com/dynatrace/sdk-proxy/internal/snapshot"
	"github.com/dynatrace/sdk-proxy/internal/sse"
	"github.com/dynatrace/sdk-proxy/internal/telemetry"
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

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Set up OTel metrics. Gate on OTEL_EXPORTER_OTLP_ENDPOINT; fall back to no-op
	// so components can record metrics unconditionally without nil checks.
	var meter = noop.NewMeterProvider().Meter("github.com/dynatrace/sdk-proxy")
	if cfg.OTLPEnabled {
		mp, err := metrics.NewProvider(ctx)
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
			AccessKeyID:     cfg.SnapshotS3AccessKey,
			SecretAccessKey: cfg.SnapshotS3SecretKey,
		})
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		snapshotLoaded, snapshotWriter, snapshotLoadErr = initSnapshotWriter(ctx, store, flagCache, inst, logger, cfg.SnapshotWriteQueueSize,
			slog.String("bucket", cfg.SnapshotS3Bucket),
			slog.String("prefix", cfg.SnapshotS3Prefix),
		)
	case "local":
		store, err := snapshot.NewLocalStore(cfg.SnapshotLocalDir)
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		snapshotLoaded, snapshotWriter, snapshotLoadErr = initSnapshotWriter(ctx, store, flagCache, inst, logger, cfg.SnapshotWriteQueueSize,
			slog.String("dir", cfg.SnapshotLocalDir),
		)
	}

	// Classify the startup path and emit the metric.
	// oldest is the zero time when snapshotLoaded==0; Classify guards on loaded==0 before using it.
	oldest, _ := snapshot.OldestCapturedAt(flagCache)
	switch snapshot.Classify(cfg.SnapshotBackend != "", snapshotLoaded, oldest, time.Now(), cfg.SnapshotMaxAge) {
	case snapshot.CaseColdNoPersistence:
		inst.StartupPath.Add(ctx, 1, metric.WithAttributes(attribute.String("path", "cold")))
		logger.Info("startup: no persistence configured, starting cold")
	case snapshot.CaseSnapshotFresh:
		inst.StartupPath.Add(ctx, 1, metric.WithAttributes(attribute.String("path", "snapshot")))
		logger.Info("startup: serving from fresh snapshot", slog.Int("count", snapshotLoaded))
	case snapshot.CaseSnapshotStale:
		inst.StartupPath.Add(ctx, 1, metric.WithAttributes(attribute.String("path", "snapshot_stale")))
		logger.Warn("startup: serving stale snapshot — oldest entry meets or exceeds SNAPSHOT_MAX_AGE",
			slog.Duration("age", time.Since(oldest)),
			slog.Duration("max_age", cfg.SnapshotMaxAge))
	case snapshot.CaseEmptyPersistence:
		inst.StartupPath.Add(ctx, 1, metric.WithAttributes(attribute.String("path", "empty_persistence")))
		if snapshotLoadErr != nil {
			return fmt.Errorf("startup: snapshot backend unreachable and no cached data to serve: %w", snapshotLoadErr)
		}
		return fmt.Errorf("startup: persistence configured but snapshot store is empty — proxy has no data to serve")
	}
	snapshotReady = true

	// SSE manager uses a dedicated client with no overall timeout — SSE connections
	// are long-lived. ResponseHeaderTimeout guards the initial handshake.
	sseClient := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
	sseManager := sse.New(sseClient, inst, logger)

	counter := telemetry.NewCounter()
	logReporter := telemetry.NewLogReporter(logger, cfg.ProxyInstanceID)
	flusher := telemetry.NewFlusher(counter, logReporter, telemetry.NewBoundedRetryBuffer(cfg.BillingRetryBufferSize, inst, logger), inst, cfg.BillingFlushInterval, logger)

	// snapshotWriter is nil when persistence is disabled; proxy.New accepts nil.
	proxyClient := &http.Client{Timeout: 30 * time.Second}
	proxyHandler, err := proxy.New(cfg.CDNBaseDomain, proxyClient, flagCache, sseManager, snapshotWriter, inst, counter, logger)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}

	sseHandler := sse.NewHandler(flagCache, sseManager, inst, counter, logger, 15*time.Second, configHandlerPath)

	// ReadyFunc gates /ready on snapshot load completion.
	probeHandlers := health.New(func() bool { return snapshotReady }, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/live", probeHandlers.Live)
	mux.HandleFunc("/ready", probeHandlers.Ready)
	mux.HandleFunc("/health", probeHandlers.Health)
	mux.Handle(sse.HandlerPath, sseHandler)
	mux.Handle(configHandlerPath, proxyHandler)
	mux.HandleFunc("/", http.NotFound)

	srv, err := server.New(
		fmt.Sprintf(":%d", cfg.Port),
		mux,
		cfg.ShutdownTimeout,
		logger,
	)
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}

	// Wire the refresher with both SSE manager and snapshot writer as observers.
	// NewRefresherWithObservers filters out nil values, so passing snapshotWriter
	// when nil (persistence disabled) is safe.
	refresher := cache.NewRefresherWithObservers(flagCache, proxyHandler, logger, sseManager, snapshotWriter)
	go refresher.Run(ctx)

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

		sseShutCtx, sseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer sseCancel()
		if err := sseManager.Shutdown(sseShutCtx); err != nil {
			slog.Warn("sse: shutdown timed out", slog.String("error", err.Error()))
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
