package sse

import (
	"context"
	"encoding/json"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/sdk-proxy/internal/cache"
	"github.com/dynatrace/sdk-proxy/internal/metrics"
)

// Lifecycle event type strings. These are placeholders until the upstream
// event contract is finalized (sdk-proxy-8r3). Centralized here so they
// can be updated in one edit.
const (
	EventTypeTokenRotated   = "tokenRotated"
	EventTypeStageDeleted   = "stageDeleted"
	EventTypeProjectDeleted = "projectDeleted"
)

// defaultLifecycleKeysCap is the default value for LifecycleHandler.keysCap.
// Configurable via LIFECYCLE_KEYS_CAP; see internal/config.
const defaultLifecycleKeysCap = 10000

// Invalidator is called for each key hash affected by a lifecycle event.
type Invalidator interface {
	Invalidate(keyHash string)
}

// InvalidatorFunc adapts a plain function to the Invalidator interface.
type InvalidatorFunc func(keyHash string)

// Invalidate calls f with keyHash.
func (f InvalidatorFunc) Invalidate(keyHash string) { f(keyHash) }

// lifecycleEventData is the narrow JSON parse for lifecycle event data fields.
// Either sdkKeyHashes (pre-hashed) or sdkKeys (raw, will be hashed) may be set.
type lifecycleEventData struct {
	SDKKeyHashes []string `json:"sdkKeyHashes,omitempty"`
	SDKKeys      []string `json:"sdkKeys,omitempty"`
}

// LifecycleHandler intercepts upstream SSE lifecycle events and calls Invalidate
// on each registered Invalidator for every affected key hash.
//
// Wire HandleEvent as the Manager's lifecycle function via SetLifecycleHandler.
type LifecycleHandler struct {
	invalidators []Invalidator
	inst         *metrics.Instruments
	logger       *slog.Logger
	keysCap      int
}

// NewLifecycleHandler constructs a LifecycleHandler. keysCap is the maximum
// combined number of sdkKeyHashes + sdkKeys entries accepted in a single event;
// use config.LifecycleKeysCap (default 10000). Nil invalidators are filtered.
func NewLifecycleHandler(inst *metrics.Instruments, logger *slog.Logger, keysCap int, invalidators ...Invalidator) *LifecycleHandler {
	var filtered []Invalidator
	for _, inv := range invalidators {
		if inv != nil {
			filtered = append(filtered, inv)
		}
	}
	return &LifecycleHandler{
		invalidators: filtered,
		inst:         inst,
		logger:       logger,
		keysCap:      keysCap,
	}
}

// HandleEvent inspects an upstream SSE event. If it is a known lifecycle
// event type it parses the affected key hashes and calls Invalidate on each
// registered Invalidator. Returns true when the event was consumed (must not
// be forwarded to SDK subscribers), false otherwise.
func (h *LifecycleHandler) HandleEvent(evt Event) bool {
	switch evt.EventType {
	case EventTypeTokenRotated, EventTypeStageDeleted, EventTypeProjectDeleted:
	default:
		return false
	}

	eventType := evt.EventType
	keyHashes, ok := h.parseKeyHashes(evt)
	if !ok {
		h.inst.LifecycleEventsTotal.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("event_type", eventType),
			attribute.String("result", "malformed"),
		))
		return true
	}

	h.inst.LifecycleEventsTotal.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("event_type", eventType),
		attribute.String("result", "handled"),
	))

	for _, keyHash := range keyHashes {
		h.logger.Info("sse: lifecycle invalidation",
			slog.String("event_type", eventType),
			slog.String("key_hash", keyHash),
		)
		for _, inv := range h.invalidators {
			inv.Invalidate(keyHash)
		}
		h.inst.LifecycleInvalidatedKeys.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("event_type", eventType),
		))
	}

	return true
}

// parseKeyHashes extracts and validates key hashes from a lifecycle event's
// data field. Accepts sdkKeyHashes (pre-hashed) or sdkKeys (raw, will be
// hashed). Returns nil,false on malformed input.
func (h *LifecycleHandler) parseKeyHashes(evt Event) ([]string, bool) {
	if len(evt.Data) == 0 {
		h.logger.Warn("sse: lifecycle event with empty data field",
			slog.String("event_type", evt.EventType),
		)
		return nil, false
	}

	var d lifecycleEventData
	if err := json.Unmarshal(evt.Data, &d); err != nil {
		h.logger.Warn("sse: malformed lifecycle event data",
			slog.String("event_type", evt.EventType),
			slog.String("error", err.Error()),
		)
		return nil, false
	}

	if len(d.SDKKeyHashes) == 0 && len(d.SDKKeys) == 0 {
		h.logger.Warn("sse: lifecycle event data has no key identifiers",
			slog.String("event_type", evt.EventType),
		)
		return nil, false
	}

	if total := len(d.SDKKeyHashes) + len(d.SDKKeys); total > h.keysCap {
		h.logger.Warn("sse: lifecycle event exceeds key array cap, rejecting",
			slog.String("event_type", evt.EventType),
			slog.Int("key_hashes_len", len(d.SDKKeyHashes)),
			slog.Int("keys_len", len(d.SDKKeys)),
			slog.Int("total", total),
			slog.Int("cap", h.keysCap),
		)
		return nil, false
	}

	var hashes []string

	for _, keyHash := range d.SDKKeyHashes {
		if !isValidKeyHash(keyHash) {
			h.logger.Warn("sse: skipping invalid sdkKeyHash in lifecycle event",
				slog.String("event_type", evt.EventType),
			)
			continue
		}
		hashes = append(hashes, keyHash)
	}

	for _, key := range d.SDKKeys {
		hashes = append(hashes, cache.Key(key))
	}

	return hashes, true
}

// isValidKeyHash returns true for exactly 16 lowercase hex chars.
func isValidKeyHash(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
