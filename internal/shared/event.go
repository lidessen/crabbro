package shared

import "time"

// EventType enumerates the structured events emitted by the daemon during the
// lifecycle of an HTTP exchange.
type EventType string

const (
	// EventRequestStarted is emitted when a new request arrives.
	EventRequestStarted EventType = "request_started"
	// EventRequestHeaders is emitted when request headers have been received.
	EventRequestHeaders EventType = "request_headers"
	// EventRequestBodyAvailable is emitted when the request body is fully received.
	EventRequestBodyAvailable EventType = "request_body_available"
	// EventRuleEvaluated is emitted after a rule is evaluated (match or not).
	EventRuleEvaluated EventType = "rule_evaluated"
	// EventRuleApplied is emitted when a matched rule produces at least one action.
	EventRuleApplied EventType = "rule_applied"
	// EventResponseHeaders is emitted when response headers have been received.
	EventResponseHeaders EventType = "response_headers"
	// EventResponseBodyAvailable is emitted when the response body is fully received.
	EventResponseBodyAvailable EventType = "response_body_available"
	// EventResponseCompleted is emitted when the exchange is complete.
	EventResponseCompleted EventType = "response_completed"
	// EventRequestFailed is emitted when the exchange ends with an error.
	EventRequestFailed EventType = "request_failed"
)

// Event is a structured observation emitted by the daemon. Events are
// append-only and delivered via SSE on the control API.
type Event struct {
	Type      EventType `json:"type"`
	RequestID RequestID `json:"requestId"`
	Timestamp time.Time `json:"timestamp"`

	// Payload is an optional event-specific data object.
	Payload any `json:"payload,omitempty"`
}
