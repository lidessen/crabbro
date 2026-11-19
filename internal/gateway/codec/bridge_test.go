package codec

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/gateway/router"
)

func testCand(proto, model string) router.Candidate {
	return router.Candidate{
		Plan: &config.Plan{
			ID:         "test-plan",
			Credential: "test_sec",
			Models: map[string]*config.ModelSpec{
				model: {Tier: config.TierStrong, ContextWindow: 128000},
			},
		},
		Target:        config.Target{Plan: "test-plan", Model: model},
		ModelSpec:     &config.ModelSpec{Tier: config.TierStrong, ContextWindow: 128000},
		EndpointProto: proto,
	}
}

type noopFlusher struct{}

func (noopFlusher) Flush() {}

func TestBridgeAnthropicStreamingClientToOpenAIChatUpstream(t *testing.T) {
	br := NewBridge()
	ingress := IngressInfo{
		Protocol:       config.ProtocolAnthropic,
		Family:         config.FamilyGenerative,
		Action:         "messages",
		RequestedModel: "coding-main",
		Stream:         true,
	}
	rawBody := `{
		"model": "coding-main",
		"max_tokens": 1024,
		"system": "Be concise",
		"stream": true,
		"tools": [{"name": "read_file", "description": "Read a file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}}}],
		"messages": [
			{"role": "user", "content": [{"type": "thinking", "thinking": "stale", "signature": "sig-foreign-anth"}, {"type": "text", "text": "Read main.go"}]}
		]
	}`

	cand := testCand(config.ProtocolOpenAIChat, "gpt-5")
	upBody, state, err := br.BuildUpstreamBody(cand, ingress, []byte(rawBody))
	if err != nil {
		t.Fatalf("BuildUpstreamBody: %v", err)
	}
	if strings.Contains(string(upBody), "sig-foreign-anth") {
		t.Fatalf("foreign thinking signature leaked into upstream OpenAI request: %s", upBody)
	}
	if !strings.Contains(string(upBody), `"stream_options":{"include_usage":true}`) {
		t.Fatalf("expected stream_options.include_usage in OpenAI Chat request: %s", upBody)
	}

	upstreamSSE := strings.Join([]string{
		`data: {"id":"chatcmpl-1","model":"gpt-5","choices":[{"index":0,"delta":{"role":"assistant","content":"Checking "}}]}`,
		``,
		`data: {"id":"chatcmpl-1","model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		``,
		`data: {"id":"chatcmpl-1","model":"gpt-5","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"main.go\"}"}}]}}]}`,
		``,
		`data: {"id":"chatcmpl-1","model":"gpt-5","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":42,"completion_tokens":18,"prompt_tokens_details":{"cached_tokens":12}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var out bytes.Buffer
	usage, err := br.TranslateStream(cand, ingress, state, nil, io.NopCloser(strings.NewReader(upstreamSSE)), &out, noopFlusher{})
	if err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	if usage.InputTokens != 42 || usage.OutputTokens != 18 || usage.CacheReadTokens != 12 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	got := out.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"text_delta"`,
		`"tool_use"`,
		`"name":"read_file"`,
		`"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("translated Anthropic SSE missing %q:\n%s", want, got)
		}
	}
}

func TestBridgeOpenAIChatStreamingClientToAnthropicUpstreamDropsForeignSignature(t *testing.T) {
	br := NewBridge()
	ingress := IngressInfo{
		Protocol:       config.ProtocolOpenAIChat,
		Family:         config.FamilyGenerative,
		Action:         "chat",
		RequestedModel: "coding-main",
		Stream:         true,
	}
	rawBody := `{
		"model": "coding-main",
		"stream": true,
		"messages": [
			{"role": "assistant", "reasoning_content": "foreign reasoning", "content": "working"},
			{"role": "user", "content": "continue"}
		]
	}`
	cand := testCand(config.ProtocolAnthropic, "claude-sonnet-4-5")
	upBody, state, err := br.BuildUpstreamBody(cand, ingress, []byte(rawBody))
	if err != nil {
		t.Fatalf("BuildUpstreamBody: %v", err)
	}
	if strings.Contains(string(upBody), "foreign reasoning") {
		t.Fatalf("unsigned cross-provider thinking block must not be sent as Anthropic thinking: %s", upBody)
	}
	if !strings.Contains(string(upBody), `"max_tokens":4096`) {
		t.Fatalf("expected default max_tokens=4096 in Anthropic request: %s", upBody)
	}

	upstreamSSE := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":30,"cache_read_input_tokens":10,"output_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step 1"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Done."}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var out bytes.Buffer
	usage, err := br.TranslateStream(cand, ingress, state, nil, io.NopCloser(strings.NewReader(upstreamSSE)), &out, noopFlusher{})
	if err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	if usage.InputTokens != 30 || usage.OutputTokens != 9 || usage.CacheReadTokens != 10 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	got := out.String()
	for _, want := range []string{
		`"reasoning_content":"step 1"`,
		`"content":"Done."`,
		`"finish_reason":"stop"`,
		`data: [DONE]`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("translated OpenAI Chat SSE missing %q:\n%s", want, got)
		}
	}
}

func TestBridgeOpenAIResponsesToGeminiAndAnthropicMultiTurnToolLoop(t *testing.T) {
	br := NewBridge()

	// 1) OpenAI Responses -> Gemini strips additionalProperties & translates tool response.
	respIngress := IngressInfo{
		Protocol:       config.ProtocolOpenAIResponses,
		Family:         config.FamilyGenerative,
		Action:         "responses",
		RequestedModel: "gem-pro",
		Stream:         false,
	}
	respReq := `{
		"model": "gem-pro",
		"instructions": "Use tools",
		"tools": [{
			"type": "function",
			"name": "lookup",
			"parameters": {"type": "object", "properties": {"id": {"type": "string"}}, "additionalProperties": false}
		}],
		"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "find 7"}]}]
	}`
	gemCand := testCand(config.ProtocolGemini, "gemini-2.5-pro")
	gemReqBytes, state, err := br.BuildUpstreamBody(gemCand, respIngress, []byte(respReq))
	if err != nil {
		t.Fatalf("OpenAI Responses -> Gemini BuildUpstreamBody: %v", err)
	}
	if strings.Contains(string(gemReqBytes), "additionalProperties") {
		t.Fatalf("Gemini payload must strip additionalProperties: %s", gemReqBytes)
	}

	gemRespBody := `{
		"candidates": [{
			"content": {"role": "model", "parts": [{"functionCall": {"name": "lookup", "args": {"id": "7"}}}]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {"promptTokenCount": 15, "candidatesTokenCount": 6}
	}`
	_, _, outResp, usage, err := br.TranslateResponse(gemCand, respIngress, state, 200, make(http.Header), []byte(gemRespBody))
	if err != nil {
		t.Fatalf("Gemini -> OpenAI Responses TranslateResponse: %v", err)
	}
	if usage.InputTokens != 15 || usage.OutputTokens != 6 {
		t.Fatalf("unexpected Gemini usage: %+v", usage)
	}
	if !strings.Contains(string(outResp), `"type":"function_call"`) || !strings.Contains(string(outResp), `"name":"lookup"`) {
		t.Fatalf("expected function_call item in OpenAI Responses output: %s", outResp)
	}

	// Also verify streaming Gemini -> OpenAI Responses.
	respIngressStream := respIngress
	respIngressStream.Stream = true
	gemSSE := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"thinking..."}]}}]}`,
		``,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Answer: 7"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":5}}`,
		``,
	}, "\n")
	var streamOut bytes.Buffer
	sUsage, err := br.TranslateStream(gemCand, respIngressStream, state, nil, io.NopCloser(strings.NewReader(gemSSE)), &streamOut, noopFlusher{})
	if err != nil {
		t.Fatalf("Gemini -> OpenAI Responses TranslateStream: %v", err)
	}
	if sUsage.InputTokens != 20 || sUsage.OutputTokens != 5 {
		t.Fatalf("unexpected stream usage: %+v", sUsage)
	}
	if !strings.Contains(streamOut.String(), "response.reasoning_summary_text.delta") ||
		!strings.Contains(streamOut.String(), "response.output_text.delta") ||
		!strings.Contains(streamOut.String(), "response.completed") {
		t.Fatalf("unexpected OpenAI Responses SSE output:\n%s", streamOut.String())
	}

	// 2) Anthropic multi-turn tool_use + tool_result -> OpenAI Responses function_call + function_call_output.
	anthIngress := IngressInfo{
		Protocol:       config.ProtocolAnthropic,
		Family:         config.FamilyGenerative,
		Action:         "messages",
		RequestedModel: "gpt-resp",
	}
	anthTurn2 := `{
		"model": "gpt-resp",
		"max_tokens": 512,
		"messages": [
			{"role": "user", "content": "check x"},
			{"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "check", "input": {"k": "v"}}]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}]}
		]
	}`
	oaRespCand := testCand(config.ProtocolOpenAIResponses, "gpt-5")
	oaReqBytes, _, err := br.BuildUpstreamBody(oaRespCand, anthIngress, []byte(anthTurn2))
	if err != nil {
		t.Fatalf("Anthropic -> OpenAI Responses multi-turn failed: %v", err)
	}
	if !strings.Contains(string(oaReqBytes), `"type":"function_call"`) || !strings.Contains(string(oaReqBytes), `"type":"function_call_output"`) {
		t.Fatalf("expected function_call and function_call_output in OpenAI Responses request: %s", oaReqBytes)
	}
}

func TestBridgeTypeSafeJevEmulationAcrossOpenAIGeminiAnthropicAndCountTokensGuard(t *testing.T) {
	br := NewBridge()
	jevIngress := IngressInfo{
		Protocol:       config.ProtocolTypeSafeJev,
		Family:         config.FamilyDecision,
		Action:         "systemone",
		RequestedModel: "decision",
	}
	jevReq := `{
		"model": "decision",
		"state": {"query": "error"},
		"questions": {
			"q1": {"type": "choice", "instructions": "pick", "criteria": ["block", "tag"]}
		}
	}`

	// 1) Emulated via OpenAI Chat.
	oaCand := testCand(config.ProtocolOpenAIChat, "gpt-4o-mini")
	oaBody, oaState, err := br.BuildUpstreamBody(oaCand, jevIngress, []byte(jevReq))
	if err != nil {
		t.Fatalf("Jev -> OpenAI BuildUpstreamBody: %v", err)
	}
	if !strings.Contains(string(oaBody), `"json_schema"`) {
		t.Fatalf("expected json_schema in OpenAI request: %s", oaBody)
	}
	oaResp := `{"id":"c1","model":"gpt-4o-mini","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"answers\":{\"q1\":{\"choice\":\"block\",\"confidence\":0.91}}}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	dresp, err := br.DecodeDecisionResponse(oaCand, oaState, []byte(oaResp))
	if err != nil {
		t.Fatalf("Jev <- OpenAI DecodeDecisionResponse: %v", err)
	}
	if dresp.Calibrated || dresp.Mode != "emulated" || dresp.Answers["q1"].Choice != "block" {
		t.Fatalf("unexpected OpenAI emulated DecisionResponse: %+v", dresp)
	}

	// 2) Emulated via Gemini (must strip additionalProperties!).
	gemCand := testCand(config.ProtocolGemini, "gemini-2.5-flash")
	gemBody, gemState, err := br.BuildUpstreamBody(gemCand, jevIngress, []byte(jevReq))
	if err != nil {
		t.Fatalf("Jev -> Gemini BuildUpstreamBody: %v", err)
	}
	if strings.Contains(string(gemBody), "additionalProperties") {
		t.Fatalf("Gemini responseSchema must strip additionalProperties: %s", gemBody)
	}
	gemResp := `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"{\"answers\":{\"q1\":{\"choice\":\"tag\",\"confidence\":0.88}}}"}]}}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4}}`
	drespGem, err := br.DecodeDecisionResponse(gemCand, gemState, []byte(gemResp))
	if err != nil {
		t.Fatalf("Jev <- Gemini DecodeDecisionResponse: %v", err)
	}
	if drespGem.Calibrated || drespGem.Answers["q1"].Choice != "tag" {
		t.Fatalf("unexpected Gemini emulated DecisionResponse: %+v", drespGem)
	}

	// 3) Emulated via Anthropic (forced tool submit_decision without thinking).
	anthCand := testCand(config.ProtocolAnthropic, "claude-haiku")
	anthBody, anthState, err := br.BuildUpstreamBody(anthCand, jevIngress, []byte(jevReq))
	if err != nil {
		t.Fatalf("Jev -> Anthropic BuildUpstreamBody: %v", err)
	}
	var anthMap map[string]any
	_ = json.Unmarshal(anthBody, &anthMap)
	if anthMap["thinking"] != nil {
		t.Fatalf("Anthropic forced tool_choice request must not set thinking: %s", anthBody)
	}
	if !strings.Contains(string(anthBody), `"name":"submit_decision"`) {
		t.Fatalf("expected submit_decision forced tool in Anthropic request: %s", anthBody)
	}
	anthResp := `{"id":"m1","model":"claude-haiku","stop_reason":"tool_use","content":[{"type":"tool_use","id":"tu1","name":"submit_decision","input":{"answers":{"q1":{"choice":"block","confidence":0.95}}}}],"usage":{"input_tokens":12,"output_tokens":7}}`
	_, _, outHTTP, _, err := br.TranslateResponse(anthCand, jevIngress, anthState, 200, make(http.Header), []byte(anthResp))
	if err != nil {
		t.Fatalf("Jev <- Anthropic TranslateResponse: %v", err)
	}
	var parsedHTTP ir.DecisionResponse
	if err := json.Unmarshal(outHTTP, &parsedHTTP); err != nil {
		t.Fatal(err)
	}
	if parsedHTTP.Calibrated || parsedHTTP.Mode != "emulated" || parsedHTTP.Answers["q1"].Choice != "block" {
		t.Fatalf("unexpected HTTP DecisionResponse: %+v", parsedHTTP)
	}

	// 4) count_tokens rejects cross-protocol bridge cleanly.
	ctIngress := IngressInfo{
		Protocol: config.ProtocolAnthropic,
		Family:   config.FamilyGenerative,
		Action:   "count_tokens",
	}
	if _, _, err := br.BuildUpstreamBody(oaCand, ctIngress, []byte(`{"model":"x","messages":[]}`)); err == nil {
		t.Fatal("expected count_tokens cross-protocol bridge to return error")
	}
}
