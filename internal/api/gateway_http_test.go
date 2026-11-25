package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/provider"
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

func TestGatewayQuotaResetEndpoint(t *testing.T) {
	var mu sync.Mutex
	useCalls := 0
	drained := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/monitor/usage/quota/limit":
			fmt.Fprint(w, `{"code":0,"data":{"level":"lite","limits":[{"unit":3,"usage":100,"remaining":100}]}}`)
		case "/api/biz/customer-package-reset/list":
			mu.Lock()
			empty := drained
			mu.Unlock()
			if empty {
				fmt.Fprint(w, `{"code":200,"success":true,"data":{}}`)
				return
			}
			fmt.Fprintf(w, `{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":4242,"grantType":"DIRECT","expireTime":%q,"available":true}]}}`,
				time.Now().Add(time.Hour).Format("2006-01-02 15:04:05"))
		case "/api/biz/customer-package-reset/use":
			mu.Lock()
			useCalls++
			drained = true
			mu.Unlock()
			fmt.Fprint(w, `{"code":200,"msg":"ok","success":true,"data":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	s := testMITMServer(t)
	sec := secret.New(t.TempDir())
	if err := sec.Set("zcode_key", "sk-zcode-test"); err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Options{RootDir: t.TempDir()}, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gw.Stop() }()
	s.Gateway = gw
	snap := gw.Config.Snapshot()
	snap.AllPlans["zcode"] = &config.Plan{
		ID:         "zcode",
		Credential: "zcode_key",
		Endpoints:  map[string]*config.EndpointSpec{"anthropic": {BaseURL: server.URL}},
	}

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/gateway/quotas/reset", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}

	// Dry run selects without spending.
	w := post(`{"plan":"zcode","scope":"fiveHour","dryRun":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"redeemed":false`) || !strings.Contains(w.Body.String(), `"couponsAvailable":1`) {
		t.Fatalf("dry-run = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "4242") || strings.Contains(w.Body.String(), "recordId") {
		t.Fatalf("dry-run leaked a coupon identifier: %s", w.Body.String())
	}
	mu.Lock()
	calls := useCalls
	mu.Unlock()
	if calls != 0 {
		t.Fatalf("dry run spent a coupon (%d /use calls)", calls)
	}

	// Real redemption clears cooling and reports the refreshed snapshot.
	gw.Health.EnterPlanCooldown("zcode", "plan", "quota exhausted (429)", 5*time.Minute, time.Now().UTC())
	w = post(`{"plan":"zcode"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"redeemed":true`) || !strings.Contains(w.Body.String(), `"scope":"fiveHour"`) {
		t.Fatalf("reset = %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "4242") || strings.Contains(w.Body.String(), "recordId") {
		t.Fatalf("reset leaked a coupon identifier: %s", w.Body.String())
	}
	var resp shared.GatewayQuotaResetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status == nil || resp.Status.CooldownUntil != nil {
		t.Fatalf("plan status after reset = %+v", resp.Status)
	}
	mu.Lock()
	calls = useCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("/use called %d times, want 1", calls)
	}

	// Drained inventory reports the failure without spending again.
	w = post(`{"plan":"zcode","scope":"weekly"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("drained reset = %d %s", w.Code, w.Body.String())
	}

	// Validation and unknown-plan handling.
	if w := post(`{"plan":"zcode","scope":"hourly"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope = %d %s", w.Code, w.Body.String())
	}
	if w := post(`{"scope":"fiveHour"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing plan = %d %s", w.Code, w.Body.String())
	}
	if w := post(`{"plan":"ghost"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown plan = %d %s", w.Code, w.Body.String())
	}
	if req := httptest.NewRequest(http.MethodGet, "/v1/gateway/quotas/reset", nil); func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}().Code != http.StatusMethodNotAllowed {
		t.Fatal("GET /v1/gateway/quotas/reset must be rejected")
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

func TestGatewayPlansSIWCProjection(t *testing.T) {
	s := testMITMServer(t)
	sec := secret.New(t.TempDir())
	gw, err := gateway.New(gateway.Options{RootDir: t.TempDir(), ListenAddr: "127.0.0.1:0"}, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()
	gw.Config.SetSnapshot(&config.Snapshot{AllPlans: map[string]*config.Plan{
		"siwc": {ID: "siwc", SIWC: &config.SIWCSpec{SessionRef: "session-ref", HostIDRef: "host-ref"}},
		"key":  {ID: "key", Credential: "key-ref"},
		"new":  {ID: "new", Auth: &config.AuthSpec{Kind: provider.OAuthSIWC, SessionRef: "session-ref", HostIDRef: "host-ref"}},
	}})
	s.Gateway = gw
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/gateway/plans", nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	var out struct {
		Plans []struct {
			ID   string          `json:"id"`
			SIWC json.RawMessage `json:"siwc"`
		} `json:"plans"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Plans {
		switch p.ID {
		case "key":
			if string(p.SIWC) != "null" {
				t.Fatal("missing explicit null")
			}
		case "siwc", "new":
			if string(p.SIWC) != `{"sessionRef":"session-ref","hostIdRef":"host-ref"}` {
				t.Fatalf("incorrect projection: %s", p.SIWC)
			}
		default:
			t.Fatal("unexpected plan")
		}
	}
	if len(out.Plans) != 3 {
		t.Fatal("missing plans")
	}
}
