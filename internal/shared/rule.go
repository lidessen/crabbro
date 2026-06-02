package shared

import "time"

// RuleID is the stable identifier for a rule.
type RuleID string

// Rule represents a single JavaScript rule stored and executed by the daemon.
type Rule struct {
	ID        RuleID    `json:"id"`
	Name      string    `json:"name,omitempty"`
	Priority  int       `json:"priority"`
	Enabled   bool      `json:"enabled"`
	Script    string    `json:"script"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
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
	// ActionTag adds tags to the request record.
	ActionTag RuleActionType = "tag"
	// ActionBlock aborts the request with a configured response.
	ActionBlock RuleActionType = "block"
	// ActionMockResponse replaces the upstream response with a synthetic one.
	ActionMockResponse RuleActionType = "mockResponse"
)

// RuleAction is a declarative action returned by a rule evaluation.
type RuleAction struct {
	Type   RuleActionType `json:"type"`
	Params map[string]any `json:"params,omitempty"`
}
