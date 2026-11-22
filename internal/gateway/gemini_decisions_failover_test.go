package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/secret"
)

func TestGeminiDecisionsNativeAndFailover(t *testing.T) {
	var status atomic.Int32
	var gemCalls atomic.Int32
	gem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gemCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1beta/decisions" || r.Header.Get("x-goog-api-key") != "test-value" {
			t.Errorf("unexpected Gemini request: %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"model":"gemini-decision-1"`) || !strings.Contains(string(b), `"type":"noul"`) {
			t.Errorf("request: %s", b)
		}
		if n := status.Load(); n != 0 {
			w.WriteHeader(int(n))
			return
		}
		io.WriteString(w, `{"answers":{"ok":{"type":"noul","noul":0.9,"confidence":0.9}},"usage":{"total_input_tokens":12,"total_output_tokens":0}}`)
	}))
	defer gem.Close()
	var oaFail atomic.Bool
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if oaFail.Load() {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, `{"model":"oa","answers":[{"type":"predicate","name":"ok","probability":0.8}]}`)
	}))
	defer oa.Close()
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"model":"jev","answers":{"ok":{"type":"noul","noul":0.7}}}`)
	}))
	defer jev.Close()
	dir := t.TempDir()
	sec := secret.New(filepath.Join(dir, "secrets"))
	if err := sec.Set("test", "test-value"); err != nil {
		t.Fatal(err)
	}
	gw, err := New(Options{ListenAddr: "127.0.0.1:0", RootDir: filepath.Join(dir, "gateway")}, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()
	plan := func(id, proto, url, model string) string {
		return fmt.Sprintf("version: 1\nid: %s\ncredential: test\nendpoints:\n  %s:\n    baseUrl: %s\nmodels:\n  %s:\n    tier: fast\n", id, proto, url, model)
	}
	route := `version: 1
id: decision
enabled: true
family: decision
match:
  protocols: [typesafe-jev, openai-decisions]
policy:
  preset: strict-failover
  allowCrossProtocol: true
  preferNativeProtocol: false
  allowEmulation: false
targets:
  - plan: a
    model: gemini-decision-1
    endpointProtocol: gemini-decisions
    priority: 1
  - plan: b
    model: oa
    endpointProtocol: openai-decisions
    priority: 2
  - plan: c
    model: jev
    endpointProtocol: typesafe-jev
    priority: 3
`
	for _, c := range []struct{ k, id, body string }{{config.KindPlan, "a", plan("a", "gemini-decisions", gem.URL, "gemini-decision-1")}, {config.KindPlan, "b", plan("b", "openai-decisions", oa.URL, "oa")}, {config.KindPlan, "c", plan("c", "typesafe-jev", jev.URL, "jev")}, {config.KindRoute, "decision", route}} {
		if err = gw.Config.Draft(c.k, c.id, []byte(c.body)); err != nil {
			t.Fatal(err)
		}
		if err = gw.Config.Commit(c.k, c.id); err != nil {
			t.Fatal(err)
		}
	}
	if err = gw.Start(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, body string }{{"/v1/systemone", `{"model":"decision","state":"hello","questions":{"ok":{"type":"noul","instructions":"ok?"}}}`}, {"/v1/decisions", `{"model":"decision","input":"hello","questions":[{"type":"predicate","name":"ok","instructions":"ok?"}]}`}} {
		r, err := http.Post("http://"+gw.Addr()+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 200 || r.Header.Get("X-Crabbro-Plan") != "a" || r.Header.Get("X-Crabbro-Decision-Mode") != "native" || !strings.Contains(string(b), "0.9") {
			t.Fatalf("native: %d %v %s", r.StatusCode, r.Header, b)
		}
	}
	result, err := gw.InvokeDecision(context.Background(), "decision", &ir.DecisionRequest{State: "hello", Questions: map[string]ir.DecisionQuestion{"ok": {Type: "noul"}}})
	if err != nil || result.Mode != "native" || !result.Calibrated || result.Usage.InputTokens != 12 {
		t.Fatalf("invoke: %#v %v", result, err)
	}
	for _, tc := range []struct {
		status int32
		failOA bool
		plan   string
	}{{429, false, "b"}, {500, true, "c"}} {
		gw.Health.ResetCooldown("a")
		before := gemCalls.Load()
		status.Store(tc.status)
		oaFail.Store(tc.failOA)
		r, err := http.Post("http://"+gw.Addr()+"/v1/systemone", "application/json", strings.NewReader(`{"model":"decision","state":"hello","questions":{"ok":{"type":"noul","instructions":"ok?"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if gemCalls.Load() != before+1 {
			t.Fatalf("Gemini did not receive status %d attempt", tc.status)
		}
		if r.StatusCode != 200 || r.Header.Get("X-Crabbro-Plan") != tc.plan || r.Header.Get("X-Crabbro-Decision-Mode") != "native" {
			t.Fatalf("failover: %d %v %s", r.StatusCode, r.Header, b)
		}
	}
}
