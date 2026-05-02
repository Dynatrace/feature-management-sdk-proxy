package sse

// Event is an SSE frame received from the upstream server and re-broadcast
// to SDK clients subscribed to the same flag set.
type Event struct {
	ID        string
	EventType string
	Data      []byte
}
