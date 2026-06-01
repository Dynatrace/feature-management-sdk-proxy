// Package sdkkey provides SDK key parsing, validation, and Gin middleware for
// extracting SDK keys from HTTP requests and storing them in the request context.
package sdkkey

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// contextKey is the single gin context key under which a Key is stored.
const contextKey = "sdkkey"

// FromPathFile is gin middleware that extracts a full SDK key from the ":file"
// route parameter (e.g. "/server/{sdk_key}.json"). It stores a Key in the gin
// context and aborts with 400 if the parameter does not end with ".json" or is
// otherwise empty.
func FromPathFile() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := FromFileName(c.Param("file"))
		if !ok {
			c.String(http.StatusBadRequest, "path must end with /<sdk_key>.json\n")
			c.Abort()
			return
		}
		k, _ := Parse(raw)
		if !k.IsFull() {
			// Store a partial key so any logging before the handler's format check
			// can still surface the public portion.
			k = Key{raw: raw, publicID: PublicID(raw)}
		}
		c.Set(contextKey, k)
		c.Next()
	}
}

// FromBearerToken is gin middleware that extracts a full SDK key from an
// "Authorization: Bearer <sdk_key>" header. It stores a Key in the gin context
// and aborts with 401 if the header is absent or does not use the Bearer scheme.
func FromBearerToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := FromBearer(c.GetHeader("Authorization"))
		if !ok {
			c.String(http.StatusUnauthorized, "Authorization: Bearer <sdk_key> required\n")
			c.Abort()
			return
		}
		k, _ := Parse(token)
		if !k.IsFull() {
			k = Key{raw: token, publicID: PublicID(token)}
		}
		c.Set(contextKey, k)
		c.Next()
	}
}

// FromStreamID is gin middleware that extracts a public SDK key ID from the
// "stream_id" query parameter. It validates the format against the public ID
// regex, parses the key components into a Key, and stores it in the gin context.
// Aborts with 400 if the parameter is missing or invalid.
func FromStreamID() gin.HandlerFunc {
	return func(c *gin.Context) {
		streamID := c.Query("stream_id")
		if streamID == "" {
			c.String(http.StatusBadRequest, "missing stream_id query parameter\n")
			c.Abort()
			return
		}
		k, ok := Parse(streamID)
		if !ok {
			c.String(http.StatusBadRequest, "invalid stream_id: must be a valid SDK key public prefix\n")
			c.Abort()
			return
		}
		c.Set(contextKey, k)
		c.Next()
	}
}

// KeyFromContext retrieves the Key stored by any SDK key middleware.
// Returns (Key{}, false) when no key is present in the context.
func KeyFromContext(c *gin.Context) (Key, bool) {
	v, ok := c.Get(contextKey)
	if !ok {
		return Key{}, false
	}
	k, ok := v.(Key)
	return k, ok
}

// FullKey retrieves the raw SDK key string stored by FromPathFile or
// FromBearerToken middleware. Returns ("", false) when no key is present.
func FullKey(c *gin.Context) (string, bool) {
	k, ok := KeyFromContext(c)
	if !ok {
		return "", false
	}
	return k.Raw(), ok
}

// PubID retrieves the public SDK key ID stored by any SDK key middleware.
// Returns ("", false) when no public ID is present in the context.
func PubID(c *gin.Context) (string, bool) {
	k, ok := KeyFromContext(c)
	if !ok {
		return "", false
	}
	return k.PublicID(), ok
}
