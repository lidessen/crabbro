// Package ir defines the canonical in-memory representations for generative
// LLM calls (GenIR) and TypeSafe System One decisions (DecisionIR), plus the
// one-way DecisionIR -> GenIR structured-output emulation bridge.
package ir

import (
	"encoding/json"

	"github.com/lidessen/crabbro/internal/shared"
)

const (
	PartText             = "text"
	PartImage            = "image"
	PartThinking         = "thinking"
	PartRedactedThinking = "redacted_thinking"
	PartToolUse          = "tool_use"
	PartToolResult       = "tool_result"

	EventStreamStart   = "stream_start"
	EventThinkingDelta = "thinking_delta"
	EventTextDelta     = "text_delta"
	EventToolCallStart = "tool_call_start"
	EventToolCallDelta = "tool_call_delta"
	EventToolCallEnd   = "tool_call_end"
	EventUsageDelta    = "usage_delta"
	EventStreamEnd     = "stream_end"
	EventStreamError   = "stream_error"

	DecisionKindNoul   = "noul"
	DecisionKindChoice = "choice"
	DecisionKindScore  = "score"
)

// GenRequest is the canonical generative request translated across OpenAI, Anthropic, and Gemini.
type GenRequest struct {
	Model          string
	System         []ContentPart
	Messages       []Message
	Tools          []ToolDecl
	ToolChoice     *ToolChoice
	Thinking       *ThinkingConfig
	ResponseSchema map[string]any
	SchemaName     string
	MaxTokens      int
	Temperature    *float64
	StopSequences  []string
	Stream         bool
	SessionHint    string
	ExtraBody      map[string]any
}

// Message is one turn in a GenRequest conversation.
type Message struct {
	Role  string // "user" | "assistant" | "tool"
	Parts []ContentPart
}

// ContentPart is one typed block inside System, Message, or GenResponse.
type ContentPart struct {
	Kind         string          // text | image | thinking | redacted_thinking | tool_use | tool_result
	Text         string          // text or thinking text
	Signature    string          // vendor-scoped opaque signature; never forwarded cross-provider
	RedactedData string          // encrypted anthropic redacted_thinking payload; never forwarded cross-provider
	MediaType    string          // e.g. image/png
	Data         string          // base64 image bytes or data
	URL          string          // remote/data image URL
	ToolID       string          // tool call ID
	ToolName     string          // tool function name
	InputJSON    json.RawMessage // raw JSON tool arguments
	IsError      bool            // true when tool_result reports a tool error
}

// ToolDecl describes a callable tool/function.
type ToolDecl struct {
	Name        string
	Description string
	Parameters  map[string]any
}

// ToolChoice specifies tool selection mode ("auto" | "any" | "none" | "tool").
type ToolChoice struct {
	Mode string
	Name string
}

// ThinkingConfig specifies extended reasoning budgets.
type ThinkingConfig struct {
	Enabled      bool
	BudgetTokens int
	Effort       string // low | medium | high
}

// GenResponse is a complete non-streaming generative response.
type GenResponse struct {
	ID         string
	Model      string
	StopReason string // stop | tool_calls | max_tokens | content_filter | error
	Content    []ContentPart
	Usage      shared.TokenUsage
}

// StreamEvent is one canonical streaming event in the closed GenIR stream union.
type StreamEvent struct {
	Kind       string
	ID         string
	Model      string
	Index      int
	Text       string
	ToolID     string
	ToolName   string
	StopReason string
	Usage      *shared.TokenUsage
	ErrorCode  string
	ErrorMsg   string
}

// DecisionQuestion mirrors TypeSafe's /v1/systemone question primitives.
type DecisionQuestion struct {
	ID           string `json:"-"`
	Type         string `json:"type"` // noul | choice | score
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// DecisionRequest represents a TypeSafe System One (/v1/systemone) request.
type DecisionRequest struct {
	Model          string                      `json:"model"`
	State          any                         `json:"state"`
	PromptCacheKey string                      `json:"prompt_cache_key,omitempty"`
	Questions      map[string]DecisionQuestion `json:"questions"`
}

// DecisionAnswer represents a single answered question.
type DecisionAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// DecisionResponse represents a TypeSafe System One (/v1/systemone) response.
type DecisionResponse struct {
	Model      string                    `json:"model"`
	Answers    map[string]DecisionAnswer `json:"answers"`
	Usage      shared.TokenUsage         `json:"usage"`
	Calibrated bool                      `json:"calibrated"`
	Mode       string                    `json:"mode,omitempty"` // native | emulated
}
