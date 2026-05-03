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

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/config"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/proxy"
	"github.com/dynatrace/sdk-proxy/internal/server"
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
	flusher := telemetry.NewFlusher(counter, logReporter, telemetry.NewNoopRetryBuffer(), inst, cfg.BillingFlushInterval, logger)

	proxyClient := &http.Client{Timeout: 30 * time.Second}
	proxyHandler, err := proxy.New(cfg.CDNBaseDomain, proxyClient, flagCache, sseManager, inst, counter, logger)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}

	sseHandler := sse.NewHandler(flagCache, sseManager, inst, counter, logger, 15*time.Second)

	mux := http.NewServeMux()
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

	// The refresher and flusher share the signal context so they stop before the server drains.
	refresher := cache.NewRefresherWithObserver(flagCache, proxyHandler, sseManager, logger)
	go refresher.Run(ctx)
	go flusher.Run(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		sseShutCtx, sseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer sseCancel()
		if err := sseManager.Shutdown(sseShutCtx); err != nil {
			slog.Warn("sse: shutdown timed out", slog.String("error", err.Error()))
		}
		shutdownErr := srv.Shutdown(context.Background())
		if startErr := <-errCh; startErr != nil {
			return startErr
		}
		return shutdownErr
	}
}
