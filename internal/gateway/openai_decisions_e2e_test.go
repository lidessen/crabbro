package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/secret"
)

func TestOpenAIDecisionsGateway(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"model":"gpt-6-luna"`) {
			t.Errorf("model %s", b)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"ok","probability":0.9},{"type":"choice","name":"runner","choice":"strong","probabilities":[{"value":"fast","probability":0.24},{"value":"strong","probability":0.76}]},{"type":"score","name":"sev","score":0.0,"probabilities":[{"value":0,"label":"low","probability":1.0},{"value":1,"label":"high","probability":0.0}]}],"usage":{"input_tokens":10}}`)
	}))
	defer upstream.Close()
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
	plan := fmt.Sprintf("version: 1\nid: p\ncredential: test\nendpoints:\n  openai-decisions:\n    baseUrl: %s\nmodels:\n  gpt-6-luna:\n    tier: fast\n", upstream.URL)
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
targets:
  - plan: p
    model: gpt-6-luna
    endpointProtocol: openai-decisions
`
	for _, c := range []struct{ k, id, body string }{{config.KindPlan, "p", plan}, {config.KindRoute, "decision", route}} {
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
	for _, tc := range []struct{ path, body, translated string }{{"/v1/systemone", `{"model":"decision","state":"hello","questions":{"ok":{"type":"noul","instructions":"ok?"}}}`, "1"}, {"/v1/decisions", `{"model":"decision","input":"hello","questions":[{"type":"predicate","name":"ok","instructions":"ok?"}]}`, ""}} {
		r, err := http.Post("http://"+gw.Addr()+tc.path, "application/json", strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var response map[string]json.RawMessage
		if err := json.Unmarshal(b, &response); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(response["answers"]), `"probabilities"`) {
			t.Fatalf("missing probabilities: %s", b)
		}
		if r.StatusCode != 200 || r.Header.Get("X-Crabbro-Decision-Mode") != "native" || r.Header.Get("X-Crabbro-Translated") != tc.translated {
			t.Fatalf("%s status %d headers %v body %s", tc.path, r.StatusCode, r.Header, b)
		}
	}
	result, err := gw.InvokeDecision(context.Background(), "decision", &ir.DecisionRequest{Model: "decision", State: "hello", Questions: map[string]ir.DecisionQuestion{"ok": {Type: "noul", Instructions: "ok?"}}})
	if err != nil || !result.Calibrated || result.Answers["ok"].Noul == nil || result.Answers["runner"].Probabilities["strong"] != 0.76 || result.Answers["sev"].Probabilities["low"] != 1 {
		t.Fatalf("invoke: %#v %v", result, err)
	}
}
