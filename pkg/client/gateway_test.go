package client

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/api"
	"github.com/lidessen/crabbro/internal/capture"
	"github.com/lidessen/crabbro/internal/gateway"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
	pkgapi "github.com/lidessen/crabbro/pkg/api"
)

func TestClientGatewayAndSecrets(t *testing.T) {
	dir := t.TempDir()
	rb := ring.NewRequestBuffer(32)
	bc := capture.NewBodyCache(1<<20, 64*1024)
	hub := capture.NewEventHub()
	sec := secret.New(filepath.Join(dir, "secrets"))
	sink := capture.NewDefaultSink(rb, bc, hub, nil, nil)
	gw, err := gateway.New(gateway.Options{
		ListenAddr: "127.0.0.1:0",
		RootDir:    filepath.Join(dir, "gateway"),
	}, sec, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gw.Stop() }()

	// Seed one proxy and one gateway request via the shared sink.
	r1 := sink.Start(&shared.Request{
		Kind: "proxy", Method: "GET", Scheme: "https", Host: "example.com", Path: "/", URL: "https://example.com/", Status: 200, StartedAt: time.Now().UTC(),
	})
	sink.Complete(r1)
	r2 := sink.Start(&shared.Request{
		Kind: "gateway", Method: "POST", Scheme: "http", Host: "127.0.0.1:8081", Path: "/v1/messages", URL: "http://127.0.0.1:8081/v1/messages", Status: 200, StartedAt: time.Now().UTC(),
		Gateway: &shared.GatewayTrace{RouteID: "coding", ChosenPlan: "glm-coding", ChosenModel: "glm-4.7"},
	})
	sink.Complete(r2)

	srv := api.New("", "", rb, bc, hub, nil, rule.NewRuleStore(), rule.NewRuleEngine(), "127.0.0.1:8080", nil)
	srv.Secrets = sec
	srv.Gateway = gw
	ts := httptest.NewServer(srv)
	defer ts.Close()

	c := New(Options{BaseURL: ts.URL, Timeout: 5 * time.Second})
	ctx := context.Background()

	// Secrets CRUD.
	if err := c.SetSecret(ctx, "glm_key", "sk-client-test-secret"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	secs, err := c.ListSecrets(ctx)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(secs) != 1 || secs[0].Name != "glm_key" {
		t.Fatalf("ListSecrets = %+v", secs)
	}

	// Config draft -> validate -> commit (plan & route) + draft -> discard.
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
`
	if err := c.DraftConfig(ctx, "gateway-plan", "glm-coding", []byte(planYAML)); err != nil {
		t.Fatalf("DraftConfig plan: %v", err)
	}
	ok, diags, err := c.ValidateConfig(ctx, "gateway-plan", "glm-coding")
	if err != nil || !ok || len(diags) > 0 {
		t.Fatalf("ValidateConfig plan = ok=%t diags=%+v err=%v", ok, diags, err)
	}
	if err := c.CommitConfig(ctx, "gateway-plan", "glm-coding"); err != nil {
		t.Fatalf("CommitConfig plan: %v", err)
	}

	routeYAML := `version: 1
id: coding
enabled: true
priority: 10
family: generative
match:
  protocols: [anthropic]
  models: ["coding"]
policy:
  preset: balanced
targets:
  - plan: glm-coding
    model: glm-4.7
`
	if err := c.DraftConfig(ctx, "gateway-route", "bad-route", []byte("version: 2\n")); err != nil {
		t.Fatalf("DraftConfig bad route: %v", err)
	}
	if err := c.DiscardConfig(ctx, "gateway-route", "bad-route"); err != nil {
		t.Fatalf("DiscardConfig: %v", err)
	}
	if err := c.DraftConfig(ctx, "gateway-route", "coding", []byte(routeYAML)); err != nil {
		t.Fatalf("DraftConfig route: %v", err)
	}
	if err := c.CommitConfig(ctx, "gateway-route", "coding"); err != nil {
		t.Fatalf("CommitConfig route: %v", err)
	}

	// Gateway runtime endpoints.
	st, err := c.GatewayStatus(ctx)
	if err != nil || !st.Enabled || st.PlanCount != 1 || st.RouteCount != 1 {
		t.Fatalf("GatewayStatus = %+v, err=%v", st, err)
	}
	plans, err := c.ListGatewayPlans(ctx)
	if err != nil || len(plans) != 1 || plans[0].ID != "glm-coding" || !plans[0].HasSecret {
		t.Fatalf("ListGatewayPlans = %+v, err=%v", plans, err)
	}
	routes, err := c.ListGatewayRoutes(ctx)
	if err != nil || len(routes) != 1 || routes[0].ID != "coding" {
		t.Fatalf("ListGatewayRoutes = %+v, err=%v", routes, err)
	}
	prev, err := c.PreviewGatewayRoute(ctx, pkgapi.GatewayPreviewRequest{
		Protocol: "anthropic",
		Model:    "coding",
		Tools:    true,
	})
	if err != nil || !prev.Matched || prev.RouteID != "coding" {
		t.Fatalf("PreviewGatewayRoute = %+v, err=%v", prev, err)
	}
	if err := c.ResetGatewayPlanCooldown(ctx, "glm-coding"); err != nil {
		t.Fatalf("ResetGatewayPlanCooldown: %v", err)
	}
	usage, err := c.GatewayUsage(ctx, "24h", "glm-coding")
	if err != nil || usage.Window != "24h" {
		t.Fatalf("GatewayUsage = %+v, err=%v", usage, err)
	}
	if complete, _, err := c.ReloadGateway(ctx); err != nil || !complete {
		t.Fatalf("ReloadGateway = complete=%t, err=%v", complete, err)
	}

	// RequestQuery Kind filter.
	gwReqs, err := c.ListRequests(ctx, RequestQuery{Kind: "gateway"})
	if err != nil || len(gwReqs.Requests) != 1 || gwReqs.Requests[0].ID != r2.ID {
		t.Fatalf("ListRequests(Kind=gateway) = %+v, err=%v", gwReqs, err)
	}

	// Delete secret.
	if err := c.DeleteSecret(ctx, "glm_key"); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
}
