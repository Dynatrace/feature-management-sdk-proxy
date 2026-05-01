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

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/config"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

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
