package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func init() {
	gin.SetMode(gin.TestMode)
	// Install the W3C propagator so the middleware can extract traceparent.
	otel.SetTextMapPropagator(propagation.TraceContext{})
}

func TestTracePropagation_ExtractsContext(t *testing.T) {
	router := gin.New()
	router.Use(TracePropagation())

	var extractedCtx trace.SpanContext
	router.GET("/test", func(c *gin.Context) {
		extractedCtx = trace.SpanContextFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// W3C traceparent: version-traceID-spanID-flags
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if !extractedCtx.HasTraceID() {
		t.Fatal("expected trace ID to be extracted from traceparent header")
	}
	wantTraceID := "4bf92f3577b34da6a3ce929d0e0e4736"
	if extractedCtx.TraceID().String() != wantTraceID {
		t.Errorf("trace ID = %q, want %q", extractedCtx.TraceID().String(), wantTraceID)
	}
	wantSpanID := "00f067aa0ba902b7"
	if extractedCtx.SpanID().String() != wantSpanID {
		t.Errorf("span ID = %q, want %q", extractedCtx.SpanID().String(), wantSpanID)
	}
	if !extractedCtx.IsSampled() {
		t.Error("expected sampled flag to be set")
	}
}

func TestTracePropagation_NoHeader_EmptyContext(t *testing.T) {
	router := gin.New()
	router.Use(TracePropagation())

	var extractedCtx trace.SpanContext
	router.GET("/test", func(c *gin.Context) {
		extractedCtx = trace.SpanContextFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if extractedCtx.HasTraceID() {
		t.Error("expected no trace ID when traceparent header is absent")
	}
}
