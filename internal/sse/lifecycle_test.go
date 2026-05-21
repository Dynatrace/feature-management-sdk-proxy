package sse

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"testing"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
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
		keysCap:      defaultLifecycleKeysCap,
	}

	for _, evtType := range []string{EventTypeTokenRotated, EventTypeStageDeleted, EventTypeProjectDeleted} {
		inv.mu.Lock()
		inv.keys = nil
		inv.mu.Unlock()

		evt := Event{
			EventType: evtType,
			Data:      []byte(`{"sdkKeyHashes":["dt01.server_us_ab3xy9kp2q"]}`),
		}
		got := h.HandleEvent(evt)
		if !got {
			t.Errorf("%s: HandleEvent returned false, want true", evtType)
		}
		calls := inv.called()
		if len(calls) != 1 || calls[0] != "dt01.server_us_ab3xy9kp2q" {
			t.Errorf("%s: invalidator calls = %v, want [dt01.server_us_ab3xy9kp2q]", evtType, calls)
		}
	}
}

func TestLifecycleHandler_UnknownEventType_ReturnsFalse(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
		keysCap:      defaultLifecycleKeysCap,
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
		keysCap:      defaultLifecycleKeysCap,
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
		keysCap:      defaultLifecycleKeysCap,
	}

	evt := Event{EventType: EventTypeStageDeleted, Data: nil}
	if !h.HandleEvent(evt) {
		t.Error("HandleEvent returned false for empty data, want true (event consumed)")
	}
	if len(inv.called()) != 0 {
		t.Error("invalidator should not be called for empty data")
	}
}

func TestLifecycleHandler_SDKKeys_PublicIDExtracted(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
		keysCap:      defaultLifecycleKeysCap,
	}

	const fullKey = "dt01.server_us_ab3xy9kp2q.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	evt := Event{
		EventType: EventTypeTokenRotated,
		Data:      []byte(`{"sdkKeys":["` + fullKey + `"]}`),
	}
	if !h.HandleEvent(evt) {
		t.Error("HandleEvent returned false, want true")
	}
	calls := inv.called()
	if len(calls) != 1 {
		t.Fatalf("expected 1 invalidation, got %v", calls)
	}
	// The public ID must be valid and the private segment must be stripped.
	want := cache.PublicID(fullKey)
	if calls[0] != want || !cache.IsValidID(calls[0]) {
		t.Errorf("public ID = %q, want %q (valid: %v)", calls[0], want, cache.IsValidID(calls[0]))
	}
}

func TestLifecycleHandler_MultipleKeys_AllInvalidated(t *testing.T) {
	inv := &mockInvalidator{}
	h := &LifecycleHandler{
		invalidators: []Invalidator{inv},
		inst:         newTestInstruments(t),
		logger:       slog.New(slog.DiscardHandler),
		keysCap:      defaultLifecycleKeysCap,
	}

	evt := Event{
		EventType: EventTypeProjectDeleted,
		Data:      []byte(`{"sdkKeyHashes":["dt01.server_us_ab3xy9kp2q","dt01.server_eu_0000000000","dt01.server_ap_xxxxxxxxxx"]}`),
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
		keysCap:      defaultLifecycleKeysCap,
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
	h := NewLifecycleHandler(newTestInstruments(t), slog.New(slog.DiscardHandler), defaultLifecycleKeysCap,
		inv,
		nil, // nil should be silently ignored
	)

	evt := Event{
		EventType: EventTypeTokenRotated,
		Data:      []byte(`{"sdkKeyHashes":["dt01.server_us_ab3xy9kp2q"]}`),
	}
	h.HandleEvent(evt)
	if len(inv.called()) != 1 {
		t.Errorf("expected 1 invalidation, got %v", inv.called())
	}
}

// makeLifecycleData builds lifecycle event JSON with numHashes sdkKeyHashes
// entries (valid public IDs) and numKeys sdkKeys entries (valid full keys).
func makeLifecycleData(t *testing.T, numHashes, numKeys int) []byte {
	t.Helper()
	type payload struct {
		SDKKeyHashes []string `json:"sdkKeyHashes,omitempty"`
		SDKKeys      []string `json:"sdkKeys,omitempty"`
	}
	p := payload{}
	for i := range numHashes {
		p.SDKKeyHashes = append(p.SDKKeyHashes, fmt.Sprintf("dt01.server_us_%010d", i))
	}
	for i := range numKeys {
		p.SDKKeys = append(p.SDKKeys, fmt.Sprintf("dt01.server_eu_%010d.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a", i))
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("makeLifecycleData: marshal: %v", err)
	}
	return data
}

func TestLifecycleHandler_KeyArrayCap(t *testing.T) {
	const testCap = 5 // small cap keeps the test fast regardless of defaultLifecycleKeysCap changes

	tests := []struct {
		name       string
		numHashes  int
		numKeys    int
		wantOK     bool // true = event consumed without rejection
		wantCalled bool // true = invalidators should have been called
	}{
		{
			name:       "exactly at cap is accepted",
			numHashes:  testCap,
			numKeys:    0,
			wantOK:     true,
			wantCalled: true,
		},
		{
			name:       "one over cap is rejected",
			numHashes:  testCap + 1,
			numKeys:    0,
			wantOK:     true, // HandleEvent returns true (consumed) even for malformed
			wantCalled: false,
		},
		{
			name:       "combined total over cap is rejected",
			numHashes:  testCap/2 + 1,
			numKeys:    testCap/2 + 1,
			wantOK:     true,
			wantCalled: false,
		},
		{
			name:       "sdkKeys only over cap is rejected",
			numHashes:  0,
			numKeys:    testCap + 1,
			wantOK:     true,
			wantCalled: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := &mockInvalidator{}
			h := &LifecycleHandler{
				invalidators: []Invalidator{inv},
				inst:         newTestInstruments(t),
				logger:       slog.New(slog.DiscardHandler),
				keysCap:      testCap,
			}
			evt := Event{
				EventType: EventTypeTokenRotated,
				Data:      makeLifecycleData(t, tc.numHashes, tc.numKeys),
			}
			got := h.HandleEvent(evt)
			if got != tc.wantOK {
				t.Errorf("HandleEvent returned %v, want %v", got, tc.wantOK)
			}
			calls := inv.called()
			if tc.wantCalled && len(calls) == 0 {
				t.Error("expected invalidator to be called, was not")
			}
			if !tc.wantCalled && len(calls) != 0 {
				t.Errorf("expected no invalidator calls, got %d", len(calls))
			}
		})
	}
}

func TestIsValidID(t *testing.T) {
	valid := []string{
		"dt01.server_us_ab3xy9kp2q",
		"dt02.mobile_eu_0000000000",
		"dt99.web_ap_xXxXxXxXxX",
	}
	for _, h := range valid {
		if !cache.IsValidID(h) {
			t.Errorf("cache.IsValidID(%q) = false, want true", h)
		}
	}
	invalid := []string{
		"",
		"abc",
		"abc0000000000001", // old-format hash
		"ABCDEFGHIJKLMNOP",
		"dt01.invalid_us_ab3xy9kp2q",
		"dt01.server_US_ab3xy9kp2q",
	}
	for _, h := range invalid {
		if cache.IsValidID(h) {
			t.Errorf("cache.IsValidID(%q) = true, want false", h)
		}
	}
}
