package codec

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/ir"
)

func TestGeminiDecisionsCodec(t *testing.T) {
	req := &ir.DecisionRequest{State: map[string]any{"item": "broken"}, Questions: map[string]ir.DecisionQuestion{
		"damaged":  {Type: "noul", Instructions: "Damaged?"},
		"queue":    {Type: "choice", Instructions: "Queue?", Criteria: map[string]string{"billing": "Payment", "replacements": "Damage"}},
		"severity": {Type: "score", Instructions: "Severity?", Criteria: map[string]string{"a": "None", "b": "Minor", "c": "Broken"}},
	}}
	b, err := EncodeGeminiDecisionRequest(req, "gemini-decision-1")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Model     string `json:"model"`
		Input     string `json:"input"`
		Questions map[string]struct {
			Type     string `json:"type"`
			Criteria any    `json:"criteria"`
		} `json:"questions"`
	}
	if err = json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w.Model != "gemini-decision-1" || w.Input != `{"item":"broken"}` || w.Questions["damaged"].Type != "noul" || !reflect.DeepEqual(w.Questions["queue"].Criteria, map[string]any{"billing": "Payment", "replacements": "Damage"}) || !reflect.DeepEqual(w.Questions["severity"].Criteria, []any{"None", "Minor", "Broken"}) {
		t.Fatalf("request: %s", b)
	}
	response := `{"answers":{"damaged":{"type":"noul","noul":0.9,"confidence":0.8},"queue":{"type":"choice","choice":"replacements","confidence":0.9,"probabilities":{"billing":0.1,"replacements":0.9}},"severity":{"type":"score","score":1.9,"confidence":0.9,"probabilities":{"0":0.1,"1":0.2,"2":0.7},"legend":{"0":"None","1":"Minor","2":"Broken"}}},"usage":{"total_input_tokens":138,"total_output_tokens":0}}`
	got, err := DecodeGeminiDecisionResponse([]byte(response), req, "gemini-decision-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "native" || !got.Calibrated || got.Usage.InputTokens != 138 || got.Usage.OutputTokens != 0 || *got.Answers["damaged"].Noul != 0.9 || *got.Answers["damaged"].Confidence != 0.8 || got.Answers["queue"].Choice != "replacements" || got.Answers["queue"].Probabilities["billing"] != 0.1 || *got.Answers["severity"].Score != 1.9 || got.Answers["severity"].Probabilities["c"] != 0.7 {
		t.Fatalf("response: %#v", got)
	}
	for _, criteria := range []any{[]any{map[string]any{"label": "low", "description": "None"}, map[string]any{"label": "high", "description": "Broken"}}, []string{"None", "Broken"}, map[string]any{"a": map[string]any{"description": "None"}, "b": map[string]any{"description": "Broken"}}} {
		req.Questions["severity"] = ir.DecisionQuestion{Type: "score", Criteria: criteria}
		b, err = EncodeGeminiDecisionRequest(req, "m")
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, &w); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(w.Questions["severity"].Criteria, []any{"None", "Broken"}) {
			t.Fatalf("criteria: %s", b)
		}
	}
	if _, err = DecodeGeminiDecisionResponse([]byte(`{"answers":{}}`), req, "m"); err == nil {
		t.Fatal("missing answer accepted")
	}
}
