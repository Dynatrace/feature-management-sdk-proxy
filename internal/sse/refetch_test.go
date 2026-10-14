package sse

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
)

// gatedRefetcher records RefreshKey calls and blocks each until released.
type gatedRefetcher struct {
	mu      sync.Mutex
	hints   []cache.RefetchHint
	gate    chan struct{}
	outcome cache.RefreshOutcome
}

func newGatedRefetcher(outcome cache.RefreshOutcome) *gatedRefetcher {
	return &gatedRefetcher{gate: make(chan struct{}, 100), outcome: outcome}
}

func (g *gatedRefetcher) RefreshKey(_ context.Context, _ string, hint cache.RefetchHint) cache.RefreshOutcome {
	g.mu.Lock()
	g.hints = append(g.hints, hint)
	g.mu.Unlock()
	<-g.gate
	return g.outcome
}

func (g *gatedRefetcher) calls() []cache.RefetchHint {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]cache.RefetchHint(nil), g.hints...)
}

func (g *gatedRefetcher) release(n int) {
	for range n {
		g.gate <- struct{}{}
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func upstreamRefetch(etag string) Event {
	return Event{EventType: "message", Data: []byte(`{"type":"refetchConfig","etag":"` + etag + `"}`)}
}

func TestManager_UpstreamRefetch_RefreshesBeforeForwarding(t *testing.T) {
	m := newTestManager(t, nil)
	r := newGatedRefetcher(cache.RefreshUpdated)
	m.SetRefetcher(r)
	keyID, stop := subscribe(t, m)

	m.onUpstreamEvent(keyID, upstreamRefetch("v2"), trace.SpanContext{})
	waitFor(t, func() bool { return len(r.calls()) == 1 }, "RefreshKey not called")
	if got := r.calls()[0]; got.ETag != "v2" {
		t.Errorf("hint etag = %q, want v2", got.ETag)
	}
	r.release(1)

	if n := strings.Count(stop(), `"type":"refetchConfig"`); n != 1 {
		t.Errorf("forwarded events = %d, want 1", n)
	}
}

func TestManager_UpstreamRefetch_NotForwardedWhileRefreshing(t *testing.T) {
	m := newTestManager(t, nil)
	r := newGatedRefetcher(cache.RefreshUpdated)
	m.SetRefetcher(r)
	keyID, stop := subscribe(t, m)

	m.onUpstreamEvent(keyID, upstreamRefetch("v2"), trace.SpanContext{})
	waitFor(t, func() bool { return len(r.calls()) == 1 }, "RefreshKey not called")

	// Disconnect before releasing: the subscriber must have seen nothing.
	body := stop()
	r.release(1)
	if strings.Contains(body, "refetchConfig") {
		t.Errorf("event forwarded before the cache refresh finished: %q", body)
	}
}

func TestManager_UpstreamRefetch_CoalescesBurst(t *testing.T) {
	m := newTestManager(t, nil)
	r := newGatedRefetcher(cache.RefreshUpdated)
	m.SetRefetcher(r)
	keyID, stop := subscribe(t, m)

	m.onUpstreamEvent(keyID, upstreamRefetch("v1"), trace.SpanContext{})
	waitFor(t, func() bool { return len(r.calls()) == 1 }, "first RefreshKey not called")
	for _, etag := range []string{"v2", "v3", "v4", "v5"} {
		m.onUpstreamEvent(keyID, upstreamRefetch(etag), trace.SpanContext{})
	}
	r.release(1)
	waitFor(t, func() bool { return len(r.calls()) == 2 }, "follow-up RefreshKey not called")
	r.release(1)
	body := stop()

	calls := r.calls()
	if len(calls) != 2 || calls[1].ETag != "v5" {
		t.Errorf("RefreshKey hints = %+v, want 2 calls ending with the latest (v5)", calls)
	}
	if n := strings.Count(body, `"type":"refetchConfig"`); n != 2 {
		t.Errorf("forwarded events = %d, want 2 (first + coalesced latest)", n)
	}
	if !strings.Contains(body, `"etag":"v5"`) || strings.Contains(body, `"etag":"v3"`) {
		t.Errorf("expected v1 and v5 only: %q", body)
	}
}

func TestManager_UpstreamRefetch_EvictedNotForwarded(t *testing.T) {
	m := newTestManager(t, nil)
	r := newGatedRefetcher(cache.RefreshEvicted)
	m.SetRefetcher(r)
	keyID, stop := subscribe(t, m)

	m.onUpstreamEvent(keyID, upstreamRefetch("v2"), trace.SpanContext{})
	r.release(1)
	waitFor(t, func() bool { return len(r.calls()) == 1 }, "RefreshKey not called")

	if body := stop(); strings.Contains(body, "refetchConfig") {
		t.Errorf("event for an evicted key must not be forwarded: %q", body)
	}
}

func TestManager_UpstreamRefetch_FailureStillForwarded(t *testing.T) {
	m := newTestManager(t, nil)
	r := newGatedRefetcher(cache.RefreshFailed)
	m.SetRefetcher(r)
	keyID, stop := subscribe(t, m)

	m.onUpstreamEvent(keyID, upstreamRefetch("v2"), trace.SpanContext{})
	r.release(1)

	if body := stop(); !strings.Contains(body, "refetchConfig") {
		t.Errorf("event should still be forwarded when the refresh fails: %q", body)
	}
}

func TestManager_OtherEvents_ForwardedWithoutRefresh(t *testing.T) {
	m := newTestManager(t, nil)
	r := newGatedRefetcher(cache.RefreshUpdated)
	m.SetRefetcher(r)
	keyID, stop := subscribe(t, m)

	m.onUpstreamEvent(keyID, Event{EventType: "message", Data: []byte(`{"type":"somethingNew"}`)}, trace.SpanContext{})

	if body := stop(); !strings.Contains(body, "somethingNew") {
		t.Errorf("unknown event types must still be forwarded: %q", body)
	}
	if calls := r.calls(); len(calls) != 0 {
		t.Errorf("RefreshKey called %d times for a non-refetch event", len(calls))
	}
}

func TestManager_Shutdown_WaitsForRefetch(t *testing.T) {
	m := newTestManager(t, nil)
	r := newGatedRefetcher(cache.RefreshUpdated)
	m.SetRefetcher(r)

	m.onUpstreamEvent("dt01.server_us_ab3xy9kp2q", upstreamRefetch("v2"), trace.SpanContext{})
	waitFor(t, func() bool { return len(r.calls()) == 1 }, "RefreshKey not called")

	done := make(chan error, 1)
	go func() { done <- m.Shutdown(context.Background()) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a refetch was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	r.release(1)
	if err := <-done; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

func TestParseRefetch(t *testing.T) {
	cases := []struct {
		name   string
		evt    Event
		wantOK bool
		want   cache.RefetchHint
	}{
		{"flat refetchConfig", Event{EventType: "message", Data: []byte(`{"type":"refetchConfig","etag":"\"abc\"","lastModified":1771622898000}`)},
			true, cache.RefetchHint{ETag: `"abc"`}},
		{"Ably envelope", Event{EventType: "message", Data: []byte(`{"id":"x","name":"refetchConfig","data":"{\"type\":\"refetchConfig\",\"etag\":\"\\\"abc\\\"\",\"lastModified\":1771622898000}"}`)},
			true, cache.RefetchHint{ETag: `"abc"`}},
		{"refetchEvaluation", Event{EventType: "message", Data: []byte(`{"type":"refetchEvaluation"}`)}, true, cache.RefetchHint{}},
		{"named event, untyped data", Event{EventType: "refetchConfig", Data: []byte(`{"flags":"all"}`)}, true, cache.RefetchHint{}},
		{"other type", Event{EventType: "message", Data: []byte(`{"type":"other"}`)}, false, cache.RefetchHint{}},
		{"not JSON", Event{EventType: "message", Data: []byte(`hello`)}, false, cache.RefetchHint{}},
	}
	for _, tc := range cases {
		got, ok := parseRefetch(tc.evt)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("%s: parseRefetch = %+v, %v; want %+v, %v", tc.name, got, ok, tc.want, tc.wantOK)
		}
	}
}
