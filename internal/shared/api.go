package shared

// ---------------------------------------------------------------------------
// Control API request / response types
// ---------------------------------------------------------------------------

// StatusResponse is returned by GET /v1/status.
type StatusResponse struct {
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	ProxyAddr     string `json:"proxyAddr"`
	RequestCount  int64  `json:"requestCount"`
	RuleCount     int    `json:"ruleCount"`
}

// ListRequestsResponse is returned by GET /v1/requests.
type ListRequestsResponse struct {
	Requests   []RequestSummary `json:"requests"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// RequestSummary is a lightweight view of a request used in list responses.
type RequestSummary struct {
	ID         RequestID `json:"id"`
	StartedAt  string    `json:"startedAt"`
	Method     string    `json:"method"`
	URL        string    `json:"url"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"durationMs"`
	Tags       []string  `json:"tags,omitempty"`
}

// RequestDetailResponse is returned by GET /v1/requests/:id.
type RequestDetailResponse struct {
	Request Request  `json:"request"`
	Body    *string  `json:"body,omitempty"`
	Headers *Headers `json:"headers,omitempty"`
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

// Standard error codes.
const (
	ErrCodeNotFound      = "NOT_FOUND"
	ErrCodeBadRequest    = "BAD_REQUEST"
	ErrCodeInternal      = "INTERNAL_ERROR"
	ErrCodeRuleEvalError = "RULE_EVAL_ERROR"
)
