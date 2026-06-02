package shared

import "time"

// RequestID is the stable identifier for a captured HTTP exchange.
type RequestID string

// Request represents a single captured HTTP exchange (request + response).
//
// In-flight requests omit CompletedAt, Status, and all response fields.
// CONNECT tunnels produce a minimal record: Method="CONNECT", Scheme="https",
// Host="target:port", Path="", Status=200 on success.
type Request struct {
	ID              RequestID   `json:"id"`
	StartedAt       time.Time   `json:"startedAt"`
	CompletedAt     *time.Time  `json:"completedAt,omitempty"`
	Method          string      `json:"method"`
	Scheme          string      `json:"scheme"`
	Host            string      `json:"host"`
	Path            string      `json:"path"`
	Query           string      `json:"query"`
	URL             string      `json:"url"`
	RequestHeaders  Headers     `json:"requestHeaders"`
	RequestBodyRef  BodyRef     `json:"requestBodyRef,omitempty"`
	Status          int         `json:"status,omitempty"`
	ResponseHeaders Headers     `json:"responseHeaders,omitempty"`
	ResponseBodyRef BodyRef     `json:"responseBodyRef,omitempty"`
	DurationMs      int64       `json:"durationMs,omitempty"`
	ClientAddr      string      `json:"clientAddr"`
	RuleTrace       []RuleTrace `json:"ruleTrace,omitempty"`
	Tags            []string    `json:"tags,omitempty"`
}

// Headers is a map of header name to its first value.
type Headers map[string]string

// BodyRef is a reference key into the body cache. A zero value or empty string
// means no body is available (e.g. CONNECT tunnel, evicted from cache).
type BodyRef string
