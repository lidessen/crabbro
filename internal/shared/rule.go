package shared

import "time"

// RuleID is the stable identifier for a rule.
type RuleID string

// Rule represents a single JavaScript rule stored and executed by the daemon.
type Rule struct {
	ID       RuleID `json:"id"`
	Name     string `json:"name,omitempty"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
	Script   string `json:"script"`
	// Version increments each time the script changes. Traces record the version
	// that ran.
	Version int `json:"version,omitempty"`
	// FailureMode is "fail-open" (default) or "fail-closed".
	// fail-open skips a rule whose evaluation or validation fails.
	// fail-closed returns a defined rejection response instead.
	FailureMode string    `json:"failureMode,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// RuleTrace records one rule evaluation against a request.
type RuleTrace struct {
	RuleID      RuleID       `json:"ruleId"`
	RuleVersion int          `json:"ruleVersion"`
	Matched     bool         `json:"matched"`
	Actions     []RuleAction `json:"actions,omitempty"`
	DurationMs  int64        `json:"durationMs"`
	Error       string       `json:"error,omitempty"`
}

// RuleActionType enumerates the supported rule action types.
type RuleActionType string

const (
	// ActionAbort aborts the request without a response.
	ActionAbort RuleActionType = "abort"
	// ActionBlock aborts the request with a configured response.
	ActionBlock RuleActionType = "block"
	// ActionMockResponse replaces the upstream response with a synthetic one.
	ActionMockResponse RuleActionType = "mockResponse"
	// ActionRemoveRequestHeader removes a header from the proxied request.
	ActionRemoveRequestHeader RuleActionType = "removeRequestHeader"
	// ActionSetRequestHeader sets a header on the proxied request.
	ActionSetRequestHeader RuleActionType = "setRequestHeader"
	// ActionTag adds tags to the request record.
	ActionTag RuleActionType = "tag"
	// ActionRewriteURL changes the upstream URL before forwarding.
	ActionRewriteURL RuleActionType = "rewriteURL"
)

// RuleAction is a declarative action returned by a rule evaluation.
type RuleAction struct {
	Type   RuleActionType `json:"type"`
	Params map[string]any `json:"params,omitempty"`
}
