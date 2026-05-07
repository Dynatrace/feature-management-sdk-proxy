package config

import "log/slog"

// Secret is a string type that prevents credential leakage in logs and serialized output.
// The raw value is intentionally not exposed by any standard formatting or encoding method.
type Secret string

// String implements fmt.Stringer. Returns "[REDACTED]" for %v, %s, and %+v formatting.
func (s Secret) String() string { return "[REDACTED]" }

// GoString implements fmt.GoStringer. Returns "[REDACTED]" for %#v formatting.
func (s Secret) GoString() string { return "[REDACTED]" }

// LogValue implements slog.LogValuer. Prevents the raw value from appearing in structured logs.
func (s Secret) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// MarshalJSON prevents the raw value from appearing in json.Marshal output.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// MarshalText prevents the raw value from appearing in text/YAML encoders.
func (s Secret) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// Unwrap returns the underlying secret value for passing to APIs that require a plain string.
func (s Secret) Unwrap() string { return string(s) }
