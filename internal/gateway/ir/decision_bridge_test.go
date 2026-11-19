package ir

import (
	"encoding/json"
	"testing"
)

func TestDecisionToGenAndBack(t *testing.T) {
	dreq := &DecisionRequest{
		Model: "decision",
		State: map[string]any{"prompt": "fix a typo"},
		Questions: map[string]DecisionQuestion{
			"tier": {
				Type:         DecisionKindChoice,
				Instructions: "Pick coding tier",
				Criteria:     map[string]string{"fast": "trivial", "strong": "normal", "frontier": "hard"},
			},
			"needs_tools": {
				Type:         DecisionKindNoul,
				Instructions: "Needs tools?",
			},
		},
	}

	greq, err := DecisionToGenRequest(dreq, "gemini-2.5-flash")
	if err != nil {
		t.Fatal(err)
	}
	if greq.ResponseSchema == nil || greq.Model != "gemini-2.5-flash" {
		t.Fatalf("unexpected greq: %+v", greq)
	}

	// Valid text JSON response
	gresp := &GenResponse{
		Model: "gemini-2.5-flash",
		Content: []ContentPart{{
			Kind: PartText,
			Text: `{"answers":{"tier":{"choice":"fast","confidence":0.92},"needs_tools":{"noul":0.1}}}`,
		}},
	}
	dresp, err := GenToDecisionResponse(dreq, gresp)
	if err != nil {
		t.Fatal(err)
	}
	if dresp.Calibrated || dresp.Mode != "emulated" {
		t.Fatalf("expected uncalibrated emulated mode, got %+v", dresp)
	}
	if dresp.Answers["tier"].Choice != "fast" || *dresp.Answers["needs_tools"].Noul != 0.1 {
		t.Fatalf("answers = %+v", dresp.Answers)
	}

	// Tool-use forced JSON (Anthropic style)
	grespTool := &GenResponse{
		Model: "claude-haiku",
		Content: []ContentPart{{
			Kind:      PartToolUse,
			ToolName:  DecisionToolName,
			InputJSON: json.RawMessage(`{"answers":{"tier":{"choice":"strong","confidence":0.8},"needs_tools":{"noul":0.7}}}`),
		}},
	}
	dresp2, err := GenToDecisionResponse(dreq, grespTool)
	if err != nil || dresp2.Answers["tier"].Choice != "strong" {
		t.Fatalf("tool parse failed: %+v, err=%v", dresp2, err)
	}

	// Out of range / invalid choice rejected
	badResp := &GenResponse{
		Model: "bad",
		Content: []ContentPart{{
			Kind: PartText,
			Text: `{"answers":{"tier":{"choice":"invented","confidence":0.9},"needs_tools":{"noul":1.5}}}`,
		}},
	}
	if _, err := GenToDecisionResponse(dreq, badResp); err == nil {
		t.Fatal("expected validation error for out-of-criteria choice and noul>1")
	}
}
