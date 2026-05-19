package sse

import "github.com/launchdarkly/eventsource"

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

func (e publishableEvent) Id() string    { return e.evt.ID }
func (e publishableEvent) Event() string { return e.evt.EventType }
func (e publishableEvent) Data() string  { return string(e.evt.Data) }
