package sse

import (
	"log/slog"
	"sync"
	"testing"
)

type mockInvalidator struct {
	mu   sync.Mutex
	keys []string
}

func (m *mockInvalidator) Invalidate(keyHash string) {
	m.mu.Lock()
	m.keys = append(m.keys, keyHash)
	m.mu.Unlock()
}

func (m *mockInvalidator) called() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.keys))
	copy(out, m.keys)
	return out
}

func TestLifecycleHandler_KnownEventType_CallsInvalidators(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
	}

	for _, evtType := range []string{EventTypeTokenRotated, EventTypeStageDeleted, EventTypeProjectDeleted} {
		inv.mu.Lock()
		inv.keys = nil
		inv.mu.Unlock()

		evt := Event{
			EventType: evtType,
			Data:      []byte(`{"sdkKeyHashes":["abc0000000000001"]}`),
		}
		got := h.HandleEvent(evt)
		if !got {
			t.Errorf("%s: HandleEvent returned false, want true", evtType)
		}
		calls := inv.called()
		if len(calls) != 1 || calls[0] != "abc0000000000001" {
			t.Errorf("%s: invalidator calls = %v, want [abc0000000000001]", evtType, calls)
		}
	}
}

func TestLifecycleHandler_UnknownEventType_ReturnsFalse(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
	}

	evt := Event{EventType: "refetchEvaluation", Data: []byte(`{}`)}
	if h.HandleEvent(evt) {
		t.Error("HandleEvent returned true for unknown event type, want false")
	}
	if len(inv.called()) != 0 {
		t.Error("invalidator should not be called for unknown event type")
	}
}

func TestLifecycleHandler_MalformedJSON_ReturnsTrue_NoInvalidation(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
	}

	evt := Event{EventType: EventTypeTokenRotated, Data: []byte(`not json`)}
	if !h.HandleEvent(evt) {
		t.Error("HandleEvent returned false for malformed JSON, want true (event consumed)")
	}
	if len(inv.called()) != 0 {
		t.Error("invalidator should not be called for malformed event data")
	}
}

func TestLifecycleHandler_EmptyData_ReturnsTrue_NoInvalidation(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
	}

	evt := Event{EventType: EventTypeStageDeleted, Data: nil}
	if !h.HandleEvent(evt) {
		t.Error("HandleEvent returned false for empty data, want true (event consumed)")
	}
	if len(inv.called()) != 0 {
		t.Error("invalidator should not be called for empty data")
	}
}

func TestLifecycleHandler_SDKKeys_AreHashed(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
	}

	evt := Event{
		EventType: EventTypeTokenRotated,
		Data:      []byte(`{"sdkKeys":["dt_server_us_abc123"]}`),
	}
	if !h.HandleEvent(evt) {
		t.Error("HandleEvent returned false, want true")
	}
	calls := inv.called()
	if len(calls) != 1 {
		t.Fatalf("expected 1 invalidation, got %v", calls)
	}
	// The hash must be 16 lowercase hex chars and deterministic.
	if len(calls[0]) != 16 {
		t.Errorf("key hash length = %d, want 16", len(calls[0]))
	}
}

func TestLifecycleHandler_MultipleKeys_AllInvalidated(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
	}

	evt := Event{
		EventType: EventTypeProjectDeleted,
		Data:      []byte(`{"sdkKeyHashes":["abc0000000000001","abc0000000000002","abc0000000000003"]}`),
	}
	if !h.HandleEvent(evt) {
		t.Error("HandleEvent returned false, want true")
	}
	calls := inv.called()
	if len(calls) != 3 {
		t.Errorf("expected 3 invalidations, got %v", calls)
	}
}

func TestLifecycleHandler_InvalidHashFormat_MalformedResult(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
	}

	// Hash contains uppercase — should be rejected as malformed.
	evt := Event{
		EventType: EventTypeTokenRotated,
		Data:      []byte(`{"sdkKeyHashes":["ABCDEF0000000001"]}`),
	}
	if !h.HandleEvent(evt) {
		t.Error("HandleEvent returned false, want true (event consumed even if malformed)")
	}
	if len(inv.called()) != 0 {
		t.Error("invalidator should not be called for invalid key hash format")
	}
}

func TestLifecycleHandler_NilInvalidatorsFiltered(t *testing.T) {
	inv := &mockInvalidator{}
	h := NewLifecycleHandler(newTestInstruments(t), slog.New(slog.DiscardHandler),
		inv,
		nil, // nil should be silently ignored
	)

	evt := Event{
		EventType: EventTypeTokenRotated,
		Data:      []byte(`{"sdkKeyHashes":["abc0000000000001"]}`),
	}
	h.HandleEvent(evt)
	if len(inv.called()) != 1 {
		t.Errorf("expected 1 invalidation, got %v", inv.called())
	}
}

func TestIsValidKeyHash(t *testing.T) {
	valid := []string{"abc0000000000001", "0000000000000000", "ffffffffffffffff"}
	for _, h := range valid {
		if !isValidKeyHash(h) {
			t.Errorf("isValidKeyHash(%q) = false, want true", h)
		}
	}
	invalid := []string{"", "abc", "ABCDEFGHIJKLMNOP", "gggggggggggggggg", "abc000000000000g", "abc00000000000001"}
	for _, h := range invalid {
		if isValidKeyHash(h) {
			t.Errorf("isValidKeyHash(%q) = true, want false", h)
		}
	}
}
