package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/capture"
	"github.com/lidessen/crabbro/internal/decision"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/session"
	"github.com/lidessen/crabbro/internal/shared"
)

func TestE2ECrossProtocolStreamingToolLoopAndUsageWithoutHistory(t *testing.T) {
	dir := t.TempDir()
	sec := secret.New(filepath.Join(dir, "secrets"))
	if err := sec.Set("oa_key", "sk-oa-top-secret-value"); err != nil {
		t.Fatal(err)
	}

	var turnCount atomic.Int32
	oaUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		turn := turnCount.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if turn == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "data: {\"id\":\"cmpl_1\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"\"}}]}}]}\n\n")
			io.WriteString(w, "data: {\"id\":\"cmpl_1\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"go.mod\\\"}\"}}]}}]}\n\n")
			io.WriteString(w, "data: {\"id\":\"cmpl_1\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20}}\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		if !strings.Contains(string(raw), `"role":"tool"`) || !strings.Contains(string(raw), `"tool_call_id":"call_abc"`) {
			t.Errorf("turn 2 expected OpenAI role:tool tool_call_id=call_abc, got: %s", raw)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"id\":\"cmpl_2\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Module crabbro.\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"cmpl_2\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":150,\"completion_tokens\":15,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer oaUpstream.Close()

	rb := ring.NewRequestBuffer(32)
	bc := capture.NewBodyCache(1<<20, 64*1024)
	hub := capture.NewEventHub()
	// History deliberately nil (--history off) to assert usage.db remains independent.
	sink := capture.NewDefaultSink(rb, bc, hub, session.New(32), nil)

	gw, err := New(Options{
		ListenAddr: "127.0.0.1:0",
		RootDir:    filepath.Join(dir, "gateway"),
	}, sec, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()

	planYAML := fmt.Sprintf(`version: 1
id: openai-plan
credential: oa_key
endpoints:
  openai-chat:
    baseUrl: %s
models:
  gpt-5:
    tier: strong
    codingScore: 0.9
    capabilities:
      tools: true
`, oaUpstream.URL)
	routeYAML := `version: 1
id: code-bridge
enabled: true
family: generative
match:
  protocols: [anthropic]
  models: ["coding-main"]
policy:
  preset: balanced
targets:
  - plan: openai-plan
    model: gpt-5
`
	if err := gw.Config.Draft(config.KindPlan, "openai-plan", []byte(planYAML)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Config.Commit(config.KindPlan, "openai-plan"); err != nil {
		t.Fatal(err)
	}
	if err := gw.Config.Draft(config.KindRoute, "code-bridge", []byte(routeYAML)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Config.Commit(config.KindRoute, "code-bridge"); err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(); err != nil {
		t.Fatal(err)
	}

	baseURL := "http://" + gw.Addr()

	// Turn 1: Anthropic streaming request with tools -> bridged to OpenAI Chat.
	t1Body := `{"model":"coding-main","max_tokens":1024,"stream":true,"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"messages":[{"role":"user","content":"read go.mod"}]}`
	resp1, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(t1Body))
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	if resp1.StatusCode != 200 || resp1.Header.Get("X-Crabbro-Translated") != "1" {
		t.Fatalf("turn 1 status=%d hdr=%v body=%s", resp1.StatusCode, resp1.Header, b1)
	}
	if !strings.Contains(string(b1), `"type":"tool_use"`) || !strings.Contains(string(b1), `"stop_reason":"tool_use"`) {
		t.Fatalf("turn 1 Anthropic SSE missing tool_use:\n%s", b1)
	}

	// Turn 2: Anthropic tool_result -> bridged to OpenAI role:tool.
	t2Body := `{"model":"coding-main","max_tokens":1024,"stream":true,"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],"messages":[{"role":"user","content":"read go.mod"},{"role":"assistant","content":[{"type":"tool_use","id":"call_abc","name":"read_file","input":{"path":"go.mod"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_abc","content":"module github.com/lidessen/crabbro"}]}]}`
	resp2, err := http.Post(baseURL+"/v1/messages", "application/json", strings.NewReader(t2Body))
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || !strings.Contains(string(b2), "Module crabbro.") {
		t.Fatalf("turn 2 unexpected SSE:\n%s", b2)
	}

	usage, err := gw.Usage("24h", "")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Totals.Requests != 2 || usage.Totals.InputTokens != 250 || usage.Totals.OutputTokens != 35 || usage.Totals.CacheReadTokens != 80 {
		t.Fatalf("usage.db did not accumulate translated streams independently of history: %+v", usage.Totals)
	}

	reqs := rb.List(0)
	if len(reqs) != 2 || reqs[1].Gateway == nil || !reqs[1].Gateway.Translated || reqs[1].Gateway.Usage == nil {
		t.Fatalf("captured requests missing gateway trace: %+v", reqs)
	}
}

func TestE2ESemanticTieringColdBoundaryAndGeminiKeyScrubbing(t *testing.T) {
	dir := t.TempDir()
	sec := secret.New(filepath.Join(dir, "secrets"))
	_ = sec.Set("fast_key", "sk-fast-upstream-secret")
	_ = sec.Set("front_key", "sk-frontier-upstream-secret")
	_ = sec.Set("jev_key", "sk-jev-upstream-secret")

	var decisionCalls atomic.Int32
	jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decisionCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		conf := 0.95
		_ = json.NewEncoder(w).Encode(ir.DecisionResponse{
			Model: "jev-latest",
			Answers: map[string]ir.DecisionAnswer{
				"tier": {Type: ir.DecisionKindChoice, Choice: "fast", Confidence: &conf},
			},
		})
	}))
	defer jevSrv.Close()

	gemSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "" {
			t.Errorf("upstream Gemini URL leaked client ?key query param: %s", r.URL.RawQuery)
		}
		if r.Header.Get("x-goog-api-key") != "sk-fast-upstream-secret" {
			t.Errorf("expected upstream x-goog-api-key header, got %q", r.Header.Get("x-goog-api-key"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"hi fast (ok)"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}`)
	}))
	defer gemSrv.Close()

	frontSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"hi frontier"}]}}]}`)
	}))
	defer frontSrv.Close()

	rb := ring.NewRequestBuffer(64)
	bc := capture.NewBodyCache(1<<20, 64*1024)
	hub := capture.NewEventHub()
	sink := capture.NewDefaultSink(rb, bc, hub, session.New(32), nil)

	gw, err := New(Options{
		ListenAddr: "127.0.0.1:0",
		RootDir:    filepath.Join(dir, "gateway"),
	}, sec, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()

	for _, p := range []struct {
		id, yaml string
	}{
		{"fast-gem", fmt.Sprintf("version: 1\nid: fast-gem\ncredential: fast_key\nendpoints:\n  gemini:\n    baseUrl: %s\nmodels:\n  gemini-2.5-flash:\n    tier: fast\n", gemSrv.URL)},
		{"front-gem", fmt.Sprintf("version: 1\nid: front-gem\ncredential: front_key\nendpoints:\n  gemini:\n    baseUrl: %s\nmodels:\n  gemini-2.5-pro:\n    tier: frontier\n", frontSrv.URL)},
		{"jev-dec", fmt.Sprintf("version: 1\nid: jev-dec\ncredential: jev_key\nendpoints:\n  typesafe-jev:\n    baseUrl: %s\nmodels:\n  jev-latest:\n    tier: fast\n", jevSrv.URL)},
	} {
		if err := gw.Config.Draft(config.KindPlan, p.id, []byte(p.yaml)); err != nil {
			t.Fatal(err)
		}
		if err := gw.Config.Commit(config.KindPlan, p.id); err != nil {
			t.Fatal(err)
		}
	}

	decRoute := `version: 1
id: decision
enabled: true
family: decision
match:
  protocols: [typesafe-jev]
policy:
  preset: balanced
targets:
  - plan: jev-dec
    model: jev-latest
`
	genRoute := `version: 1
id: auto-tier
enabled: true
family: generative
match:
  protocols: [gemini]
  models: ["smart-gemini"]
policy:
  preset: quality-first
  semanticTiering:
    enabled: true
    decisionRoute: decision
targets:
  - plan: front-gem
    model: gemini-2.5-pro
    whenTier: [frontier]
  - plan: fast-gem
    model: gemini-2.5-flash
    whenTier: [fast]
`
	for _, rt := range []struct{ id, yaml string }{{"decision", decRoute}, {"auto-tier", genRoute}} {
		if err := gw.Config.Draft(config.KindRoute, rt.id, []byte(rt.yaml)); err != nil {
			t.Fatal(err)
		}
		if err := gw.Config.Commit(config.KindRoute, rt.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := gw.Start(); err != nil {
		t.Fatal(err)
	}

	// Send Gemini request with ?key=sk-client-leak-secret on cold turn.
	clientSecret := "sk-client-leak-secret"
	resp, err := http.Post("http://"+gw.Addr()+"/v1beta/models/smart-gemini:generateContent?key="+clientSecret+"&foo=bar", "application/json",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	outBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(outBytes), "hi fast") {
		t.Fatalf("unexpected semanticTiering response (%d): %s", resp.StatusCode, outBytes)
	}
	if decisionCalls.Load() != 1 {
		t.Fatalf("expected 1 cold-boundary decision call without recursion, got %d", decisionCalls.Load())
	}

	// Verify captured requests/bodies scrubbed BOTH upstream secret AND Gemini ?key=...
	for _, r := range rb.List(0) {
		raw, _ := json.Marshal(r)
		s := string(raw)
		for _, forbidden := range []string{clientSecret, "sk-fast-upstream-secret", "sk-jev-upstream-secret"} {
			if strings.Contains(s, forbidden) {
				t.Fatalf("captured request %s leaked secret %q: %s", r.ID, forbidden, s)
			}
		}
		if r.URL != "" && strings.Contains(r.URL, clientSecret) {
			t.Fatalf("captured URL contained ?key secret: %s", r.URL)
		}
	}

	// Unified internal/decision Client backed by the same Gateway.
	dc := decision.New(gw, "decision")
	if !dc.Status().Configured {
		t.Fatal("expected decision client configured")
	}

	// Check ToolLoop turn does NOT re-call semanticTiering decision engine.
	resp2, err := http.Post("http://"+gw.Addr()+"/v1beta/models/smart-gemini:generateContent", "application/json",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"functionCall":{"name":"t","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"t","response":{"result":"ok"}}}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if decisionCalls.Load() != 1 {
		t.Fatalf("expected ToolLoop turn to skip semanticTiering (still 1 call), got %d", decisionCalls.Load())
	}
}

func TestE2EToolLoopCachePinTTLExpiryAndDecisionHTTP(t *testing.T) {
	dir := t.TempDir()
	sec := secret.New(filepath.Join(dir, "secrets"))
	_ = sec.Set("k1", "sk-1")
	_ = sec.Set("k2", "sk-2")

	var p1Hits, p2Hits atomic.Int32
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p1Hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m1","model":"m","stop_reason":"end_turn","content":[{"type":"text","text":"p1"}],"usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer up1.Close()

	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p2Hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c2","model":"gpt","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"{\"answers\":{\"score\":{\"score\":0.75,\"confidence\":0.9}}}"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer up2.Close()

	gw, err := New(Options{
		ListenAddr: "127.0.0.1:0",
		RootDir:    filepath.Join(dir, "gateway"),
	}, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()

	p1YAML := fmt.Sprintf(`version: 1
id: p1
credential: k1
cache:
  ttl: 50ms
limits:
  windows:
    - name: daily
      duration: 24h
      maxTokens: 10000
      mode: soft
endpoints:
  anthropic:
    baseUrl: %s
models:
  m-p1:
    tier: strong
    codingScore: 0.90
`, up1.URL)
	p2YAML := fmt.Sprintf(`version: 1
id: p2
credential: k2
endpoints:
  openai-chat:
    baseUrl: %s
models:
  m-p2:
    tier: fast
    codingScore: 0.80
`, up2.URL)
	rtGen := `version: 1
id: gen
enabled: true
family: generative
match:
  protocols: [anthropic]
  models: ["coding"]
policy:
  preset: quality-first
targets:
  - plan: p1
    model: m-p1
  - plan: p2
    model: m-p2
`
	rtDec := `version: 1
id: dec
enabled: true
family: decision
match:
  protocols: [typesafe-jev]
policy:
  preset: balanced
  allowEmulation: true
targets:
  - plan: p2
    model: m-p2
    endpointProtocol: openai-chat
`
	for _, cfg := range []struct{ k, id, y string }{
		{config.KindPlan, "p1", p1YAML},
		{config.KindPlan, "p2", p2YAML},
		{config.KindRoute, "gen", rtGen},
		{config.KindRoute, "dec", rtDec},
	} {
		if err := gw.Config.Draft(cfg.k, cfg.id, []byte(cfg.y)); err != nil {
			t.Fatal(err)
		}
		if err := gw.Config.Commit(cfg.k, cfg.id); err != nil {
			t.Fatal(err)
		}
	}
	if err := gw.Start(); err != nil {
		t.Fatal(err)
	}

	req1, _ := http.NewRequest(http.MethodPost, "http://"+gw.Addr()+"/v1/messages",
		bytes.NewBufferString(`{"model":"coding","max_tokens":64,"messages":[{"role":"user","content":"turn 1"}]}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("X-Crabbro-Session", "sess-ttl-1")
	r1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	r1.Body.Close()
	if r1.Header.Get("X-Crabbro-Plan") != "p1" {
		t.Fatalf("turn 1 plan = %q, want p1", r1.Header.Get("X-Crabbro-Plan"))
	}

	// Turn 2 with tool_result stays hard-pinned to p1 while within 50ms TTL.
	req2, _ := http.NewRequest(http.MethodPost, "http://"+gw.Addr()+"/v1/messages",
		bytes.NewBufferString(`{"model":"coding","max_tokens":64,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Crabbro-Session", "sess-ttl-1")
	r2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.Header.Get("X-Crabbro-Plan") != "p1" || p1Hits.Load() != 2 {
		t.Fatalf("turn 2 expected ToolLoop pin to p1, got plan=%q p1Hits=%d", r2.Header.Get("X-Crabbro-Plan"), p1Hits.Load())
	}

	// Wait past 50ms TTL so CacheGravity decays completely.
	time.Sleep(80 * time.Millisecond)

	// HTTP /v1/systemone decision endpoint emulated via OpenAI Chat returns calibrated=false, mode=emulated.
	jevResp, err := http.Post("http://"+gw.Addr()+"/v1/systemone", "application/json",
		strings.NewReader(`{"model":"dec","state":{"ok":true},"questions":{"score":{"type":"score","instructions":"rate"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	jb, _ := io.ReadAll(jevResp.Body)
	jevResp.Body.Close()
	if jevResp.StatusCode != 200 {
		t.Fatalf("POST /v1/systemone = %d: %s", jevResp.StatusCode, jb)
	}
	var dresp shared.GatewayTrace
	_ = dresp
	var parsed ir.DecisionResponse
	if err := json.Unmarshal(jb, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Calibrated || parsed.Mode != "emulated" || parsed.Answers["score"].Score == nil || *parsed.Answers["score"].Score != 0.75 {
		t.Fatalf("unexpected HTTP emulated DecisionResponse: %+v", parsed)
	}
	if p2Hits.Load() != 1 {
		t.Fatalf("expected p2Hits=1, got %d", p2Hits.Load())
	}
}
