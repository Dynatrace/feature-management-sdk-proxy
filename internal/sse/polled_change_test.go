package sse

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
)

// subscribe connects one downstream SDK client to m for testSDKKey and
// returns a func that disconnects it and returns everything it received.
func subscribe(t *testing.T, m *Manager) (keyID string, stop func() string) {
	t.Helper()
	keyID = sdkkey.PublicID(testSDKKey)
	c := cache.New()
	c.Set(keyID, cache.Entry{Payload: []byte(`{}`)})
	h := NewHandler(c, m.Server(), m, newTestInstruments(t), nil, slog.Default(), time.Minute, "/server/")

	ctx, cancel := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sse?stream_id="+keyID, nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Handle(ginContext(rr, req))
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !m.HasSubscribers(keyID) {
		if time.Now().After(deadline) {
			t.Fatal("subscriber never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the eventsource server attach the channel

	return keyID, func() string {
		time.Sleep(100 * time.Millisecond) // allow delivery
		cancel()
		<-done
		return rr.Body.String()
	}
}

func setUpstreamState(m *Manager, keyID string, streaming bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := &keyState{sseURL: "https://upstream.test/sse"}
	state.streaming.Store(streaming)
	m.conns[keyID] = state
}

func polledEntry() cache.Entry {
	return cache.Entry{
		Payload:      []byte(`{"flags":{"f":{"state":"ENABLED"}}}`),
		ETag:         `"v2"`,
		LastModified: time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC),
	}
}

// refetchData extracts the data payloads of the change notifications in an SSE body.
func refetchData(t *testing.T, body string) []changeNotification {
	t.Helper()
	var out []changeNotification
	for line := range strings.SplitSeq(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var d changeNotification
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			t.Fatalf("event data %q: %v", data, err)
		}
		if d.Type == EventTypeRefetchConfig || d.Type == EventTypeRefetchEvaluation {
			out = append(out, d)
		}
	}
	return out
}

func TestManager_PolledChange_NotifiesWhenUpstreamNotStreaming(t *testing.T) {
	cases := map[string]func(m *Manager, keyID string){
		"no upstream connection": func(*Manager, string) {},
		"upstream reconnecting":  func(m *Manager, keyID string) { setUpstreamState(m, keyID, false) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			m := newTestManager(t, nil)
			keyID, stop := subscribe(t, m)
			setup(m, keyID)

			m.OnRefreshed(keyID, polledEntry())

			body := stop()
			got := refetchData(t, body)
			if len(got) != 1 {
				t.Fatalf("want 1 change notification, got %d in %q", len(got), body)
			}
			want := changeNotification{Type: EventTypeRefetchConfig, ETag: `"v2"`, LastModified: polledEntry().LastModified.Unix()}
			if got[0] != want {
				t.Errorf("event data = %+v, want %+v", got[0], want)
			}
			if !strings.Contains(body, "event: message") {
				t.Errorf("synthesized event should be framed like forwarded ones (event: message): %q", body)
			}
		})
	}
}

func TestManager_PolledChange_SilentWhileUpstreamStreaming(t *testing.T) {
	m := newTestManager(t, nil)
	keyID, stop := subscribe(t, m)
	setUpstreamState(m, keyID, true)

	m.OnRefreshed(keyID, polledEntry())

	if got := refetchData(t, stop()); len(got) != 0 {
		t.Errorf("upstream is streaming; want no synthesized event, got %+v", got)
	}
}

func TestManager_PolledChange_OnlyNotifiesOwnKey(t *testing.T) {
	m := newTestManager(t, nil)
	_, stop := subscribe(t, m)

	m.OnRefreshed("dt01.server_eu_0000000001", polledEntry())

	if got := refetchData(t, stop()); len(got) != 0 {
		t.Errorf("change for another key must not reach this subscriber, got %+v", got)
	}
}

func TestRefetchEvent_OmitsMissingFields(t *testing.T) {
	evt := refetchEvent(sdkkey.PublicID(testSDKKey), cache.Entry{})
	if string(evt.Data) != `{"type":"refetchConfig"}` {
		t.Errorf("data = %s", evt.Data)
	}
}

func TestRefetchEvent_TypeByPlatform(t *testing.T) {
	for keyID, want := range map[string]string{
		"dt01.server_us_ab3xy9kp2q": EventTypeRefetchConfig,
		"dt01.web_us_ab3xy9kp2q":    EventTypeRefetchEvaluation,
		"dt01.mobile_us_ab3xy9kp2q": EventTypeRefetchEvaluation,
	} {
		var d changeNotification
		if err := json.Unmarshal(refetchEvent(keyID, cache.Entry{}).Data, &d); err != nil || d.Type != want {
			t.Errorf("%s: type = %q (err %v), want %q", keyID, d.Type, err, want)
		}
	}
}
