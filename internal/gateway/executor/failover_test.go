package executor

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/codec"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/router"
	"github.com/lidessen/crabbro/internal/secret"
)

type testUpstream struct {
	server *httptest.Server
	hits   atomic.Int32
	body   atomic.Value // string
}

func newTestUpstream(t *testing.T, handler http.HandlerFunc) *testUpstream {
	t.Helper()
	up := &testUpstream{}
	up.body.Store("")
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.hits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		up.body.Store(string(raw))
		handler(w, r)
	}))
	t.Cleanup(up.server.Close)
	return up
}

func newExecutorFixture(t *testing.T, upstreams ...*testUpstream) (*Executor, *config.Snapshot) {
	t.Helper()
	sec := secret.New(t.TempDir())
	plans := map[string]*config.Plan{}
	targets := make([]config.Target, 0, len(upstreams))
	for i, up := range upstreams {
		id := fmt.Sprintf("plan-%d", i)
		model := fmt.Sprintf("claude-%d", i)
		if err := sec.Set(id, "test-key"); err != nil {
			t.Fatal(err)
		}
		plans[id] = &config.Plan{
			ID:         id,
			Credential: id,
			Endpoints: map[string]*config.EndpointSpec{
				config.ProtocolAnthropic: {BaseURL: up.server.URL},
			},
			Models: map[string]*config.ModelSpec{
				model: {Tier: config.TierStrong, ContextWindow: 200000, Capabilities: config.Capabilities{Tools: true, Thinking: true}},
			},
		}
		targets = append(targets, config.Target{Plan: id, Model: model, Priority: i + 1})
	}
	route := &config.Route{
		ID:      "coding",
		Family:  config.FamilyGenerative,
		Match:   config.RouteMatch{Protocols: []string{config.ProtocolAnthropic}, Models: []string{"coding-main"}},
		Policy:  config.RoutePolicy{Preset: config.PresetStrictFailover, MaxAttempts: len(targets)},
		Targets: targets,
	}
	snap := &config.Snapshot{
		Generation: 1,
		Plans:      plans,
		AllPlans:   plans,
		Routes:     []*config.Route{route},
		AllRoutes:  []*config.Route{route},
	}
	rtr := router.New(sec, router.NewSessionCacheStore(16), router.NewHealthStore(), nil)
	exec := New(rtr, sec, nil, nil)
	exec.Bridge = codec.NewBridge()
	return exec, snap
}

func serveAnthropic(t *testing.T, exec *Executor, snap *config.Snapshot, body string) *httptest.ResponseRecorder {
	return serveAnthropicAs(t, exec, snap, "coding-main", body)
}

func serveAnthropicAs(t *testing.T, exec *Executor, snap *config.Snapshot, model, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	exec.ServeHTTP(rec, req, snap, codec.IngressInfo{
		Protocol:       config.ProtocolAnthropic,
		Family:         config.FamilyGenerative,
		Action:         "messages",
		RequestedModel: model,
	}, []byte(body))
	return rec
}

const anthropicOK = `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`

// Same-protocol failover across providers must not forward a signature minted
// by the first provider to the fallback provider.
func TestExecutorSameProtocolFailoverStripsForeignThinkingSignature(t *testing.T) {
	first := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"type":"server_error"}}`)
	})
	second := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, anthropicOK)
	})
	exec, snap := newExecutorFixture(t, first, second)
	cands := []router.Candidate{
		{Plan: snap.Plans["plan-0"], Target: config.Target{Plan: "plan-0", Model: "claude-0"}, EndpointProto: config.ProtocolAnthropic, Endpoint: snap.Plans["plan-0"].Endpoints[config.ProtocolAnthropic], ModelSpec: snap.Plans["plan-0"].Models["claude-0"]},
		{Plan: snap.Plans["plan-1"], Target: config.Target{Plan: "plan-1", Model: "claude-1"}, EndpointProto: config.ProtocolAnthropic, Endpoint: snap.Plans["plan-1"].Endpoints[config.ProtocolAnthropic], ModelSpec: snap.Plans["plan-1"].Models["claude-1"]},
	}
	exec.Signatures.Record("sig-origin-a", codec.CandidateSignatureOrigin(cands[0]))

	body := `{"model":"coding-main","max_tokens":1024,"messages":[
		{"role":"user","content":"start"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"from a","signature":"sig-origin-a"},{"type":"text","text":"working"}]},
		{"role":"user","content":"continue"}
	]}`
	rec := serveAnthropic(t, exec, snap, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if first.hits.Load() != 1 || second.hits.Load() != 1 {
		t.Fatalf("hits first=%d second=%d", first.hits.Load(), second.hits.Load())
	}
	if !strings.Contains(first.body.Load().(string), "sig-origin-a") {
		t.Fatalf("matching signature should survive to its origin upstream: %s", first.body.Load())
	}
	if strings.Contains(second.body.Load().(string), "sig-origin-a") || strings.Contains(second.body.Load().(string), "from a") {
		t.Fatalf("foreign signature leaked to the fallback provider: %s", second.body.Load())
	}
}

// A signature minted in one gateway response is recognized on the next client
// turn; history signed before the process started (unknown provenance) is not.
func TestExecutorRecordsResponseSignatureProvenance(t *testing.T) {
	var turn atomic.Int32
	up := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if turn.Add(1) == 1 {
			fmt.Fprint(w, `{"id":"m1","model":"claude-0","content":[{"type":"thinking","thinking":"step","signature":"sig-minted"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		fmt.Fprint(w, anthropicOK)
	})
	exec, snap := newExecutorFixture(t, up)
	exec.ServeHTTP(httptest.NewRecorder(), func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"coding-main","messages":[{"role":"user","content":"one"}]}`))
		return r
	}(), snap, codec.IngressInfo{Protocol: config.ProtocolAnthropic, Family: config.FamilyGenerative, Action: "messages", RequestedModel: "coding-main"}, []byte(`{"model":"coding-main","messages":[{"role":"user","content":"one"}]}`))
	body := `{"model":"coding-main","messages":[
		{"role":"user","content":"one"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"step","signature":"sig-minted"}]},
		{"role":"user","content":"two"}
	]}`
	rec := serveAnthropic(t, exec, snap, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(up.body.Load().(string), "sig-minted") {
		t.Fatalf("signature minted by the gateway was not recognized: %s", up.body.Load())
	}
}

// Cross-protocol failover must never serialize Anthropic signatures or
// redacted blobs into an OpenAI/Gemini tool loop, and the converted assistant
// turn must keep its tool_calls/content pairing.
func TestExecutorCrossProtocolFailoverDropsThinkingSignatures(t *testing.T) {
	first := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"type":"server_error"}}`)
	})
	second := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"bridged"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`)
	})
	sec := secret.New(t.TempDir())
	for _, id := range []string{"oa-plan", "gem-plan"} {
		if err := sec.Set(id, "test-key"); err != nil {
			t.Fatal(err)
		}
	}
	oaPlan := &config.Plan{
		ID: "oa-plan", Credential: "oa-plan",
		Endpoints: map[string]*config.EndpointSpec{config.ProtocolOpenAIChat: {BaseURL: first.server.URL}},
		Models:    map[string]*config.ModelSpec{"gpt-5": {Tier: config.TierStrong, Capabilities: config.Capabilities{Tools: true, Thinking: true}}},
	}
	gemPlan := &config.Plan{
		ID: "gem-plan", Credential: "gem-plan",
		Endpoints: map[string]*config.EndpointSpec{config.ProtocolGemini: {BaseURL: second.server.URL}},
		Models:    map[string]*config.ModelSpec{"gemini-pro": {Tier: config.TierStrong, Capabilities: config.Capabilities{Tools: true, Thinking: true}}},
	}
	route := &config.Route{
		ID: "coding", Family: config.FamilyGenerative,
		Match:  config.RouteMatch{Protocols: []string{config.ProtocolAnthropic}, Models: []string{"coding-main"}},
		Policy: config.RoutePolicy{Preset: config.PresetStrictFailover, AllowCrossProtocol: boolPtr(true), PreferNativeProtocol: boolPtr(false), MaxAttempts: 2},
		Targets: []config.Target{
			{Plan: "oa-plan", Model: "gpt-5", EndpointProtocol: config.ProtocolOpenAIChat, Priority: 1},
			{Plan: "gem-plan", Model: "gemini-pro", EndpointProtocol: config.ProtocolGemini, Priority: 2},
		},
	}
	snap := &config.Snapshot{
		Generation: 1,
		Plans:      map[string]*config.Plan{"oa-plan": oaPlan, "gem-plan": gemPlan},
		AllPlans:   map[string]*config.Plan{"oa-plan": oaPlan, "gem-plan": gemPlan},
		Routes:     []*config.Route{route},
		AllRoutes:  []*config.Route{route},
	}
	rtr := router.New(sec, router.NewSessionCacheStore(16), router.NewHealthStore(), nil)
	exec := New(rtr, sec, nil, nil)
	exec.Bridge = codec.NewBridge()

	body := `{
		"model":"coding-main",
		"max_tokens":1024,
		"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
		"messages":[
			{"role":"user","content":"read"},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"private plan","signature":"sig-foreign"},
				{"type":"redacted_thinking","data":"encrypted-blob"},
				{"type":"text","text":"calling"},
				{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"go.mod"}}
			]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"module crabbro"}]}
		]
	}`
	rec := serveAnthropic(t, exec, snap, body)
	if rec.Code != http.StatusOK || first.hits.Load() != 1 || second.hits.Load() != 1 {
		t.Fatalf("cross-protocol failover status=%d first=%d second=%d body=%s", rec.Code, first.hits.Load(), second.hits.Load(), rec.Body.String())
	}
	for _, leaked := range []string{"sig-foreign", "private plan", "encrypted-blob", "redacted_thinking"} {
		if strings.Contains(first.body.Load().(string), leaked) || strings.Contains(second.body.Load().(string), leaked) {
			t.Fatalf("cross-protocol failover leaked %q: first=%s second=%s", leaked, first.body.Load(), second.body.Load())
		}
	}
	firstBody := first.body.Load().(string)
	if !strings.Contains(firstBody, `"tool_calls"`) || !strings.Contains(firstBody, `"tool_call_id":"call_1"`) {
		t.Fatalf("converted tool pairing missing: %s", firstBody)
	}
}

func boolPtr(v bool) *bool { return &v }

// Direct <plan>/<model> addressing is strict unless the plan opts into an
// explicit fallback route.
func TestExecutorDirectFallbackPolicy(t *testing.T) {
	body := `{"model":"plan-0/claude-0","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
	newFixture := func(t *testing.T) (*Executor, *config.Snapshot, *testUpstream, *testUpstream) {
		first := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"type":"server_error"}}`)
		})
		second := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, anthropicOK)
		})
		exec, snap := newExecutorFixture(t, first, second)
		return exec, snap, first, second
	}

	t.Run("default remains single-target", func(t *testing.T) {
		exec, snap, first, second := newFixture(t)
		rec := serveAnthropicAs(t, exec, snap, "plan-0/claude-0", body)
		if first.hits.Load() != 1 || second.hits.Load() != 0 || rec.Code != http.StatusInternalServerError {
			t.Fatalf("default direct must not fall back: first=%d second=%d status=%d", first.hits.Load(), second.hits.Load(), rec.Code)
		}
	})

	t.Run("opt-in fallback route", func(t *testing.T) {
		exec, snap, first, second := newFixture(t)
		snap.Plans["plan-0"].Direct = &config.DirectSpec{FallbackRoute: "coding"}
		rec := serveAnthropicAs(t, exec, snap, "plan-0/claude-0", body)
		if first.hits.Load() != 1 || second.hits.Load() != 1 || rec.Code != http.StatusOK {
			t.Fatalf("opt-in direct fallback: first=%d second=%d status=%d body=%s", first.hits.Load(), second.hits.Load(), rec.Code, rec.Body.String())
		}
	})
}

func TestExecutorRetryBoundaries400Versus429(t *testing.T) {
	t.Run("400 stops before fallback", func(t *testing.T) {
		first := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"bad"}}`)
		})
		second := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, anthropicOK)
		})
		exec, snap := newExecutorFixture(t, first, second)
		rec := serveAnthropic(t, exec, snap, `{"model":"coding-main","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusBadRequest || first.hits.Load() != 1 || second.hits.Load() != 0 {
			t.Fatalf("rec=%d first=%d second=%d body=%s", rec.Code, first.hits.Load(), second.hits.Load(), rec.Body.String())
		}
	})

	t.Run("429 reaches fallback", func(t *testing.T) {
		first := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"type":"rate_limit_error"}}`)
		})
		second := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, anthropicOK)
		})
		exec, snap := newExecutorFixture(t, first, second)
		rec := serveAnthropic(t, exec, snap, `{"model":"coding-main","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK || first.hits.Load() != 1 || second.hits.Load() != 1 {
			t.Fatalf("rec=%d first=%d second=%d body=%s", rec.Code, first.hits.Load(), second.hits.Load(), rec.Body.String())
		}
	})
}

func TestExecutorSSEPreCommitFailoverAndPostCommitNoRetry(t *testing.T) {
	t.Run("pre-commit error fails over", func(t *testing.T) {
		first := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n")
		})
		second := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered\"}}\n\n")
			io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		})
		exec, snap := newExecutorFixture(t, first, second)
		rec := serveAnthropic(t, exec, snap, `{"model":"coding-main","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK || first.hits.Load() != 1 || second.hits.Load() != 1 {
			t.Fatalf("rec=%d first=%d second=%d body=%s", rec.Code, first.hits.Load(), second.hits.Load(), rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "recovered") {
			t.Fatalf("fallback stream missing: %s", rec.Body.String())
		}
	})

	t.Run("post-commit truncation never retries", func(t *testing.T) {
		first := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
			// Returning with fewer bytes than Content-Length makes the client
			// observe a read error after the response is committed.
		})
		second := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, anthropicOK)
		})
		exec, snap := newExecutorFixture(t, first, second)
		rec := serveAnthropic(t, exec, snap, `{"model":"coding-main","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if first.hits.Load() != 1 || second.hits.Load() != 0 {
			t.Fatalf("post-commit failover must not run: first=%d second=%d", first.hits.Load(), second.hits.Load())
		}
		if !strings.Contains(rec.Body.String(), "partial") || !strings.Contains(rec.Body.String(), "event: error") {
			t.Fatalf("expected committed partial stream plus mid-stream error: %s", rec.Body.String())
		}
	})
}
