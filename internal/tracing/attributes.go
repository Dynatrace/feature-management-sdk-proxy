package tracing

import "go.opentelemetry.io/otel/attribute"

// SDKKeyHash returns a span attribute for the opaque SDK key identifier.
// The value must be cache.Key(sdkKey) — never the raw SDK key.
func SDKKeyHash(keyHash string) attribute.KeyValue {
	return attribute.String("sdk_key_hash", keyHash)
}
