package sse

import (
	"context"
	"encoding/json"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
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

// Invalidator is called for each key ID affected by a lifecycle event.
type Invalidator interface {
	Invalidate(keyID string)
}

// InvalidatorFunc adapts a plain function to the Invalidator interface.
type InvalidatorFunc func(keyID string)

// Invalidate calls f with keyID.
func (f InvalidatorFunc) Invalidate(keyID string) { f(keyID) }

// lifecycleEventData is the narrow JSON parse for lifecycle event data fields.
// Either sdkKeyHashes (public key identifiers) or sdkKeys (raw full keys) may be set.
// The JSON field name "sdkKeyHashes" is kept for wire compatibility with the upstream service.
type lifecycleEventData struct {
	SDKKeyIDs []string `json:"sdkKeyHashes,omitempty"` // public key identifiers; kept for wire compat
	SDKKeys   []string `json:"sdkKeys,omitempty"`
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
// combined number of sdkKeyIDs + sdkKeys entries accepted in a single event;
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
	keyIDs, ok := h.parseKeyIDs(evt)
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

	for _, keyID := range keyIDs {
		h.logger.Info("sse: lifecycle invalidation",
			slog.String("event_type", eventType),
			slog.String("sdk_key_public_id", keyID),
		)
		for _, inv := range h.invalidators {
			inv.Invalidate(keyID)
		}
		h.inst.LifecycleInvalidatedKeys.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("event_type", eventType),
		))
	}

	return true
}

// parseKeyIDs extracts and validates key identifiers from a lifecycle event's
// data field. Accepts sdkKeyHashes (public key IDs) or sdkKeys (raw full keys;
// public prefix is extracted). Returns nil,false on malformed input.
func (h *LifecycleHandler) parseKeyIDs(evt Event) ([]string, bool) {
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

	if len(d.SDKKeyIDs) == 0 && len(d.SDKKeys) == 0 {
		h.logger.Warn("sse: lifecycle event data has no key identifiers",
			slog.String("event_type", evt.EventType),
		)
		return nil, false
	}

	if total := len(d.SDKKeyIDs) + len(d.SDKKeys); total > h.keysCap {
		h.logger.Warn("sse: lifecycle event exceeds key array cap, rejecting",
			slog.String("event_type", evt.EventType),
			slog.Int("key_ids_len", len(d.SDKKeyIDs)),
			slog.Int("keys_len", len(d.SDKKeys)),
			slog.Int("total", total),
			slog.Int("cap", h.keysCap),
		)
		return nil, false
	}

	var ids []string

	for _, keyID := range d.SDKKeyIDs {
		if !cache.IsValidID(keyID) {
			h.logger.Warn("sse: skipping invalid sdkKeyHash in lifecycle event",
				slog.String("event_type", evt.EventType),
			)
			continue
		}
		ids = append(ids, keyID)
	}

	for _, key := range d.SDKKeys {
		id := cache.PublicID(key)
		if !cache.IsValidID(id) {
			h.logger.Warn("sse: skipping sdkKey with invalid public prefix in lifecycle event",
				slog.String("event_type", evt.EventType),
			)
			continue
		}
		ids = append(ids, id)
	}

	return ids, true
}
