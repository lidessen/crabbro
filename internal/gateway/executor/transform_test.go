package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/codec"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/transform"
)

func TestRequestTransformsGenerationAndDecision(t *testing.T) {
	for _, decision := range []bool{false, true} {
		t.Run(fmt.Sprint(decision), func(t *testing.T) {
			up := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/custom" || r.URL.Query().Get("mode") != "model" || r.URL.Query().Has("remove") {
					t.Errorf("URL %s", r.URL)
				}
				if r.Header.Get("anthropic-beta") != "" || r.Header.Get("x-stainless-lang") != "" || r.UserAgent() != "" || r.Header.Get("X-Order") != "model" {
					t.Errorf("normalization headers incorrect")
				}
				auth := "X-Api-Key"
				if decision {
					auth = "Authorization"
				}
				if r.Header.Get(auth) == "" {
					t.Error("missing upstream auth")
				}
				if decision {
					fmt.Fprint(w, `{"model":"ok","answers":{"ok":{"type":"noul","noul":0.8}}}`)
				} else {
					fmt.Fprint(w, anthropicOK)
				}
			})
			exec, snap := newExecutorFixture(t, up)
			p := snap.Plans["plan-0"]
			ep := p.Endpoints[config.ProtocolAnthropic]
			model := p.Models["claude-0"]
			path, mode := "/custom", "model"
			ep.Headers = map[string]string{"anthropic-beta": "beta", "x-stainless-lang": "go"}
			ep.RequestTransform = transform.Spec{StripHeaders: []string{"anthropic-beta", "x-stainless-*", "user-agent"}, Path: &path, Query: map[string]*string{"mode": &mode, "remove": nil}, DropBodyFields: []string{"drop"}, RenameBodyFields: map[string]string{"old": "new"}, SetHeaders: map[string]string{"X-Order": "endpoint"}}
			model.RequestTransform = transform.Spec{SetHeaders: map[string]string{"X-Order": "model"}, SetBodyFields: map[string]any{"new": "model"}}
			if decision {
				p.Endpoints = map[string]*config.EndpointSpec{config.ProtocolTypeSafeJev: ep}
				model.Capabilities.StructuredOutput = true
				rt := snap.Routes[0]
				rt.ID = "decision"
				rt.Family = config.FamilyDecision
				rt.Match.Protocols = []string{config.ProtocolTypeSafeJev}
				rt.Match.Models = nil
				_, err := exec.ExecuteDecision(context.Background(), snap, "decision", &ir.DecisionRequest{State: "hello", Questions: map[string]ir.DecisionQuestion{"ok": {Type: "noul"}}})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				body := `{"model":"coding-main","messages":[],"old":"value","drop":true}`
				req := httptest.NewRequest("POST", "/v1/messages?remove=1", strings.NewReader(body))
				req.Header.Set("anthropic-beta", "client")
				req.Header.Set("x-stainless-lang", "go")
				rec := httptest.NewRecorder()
				exec.ServeHTTP(rec, req, snap, codec.IngressInfo{Protocol: config.ProtocolAnthropic, Family: config.FamilyGenerative, Action: "messages", RequestedModel: "coding-main", CleanQuery: "remove=1"}, []byte(body))
				if rec.Code != 200 {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
				}
			}
			b := up.body.Load().(string)
			if !strings.Contains(b, `"new":"model"`) || strings.Contains(b, `"drop"`) || strings.Contains(b, `"old"`) {
				t.Errorf("body %s", b)
			}
		})
	}
}

func TestResponseTransformsGenerationAndDecision(t *testing.T) {
	for _, decision := range []bool{false, true} {
		t.Run(fmt.Sprint(decision), func(t *testing.T) {
			up := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Remove", "yes")
				w.Header().Set("X-Order", "endpoint")
				if decision {
					fmt.Fprint(w, `{"data":{"model":"ok","answers":{"ok":{"type":"noul","noul":0.8}}}}`)
				} else {
					fmt.Fprint(w, `{"data":{"id":"x","type":"message","role":"assistant","model":"x","content":[],"usage":{"in":7,"out":2}}}`)
				}
			})
			exec, snap := newExecutorFixture(t, up)
			p := snap.Plans["plan-0"]
			ep := p.Endpoints[config.ProtocolAnthropic]
			model := p.Models["claude-0"]
			ep.ResponseTransform = transform.ResponseSpec{UnwrapPath: "data", StripHeaders: []string{"x-remove"}}
			model.ResponseTransform = transform.ResponseSpec{RenameBodyFields: map[string]string{"usage.in": "usage.input_tokens", "usage.out": "usage.output_tokens"}, SetHeaders: map[string]string{"X-Order": "model"}}
			if decision {
				p.Endpoints = map[string]*config.EndpointSpec{config.ProtocolTypeSafeJev: ep}
				model.Capabilities.StructuredOutput = true
				rt := snap.Routes[0]
				rt.ID = "decision"
				rt.Family = config.FamilyDecision
				rt.Match.Protocols = []string{config.ProtocolTypeSafeJev}
				rt.Match.Models = nil
				if _, err := exec.ExecuteDecision(context.Background(), snap, "decision", &ir.DecisionRequest{State: "hello", Questions: map[string]ir.DecisionQuestion{"ok": {Type: "noul"}}}); err != nil {
					t.Fatal(err)
				}
			} else {
				body := `{"model":"coding-main","messages":[]}`
				req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
				rec := httptest.NewRecorder()
				exec.ServeHTTP(rec, req, snap, codec.IngressInfo{Protocol: config.ProtocolAnthropic, Family: config.FamilyGenerative, Action: "messages", RequestedModel: "coding-main"}, []byte(body))
				if rec.Code != 200 || rec.Header().Get("X-Remove") != "" || rec.Header().Get("X-Order") != "model" || !strings.Contains(rec.Body.String(), `"input_tokens":7`) {
					t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
				}
			}
		})
	}
}

func TestResponseTransformErrorAndSSE(t *testing.T) {
	t.Run("HTTP error", func(t *testing.T) {
		up := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Remove", "yes")
			w.Header().Set("X-Echo", "test-key")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"data":{"error":{"message":"test-key"}}}`)
		})
		exec, snap := newExecutorFixture(t, up)
		snap.Plans["plan-0"].Endpoints[config.ProtocolAnthropic].ResponseTransform = transform.ResponseSpec{ExtractField: "data", StripHeaders: []string{"x-remove"}}
		rec := serveAnthropic(t, exec, snap, `{"model":"coding-main","messages":[]}`)
		if rec.Code != 400 || rec.Header().Get("X-Remove") != "" || strings.Contains(rec.Header().Get("X-Echo"), "test-key") || strings.Contains(rec.Body.String(), "test-key") || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
		}
	})
	t.Run("SSE untouched", func(t *testing.T) {
		const stream = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
		up := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Order", "upstream")
			fmt.Fprint(w, stream)
		})
		exec, snap := newExecutorFixture(t, up)
		snap.Plans["plan-0"].Endpoints[config.ProtocolAnthropic].ResponseTransform = transform.ResponseSpec{UnwrapPath: "missing", SetHeaders: map[string]string{"X-Order": "changed"}}
		rec := serveAnthropic(t, exec, snap, `{"model":"coding-main","stream":true,"messages":[]}`)
		if rec.Code != 200 || rec.Body.String() != stream || rec.Header().Get("X-Order") != "upstream" {
			t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body)
		}
	})
}
