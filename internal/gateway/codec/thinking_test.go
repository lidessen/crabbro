package codec

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
)

func anthTarget() SignatureOrigin {
	return SignatureOrigin{
		PlanID:   "plan-a",
		Provider: "https://api.a.example/anthropic",
		Protocol: config.ProtocolAnthropic,
		Model:    "claude-a",
	}
}

func decodeJSONMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json: %v\n%s", err, raw)
	}
	return out
}

// Signed thinking is replayed only to the exact upstream that minted it.
// Foreign signatures, unknown signatures, and redacted blobs from another
// provider are removed entirely, and a thinking-only turn disappears instead
// of becoming an empty assistant message.
func TestNormalizeAnthropicThinkingProvenance(t *testing.T) {
	target := anthTarget()
	foreign := SignatureOrigin{
		PlanID:   "plan-b",
		Provider: "https://api.b.example/anthropic",
		Protocol: config.ProtocolAnthropic,
		Model:    "claude-b",
	}
	prov := NewSignatureProvenance()
	prov.Record("sig-a", target)
	prov.Record("redacted-a", target)
	prov.Record("sig-b", foreign)
	prov.Record("redacted-b", foreign)

	body := []byte(`{
		"model":"claude-a",
		"max_tokens":1024,
		"thinking":{"type":"enabled","budget_tokens":2048},
		"messages":[
			{"role":"user","content":"continue"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"mine","signature":"sig-a"},
				{"type":"thinking","thinking":"foreign","signature":"sig-b"},
				{"type":"redacted_thinking","data":"redacted-a"},
				{"type":"redacted_thinking","data":"redacted-b"},
				{"type":"thinking","thinking":"unknown","signature":"sig-unknown"},
				{"type":"text","text":"kept"}
			]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"only foreign","signature":"sig-b"}
			]},
			{"role":"user","content":"next"}
		]
	}`)
	out, err := NormalizeSameProtocolBody(body, config.ProtocolAnthropic, target, prov)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{`"sig-a"`, `"redacted-a"`, `"kept"`, `"thinking":{"type":"enabled"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("normalized body missing %q:\n%s", want, text)
		}
	}
	for _, banned := range []string{"sig-b", "redacted-b", "sig-unknown", "only foreign"} {
		if strings.Contains(text, banned) {
			t.Fatalf("normalized body leaked foreign thinking %q:\n%s", banned, text)
		}
	}
	doc := decodeJSONMap(t, out)
	messages := doc["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("thinking-only assistant turn must be dropped, got %d messages:\n%s", len(messages), text)
	}
	for _, raw := range messages {
		msg := raw.(map[string]any)
		if msg["role"] == "assistant" {
			content := msg["content"].([]any)
			if len(content) == 0 {
				t.Fatalf("empty assistant content array survived:\n%s", text)
			}
		}
	}
}

// Unknown provenance (including history from before a gateway restart) is
// stripped even when no foreign origin was ever observed.
func TestNormalizeAnthropicUnknownSignatureStripsThinkingOnlyTurn(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"sig-after-restart"}]}
	]}`)
	out, err := NormalizeSameProtocolBody(body, config.ProtocolAnthropic, anthTarget(), NewSignatureProvenance())
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeJSONMap(t, out)
	messages := doc["messages"].([]any)
	if len(messages) != 1 || strings.Contains(string(out), "sig-after-restart") || strings.Contains(string(out), "private") {
		t.Fatalf("unknown signed thinking must be stripped cleanly:\n%s", out)
	}
}

func TestNormalizeGeminiThoughtSignatures(t *testing.T) {
	target := SignatureOrigin{
		PlanID:   "gem-plan",
		Provider: "https://generativelanguage.example",
		Protocol: config.ProtocolGemini,
		Model:    "gemini-pro",
	}
	prov := NewSignatureProvenance()
	prov.Record("sig-gem", target)
	body := []byte(`{"contents":[
		{"role":"model","parts":[
			{"thought":true,"thoughtSignature":"sig-gem","text":"mine"},
			{"thought":true,"thoughtSignature":"sig-other","text":"foreign"}
		]},
		{"role":"model","parts":[
			{"functionCall":{"name":"lookup","args":{"id":"7"}},"thoughtSignature":"sig-other"}
		]},
		{"role":"user","parts":[{"text":"go"}]}
	]}`)
	out, err := NormalizeSameProtocolBody(body, config.ProtocolGemini, target, prov)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, "sig-gem") || !strings.Contains(text, `"name":"lookup"`) {
		t.Fatalf("matching thought signature / functionCall missing:\n%s", text)
	}
	for _, banned := range []string{"sig-other", "foreign"} {
		if strings.Contains(text, banned) {
			t.Fatalf("foreign Gemini thought leaked %q:\n%s", banned, text)
		}
	}
	doc := decodeJSONMap(t, out)
	contents := doc["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want the functionCall and user turns preserved:\n%s", len(contents), text)
	}
}

func TestSignatureStreamTapRecordsAnthropicSignatures(t *testing.T) {
	target := anthTarget()
	prov := NewSignatureProvenance()
	tap := NewSignatureStreamTap(config.ProtocolAnthropic, target, prov)
	stream := strings.Join([]string{
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"redacted-stream"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"sig-str"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"signature_delta","signature":"eam"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
	}, "\n")
	// Split writes prove the tap handles arbitrary SSE chunk boundaries.
	for i := 0; i < len(stream); i += 7 {
		end := i + 7
		if end > len(stream) {
			end = len(stream)
		}
		_, _ = tap.Write([]byte(stream[i:end]))
	}
	tap.Close()
	if !prov.Matches("sig-stream", target) || !prov.Matches("redacted-stream", target) {
		t.Fatal("stream tap did not record Anthropic signature provenance")
	}
}

func TestSignatureStreamTapRecordsGeminiSignatures(t *testing.T) {
	target := SignatureOrigin{PlanID: "g", Provider: "https://g.example", Protocol: config.ProtocolGemini, Model: "gem"}
	prov := NewSignatureProvenance()
	tap := NewSignatureStreamTap(config.ProtocolGemini, target, prov)
	_, _ = tap.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"thoughtSignature\":\"sig-gem-stream\"}]}}]}\n\n"))
	tap.Close()
	if !prov.Matches("sig-gem-stream", target) {
		t.Fatal("stream tap did not record Gemini thoughtSignature")
	}
}

// A thinking-only Anthropic turn must never become {"role":"assistant"} with
// neither content nor tool_calls when converted to OpenAI Chat.
func TestOpenAIChatEncodeDropsThinkingOnlyAssistantTurn(t *testing.T) {
	req := &ir.GenRequest{
		Model: "gpt-5",
		Messages: []ir.Message{
			{Role: "user", Parts: []ir.ContentPart{{Kind: ir.PartText, Text: "read"}}},
			{Role: "assistant", Parts: []ir.ContentPart{{Kind: ir.PartThinking, Text: "planning only"}}},
			{Role: "user", Parts: []ir.ContentPart{{Kind: ir.PartText, Text: "continue"}}},
		},
	}
	body, err := (oaChatCodec{}).EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Messages) != 2 {
		t.Fatalf("thinking-only assistant must be dropped, messages=%v", doc.Messages)
	}
	for _, msg := range doc.Messages {
		if msg["role"] != "assistant" {
			continue
		}
		_, hasContent := msg["content"]
		_, hasTools := msg["tool_calls"]
		if !hasContent && !hasTools {
			t.Fatalf("empty assistant message emitted: %v", msg)
		}
	}
}

// A thinking + tool_use turn remains a valid assistant tool-call message.
func TestOpenAIChatEncodeKeepsThinkingToolUseTurn(t *testing.T) {
	req := &ir.GenRequest{Model: "gpt-5", Messages: []ir.Message{{
		Role: "assistant",
		Parts: []ir.ContentPart{
			{Kind: ir.PartThinking, Text: "planning"},
			{Kind: ir.PartToolUse, ToolID: "call_1", ToolName: "read", InputJSON: json.RawMessage(`{"path":"a"}`)},
		},
	}}}
	body, err := (oaChatCodec{}).EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Messages) != 1 || doc.Messages[0]["tool_calls"] == nil {
		t.Fatalf("tool-call turn missing: %s", body)
	}
}

// Cross-protocol Gemini conversion must not emit empty model turns or
// functionCall/functionResponse blocks without the required name.
func TestGeminiEncodeRequestDropsMalformedTurns(t *testing.T) {
	req := &ir.GenRequest{
		Model: "gemini-pro",
		Messages: []ir.Message{
			{Role: "assistant", Parts: []ir.ContentPart{{Kind: ir.PartThinking, Text: "only thinking"}}},
			{Role: "assistant", Parts: []ir.ContentPart{
				{Kind: ir.PartToolUse, ToolID: "call_1", ToolName: ""},
				{Kind: ir.PartToolResult, ToolID: "call_missing"},
			}},
			{Role: "user", Parts: []ir.ContentPart{{Kind: ir.PartText, Text: "hello"}}},
		},
	}
	body, err := (geminiCodec{}).EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "functionCall") || strings.Contains(text, "functionResponse") {
		t.Fatalf("malformed Gemini tool turn emitted: %s", text)
	}
	var doc struct {
		Contents []map[string]any `json:"contents"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Contents) != 1 {
		t.Fatalf("contents = %d, want only the user turn: %s", len(doc.Contents), text)
	}
	if parts := doc.Contents[0]["parts"].([]any); len(parts) != 1 {
		t.Fatalf("user parts = %v", parts)
	}
}

// Cross-protocol Anthropic -> OpenAI Chat never forwards redacted encrypted
// thinking, and leaves no empty assistant skeleton behind.
func TestBridgeDropsRedactedThinkingAcrossProtocols(t *testing.T) {
	br := NewBridge()
	raw := []byte(`{
		"model":"coding-main",
		"max_tokens":512,
		"messages":[
			{"role":"user","content":"start"},
			{"role":"assistant","content":[
				{"type":"redacted_thinking","data":"encrypted-anthropic-blob"},
				{"type":"text","text":"done"}
			]}
		]
	}`)
	cand := testCand(config.ProtocolOpenAIChat, "gpt-5")
	body, _, err := br.BuildUpstreamBody(cand, IngressInfo{
		Protocol:       config.ProtocolAnthropic,
		Family:         config.FamilyGenerative,
		Action:         "messages",
		RequestedModel: "coding-main",
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "encrypted-anthropic-blob") || strings.Contains(string(body), "redacted") {
		t.Fatalf("redacted thinking leaked across protocols: %s", body)
	}

	// A redacted-only assistant turn must disappear rather than becoming an
	// empty assistant message.
	raw = []byte(`{
		"model":"coding-main",
		"max_tokens":512,
		"messages":[
			{"role":"user","content":"start"},
			{"role":"assistant","content":[{"type":"redacted_thinking","data":"encrypted-only"}]},
			{"role":"user","content":"continue"}
		]
	}`)
	body, _, err = br.BuildUpstreamBody(cand, IngressInfo{
		Protocol:       config.ProtocolAnthropic,
		Family:         config.FamilyGenerative,
		Action:         "messages",
		RequestedModel: "coding-main",
	}, raw)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Messages) != 2 {
		t.Fatalf("redacted-only assistant turn must be dropped: %s", body)
	}
	for _, msg := range doc.Messages {
		if msg["role"] == "assistant" {
			t.Fatalf("empty assistant survived: %s", body)
		}
	}
}

// Anthropic decode keeps redacted thinking in a dedicated IR kind so it can
// be stripped rather than mis-encoded as ordinary thinking.
func TestAnthropicDecodeRedactedThinking(t *testing.T) {
	req, err := (anthropicCodec{}).DecodeRequest([]byte(`{
		"model":"claude",
		"max_tokens":1024,
		"messages":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"visible","signature":"sig"},
				{"type":"redacted_thinking","data":"opaque"},
				{"type":"text","text":"hello"}
			]}
		]
	}`), IngressInfo{Protocol: config.ProtocolAnthropic, RequestedModel: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	var redacted, thinking int
	for _, part := range req.Messages[0].Parts {
		switch part.Kind {
		case ir.PartRedactedThinking:
			redacted++
			if part.RedactedData != "opaque" {
				t.Fatalf("redacted data = %q", part.RedactedData)
			}
		case ir.PartThinking:
			thinking++
			if part.Signature != "sig" {
				t.Fatalf("signature = %q", part.Signature)
			}
		}
	}
	if redacted != 1 || thinking != 1 {
		t.Fatalf("redacted=%d thinking=%d parts=%+v", redacted, thinking, req.Messages[0].Parts)
	}
}
