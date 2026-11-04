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

	// Intercepted is true when the request was MITM-decrypted (as opposed to a
	// raw TCP CONNECT tunnel passthrough).
	Intercepted bool `json:"intercepted,omitempty"`

	// TunnelID is the parent CONNECT tunnel for a decrypted HTTP exchange.
	TunnelID RequestID `json:"tunnelId,omitempty"`
	// SessionID is the recording session that was active when this exchange was captured.
	SessionID string `json:"sessionId,omitempty"`

	// OriginalURL is the URL as the client sent it. EffectiveURL is the URL
	// after rule patches. URL remains the original client URL so agents can
	// find the request they sent.
	OriginalURL  string `json:"originalURL,omitempty"`
	EffectiveURL string `json:"effectiveURL,omitempty"`

	// Repeated header values. RequestHeaders / ResponseHeaders keep a
	// compatibility map (joined values, except Set-Cookie which keeps the
	// first value so cookies are not comma-joined).
	RequestHeaderList  []HeaderField `json:"requestHeaderList,omitempty"`
	ResponseHeaderList []HeaderField `json:"responseHeaderList,omitempty"`

	RequestBodyBytes      int64 `json:"requestBodyBytes,omitempty"`
	RequestBodyTruncated  bool  `json:"requestBodyTruncated,omitempty"`
	ResponseBodyBytes     int64 `json:"responseBodyBytes,omitempty"`
	ResponseBodyTruncated bool  `json:"responseBodyTruncated,omitempty"`

	FailureStage     string `json:"failureStage,omitempty"`
	PolicyGeneration uint64 `json:"policyGeneration,omitempty"`
	RuleGeneration   uint64 `json:"ruleGeneration,omitempty"`

	// Attribution is reserved for traffic inspection (docs/design/inspect.md).
	// The proxy does not fill it yet. List, rules, and history ignore it.
	// Omitempty keeps today's JSON unchanged until a later change populates it.
	Attribution *Attribution `json:"attribution,omitempty"`
}

// Attribution identifies where a captured exchange came from. It is a placeholder
// so inspection can attach a process, page, initiator, and resource type without
// a second record shape. Nothing in this tree sets it.
type Attribution struct {
	SourceID     string `json:"sourceId,omitempty"`
	PageID       string `json:"pageId,omitempty"`
	InitiatorURL string `json:"initiatorUrl,omitempty"`
	ResourceType string `json:"resourceType,omitempty"`
	Confidence   string `json:"confidence,omitempty"`
}

// HeaderField is one header name and every value received for it, in order.
type HeaderField struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// Headers is a map of header name to its first value.
type Headers map[string]string

// BodyRef is a reference key into the body cache. A zero value or empty string
// means no body is available (e.g. CONNECT tunnel, evicted from cache).
type BodyRef string
