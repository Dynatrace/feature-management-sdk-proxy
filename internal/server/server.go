package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server wraps an http.Server and exposes Start/Shutdown lifecycle methods.
type Server struct {
	http     *http.Server
	listener net.Listener
	logger   *slog.Logger
	drainFor time.Duration
}

// New creates a Server and opens the TCP listener immediately.
// Callers can call Addr() before Start() to discover the bound port (useful
// when addr uses ":0" for an ephemeral port).
func New(addr string, mux http.Handler, drainTimeout time.Duration, logger *slog.Logger) (*Server, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return &Server{
		http: &http.Server{
			Handler:           mux,
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
