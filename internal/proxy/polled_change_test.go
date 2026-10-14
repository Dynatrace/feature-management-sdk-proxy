package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sse"
)

// TestHandler_DemandReval_NotifiesSSESubscribers covers the restart case: SDK
// clients subscribe to /sse against a snapshot-restored config before the
// upstream SSE connection exists. When demand revalidation finds new content,
// they must be told to refetch; when it finds none (304), they must not.
func TestHandler_DemandReval_NotifiesSSESubscribers(t *testing.T) {
	cases := []struct {
		name      string
		result    cache.FetchResult
		wantEvent bool
	}{
		{"200 new content", cache.FetchResult{Entry: &cache.Entry{
			Payload: []byte(`{"flags":{"new":true}}`), ETag: `"etag2"`, FetchedAt: time.Now(),
		}}, true},
		{"304 unchanged", cache.FetchResult{NotModified: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst, err := metrics.Register(noop.NewMeterProvider().Meter("test"))
			if err != nil {
				t.Fatalf("metrics.Register: %v", err)
			}
			mgr := sse.New(nil, inst, slog.Default(), sse.Config{})
			t.Cleanup(func() { _ = mgr.Shutdown(context.Background()) })

			c := cache.New()
			c.Set(testKeyID, staleSnapshotEntry([]byte(`{"flags":{"old":true}}`), `"etag1"`))

			// Downstream SDK client subscribed to the snapshot-restored config.
			sseHandler := sse.NewHandler(c, mgr.Server(), mgr, inst, nil, slog.Default(), time.Minute, "/server/")
			subCtx, stopSub := context.WithCancel(context.Background())
			sub := httptest.NewRecorder()
			subDone := make(chan struct{})
			router := gin.New()
			router.GET("/sse", sdkkey.FromStreamID(), sseHandler.Handle)
			go func() {
				defer close(subDone)
				router.ServeHTTP(sub, httptest.NewRequest(http.MethodGet, "/sse?stream_id="+testKeyID, nil).WithContext(subCtx))
			}()
			for deadline := time.Now().Add(2 * time.Second); !mgr.HasSubscribers(testKeyID); {
				if time.Now().After(deadline) {
					t.Fatal("subscriber never registered")
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected call to proxy upstream")
			}))
			t.Cleanup(upstream.Close)
			h, err := New("fm.test", newRedirectingClient(upstream), c, mgr, nil, &mockFetcher{result: tc.result}, inst, nil, slog.Default(), testTracer)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			rec := httptest.NewRecorder()
			h.Handle(ginContext(rec, httptest.NewRequest(http.MethodGet, "/server/"+testSDKKey+".json", nil)))
			if rec.Code != http.StatusOK {
				t.Fatalf("config request: got %d", rec.Code)
			}

			time.Sleep(100 * time.Millisecond)
			stopSub()
			<-subDone
			body := sub.Body.String()

			gotEvent := strings.Contains(body, `"type":"refetchConfig"`)
			if gotEvent != tc.wantEvent {
				t.Fatalf("refetchConfig delivered = %v, want %v; stream: %q", gotEvent, tc.wantEvent, body)
			}
			if tc.wantEvent && !strings.Contains(body, `"etag":"\"etag2\""`) {
				t.Errorf("event should carry the new ETag; stream: %q", body)
			}
		})
	}
}
