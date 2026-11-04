package api

import "time"

// ---------------------------------------------------------------------------
// Control API request / response types
// ---------------------------------------------------------------------------

// StatusResponse is returned by GET /v1/status.
type StatusResponse struct {
	Version           string `json:"version"`
	UptimeSeconds     int64  `json:"uptimeSeconds"`
	ProxyAddr         string `json:"proxyAddr"`
	RequestCount      int64  `json:"requestCount"`
	RuleCount         int    `json:"ruleCount"`
	MITMEnabled       bool   `json:"mitmEnabled"`
	MITMCertInstalled bool   `json:"mitmCertInstalled"`
}

// ListRequestsResponse is returned by GET /v1/requests.
type ListRequestsResponse struct {
	Requests    []RequestSummary `json:"requests"`
	NextCursor  string           `json:"nextCursor,omitempty"`
	HiddenCount int              `json:"hiddenCount,omitempty"`
	ScopeActive bool             `json:"scopeActive,omitempty"`
}

// RequestSummary is a lightweight view of a request used in list responses.
type RequestSummary struct {
	ID           RequestID `json:"id"`
	StartedAt    string    `json:"startedAt"`
	Method       string    `json:"method"`
	URL          string    `json:"url"`
	Status       int       `json:"status"`
	DurationMs   int64     `json:"durationMs"`
	Tags         []string  `json:"tags,omitempty"`
	Hidden       bool      `json:"hidden,omitempty"`
	HiddenReason []string  `json:"hiddenReason,omitempty"`
	ResourceType string    `json:"resourceType,omitempty"`
	InitiatorURL string    `json:"initiatorUrl,omitempty"`
	SourceID     string    `json:"sourceId,omitempty"`
}

// RequestDetailResponse is returned by GET /v1/requests/:id.
type RequestDetailResponse struct {
	Request Request `json:"request"`
	// Body is the captured request body when include=body.
	Body *string `json:"body,omitempty"`
	// ResponseBody is the captured response body when include=body.
	ResponseBody *string  `json:"responseBody,omitempty"`
	Headers      *Headers `json:"headers,omitempty"`
}

// ---------------------------------------------------------------------------
// Rule types
// ---------------------------------------------------------------------------

// CreateRuleRequest is the body for POST /v1/rules.
type CreateRuleRequest struct {
	Name        string `json:"name,omitempty"`
	Priority    int    `json:"priority,omitempty"`
	Script      string `json:"script"`
	FailureMode string `json:"failureMode,omitempty"`
}

// UpdateRuleRequest is the body for PUT /v1/rules/:id.
type UpdateRuleRequest struct {
	Name     *string `json:"name,omitempty"`
	Priority *int    `json:"priority,omitempty"`
	Script   *string `json:"script,omitempty"`
}

// ToggleRuleRequest is the body for POST /v1/rules/:id/toggle.
type ToggleRuleRequest struct {
	Enabled bool `json:"enabled"`
}

// ListRulesResponse is returned by GET /v1/rules.
type ListRulesResponse struct {
	Rules []*Rule `json:"rules"`
}

// GetRuleResponse is returned by GET /v1/rules/:id.
type GetRuleResponse struct {
	Rule Rule `json:"rule"`
}

// ---------------------------------------------------------------------------
// Fixture types
// ---------------------------------------------------------------------------

// CreateFixtureRequest is the body for POST /v1/fixtures.
type CreateFixtureRequest struct {
	RequestID RequestID `json:"requestId"`
	Name      string    `json:"name"`
}

// ListFixturesResponse is returned by GET /v1/fixtures.
type ListFixturesResponse struct {
	Fixtures []FixtureSummary `json:"fixtures"`
}

// FixtureSummary is a lightweight view of a fixture used in list responses.
type FixtureSummary struct {
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt"`
	Method    string `json:"method"`
	Host      string `json:"host"`
	Path      string `json:"path"`
}

// GetFixtureResponse is returned by GET /v1/fixtures/:name.
type GetFixtureResponse struct {
	Fixture FixtureDetail `json:"fixture"`
}

// FixtureDetail is the full fixture data returned by GET /v1/fixtures/:name.
type FixtureDetail struct {
	Name      string         `json:"name"`
	CreatedAt string         `json:"createdAt"`
	Request   FixtureRequest `json:"request"`
}

// FixtureRequest captures the HTTP request and response fields of an exchange.
type FixtureRequest struct {
	Method          string            `json:"method"`
	Scheme          string            `json:"scheme"`
	Host            string            `json:"host"`
	Path            string            `json:"path"`
	Query           string            `json:"query"`
	URL             string            `json:"url"`
	RequestHeaders  map[string]string `json:"requestHeaders"`
	RequestBody     string            `json:"requestBody,omitempty"`
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty"`
	ResponseBody    string            `json:"responseBody,omitempty"`
	Status          int               `json:"status,omitempty"`
	ClientAddr      string            `json:"clientAddr"`
}

// ---------------------------------------------------------------------------
// Rule test types
// ---------------------------------------------------------------------------

// TestRuleRequest is the body for POST /v1/rules/test.
type TestRuleRequest struct {
	RuleScript  string `json:"ruleScript"`
	Fixture     string `json:"fixture,omitempty"`
	FixtureID   string `json:"fixtureId,omitempty"`
	FixtureFile string `json:"fixtureFile,omitempty"`
}

// TestRuleResponse is returned by POST /v1/rules/test.
type TestRuleResponse struct {
	Matched     bool         `json:"matched"`
	Actions     []RuleAction `json:"actions,omitempty"`
	DurationMs  int64        `json:"durationMs"`
	SchemaValid bool         `json:"schemaValid"`
	SchemaErrs  []string     `json:"schemaErrors,omitempty"`
	Error       string       `json:"error,omitempty"`
	Trace       RuleTrace    `json:"trace"`
}

// ---------------------------------------------------------------------------
// Error types
// ---------------------------------------------------------------------------

// ErrorResponse is the standard JSON error envelope for all API errors.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody contains the structured error details.
type ErrorBody struct {
	Code    string        `json:"code"`
	Message string        `json:"message"`
	Details []ErrorDetail `json:"details,omitempty"`
}

// ErrorDetail describes a single field-level validation error.
type ErrorDetail struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ---------------------------------------------------------------------------
// MITM types
// ---------------------------------------------------------------------------

// MITMStatusResponse is returned by GET /v1/mitm/status.
type MITMStatusResponse struct {
	Enabled       bool     `json:"enabled"`
	CertInstalled bool     `json:"certInstalled"`
	AllowPatterns []string `json:"allowPatterns,omitempty"`
	DenyPatterns  []string `json:"denyPatterns,omitempty"`
	// EmptyAllowDenies reports the network-policy default: an empty allowlist
	// does not intercept. A wildcard must be configured explicitly.
	EmptyAllowDenies bool `json:"emptyAllowDenies"`
	// PolicyGeneration is the snapshot CONNECT routing is using.
	PolicyGeneration uint64 `json:"policyGeneration,omitempty"`
}

// MITMDomainRequest is the body for POST /v1/mitm/allow and POST /v1/mitm/deny.
type MITMDomainRequest struct {
	Pattern string `json:"pattern"`
}

// Standard error codes.
const (
	ErrCodeNotFound      = "NOT_FOUND"
	ErrCodeBadRequest    = "BAD_REQUEST"
	ErrCodeInternal      = "INTERNAL_ERROR"
	ErrCodeRuleEvalError = "RULE_EVAL_ERROR"
	ErrCodeUnauthorized  = "UNAUTHORIZED"
)

// Session is a recording window used by the agent loop and, later, the
// trajectory UI. Requests captured while a session is recording carry its ID.
type Session struct {
	ID         string     `json:"id"`
	Name       string     `json:"name,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	StoppedAt  *time.Time `json:"stoppedAt,omitempty"`
	Recording  bool       `json:"recording"`
	RequestIDs []string   `json:"requestIds,omitempty"`
}

// Activity is one entry on the trajectory timeline (recordings, captures,
// rule edits, and verify results).
type Activity struct {
	ID        string    `json:"id"`
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	SessionID string    `json:"sessionId,omitempty"`
	RequestID string    `json:"requestId,omitempty"`
	RuleID    string    `json:"ruleId,omitempty"`
	Summary   string    `json:"summary"`
}

// VerifyRequest is the body for POST /v1/verify/replay.
type VerifyRequest struct {
	RequestID          string `json:"requestId"`
	ExpectStatus       int    `json:"expectStatus,omitempty"`
	ExpectBodyContains string `json:"expectBodyContains,omitempty"`
}

// VerifyResult is the outcome of replaying a captured request through the proxy.
type VerifyResult struct {
	OK              bool        `json:"ok"`
	ReplayRequestID RequestID   `json:"replayRequestId,omitempty"`
	Status          int         `json:"status,omitempty"`
	ResponseBody    string      `json:"responseBody,omitempty"`
	RuleTrace       []RuleTrace `json:"ruleTrace,omitempty"`
	MatchedRules    []string    `json:"matchedRules,omitempty"`
	Notes           []string    `json:"notes,omitempty"`
	Errors          []string    `json:"errors,omitempty"`
}
