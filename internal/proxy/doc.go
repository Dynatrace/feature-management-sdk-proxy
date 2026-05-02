// Package proxy serves flagd config payloads from an in-memory cache, fetching
// from the upstream CDN on a miss. The SDK key is never logged or persisted;
// only its SHA-256 hash is used as a stable identifier.
package proxy
