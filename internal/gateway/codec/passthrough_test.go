package codec

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
)

func TestParseIngressAllProtocolsAndProfileAndScrubQuery(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/p/work/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4","stream":true,"metadata":{"user_id":"u1"}}`))
	info, err := ParseIngress(req, []byte(`{"model":"claude-sonnet-4","stream":true,"metadata":{"user_id":"u1"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if info.Profile != "work" || info.Protocol != config.ProtocolAnthropic || info.Family != config.FamilyGenerative || !info.Stream || info.SessionHint != "u1" {
		t.Fatalf("anthropic info = %+v", info)
	}

	gReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:streamGenerateContent?key=AIzaSecret&foo=bar", nil)
	gInfo, err := ParseIngress(gReq, []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if gInfo.Protocol != config.ProtocolGemini || !gInfo.Stream || gInfo.RequestedModel != "gemini-2.5-flash" {
		t.Fatalf("gemini info = %+v", gInfo)
	}
	if strings.Contains(gInfo.CleanQuery, "AIzaSecret") || strings.Contains(gInfo.CleanQuery, "key=") {
		t.Fatalf("CleanQuery leaked secret: %q", gInfo.CleanQuery)
	}

	jReq := httptest.NewRequest(http.MethodPost, "/v1/systemone", nil)
	jInfo, err := ParseIngress(jReq, []byte(`{"model":"decision","state":{},"questions":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if jInfo.Protocol != config.ProtocolTypeSafeJev || jInfo.Family != config.FamilyDecision {
		t.Fatalf("jev info = %+v", jInfo)
	}
}

func TestPatchModelPreservesUnknownFields(t *testing.T) {
	in := []byte(`{
		"model": "claude-sonnet-4",
		"stream": true,
		"thinking": {"type": "enabled", "budget_tokens": 4096},
		"system": [{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}],
		"messages": [{"role": "user", "content": "ok"}]
	}`)
	out, err := PatchModelAndExtraBody(in, config.ProtocolAnthropic, "glm-4.7", map[string]any{"temperature": 0.2})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if m["model"] != "glm-4.7" {
		t.Fatalf("model = %v", m["model"])
	}
	if m["temperature"] != 0.2 {
		t.Fatalf("extraBody missing: %v", m)
	}
	if !strings.Contains(string(out), `"cache_control":{"type":"ephemeral"}`) || !strings.Contains(string(out), `"budget_tokens":4096`) {
		t.Fatalf("lost vendor fields: %s", out)
	}
}

func TestBuildUpstreamURLAndInjectAuth(t *testing.T) {
	u, err := BuildUpstreamURL("https://open.bigmodel.cn/api/coding/paas/v4", config.ProtocolOpenAIChat, "chat", "glm-4.7", "", false)
	if err != nil || u != "https://open.bigmodel.cn/api/coding/paas/v4/chat/completions" {
		t.Fatalf("openai-chat url = %q, %v", u, err)
	}
	gu, err := BuildUpstreamURL("https://generativelanguage.googleapis.com", config.ProtocolGemini, "streamGenerateContent", "gemini-2.5-flash", "foo=1", true)
	if err != nil || gu != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse&foo=1" {
		t.Fatalf("gemini url = %q, %v", gu, err)
	}
	guModelsPrefix, err := BuildUpstreamURL("https://generativelanguage.googleapis.com/v1beta", config.ProtocolGemini, "generateContent", "models/gemini-2.5-flash", "", false)
	if err != nil || guModelsPrefix != "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("gemini models/ prefix url = %q, %v", guModelsPrefix, err)
	}
	for _, tc := range []struct {
		base, action, want string
	}{
		{"https://api.anthropic.com", "messages", "https://api.anthropic.com/v1/messages"},
		{"https://api.anthropic.com/v1", "messages", "https://api.anthropic.com/v1/messages"},
		{"https://api.deepseek.com/anthropic", "messages", "https://api.deepseek.com/anthropic/v1/messages"},
		{"https://api.kimi.com/coding", "messages", "https://api.kimi.com/coding/v1/messages"},
		{"https://api.deepseek.com/anthropic", "count_tokens", "https://api.deepseek.com/anthropic/v1/messages/count_tokens"},
	} {
		got, err := BuildUpstreamURL(tc.base, config.ProtocolAnthropic, tc.action, "m", "", false)
		if err != nil || got != tc.want {
			t.Fatalf("anthropic(%q, %q) = %q (%v), want %q", tc.base, tc.action, got, err, tc.want)
		}
	}

	h := http.Header{}
	h.Set("Authorization", "Bearer client-token")
	h.Set("X-Api-Key", "client-ant")
	InjectUpstreamAuth(h, &config.EndpointSpec{
		BaseURL:    "https://open.bigmodel.cn/api/anthropic",
		APIVersion: "2023-06-01",
		Headers:    map[string]string{"X-Custom": "1"},
	}, config.ProtocolAnthropic, "sk-real-key")
	if h.Get("Authorization") != "" {
		t.Fatalf("client Authorization leaked: %v", h)
	}
	if h.Get("x-api-key") != "sk-real-key" || h.Get("anthropic-version") != "2023-06-01" || h.Get("X-Custom") != "1" {
		t.Fatalf("injected headers = %v", h)
	}
}

func TestUsageTapAndSSEErrorDetection(t *testing.T) {
	tap := NewUsageTap(config.ProtocolAnthropic)
	sse := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":500}}}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","usage":{"output_tokens":42}}`,
		"",
	}, "\n")
	_, _ = tap.Write([]byte(sse))
	u := tap.Usage()
	if u.InputTokens != 100 || u.CacheReadTokens != 500 || u.OutputTokens != 42 {
		t.Fatalf("tapped usage = %+v", u)
	}

	code, msg, isErr := DetectSSEError(config.ProtocolAnthropic, "error", []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	if !isErr || code != "rate_limit_error" || msg != "slow down" {
		t.Fatalf("DetectSSEError = (%q, %q, %v)", code, msg, isErr)
	}

	ku := ExtractUsageJSON(config.ProtocolAnthropic, []byte(`{"usage":{"input_tokens":10,"cache_read_input_tokens":88,"output_tokens":16,"output_tokens_details":{"thinking_tokens":13}}}`))
	if ku.ReasoningTokens != 13 || ku.CacheReadTokens != 88 {
		t.Fatalf("expected thinking_tokens=13, got %+v", ku)
	}
}
