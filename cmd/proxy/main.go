package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/config"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
	"github.com/dynatrace/sdk-proxy/internal/proxy"
	"github.com/dynatrace/sdk-proxy/internal/server"
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
	var meter otelmetric.Meter = noop.NewMeterProvider().Meter("github.com/dynatrace/sdk-proxy")
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
	_ = inst // components will consume this as they are wired in

	flagCache := cache.New()

	proxyHandler, err := proxy.New(cfg.UpstreamCDNURL, &http.Client{Timeout: 30 * time.Second}, flagCache, logger)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}

	mux := http.NewServeMux()
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

	// The refresher shares the signal context so it stops before the server drains.
	refresher := cache.NewRefresher(flagCache, proxyHandler, logger)
	go refresher.Run(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownErr := srv.Shutdown(context.Background())
		if startErr := <-errCh; startErr != nil {
			return startErr
		}
		return shutdownErr
	}
}
