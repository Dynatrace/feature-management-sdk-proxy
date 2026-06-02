package sdkkey

import (
	"regexp"
	"strings"
)

var (
	// publicIDRe matches a valid SDK key public identifier: {token}.{platform}_{geo}_{publicRandom}
	publicIDRe = regexp.MustCompile(`^dt[0-9]{2}\.(web|mobile|server)_[a-z]{2}_[a-zA-Z0-9]{10}$`)
	// fullKeyRe matches a valid full SDK key: {token}.{platform}_{geo}_{publicRandom}.{secret}_{sha1}
	fullKeyRe = regexp.MustCompile(`^dt[0-9]{2}\.(?:web|mobile|server)_[a-z]{2}_[a-zA-Z0-9]{10}\.[a-f0-9]{32}_[a-f0-9]{10,}$`)
)

// Platform constants matching the platform segment of an SDK key.
const (
	PlatformServer = "server"
	PlatformWeb    = "web"
	PlatformMobile = "mobile"
)

// Key represents a parsed SDK key — either a full key (with private segments) or
// a public-only ID. It provides structured access to all components without
// requiring callers to re-parse the raw string.
//
// Raw key material must never appear in logs, trace spans, or URLs. Use PublicID
// (or String, which returns the same) for any observable identifier. Secret and
// Hash are non-empty only when IsFull returns true.
type Key struct {
	raw          string // full raw value — treat as a secret, never log
	publicID     string // log-safe prefix: token.platform_geo_rand
	token        string // version token, e.g. "dt01"
	platform     string // "web", "mobile", or "server"
	geo          string // two-letter geo code, e.g. "us"
	publicRandom string // 10-char alphanumeric public random segment
	secret       string // 32-hex private secret (empty for public-only keys)
	keyHash      string // 10+-hex SHA-1 derivative (empty for public-only keys)
}

// Raw returns the original key string. Never log or embed this value in URLs.
func (k Key) Raw() string { return k.raw }

// PublicID returns the log-safe public prefix: token.platform_geo_rand.
func (k Key) PublicID() string { return k.publicID }

// Token returns the version token segment, e.g. "dt01".
func (k Key) Token() string { return k.token }

// Platform returns the platform segment: "web", "mobile", or "server".
func (k Key) Platform() string { return k.platform }

// Geo returns the two-letter geo/region code, e.g. "us".
func (k Key) Geo() string { return k.geo }

// PublicRandom returns the 10-char alphanumeric random segment.
func (k Key) PublicRandom() string { return k.publicRandom }

// Secret returns the 32-hex private secret segment.
// Empty when IsFull returns false.
func (k Key) Secret() string { return k.secret }

// Hash returns the hex SHA-1 derivative segment.
// Empty when IsFull returns false.
func (k Key) Hash() string { return k.keyHash }

// IsFull reports whether this key has private segments (Secret and Hash).
func (k Key) IsFull() bool { return k.secret != "" }

// String returns the public ID — safe for use in logs and formatted output.
// It intentionally does NOT return Raw to prevent accidental key exposure.
func (k Key) String() string { return k.publicID }

// Parse parses a full SDK key or a public ID string into a Key.
// Returns (Key, true) when s matches either the full key or public ID format.
// Returns (Key{}, false) for any other input.
func Parse(s string) (Key, bool) {
	if ValidateFullKey(s) {
		return parseFull(s), true
	}
	if IsValidID(s) {
		return parsePublicID(s), true
	}
	return Key{}, false
}

// ParseFull parses a full SDK key (requiring the private segment).
// Returns (Key, true) only when s matches the full key format.
func ParseFull(s string) (Key, bool) {
	if !ValidateFullKey(s) {
		return Key{}, false
	}
	return parseFull(s), true
}

func parseFull(s string) Key {
	dots := strings.SplitN(s, ".", 3)
	// dots: [token, platform_geo_rand, secret_hash]
	under := strings.SplitN(dots[1], "_", 3)
	// under: [platform, geo, publicRandom]
	priv := strings.SplitN(dots[2], "_", 2)
	// priv: [secret, hash]
	return Key{
		raw:          s,
		publicID:     dots[0] + "." + dots[1],
		token:        dots[0],
		platform:     under[0],
		geo:          under[1],
		publicRandom: under[2],
		secret:       priv[0],
		keyHash:      priv[1],
	}
}

func parsePublicID(s string) Key {
	dots := strings.SplitN(s, ".", 2)
	// dots: [token, platform_geo_rand]
	under := strings.SplitN(dots[1], "_", 3)
	// under: [platform, geo, publicRandom]
	return Key{
		raw:          s,
		publicID:     s,
		token:        dots[0],
		platform:     under[0],
		geo:          under[1],
		publicRandom: under[2],
	}
}

// PublicID returns the log-safe public prefix of an SDK key string.
// It splits on "." and drops the last segment (the private segment).
// The raw SDK key must never be logged; use this function when logging key context.
func PublicID(sdkKey string) string {
	i := strings.LastIndex(sdkKey, ".")
	if i < 0 {
		return ""
	}
	return sdkKey[:i]
}

// IsValidID reports whether s is a valid SDK key public identifier:
// {token}.{platform}_{geo}_{publicRandom}
func IsValidID(s string) bool {
	return publicIDRe.MatchString(s)
}

// ValidateFullKey reports whether s is a well-formed full SDK key:
// {token}.{platform}_{geo}_{publicRandom}.{secret}_{sha1}
func ValidateFullKey(s string) bool {
	return fullKeyRe.MatchString(s)
}

// ExtractGeo parses the geo segment from an SDK key in the format
// {token}.{platform}_{geo}_{publicRandom}.{secret}_{sha1}.
// Returns ("", false) for any other format. The geo is always [a-z]{2} per the
// full-key regex, so it is safe to embed in derived upstream URLs.
func ExtractGeo(sdkKey string) (string, bool) {
	k, ok := ParseFull(sdkKey)
	if !ok {
		return "", false
	}
	return k.Geo(), true
}

// FromFileName extracts an SDK key from a route parameter value like
// "{sdk_key}.json", stripping the ".json" suffix.
// Returns ("", false) if the parameter does not end with ".json" or is empty.
func FromFileName(file string) (string, bool) {
	if !strings.HasSuffix(file, ".json") {
		return "", false
	}
	key := strings.TrimSuffix(file, ".json")
	if key == "" {
		return "", false
	}
	return key, true
}

// FromBearer extracts a Bearer token from an Authorization header value.
// Returns ("", false) if the header is absent or does not use the Bearer scheme.
func FromBearer(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}
