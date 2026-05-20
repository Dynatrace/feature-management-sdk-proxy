package telemetry

import (
	"context"
	"errors"
	"log/slog"
)

// TeeHandler fans slog records out to multiple slog.Handler implementations.
// It is used to write structured JSON to stdout while also forwarding to
// the OTel log bridge, so operational tooling (kubectl logs, docker logs)
// continues to work when OTLP export is enabled.
type TeeHandler struct {
	handlers []slog.Handler
}

// NewTeeHandler returns a TeeHandler that delegates to each of the provided handlers.
func NewTeeHandler(h ...slog.Handler) *TeeHandler {
	return &TeeHandler{handlers: h}
}

// Enabled returns true when any child handler is enabled for the given level.
func (t *TeeHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	for _, h := range t.handlers {
		if h.Enabled(ctx, lvl) {
			return true
		}
	}
	return false
}

// Handle delivers the record to every enabled child handler.
// The record is cloned before each delivery to prevent mutation races.
// All handler errors are joined so no failure is silently discarded.
func (t *TeeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range t.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// WithAttrs returns a new TeeHandler with each child wrapped via WithAttrs.
func (t *TeeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(t.handlers))
	for i, h := range t.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &TeeHandler{handlers: next}
}

// WithGroup returns a new TeeHandler with each child wrapped via WithGroup.
func (t *TeeHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(t.handlers))
	for i, h := range t.handlers {
		next[i] = h.WithGroup(name)
	}
	return &TeeHandler{handlers: next}
}
