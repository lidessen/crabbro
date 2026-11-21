package router

import (
	"context"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
)

func TestDirectResolutionAndPrecedence(t *testing.T) {
	r, snap, _, _ := setupRouterFixture(t)
	p := snap.Plans["glm-coding"]
	p.Models["z-ai/glm"] = &config.ModelSpec{}
	p.Models["qwen3:32b"] = &config.ModelSpec{}
	p.Models["MiniMax-M3.1-Flash"] = &config.ModelSpec{}
	fallback := &config.Route{ID: "fallback", Match: config.RouteMatch{Models: []string{"*"}}, Targets: []config.Target{{Plan: p.ID, Model: "glm-4.7"}}}
	explicit := &config.Route{ID: "specific", Match: config.RouteMatch{Models: []string{"group/smart", "glm-coding/glm-4.7"}}, Targets: fallback.Targets}
	snap.Routes = []*config.Route{fallback, explicit}
	for _, tc := range []struct{ model, resolution, route, canonical string }{
		{"group/smart[1m]", ResolutionExplicit, "specific", ""},
		{"glm-coding/glm-4.7", ResolutionExplicit, "specific", ""},
		{"glm-coding/z-ai/glm[1m]:max", ResolutionDirect, "direct:glm-coding", "z-ai/glm"},
		{"glm-coding/z-ai/glm:HIGH[200k]", ResolutionDirect, "direct:glm-coding", "z-ai/glm"},
		{"glm-coding/qwen3:32b", ResolutionDirect, "direct:glm-coding", "qwen3:32b"},
		{"glm-coding/minimax-m3.1-flash", ResolutionDirect, "direct:glm-coding", "MiniMax-M3.1-Flash"},
		{"unknown/model", ResolutionFallback, "fallback", ""},
	} {
		t.Run(tc.model, func(t *testing.T) {
			rt, res, err := r.ResolveRoute(snap, Signals{Protocol: config.ProtocolAnthropic, Family: config.FamilyGenerative, RequestedModel: tc.model})
			if err != nil || res != tc.resolution || rt.ID != tc.route {
				t.Fatalf("route=%+v resolution=%s err=%v", rt, res, err)
			}
			if tc.canonical != "" && rt.Targets[0].Model != tc.canonical {
				t.Fatal(rt.Targets[0].Model)
			}
		})
	}
	rt, res, err := r.ResolveRoute(snap, Signals{RouteOverride: "fallback", RequestedModel: "group/smart"})
	if err != nil || rt.ID != "fallback" || res != ResolutionOverride {
		t.Fatalf("override %v %s %v", rt, res, err)
	}
	if _, _, err := r.ResolveRoute(snap, Signals{RouteOverride: "absent"}); err == nil {
		t.Fatal("missing override accepted")
	}
	p.Models["MINIMAX-M3.1-FLASH"] = &config.ModelSpec{}
	if _, _, err := r.ResolveRoute(snap, Signals{RequestedModel: "glm-coding/minimax-m3.1-flash"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous: %v", err)
	}
}

func TestDirectFailClosedAndEndpointRestrictions(t *testing.T) {
	r, snap, _, _ := setupRouterFixture(t)
	p := snap.Plans["glm-coding"]
	snap.Routes = []*config.Route{{ID: "fallback", Match: config.RouteMatch{Models: []string{"*"}}, Targets: []config.Target{{Plan: p.ID, Model: "glm-4.7"}}}}
	sig := Signals{Protocol: config.ProtocolAnthropic, Family: config.FamilyGenerative, RequestedModel: "glm-coding/glm-4.7"}
	no := false
	for _, tc := range []struct {
		name, want string
		setup      func()
		reset      func()
	}{
		{"disabled", "disabled", func() { p.Enabled = &no }, func() { p.Enabled = nil }},
		{"direct disabled", "direct addressing is disabled", func() { p.Direct = &config.DirectSpec{Enabled: &no} }, func() { p.Direct = nil }},
		{"profile", "not allowed", func() { p.Direct = &config.DirectSpec{Profiles: []string{"other"}} }, func() { p.Direct = nil }},
		{"model", "not declared", func() { sig.RequestedModel = "glm-coding/missing" }, func() { sig.RequestedModel = "glm-coding/glm-4.7" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup()
			defer tc.reset()
			sel, err := r.Select(context.Background(), snap, sig)
			if err == nil || !strings.Contains(err.Error(), tc.want) || sel != nil {
				t.Fatalf("selection=%v err=%v", sel, err)
			}
		})
	}
	ms := p.Models["glm-4.7"]
	ms.Endpoints = []string{config.ProtocolOpenAIChat}
	sel, err := r.Select(context.Background(), snap, sig)
	if err != nil || sel.Candidates[0].EndpointProto != config.ProtocolOpenAIChat || sel.Decision.Resolution != ResolutionDirect {
		t.Fatalf("selection=%+v err=%v", sel, err)
	}
	sig.Family = config.FamilyDecision
	sig.Protocol = config.ProtocolOpenAIDecisions
	sel, err = r.Select(context.Background(), snap, sig)
	if err == nil || sel == nil || sel.Decision.Resolution != ResolutionDirect || !strings.Contains(err.Error(), "compatible") {
		t.Fatalf("selection=%+v err=%v", sel, err)
	}
	p.Endpoints[config.ProtocolOpenAIDecisions] = &config.EndpointSpec{}
	ms.Endpoints = []string{config.ProtocolOpenAIDecisions}
	sig.Family = config.FamilyGenerative
	sig.Protocol = config.ProtocolAnthropic
	sel, err = r.Select(context.Background(), snap, sig)
	if err == nil || sel == nil || sel.Decision.RouteID != "direct:glm-coding" {
		t.Fatalf("selection=%+v err=%v", sel, err)
	}
}

func TestExplicitRouteModelEndpoints(t *testing.T) {
	r, snap, _, _ := setupRouterFixture(t)
	p := snap.Plans["glm-coding"]
	p.Models["glm-4.7"].Endpoints = []string{config.ProtocolOpenAIChat}
	rt := &config.Route{ID: "explicit", Match: config.RouteMatch{Models: []string{"alias"}}, Targets: []config.Target{{Plan: p.ID, Model: "glm-4.7"}}}
	snap.Routes = []*config.Route{rt}
	sig := Signals{Protocol: config.ProtocolAnthropic, Family: config.FamilyGenerative, RequestedModel: "alias"}
	sel, err := r.Select(context.Background(), snap, sig)
	if err != nil || sel.Candidates[0].EndpointProto != config.ProtocolOpenAIChat || sel.Decision.Resolution != ResolutionExplicit {
		t.Fatalf("selection=%+v err=%v", sel, err)
	}
	rt.Targets[0].EndpointProtocol = config.ProtocolAnthropic
	sel, err = r.Select(context.Background(), snap, sig)
	if err == nil || sel.AllEvaluated[0].RejectReason != "endpoint protocol not allowed for model" {
		t.Fatalf("selection=%+v err=%v", sel, err)
	}
}
