package codec

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
)

func TestOpenAIDecisionsBridge(t *testing.T) {
	req := &ir.DecisionRequest{Model: "virtual", State: map[string]any{"a": 1}, Questions: map[string]ir.DecisionQuestion{"a": {Type: "noul", Instructions: "predicate"}, "b": {Type: "choice", Criteria: map[string]string{"z": "last", "a": "first"}}, "c": {Type: "score", Criteria: []string{"low", "high"}}}}
	b, err := EncodeOpenAIDecisionsRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire decisionWireRequest
	if err = json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Input != `{"a":1}` || wire.Questions[0].Type != "predicate" || wire.Questions[1].Choices[0].Value != "a" {
		t.Fatalf("request: %s", b)
	}
	back, err := DecodeOpenAIDecisionsRequest(b)
	if err != nil || back.Questions["a"].Type != "noul" {
		t.Fatalf("decode: %#v %v", back, err)
	}
	resp := []byte(`{"model":"gpt-6-luna","answers":[{"name":"a","type":"predicate","probability":0.9},{"name":"b","type":"choice","choice":"a","confidence":0.8,"probabilities":{"a":0.8}},{"name":"c","type":"score","score":2.8}],"usage":{"input_tokens":54,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":1}}}`)
	result, err := DecodeOpenAIDecisionsResponse(resp, req)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Calibrated || *result.Answers["a"].Noul != 0.9 || result.Usage.CacheWriteTokens != 2 || result.Usage.CacheReadTokens != 3 || result.Usage.ReasoningTokens != 1 {
		t.Fatalf("response: %#v", result)
	}
	encoded, err := EncodeOpenAIDecisionsResponse(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeOpenAIDecisionsResponse(encoded, req); err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeOpenAIDecisionsResponse([]byte(`{"answers":[{"name":"a","type":"refusal"}]}`), req); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refusal: %v", err)
	}
	if _, err = DecodeOpenAIDecisionsResponse([]byte(`{"answers":[]}`), req); err == nil {
		t.Fatal("missing answer accepted")
	}
}
func TestOpenAIDecisionsIngressURL(t *testing.T) {
	r := httptest.NewRequest("POST", "/p/dev/v1/decisions", nil)
	info, err := ParseIngress(r, []byte(`{"model":"gpt-6-luna","prompt_cache_key":"session"}`))
	if err != nil || info.Protocol != config.ProtocolOpenAIDecisions || info.SessionHint != "session" || info.Profile != "dev" {
		t.Fatalf("ingress %#v: %v", info, err)
	}
	for _, base := range []string{"https://api.openai.com", "https://api.openai.com/v1"} {
		u, err := BuildUpstreamURL(base, config.ProtocolOpenAIDecisions, "decisions", "m", "", false)
		if err != nil || u != "https://api.openai.com/v1/decisions" {
			t.Fatalf("url %q: %v", u, err)
		}
	}
}
