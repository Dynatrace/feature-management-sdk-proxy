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
	// listener. The /ready probe returns true only after this completes (or when
	// persistence is disabled).
	snapshotReady := false
	var snapshotWriter *snapshot.Writer

	if cfg.SnapshotBackend == "s3" {
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

		loaded, err := snapshot.LoadAll(ctx, store, flagCache, inst, logger)
		if err != nil {
			// Non-fatal: log and continue with a cold start.
			slog.Warn("snapshot: startup load failed; starting cold",
				slog.String("error", err.Error()),
				slog.String("bucket", cfg.SnapshotS3Bucket),
				slog.String("prefix", cfg.SnapshotS3Prefix),
			)
		} else {
			slog.Info("snapshot: loaded entries",
				slog.Int("count", loaded),
				slog.String("bucket", cfg.SnapshotS3Bucket),
				slog.String("prefix", cfg.SnapshotS3Prefix),
			)
		}

		startupPath := "cold"
		if loaded > 0 {
			startupPath = "snapshot"
		}
		inst.StartupPath.Add(ctx, 1, metric.WithAttributes(attribute.String("path", startupPath)))

		snapshotWriter = snapshot.NewWriter(store, inst, logger, cfg.SnapshotWriteQueueSize)
	} else {
		inst.StartupPath.Add(ctx, 1, metric.WithAttributes(attribute.String("path", "cold")))
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

	sseHandler := sse.NewHandler(flagCache, sseManager, inst, counter, logger, 15*time.Second)

	// ReadyFunc gates /ready on snapshot load completion.
	probeHandlers := health.New(func() bool { return snapshotReady }, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/live", probeHandlers.Live)
	mux.HandleFunc("/ready", probeHandlers.Ready)
	mux.HandleFunc("/health", probeHandlers.Health)
	mux.Handle(sse.HandlerPath, sseHandler)
	mux.Handle("/", proxyHandler)

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

const billingShutdownGrace = 10 * time.Second
const snapshotShutdownGrace = 5 * time.Second
