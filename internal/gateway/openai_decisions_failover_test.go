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

func TestOpenAIDecisionsFailoverAndReverse(t *testing.T) {
	var fail atomic.Bool
	var failJev atomic.Bool
	oa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"ok","probability":0.9}]}`)
	}))
	defer oa.Close()
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failJev.Load() {
			w.WriteHeader(500)
			return
		}
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path %s", r.URL.Path)
		}
		io.WriteString(w, `{"model":"jev","answers":{"ok":{"type":"noul","noul":0.8}}}`)
	}))
	defer jev.Close()
	gem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"{\"answers\":{\"ok\":{\"noul\":0.7}}}"}]}}],"usageMetadata":{"promptTokenCount":7}}`)
	}))
	defer gem.Close()
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
  allowEmulation: true
targets:
  - plan: a
    model: gpt-6-luna
    endpointProtocol: openai-decisions
    priority: 1
  - plan: b
    model: jev
    endpointProtocol: typesafe-jev
    priority: 2
  - plan: c
    model: gem
    endpointProtocol: gemini
    priority: 3
`
	for _, c := range []struct{ k, id, body string }{{config.KindPlan, "a", plan("a", "openai-decisions", oa.URL, "gpt-6-luna")}, {config.KindPlan, "b", plan("b", "typesafe-jev", jev.URL, "jev")}, {config.KindPlan, "c", plan("c", "gemini", gem.URL, "gem")}, {config.KindRoute, "decision", route}} {
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
	fail.Store(true)
	for _, tc := range []struct{ path, body, translated string }{{"/v1/systemone", `{"model":"decision","state":"hello","questions":{"ok":{"type":"noul","instructions":"ok?"}}}`, ""}, {"/v1/decisions", `{"model":"decision","input":"hello","questions":[{"type":"predicate","name":"ok","instructions":"ok?"}]}`, "1"}} {
		r, err := http.Post("http://"+gw.Addr()+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 200 || r.Header.Get("X-Crabbro-Plan") != "b" || r.Header.Get("X-Crabbro-Translated") != tc.translated {
			t.Fatalf("%s status %d headers %v body %s", tc.path, r.StatusCode, r.Header, b)
		}
	}
	result, err := gw.InvokeDecision(context.Background(), "decision", &ir.DecisionRequest{Model: "decision", State: "hello", Questions: map[string]ir.DecisionQuestion{"ok": {Type: "noul", Instructions: "ok?"}}})
	if err != nil || !result.Calibrated || result.Answers["ok"].Noul == nil {
		t.Fatalf("invoke: %#v %v", result, err)
	}
	failJev.Store(true)
	r, err := http.Post("http://"+gw.Addr()+"/v1/systemone", "application/json", strings.NewReader(`{"model":"decision","state":"hello","questions":{"ok":{"type":"noul","instructions":"ok?"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("X-Crabbro-Plan") != "c" || r.Header.Get("X-Crabbro-Decision-Mode") != "emulated" || !strings.Contains(string(body), `"calibrated":false`) {
		t.Fatalf("gemini fallback status %d headers %v body %s", r.StatusCode, r.Header, body)
	}

}
