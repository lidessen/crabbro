package api

import "time"

// RuleID is the stable identifier for a rule.
type RuleID string

// Rule represents one rule. Script is either a YAML document (version 1) or a
// legacy JavaScript rule, which the engine loads as a single run stage.
type Rule struct {
	ID       RuleID `json:"id"`
	Name     string `json:"name,omitempty"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
	Script   string `json:"script"`
	// Source is "file" for rules loaded from rules/<id>.yaml. Empty means the
	// rule was created through the API and is not replaced by a directory sync.
	Source string `json:"source,omitempty"`
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
	// Phase is "request", "response", "frame", or "command".
	Phase string `json:"phase,omitempty"`
	// Stages records pipeline decisions. Values that came from credentials are
	// not included.
	Stages []StageTrace `json:"stages,omitempty"`
	// Vars is the resolved variable object. It is not a credential store.
	Vars map[string]any `json:"vars,omitempty"`
}

// StageTrace is one pipeline stage. SkipReason is "if" or "unselected".
type StageTrace struct {
	ID         string         `json:"id,omitempty"`
	Uses       string         `json:"uses,omitempty"`
	Skipped    bool           `json:"skipped,omitempty"`
	SkipReason string         `json:"skipReason,omitempty"`
	Case       string         `json:"case,omitempty"`
	Effect     string         `json:"effect,omitempty"`
	Error      string         `json:"error,omitempty"`
	Outputs    map[string]any `json:"outputs,omitempty"`
}

// RuleActionType enumerates the supported rule action types.
type RuleActionType string

const (
	// ActionGatewayOffload sends a request to the local gateway.
	ActionGatewayOffload RuleActionType = "gateway-offload"
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
	// ActionSetResponseHeader sets a response header on the way back to the client.
	ActionSetResponseHeader RuleActionType = "setResponseHeader"
	// ActionRemoveResponseHeader removes a response header.
	ActionRemoveResponseHeader RuleActionType = "removeResponseHeader"
	// ActionRewriteResponseBody replaces or rewrites the response body.
	ActionRewriteResponseBody RuleActionType = "rewriteResponseBody"
	// ActionSetResponseStatus changes the response status.
	ActionSetResponseStatus RuleActionType = "setResponseStatus"
	// ActionDelay waits before the response is written.
	ActionDelay RuleActionType = "delay"
	// ActionThrottle limits the response body rate.
	ActionThrottle RuleActionType = "throttle"
	// ActionReplaceFrame replaces a reassembled WebSocket message payload.
	ActionReplaceFrame RuleActionType = "replaceFrame"
	// ActionDropFrame drops a reassembled WebSocket message.
	ActionDropFrame RuleActionType = "dropFrame"
	// ActionRespondFrame intercepts a client message and replies with a text message.
	ActionRespondFrame RuleActionType = "respondFrame"
)

// FrameView is a complete reassembled WebSocket text or binary message shown
// to a rule. Fin is always true because rules run only after the final fragment.
// Ping, pong, and close control frames are not shown to rules.
type FrameView struct {
	Direction string `json:"direction"`
	Opcode    string `json:"opcode"`
	Payload   string `json:"payload"`
	Fin       bool   `json:"fin"`
}

// RuleAction is a declarative action returned by a rule evaluation.
type RuleAction struct {
	Type   RuleActionType `json:"type"`
	Params map[string]any `json:"params,omitempty"`
}
