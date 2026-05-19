package middleware

import (
	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// TracePropagation returns a Gin middleware that extracts W3C TraceContext
// (traceparent/tracestate) from incoming request headers and injects the
// resulting span context into the request's context.Context.
//
// Downstream handlers can create child spans from c.Request.Context() without
// repeating the extraction step:
//
//	ctx, span := tracer.Start(c.Request.Context(), "my.operation")
//
// This middleware should be applied to route groups that require distributed
// tracing (e.g. proxy and SSE routes) but NOT to health probe routes.
func TracePropagation() gin.HandlerFunc {
	propagator := otel.GetTextMapPropagator()
	return func(c *gin.Context) {
		ctx := propagator.Extract(c.Request.Context(), propagation.HeaderCarrier(c.Request.Header))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
