package sse

import (
	"encoding/json"

	"github.com/launchdarkly/eventsource"

	"github.com/dynatrace/feature-management-sdk-proxy/internal/cache"
	"github.com/dynatrace/feature-management-sdk-proxy/internal/sdkkey"
)

// Upstream change notification types. Server channels receive refetchConfig;
// web and mobile channels receive refetchEvaluation.
const (
	EventTypeRefetchConfig     = "refetchConfig"
	EventTypeRefetchEvaluation = "refetchEvaluation"
)

// changeNotification is the data payload of a change notification.
type changeNotification struct {
	Type         string `json:"type"`
	ETag         string `json:"etag,omitempty"`
	LastModified int64  `json:"lastModified,omitempty"` // Unix milliseconds
}

// parseRefetch reports whether evt is a change notification and returns its
// etag hint. The data is either the notification itself or an Ably message
// whose "data" field is the notification as a JSON string.
func parseRefetch(evt Event) (cache.RefetchHint, bool) {
	var outer struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	data := evt.Data
	if json.Unmarshal(data, &outer) == nil && outer.Data != "" {
		data = []byte(outer.Data)
	}
	var d changeNotification
	_ = json.Unmarshal(data, &d) // non-JSON data just isn't type-tagged
	for _, t := range []string{d.Type, outer.Name, evt.EventType} {
		if t == EventTypeRefetchConfig || t == EventTypeRefetchEvaluation {
			return cache.RefetchHint{ETag: d.ETag}, true
		}
	}
	return cache.RefetchHint{}, false
}

// refetchEventType returns the notification type upstream sends on keyID's
// channel: refetchEvaluation for web and mobile keys, refetchConfig otherwise.
func refetchEventType(keyID string) string {
	if k, ok := sdkkey.Parse(keyID); ok && (k.Platform() == "web" || k.Platform() == "mobile") {
		return EventTypeRefetchEvaluation
	}
	return EventTypeRefetchConfig
}

// refetchEvent builds the change notification published to keyID's
// subscribers when a config change was found by polling rather than
// announced upstream.
func refetchEvent(keyID string, entry cache.Entry) Event {
	d := changeNotification{Type: refetchEventType(keyID), ETag: entry.ETag}
	if !entry.LastModified.IsZero() {
		d.LastModified = entry.LastModified.UnixMilli()
	}
	data, _ := json.Marshal(d) // cannot fail: fixed struct of strings and ints
	return Event{EventType: "message", Data: data}
}

// Event is an SSE frame received from the upstream server and re-broadcast
// to SDK clients subscribed to the same flag set.
// This specifically implements the byte array spec of the eventsource response object.
type Event struct {
	ID        string
	EventType string
	Data      []byte
}

// publishableEvent adapts Event to the eventsource.Event interface required by
// the LaunchDarkly eventsource.Server.Publish method.
type publishableEvent struct {
	evt Event
}

// Compile-time check that publishableEvent satisfies eventsource.Event.
var _ eventsource.Event = publishableEvent{}

func (e publishableEvent) Id() string    { return e.evt.ID } //nolint:revive // method name required by eventsource.Event interface
func (e publishableEvent) Event() string { return e.evt.EventType }
func (e publishableEvent) Data() string  { return string(e.evt.Data) }
