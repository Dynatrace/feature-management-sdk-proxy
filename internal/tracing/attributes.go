package tracing

import "go.opentelemetry.io/otel/attribute"

// SDKKeyPublicID returns a span attribute for the SDK key public identifier.
// The value must be cache.PublicID(sdkKey) — never the raw SDK key or the full key.
func SDKKeyPublicID(keyID string) attribute.KeyValue {
	return attribute.String("sdk_key_public_id", keyID)
}
