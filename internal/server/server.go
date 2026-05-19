package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Server wraps a Gin engine with lifecycle management: TCP listener binding,
// graceful shutdown with drain timeout, and ReadHeaderTimeout protection.
type Server struct {
	http     *http.Server
	listener net.Listener
	logger   *slog.Logger
	drainFor time.Duration
}

// New creates a Server bound to addr using the provided Gin engine.
// The TCP listener is opened immediately; callers can call Addr() before
// Start() to discover the bound port (useful when addr is ":0" for ephemeral).
//
// ReadHeaderTimeout is set to 10s to mitigate slowloris attacks.
func New(addr string, engine *gin.Engine, drainTimeout time.Duration, logger *slog.Logger) (*Server, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return &Server{
		http: &http.Server{
			Handler:           engine,
			ReadHeaderTimeout: 10 * time.Second,
		},
		listener: l,
		logger:   logger,
		drainFor: drainTimeout,
	}, nil
}

// Start begins serving and blocks until the server stops.
// It returns nil on a clean shutdown.
func (s *Server) Start() error {
	s.logger.Info("server listening", slog.String("addr", s.listener.Addr().String()))
	if err := s.http.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// Shutdown drains in-flight requests up to the configured drain timeout.
func (s *Server) Shutdown(ctx context.Context) error {
	drainCtx, cancel := context.WithTimeout(ctx, s.drainFor)
	defer cancel()

	s.logger.Info("server draining", slog.Duration("timeout", s.drainFor))
	if err := s.http.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	s.logger.Info("server stopped")
	return nil
}

// Addr returns the address the server is listening on.
// Available immediately after New returns.
func (s *Server) Addr() string {
	return s.listener.Addr().String()
}
