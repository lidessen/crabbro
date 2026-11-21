package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
)

func TestGatewayHTTPControlAndConfigWorkflow(t *testing.T) {
	s := testMITMServer(t)
	sec := secret.New(t.TempDir())
	if err := sec.Set("glm_key", "sk-test-glm-secret"); err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Options{
		RootDir:    t.TempDir(),
		ListenAddr: "127.0.0.1:0",
	}, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gw.Stop() }()
	s.Secrets = sec
	s.Gateway = gw

	// /v1/status reflects GatewayEnabled & GatewayAddr.
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/status = %d: %s", w.Code, w.Body.String())
	}
	var stResp shared.StatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &stResp); err != nil {
		t.Fatal(err)
	}
	if !stResp.GatewayEnabled || stResp.GatewayAddr == "" {
		t.Fatalf("unexpected gateway fields in /v1/status: %+v", stResp)
	}

	planYAML := `version: 1
id: glm-coding
enabled: true
credential: glm_key
endpoints:
  anthropic:
    baseUrl: https://example.com/anthropic
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
	// Draft -> Validate -> Commit plan.
	req = httptest.NewRequest(http.MethodPost, "/v1/config/draft?kind=gateway-plan&id=glm-coding", strings.NewReader(planYAML))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("draft plan = %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/config/validate?kind=gateway-plan&id=glm-coding", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("validate plan = %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/config/commit?kind=gateway-plan&id=glm-coding", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("commit plan = %d: %s", w.Code, w.Body.String())
	}

	// Draft -> Discard bad route, then Draft -> Commit good route.
	req = httptest.NewRequest(http.MethodPost, "/v1/config/draft?kind=gateway-route&id=coding-main", strings.NewReader("version: 99\n"))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("draft bad route = %d: %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/config/discard?kind=gateway-route&id=coding-main", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("discard route = %d: %s", w.Code, w.Body.String())
	}

	routeYAML := `version: 1
id: coding-main
enabled: true
priority: 10
family: generative
match:
  protocols: [anthropic]
  models: ["coding-main"]
policy:
  preset: balanced
targets:
  - plan: glm-coding
    model: glm-4.7
    priority: 10
`
	req = httptest.NewRequest(http.MethodPost, "/v1/config/draft?kind=gateway-route&id=coding-main", strings.NewReader(routeYAML))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("draft route = %d: %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/config/commit?kind=gateway-route&id=coding-main", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("commit route = %d: %s", w.Code, w.Body.String())
	}

	// GET /v1/config/status?kind=gateway
	req = httptest.NewRequest(http.MethodGet, "/v1/config/status?kind=gateway", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"glm-coding"`) || !strings.Contains(w.Body.String(), `"coding-main"`) {
		t.Fatalf("config status = %d: %s", w.Code, w.Body.String())
	}

	// /v1/gateway/plans, /v1/gateway/routes, /v1/gateway/status
	req = httptest.NewRequest(http.MethodGet, "/v1/gateway/plans", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"glm-coding"`) {
		t.Fatalf("plans = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-test-glm-secret") {
		t.Fatal("gateway plans leaked secret")
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/gateway/routes", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"coding-main"`) {
		t.Fatalf("routes = %d: %s", w.Code, w.Body.String())
	}

	// POST /v1/gateway/preview
	prevBody, _ := json.Marshal(shared.GatewayPreviewRequest{
		Protocol: "anthropic",
		Model:    "coding-main",
		Tools:    true,
	})
	req = httptest.NewRequest(http.MethodPost, "/v1/gateway/preview", bytes.NewReader(prevBody))
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("preview = %d: %s", w.Code, w.Body.String())
	}
	var prevResp shared.GatewayPreviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &prevResp); err != nil {
		t.Fatal(err)
	}
	if !prevResp.Matched || prevResp.RouteID != "coding-main" || prevResp.Decision == nil || len(prevResp.Decision.Candidates) != 1 || prevResp.Decision.Candidates[0].Plan != "glm-coding" {
		t.Fatalf("unexpected preview response: %+v", prevResp)
	}

	// POST /v1/gateway/plans/{id}/reset-cooldown & /v1/gateway/reload & /v1/gateway/usage
	req = httptest.NewRequest(http.MethodPost, "/v1/gateway/plans/glm-coding/reset-cooldown", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reset-cooldown = %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/gateway/reload", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reload = %d: %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/gateway/usage?window=24h", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("usage = %d: %s", w.Code, w.Body.String())
	}
}

func TestGatewayQuotaRefreshProjectsFailure(t *testing.T) {
	s := testMITMServer(t)
	gw, err := gateway.New(gateway.Options{RootDir: t.TempDir()}, secret.New(t.TempDir()), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()
	s.Gateway = gw
	snap := gw.Config.Snapshot()
	snap.AllPlans["zcode"] = &config.Plan{ID: "zcode", Credential: "missing"}
	req := httptest.NewRequest(http.MethodPost, "/v1/gateway/quotas/refresh", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"remoteQuota"`) || !strings.Contains(w.Body.String(), `"stale":true`) {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/v1/gateway/plans", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `"provider sync failed"`) {
		t.Fatalf("plans: %s", w.Body.String())
	}
}
