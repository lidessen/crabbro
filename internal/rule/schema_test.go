package rule

import (
	"testing"

	"github.com/lidessen/crabbro/internal/shared"
)

func TestValidateOutput_NoMatch(t *testing.T) {
	out := ruleOutput{Match: false}
	actions, errs := ValidateOutput(out)
	if len(actions) != 0 {
		t.Errorf("expected 0 actions, got %d", len(actions))
	}
	if len(errs) != 0 {
		t.Errorf("expected 0 errors, got %v", errs)
	}
}

func TestValidateOutput_MatchWithoutActions(t *testing.T) {
	out := ruleOutput{Match: true, Actions: nil}
	_, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected error for match=true with no actions")
	}
}

func TestValidateOutput_TagValid(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "tag", Params: map[string]any{"tags": []any{"a", "b"}}},
		},
	}
	actions, errs := ValidateOutput(out)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Type != shared.ActionTag {
		t.Errorf("expected tag, got %s", actions[0].Type)
	}
}

func TestValidateOutput_TagEmpty(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "tag", Params: map[string]any{"tags": []any{}}},
		},
	}
	_, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected error for empty tags")
	}
}

func TestValidateOutput_TagMissing(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "tag", Params: map[string]any{}},
		},
	}
	_, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected error for missing tags")
	}
}

func TestValidateOutput_BlockValid(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "block", Params: map[string]any{"status": int64(403)}},
		},
	}
	actions, errs := ValidateOutput(out)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if actions[0].Type != shared.ActionBlock {
		t.Errorf("expected block, got %s", actions[0].Type)
	}
}

func TestValidateOutput_BlockInvalidStatus(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "block", Params: map[string]any{"status": int64(999)}},
		},
	}
	_, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected error for invalid status")
	}
}

func TestValidateOutput_BlockMissingStatus(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "block", Params: map[string]any{}},
		},
	}
	_, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected error for missing status")
	}
}

func TestValidateOutput_MockResponseValid(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{
				Type: "mockResponse",
				Params: map[string]any{
					"status":  int64(200),
					"headers": map[string]any{"Content-Type": "application/json"},
					"body":    `{"ok":true}`,
				},
			},
		},
	}
	actions, errs := ValidateOutput(out)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if actions[0].Type != shared.ActionMockResponse {
		t.Errorf("expected mockResponse, got %s", actions[0].Type)
	}
}

func TestValidateOutput_MockResponseBodyTooLarge(t *testing.T) {
	body := make([]byte, MaxMockResponseBodyLen+1)
	for i := range body {
		body[i] = 'x'
	}
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{
				Type: "mockResponse",
				Params: map[string]any{
					"status": int64(200),
					"body":   string(body),
				},
			},
		},
	}
	_, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected error for oversized body")
	}
}

func TestValidateOutput_SetRequestHeaderValid(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "setRequestHeader", Params: map[string]any{"name": "X-Custom", "value": "test"}},
		},
	}
	actions, errs := ValidateOutput(out)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if actions[0].Params["name"] != "X-Custom" {
		t.Errorf("expected name=X-Custom")
	}
}

func TestValidateOutput_SetRequestHeaderEmptyName(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "setRequestHeader", Params: map[string]any{"name": "", "value": "test"}},
		},
	}
	_, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected error for empty name")
	}
}

func TestValidateOutput_RemoveRequestHeaderValid(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "removeRequestHeader", Params: map[string]any{"name": "Authorization"}},
		},
	}
	actions, errs := ValidateOutput(out)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if actions[0].Params["name"] != "Authorization" {
		t.Errorf("expected name=Authorization")
	}
}

func TestValidateOutput_AbortValid(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "abort", Params: map[string]any{}},
		},
	}
	actions, errs := ValidateOutput(out)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if actions[0].Type != shared.ActionAbort {
		t.Errorf("expected abort, got %s", actions[0].Type)
	}
}

func TestValidateOutput_RewriteURLValid(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "rewriteURL", Params: map[string]any{"url": "https://other.example.com/path"}},
		},
	}
	actions, errs := ValidateOutput(out)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if actions[0].Params["url"] != "https://other.example.com/path" {
		t.Errorf("unexpected URL, got %v", actions[0].Params["url"])
	}
}

func TestValidateOutput_RewriteURLInvalid(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "rewriteURL", Params: map[string]any{"url": "not-a-url"}},
		},
	}
	_, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected error for invalid URL")
	}
}

func TestValidateOutput_UnknownActionType(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "unknown_action", Params: map[string]any{}},
		},
	}
	_, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected error for unknown action type")
	}
}

func TestValidateOutput_MultipleActionsMixedValidity(t *testing.T) {
	out := ruleOutput{
		Match: true,
		Actions: []ruleAction{
			{Type: "tag", Params: map[string]any{"tags": []any{"good"}}},
			{Type: "nonexistent", Params: map[string]any{}},
			{Type: "block", Params: map[string]any{"status": int64(403)}},
		},
	}
	actions, errs := ValidateOutput(out)
	if len(errs) == 0 {
		t.Error("expected some errors for mixed validity")
	}
	// An invalid action discards every action from the result.
	if len(actions) != 0 {
		t.Errorf("expected 0 actions when any action is invalid, got %d", len(actions))
	}
}
