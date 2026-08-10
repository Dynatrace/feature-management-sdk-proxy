//go:build integration

// Behavior tested here is documented in docs/operations.md and docs/static-source.md —
// update those docs when adding or changing tests.

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/evalmetrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/health"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/metrics"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/middleware"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/server"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sse"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/staticsource"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/telemetry"
)

// staticHarness runs the proxy in CONFIG_SOURCE=static mode in-process.
type staticHarness struct {
	addr string
	path string
}

// staticOptions configures the static harness.
type staticOptions struct {
	metricsUpstreamURL string // when set, POST /v1/metrics forwards here
}

func startStaticHarness(t *testing.T, initial string, opts staticOptions) *staticHarness {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	inst, err := metrics.Register(noop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatalf("metrics.Register: %v", err)
	}
	tracer := tracenoop.NewTracerProvider().Tracer("test")

	dir := t.TempDir()
	path := filepath.Join(dir, "flags.json")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatalf("write flags: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := staticsource.New()
	watcher := staticsource.NewWatcher(store, path, 25*time.Millisecond, 10*1024*1024, inst, logger)
	if err := watcher.LoadInitial(ctx); err != nil {
		t.Fatalf("LoadInitial: %v", err)
	}
	go watcher.Run(ctx)

	counter := telemetry.NewCounter()
	configHandler := staticsource.NewHandler(store, inst, counter, logger, tracer)
	probes := health.New(store.Loaded, logger)

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.GET("/livez", probes.Live)
	router.GET("/readyz", probes.Ready)
	router.GET("/healthz", probes.Health)

	api := router.Group("/", middleware.TracePropagation())
	api.GET(sse.HandlerPath, sdkkey.FromStreamID(), func(c *gin.Context) {
		c.String(http.StatusNotImplemented, "SSE is disabled in static config mode\n")
	})
	api.GET(configHandlerPath+":file", sdkkey.FromPathFile(), configHandler.Handle)
	api.HEAD(configHandlerPath+":file", sdkkey.FromPathFile(), configHandler.Handle)

	if opts.metricsUpstreamURL != "" {
		mh, err := evalmetrics.NewStatic(opts.metricsUpstreamURL, http.DefaultClient, inst, logger, tracer)
		if err != nil {
			t.Fatalf("evalmetrics.NewStatic: %v", err)
		}
		api.POST("/v1/metrics", sdkkey.FromBearerToken(), mh.Handle)
	} else {
		api.POST("/v1/metrics", func(c *gin.Context) {
			c.String(http.StatusNotImplemented, "metrics passthrough is disabled\n")
		})
	}

	srv, err := server.New(":0", router, 5*time.Second, logger)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()
	t.Cleanup(func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		_ = srv.Shutdown(shutCtx)
	})

	return &staticHarness{addr: srv.Addr(), path: path}
}

func (h *staticHarness) get(t *testing.T, sdkKey string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+h.addr+configHandlerPath+sdkKey+".json", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func (h *staticHarness) rewrite(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(h.path, []byte(content), 0o600); err != nil {
		t.Fatalf("rewrite flags: %v", err)
	}
}

// twoSetFile assigns one flag to TestSDKKey and one to TestSDKKey2.
func twoSetFile() string {
	return `{
		"flags": {
			"usFlag": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on", "metadata": {"flagSetId": "` + TestSDKKey + `"}},
			"euFlag": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on", "metadata": {"flagSetId": "` + TestSDKKey2 + `"}}
		}
	}`
}

func flagKeys(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var top struct {
		Flags map[string]json.RawMessage `json:"flags"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return top.Flags
}

func TestStatic_ServesFilteredConfigPerFlagSet(t *testing.T) {
	h := startStaticHarness(t, twoSetFile(), staticOptions{})

	resp := h.get(t, TestSDKKey, nil)
	assertStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	flags := flagKeys(t, body)
	if _, ok := flags["usFlag"]; !ok {
		t.Errorf("expected usFlag in TestSDKKey config, got %v", flags)
	}
	if _, ok := flags["euFlag"]; ok {
		t.Errorf("euFlag must not leak into TestSDKKey config")
	}
}

func TestStatic_IfNoneMatchReturns304(t *testing.T) {
	h := startStaticHarness(t, twoSetFile(), staticOptions{})

	resp := h.get(t, TestSDKKey, nil)
	assertStatus(t, resp, http.StatusOK)
	etag := resp.Header.Get("ETag")
	resp.Body.Close()
	if etag == "" {
		t.Fatal("expected ETag header")
	}

	resp2 := h.get(t, TestSDKKey, map[string]string{"If-None-Match": etag})
	assertStatus(t, resp2, http.StatusNotModified)
	resp2.Body.Close()
}

func TestStatic_ReloadOnFileChange(t *testing.T) {
	h := startStaticHarness(t, twoSetFile(), staticOptions{})

	resp := h.get(t, TestSDKKey, nil)
	assertStatus(t, resp, http.StatusOK)
	origETag := resp.Header.Get("ETag")
	resp.Body.Close()

	// Add a second flag to TestSDKKey's set.
	h.rewrite(t, `{
		"flags": {
			"usFlag": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on", "metadata": {"flagSetId": "`+TestSDKKey+`"}},
			"usFlag2": {"state": "ENABLED", "variants": {"on": true}, "defaultVariant": "on", "metadata": {"flagSetId": "`+TestSDKKey+`"}}
		}
	}`)

	eventually(t, func() bool {
		resp := h.get(t, TestSDKKey, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		body, _ := io.ReadAll(resp.Body)
		return len(flagKeys(t, body)) == 2 && resp.Header.Get("ETag") != origETag
	}, 3*time.Second, 25*time.Millisecond, "config did not reflect file change")
}

func TestStatic_InvalidReloadKeepsLastGood(t *testing.T) {
	h := startStaticHarness(t, twoSetFile(), staticOptions{})

	resp := h.get(t, TestSDKKey, nil)
	assertStatus(t, resp, http.StatusOK)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	goodLen := len(flagKeys(t, body))

	h.rewrite(t, `{ this is not valid json`)

	// Give the watcher time to attempt (and reject) the reload.
	time.Sleep(200 * time.Millisecond)

	resp2 := h.get(t, TestSDKKey, nil)
	assertStatus(t, resp2, http.StatusOK)
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if len(flagKeys(t, body2)) != goodLen {
		t.Error("last-good config should still be served after an invalid reload")
	}
}

func TestStatic_UnknownFlagSetReturns404(t *testing.T) {
	h := startStaticHarness(t, twoSetFile(), staticOptions{})
	// A valid full key with no matching flag set.
	unknown := "dt01.server_ap_zz9yy8xx7w.03ee0ef7cd4244bfb8b71fea8f783f53_d4f8a9c12a"
	resp := h.get(t, unknown, nil)
	assertStatus(t, resp, http.StatusNotFound)
	resp.Body.Close()
}

func TestStatic_SSEReturns501(t *testing.T) {
	h := startStaticHarness(t, twoSetFile(), staticOptions{})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"http://"+h.addr+sse.HandlerPath+"?stream_id="+sdkkey.PublicID(TestSDKKey), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	assertStatus(t, resp, http.StatusNotImplemented)
	resp.Body.Close()
}

func TestStatic_MetricsPassthroughDisabledReturns501(t *testing.T) {
	h := startStaticHarness(t, twoSetFile(), staticOptions{})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+h.addr+"/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+TestSDKKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	assertStatus(t, resp, http.StatusNotImplemented)
	resp.Body.Close()
}

func TestStatic_MetricsPassthroughForwards(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer upstream.Close()

	h := startStaticHarness(t, twoSetFile(), staticOptions{metricsUpstreamURL: upstream.URL + "/v1/metrics"})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+h.addr+"/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+TestSDKKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	assertStatus(t, resp, http.StatusAccepted)
	resp.Body.Close()
	if gotAuth != "Bearer "+TestSDKKey {
		t.Errorf("Authorization forwarded = %q, want verbatim", gotAuth)
	}
}
