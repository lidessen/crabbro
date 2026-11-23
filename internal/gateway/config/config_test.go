package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEmbeddedSchemasMatchTopLevelSchemas(t *testing.T) {
	rootPlan, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "gateway-plan.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rootPlan, planSchemaJSON) {
		t.Fatal("embedded gateway-plan.v1.json differs from schemas/gateway-plan.v1.json")
	}
	rootRoute, err := os.ReadFile(filepath.Join("..", "..", "..", "schemas", "gateway-route.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rootRoute, routeSchemaJSON) {
		t.Fatal("embedded gateway-route.v1.json differs from schemas/gateway-route.v1.json")
	}
}

func TestParseDurationDays(t *testing.T) {
	d, err := ParseDuration("7d")
	if err != nil || d != 7*24*time.Hour {
		t.Fatalf("ParseDuration(7d) = %v, %v", d, err)
	}
}

func TestStoreDraftCommitReloadAndLastKnownGood(t *testing.T) {
	dir := t.TempDir()
	st := Open(dir)

	planYAML := `version: 1
id: glm-coding
enabled: true
credential: glm_key
timeout:
  ttft: 120s
limits:
  windows:
    - name: weekly
      duration: 7d
      maxTokens: 1000000
      mode: hard
endpoints:
  anthropic:
    baseUrl: https://open.bigmodel.cn/api/anthropic
  openai-chat:
    baseUrl: https://open.bigmodel.cn/api/coding/paas/v4
models:
  glm-4.7:
    tier: strong
    codingScore: 0.85
    contextWindow: 200000
    capabilities:
      tools: true
      thinking: true
      promptCache: true
`
	if err := st.Draft(KindPlan, "glm-coding", []byte(planYAML)); err != nil {
		t.Fatal(err)
	}
	if diags := st.ValidateFile(KindPlan, "glm-coding"); len(diags) > 0 {
		t.Fatalf("unexpected plan diags: %+v", diags)
	}
	if err := st.Commit(KindPlan, "glm-coding"); err != nil {
		t.Fatal(err)
	}

	routeYAML := `version: 1
id: coding-main
enabled: true
priority: 10
family: generative
match:
  protocols: [anthropic, openai-chat]
  models: ["coding-main"]
policy:
  preset: balanced
targets:
  - plan: glm-coding
    model: glm-4.7
    priority: 10
`
	if err := st.Draft(KindRoute, "coding-main", []byte(routeYAML)); err != nil {
		t.Fatal(err)
	}
	if err := st.Commit(KindRoute, "coding-main"); err != nil {
		t.Fatal(err)
	}

	snap := st.Snapshot()
	if snap.Plans["glm-coding"] == nil || len(snap.Routes) != 1 || snap.Routes[0].ID != "coding-main" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}

	// Reject generative route targeting typesafe-jev
	badRoute := `version: 1
id: bad-gen
family: generative
match:
  models: ["*"]
targets:
  - plan: glm-coding
    model: glm-4.7
    endpointProtocol: typesafe-jev
`
	if err := st.Draft(KindRoute, "bad-gen", []byte(badRoute)); err != nil {
		t.Fatal(err)
	}
	if diags := st.ValidateFile(KindRoute, "bad-gen"); len(diags) == 0 {
		t.Fatal("expected family mismatch diagnostic")
	}
	if err := st.Commit(KindRoute, "bad-gen"); err == nil {
		t.Fatal("commit succeeded for invalid route")
	}
	_ = st.Discard(KindRoute, "bad-gen")

	// Break live plan file on disk -> last-known-good remains active!
	livePlan := filepath.Join(dir, "plans", "glm-coding.yaml")
	if err := os.WriteFile(livePlan, []byte("version: 1\nid: glm-coding\nbadField: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diags, complete := st.Reload()
	if complete || len(diags) == 0 {
		t.Fatalf("expected incomplete reload with diagnostics, got complete=%v diags=%+v", complete, diags)
	}
	if st.Snapshot().Plans["glm-coding"] == nil {
		t.Fatal("last-known-good plan was dropped on broken file edit")
	}
}

func TestCrossFileValidationRejectsUnknownTargetAndMissingEmulation(t *testing.T) {
	dir := t.TempDir()
	st := Open(dir)
	planYAML := `version: 1
id: openai-api
credential: openai_key
endpoints:
  openai-chat:
    baseUrl: https://api.openai.com/v1
models:
  gpt-4o-mini:
    tier: fast
    contextWindow: 128000
`
	if err := st.Draft(KindPlan, "openai-api", []byte(planYAML)); err != nil {
		t.Fatal(err)
	}
	if err := st.Commit(KindPlan, "openai-api"); err != nil {
		t.Fatal(err)
	}

	decNoEmu := `version: 1
id: decision
family: decision
match:
  protocols: [typesafe-jev]
  models: ["*"]
targets:
  - plan: openai-api
    model: gpt-4o-mini
    endpointProtocol: openai-chat
`
	if err := st.Draft(KindRoute, "decision", []byte(decNoEmu)); err != nil {
		t.Fatal(err)
	}
	if diags := st.ValidateFile(KindRoute, "decision"); len(diags) == 0 {
		t.Fatal("expected allowEmulation validation error")
	}
}

func TestRequestTransformValidation(t *testing.T) {
	base := `version: 1
id: test
credential: ref
endpoints:
  anthropic:
    baseUrl: https://example.com
    authHeader: X-Private
    requestTransform:
      stripHeaders: [anthropic-beta, 'x-stainless-*']
      path: /custom
      query: {remove: null, mode: v1}
      renameBodyFields: {old: new}
models:
  target:
    requestTransform:
      setBodyFields: {nested.value: 1}
`
	if _, diags := ValidatePlanBytes("test.yaml", "test", []byte(base)); len(diags) > 0 {
		t.Fatalf("valid transform: %+v", diags)
	}
	for _, bad := range []string{
		"      setHeaders: {Authorization: bad}\n",
		"      setHeaders: {X-Private: bad}\n",
		"      path: relative\n",
		"      unknown: true\n",
	} {
		data := strings.Replace(base, "      setBodyFields: {nested.value: 1}\n", bad, 1)
		if _, diags := ValidatePlanBytes("test.yaml", "test", []byte(data)); len(diags) == 0 {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestModelTransformValidationUsesAvailableEndpoints(t *testing.T) {
	base := `version: 1
id: test
credential: ref
endpoints:
  anthropic:
    baseUrl: https://example.com
    authHeader: X-Anthropic-Auth
  openai-chat:
    baseUrl: https://example.com
    authHeader: X-OpenAI-Auth
models:
  target:
%s    requestTransform:
      setHeaders: {X-OpenAI-Auth: value}
`
	for _, tc := range []struct {
		name, endpoints, wantPath string
	}{
		{"restricted to another endpoint", "    endpoints: [anthropic]\n", ""},
		{"restricted to protected endpoint", "    endpoints: [openai-chat]\n", "$.models.target.requestTransform"},
		{"multiple available endpoints", "    endpoints: [anthropic, openai-chat]\n", "$.models.target.requestTransform"},
		{"all endpoints by default", "", "$.models.target.requestTransform"},
		{"all endpoints with empty list", "    endpoints: []\n", "$.models.target.requestTransform"},
		{"missing endpoint", "    endpoints: [gemini]\n", "$.models.target.endpoints[0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ValidatePlanBytes("test.yaml", "test", []byte(fmt.Sprintf(base, tc.endpoints)))
			if tc.wantPath == "" {
				if len(diags) != 0 {
					t.Fatalf("unexpected diagnostics: %+v", diags)
				}
				return
			}
			if len(diags) != 1 || diags[0].Path != tc.wantPath {
				t.Fatalf("got diagnostics %+v, want path %s", diags, tc.wantPath)
			}
		})
	}
}

func TestResponseTransformValidation(t *testing.T) {
	base := `version: 1
id: test
credential: ref
endpoints:
  anthropic:
    baseUrl: https://example.com
    authHeader: X-Private
    responseTransform:
      unwrapPath: data
      renameBodyFields: {usage.in: usage.input_tokens}
models:
  target:
    responseTransform:
      setBodyFields: {model: test}
`
	if _, diags := ValidatePlanBytes("test.yaml", "test", []byte(base)); len(diags) != 0 {
		t.Fatalf("valid: %+v", diags)
	}
	for _, bad := range []string{"      extractField: data\n      unwrapPath: data\n", "      unwrapPath: a..b\n", "      setHeaders: {X-Private: bad}\n", "      path: /bad\n"} {
		data := strings.Replace(base, "      setBodyFields: {model: test}\n", bad, 1)
		if _, diags := ValidatePlanBytes("test.yaml", "test", []byte(data)); len(diags) == 0 {
			t.Errorf("accepted %s", bad)
		}
	}
}
