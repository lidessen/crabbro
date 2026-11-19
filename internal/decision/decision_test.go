package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/secret"
)

func setupDecisionGateway(t *testing.T, planYAML, routeYAML string, secretVal string) (*Client, *gateway.Gateway) {
	t.Helper()
	dir := t.TempDir()
	sec := secret.New(filepath.Join(dir, "secrets"))
	if secretVal != "" {
		if err := sec.Set("dec_key", secretVal); err != nil {
			t.Fatal(err)
		}
	}
	gw, err := gateway.New(gateway.Options{
		ListenAddr: "127.0.0.1:0",
		RootDir:    filepath.Join(dir, "gateway"),
	}, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Stop() })

	if planYAML != "" {
		if err := gw.Config.Draft(config.KindPlan, "dec-plan", []byte(planYAML)); err != nil {
			t.Fatal(err)
		}
		if err := gw.Config.Commit(config.KindPlan, "dec-plan"); err != nil {
			t.Fatal(err)
		}
	}
	if routeYAML != "" {
		if err := gw.Config.Draft(config.KindRoute, "decision", []byte(routeYAML)); err != nil {
			t.Fatal(err)
		}
		if err := gw.Config.Commit(config.KindRoute, "decision"); err != nil {
			t.Fatal(err)
		}
	}
	return New(gw, "decision"), gw
}

func TestDecisionUnconfiguredReturnsErrNotConfigured(t *testing.T) {
	ctx := context.Background()
	cNil := New(nil, "")
	if cNil.Status().Configured {
		t.Fatal("nil invoker should not be configured")
	}
	if _, err := cNil.Find(ctx, "auth", []Flow{{ID: "req_1"}}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
	if _, err := cNil.Draft(ctx, "block bad", nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
	res, err := cNil.Ask(ctx, "is error?", "noul", nil)
	if !errors.Is(err, ErrNotConfigured) || res.Diagnostic.Category != "not_configured" {
		t.Fatalf("Ask unconfigured = %+v, %v", res, err)
	}

	cEmpty, _ := setupDecisionGateway(t, "", "", "")
	if cEmpty.Status().Configured {
		t.Fatal("empty gateway without decision route should not be configured")
	}
	if _, err := cEmpty.Find(ctx, "auth", []Flow{{ID: "req_1"}}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured on empty gateway, got %v", err)
	}
}

func TestDecisionNativeJevFindDraftAndAsk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer ts-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req ir.DecisionRequest
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		answers := make(map[string]ir.DecisionAnswer, len(req.Questions))
		for qid, q := range req.Questions {
			switch q.Type {
			case ir.DecisionKindNoul:
				val := 0.2
				if strings.Contains(q.Instructions, "req_auth") {
					val = 0.92
				} else if strings.Contains(q.Instructions, "req_pay") {
					val = 0.65
				}
				answers[qid] = ir.DecisionAnswer{Type: ir.DecisionKindNoul, Noul: &val}
			case ir.DecisionKindChoice:
				conf := 0.94
				choice := "block"
				switch c := q.Criteria.(type) {
				case []any:
					if len(c) > 0 {
						if s, ok := c[0].(string); ok {
							choice = s
						}
					}
				case map[string]any:
					if _, ok := c["block"]; !ok {
						for k := range c {
							choice = k
							break
						}
					}
				}
				answers[qid] = ir.DecisionAnswer{Type: ir.DecisionKindChoice, Choice: choice, Confidence: &conf}
			case ir.DecisionKindScore:
				if arr, ok := q.Criteria.([]any); !ok || len(arr) < 2 {
					http.Error(w, "score criteria must be array >= 2", http.StatusBadRequest)
					return
				}
				sc, conf := 0.88, 0.90
				answers[qid] = ir.DecisionAnswer{Type: ir.DecisionKindScore, Score: &sc, Confidence: &conf}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ir.DecisionResponse{
			Model:   "jev-latest",
			Answers: answers,
		})
	}))
	defer srv.Close()

	planYAML := fmt.Sprintf(`version: 1
id: dec-plan
credential: dec_key
endpoints:
  typesafe-jev:
    baseUrl: %s
models:
  jev-latest:
    tier: fast
`, srv.URL)
	routeYAML := `version: 1
id: decision
enabled: true
family: decision
match:
  protocols: [typesafe-jev]
policy:
  preset: balanced
targets:
  - plan: dec-plan
    model: jev-latest
`
	client, _ := setupDecisionGateway(t, planYAML, routeYAML, "ts-secret")
	st := client.Status()
	if !st.Configured || st.Provider != config.ProtocolTypeSafeJev || st.Model != "jev-latest" {
		t.Fatalf("unexpected status: %+v", st)
	}

	ctx := context.Background()
	found, err := client.Find(ctx, "login failures", []Flow{
		{ID: "req_other", Method: "GET", URL: "https://example.com/health", Status: 200},
		{ID: "req_pay", Method: "POST", URL: "https://example.com/pay", Status: 500},
		{ID: "req_auth", Method: "POST", URL: "https://example.com/login", Status: 401},
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(found.Hits) != 2 || found.Hits[0].RequestID != "req_auth" || found.Hits[1].RequestID != "req_pay" {
		t.Fatalf("unexpected Find hits: %+v", found)
	}

	draft, err := client.Draft(ctx, "block unauthorized requests", &Flow{
		ID: "req_auth", Method: "POST", URL: "https://api.example.com/login", Status: 401,
	})
	if err != nil {
		t.Fatalf("Draft: %v", err)
	}
	if draft.Action != "block" {
		t.Fatalf("Draft action = %q, want block", draft.Action)
	}
	if diags := rule.Validate(draft.Script, true, nil); len(diags) > 0 {
		t.Fatalf("drafted YAML rule failed validation: %+v\n%s", diags, draft.Script)
	}
	if !strings.Contains(draft.Script, "crabbro.block") {
		t.Fatalf("expected YAML rule script containing crabbro.block:\n%s", draft.Script)
	}

	askRes, err := client.Ask(ctx, "severity?", "score", nil)
	if err != nil {
		t.Fatalf("Ask score: %v", err)
	}
	if !askRes.Calibrated || askRes.Mode != "native" || askRes.Score == nil || *askRes.Score != 0.88 {
		t.Fatalf("unexpected Ask score result: %+v", askRes)
	}
}

func TestDecisionEmulatedOpenAIGeminiAnthropic(t *testing.T) {
	cases := []struct {
		name      string
		proto     string
		model     string
		endpoint  func() string
		handler   http.HandlerFunc
	}{
		{
			name:  "openai-chat",
			proto: config.ProtocolOpenAIChat,
			model: "gpt-4o-mini",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":    "chat1",
					"model": "gpt-4o-mini",
					"choices": []map[string]any{{
						"finish_reason": "stop",
						"message": map[string]any{
							"role":    "assistant",
							"content": `{"answers":{"action":{"choice":"tag","confidence":0.85},"answer":{"choice":"allow","confidence":0.9}}}`,
						},
					}},
					"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
				})
			},
		},
		{
			name:  "gemini",
			proto: config.ProtocolGemini,
			model: "gemini-2.5-flash",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"modelVersion": "gemini-2.5-flash",
					"candidates": []map[string]any{{
						"finishReason": "STOP",
						"content": map[string]any{
							"role":  "model",
							"parts": []map[string]any{{"text": `{"answers":{"action":{"choice":"tag","confidence":0.81},"answer":{"choice":"allow","confidence":0.87}}}`}},
						},
					}},
					"usageMetadata": map[string]any{"promptTokenCount": 9, "candidatesTokenCount": 4},
				})
			},
		},
		{
			name:  "anthropic",
			proto: config.ProtocolAnthropic,
			model: "claude-haiku",
			handler: func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				qMap := map[string]any{"choice": "tag", "confidence": 0.91}
				key := "action"
				if tc, ok := body["tools"].([]any); ok && len(tc) > 0 {
					// Respond to whichever question key is requested ("action" vs "answer").
					rawStr := string(b)
					if strings.Contains(rawStr, `"answer"`) && !strings.Contains(rawStr, `"action"`) {
						key = "answer"
						qMap = map[string]any{"choice": "allow", "confidence": 0.92}
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":          "msg_1",
					"model":       "claude-haiku",
					"stop_reason": "tool_use",
					"content": []map[string]any{{
						"type": "tool_use",
						"id":   "tu_1",
						"name": ir.DecisionToolName,
						"input": map[string]any{
							"answers": map[string]any{key: qMap},
						},
					}},
					"usage": map[string]any{"input_tokens": 11, "output_tokens": 6},
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			planYAML := fmt.Sprintf(`version: 1
id: dec-plan
credential: dec_key
endpoints:
  %s:
    baseUrl: %s
models:
  %s:
    tier: fast
`, tc.proto, srv.URL, tc.model)
			routeYAML := fmt.Sprintf(`version: 1
id: decision
enabled: true
family: decision
match:
  protocols: [typesafe-jev]
policy:
  preset: balanced
  allowEmulation: true
targets:
  - plan: dec-plan
    model: %s
    endpointProtocol: %s
`, tc.model, tc.proto)
			client, _ := setupDecisionGateway(t, planYAML, routeYAML, "emu-secret")

			res, err := client.Ask(context.Background(), "allow or block?", "choice", []string{"allow", "block"})
			if err != nil {
				t.Fatalf("Ask (%s): %v", tc.name, err)
			}
			if res.Calibrated || res.Mode != "emulated" || res.Choice != "allow" {
				t.Fatalf("unexpected Ask (%s): %+v", tc.name, res)
			}

			draft, err := client.Draft(context.Background(), "tag slow endpoints", nil)
			if err != nil {
				t.Fatalf("Draft (%s): %v", tc.name, err)
			}
			if draft.Action != "tag" {
				t.Fatalf("Draft action = %q, want tag", draft.Action)
			}
			if diags := rule.Validate(draft.Script, true, nil); len(diags) > 0 {
				t.Fatalf("drafted rule invalid (%s): %+v", tc.name, diags)
			}
		})
	}
}
