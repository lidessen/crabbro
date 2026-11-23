package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/crabbro/pkg/api"
)

func TestGatewayUIViewsRenderLightAndDark(t *testing.T) {
	a := newApp(nil, context.Background())
	now := time.Now().UTC()
	until := now.Add(90 * time.Second)
	cal := true
	bodyReq := `{"model":"coding-main","messages":[{"role":"user","content":"hi"}]}`
	bodyResp := `{"content":[{"type":"text","text":"hello"}]}`

	a.st = store{
		conn:           connConnected,
		gwStatusFetch:  gwFresh,
		gwPlansFetch:   gwFresh,
		gwRoutesFetch:  gwFresh,
		gwUsageFetch:   gwFresh,
		gwSecretsFetch: gwFresh,
		status: api.DaemonStatus{
			Version:        "0.1.0",
			ProxyAddr:      "127.0.0.1:8080",
			GatewayEnabled: true,
			GatewayAddr:    "127.0.0.1:8081",
		},
		gwStatus: api.GatewayStatusResponse{
			Enabled:            true,
			ListenAddr:         "127.0.0.1:8081",
			SnapshotGeneration: 3,
			PlanCount:          2,
			RouteCount:         1,
			HealthyPlans:       1,
			CoolingPlans:       1,
		},
		gwPlans: []api.GatewayPlanStatus{
			{
				ID:         "glm-coding",
				Enabled:    true,
				Credential: "glm_key",
				HasSecret:  true,
				Endpoints:  []string{"anthropic", "openai-chat"},
				Models:     []string{"glm-4.7"},
				Healthy:    true,
				Windows: []api.GatewayWindowUsage{
					{Name: "5h", Duration: "5h", Mode: "hard", UsedTokens: 12000, MaxTokens: 100000, RemainingRatio: 0.88, Surplus: 0.31},
				},
			},
			{
				ID:            "codex-sub",
				Enabled:       true,
				Credential:    "codex_key",
				HasSecret:     true,
				Endpoints:     []string{"openai-responses"},
				Models:        []string{"gpt-5-codex"},
				Healthy:       false,
				CooldownUntil: &until,
				CooldownScope: "plan",
				LastError:     "429 rate limited",
			},
		},
		gwRoutes: []api.GatewayRouteStatus{
			{
				ID:        "coding-main",
				Enabled:   true,
				Family:    "generative",
				Preset:    "balanced",
				Protocols: []string{"anthropic", "openai-chat"},
				Models:    []string{"coding-main"},
				Targets: []api.GatewayRouteTargetStatus{
					{Plan: "glm-coding", Model: "glm-4.7", Enabled: true},
					{Plan: "codex-sub", Model: "gpt-5-codex", Enabled: true},
				},
			},
		},
		gwUsage: api.GatewayUsageResponse{
			Window: "24h",
			Totals: api.GatewayUsageBucket{Requests: 14, InputTokens: 9000, OutputTokens: 3200, CacheReadTokens: 4500, TotalTokens: 16700, CacheHitRatio: 1.0 / 3, AvgTTFTMs: 180},
			Buckets: []api.GatewayUsageBucket{
				{Plan: "glm-coding", Model: "glm-4.7", Requests: 10, InputTokens: 7000, CacheReadTokens: 4000, TotalTokens: 12000, CacheHitRatio: 4.0 / 11, AvgTTFTMs: 180},
				{Plan: "glm-coding", Model: "glm-flash", Requests: 3, InputTokens: 1000, CacheReadTokens: 500, TotalTokens: 3500, CacheHitRatio: 1.0 / 3, AvgTTFTMs: 100},
				{Plan: "codex-sub", Model: "gpt-5-codex", Requests: 1, InputTokens: 1000, TotalTokens: 1200},
			},
		},
		// The range the snapshot asked for: it agrees with the default
		// selected 24h, so the card shows one range's own numbers.
		gwUsageAsked: "24h",
		gwSecrets: []api.SecretSummary{
			{Name: "glm_key", UpdatedAt: now, SizeBytes: 42},
		},
		requests: []api.RequestSummary{
			{
				ID:           "req_gw_1",
				Kind:         "gateway",
				Method:       "POST",
				URL:          "http://127.0.0.1:8081/v1/messages",
				Status:       200,
				DurationMs:   320,
				StartedAt:    now.Format(time.RFC3339Nano),
				GatewayPlan:  "glm-coding",
				GatewayModel: "glm-4.7",
				GatewayRoute: "coding-main",
			},
		},
		detailID: "req_gw_1",
		detail: &api.RequestDetailResponse{
			Request: api.Request{
				ID:     "req_gw_1",
				Kind:   "gateway",
				Method: "POST",
				URL:    "http://127.0.0.1:8081/v1/messages",
				Status: 200,
				Gateway: &api.GatewayTrace{
					RouteID:          "coding-main",
					Family:           "generative",
					IngressProtocol:  "anthropic",
					ChosenPlan:       "glm-coding",
					ChosenModel:      "glm-4.7",
					UpstreamProtocol: "openai-chat",
					Translated:       true,
					Calibrated:       &cal,
					TurnPhase:        "ToolLoop",
					SessionKey:       "ses_test",
					CacheGravity:     0.91,
					Usage:            &api.TokenUsage{InputTokens: 100, OutputTokens: 40, CacheReadTokens: 80},
					Attempts: []api.GatewayAttempt{
						{Plan: "glm-coding", Model: "glm-4.7", Protocol: "openai-chat", Status: 200, TTFTMs: 120, LatencyMs: 320},
					},
					RouteDecision: &api.RouteDecision{
						Candidates: []api.CandidateDecision{
							{Plan: "glm-coding", Model: "glm-4.7", Protocol: "openai-chat", Eligible: true, Scores: api.CandidateScores{Quality: 0.9, Cache: 0.9, Quota: 0.8, Protocol: 0.85, Health: 1, Total: 0.89}},
							{Plan: "codex-sub", Model: "gpt-5-codex", Protocol: "openai-responses", Eligible: false, RejectReason: "plan cooling"},
						},
					},
				},
			},
			Body:         &bodyReq,
			ResponseBody: &bodyResp,
		},
	}

	tt := ui.NewTester(a.view, 860, 540)
	click := func(label string) {
		t.Helper()
		if err := tt.Click(label); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
	}
	baseline := a.st
	for _, dark := range []bool{false, true} {
		tt.SetDark(dark)
		a.st = baseline
		a.gwUsageRevision = 0
		a.page = pageGateway
		a.gwSection = gwOverview
		tt.Frame()
		tt.Advance(200 * time.Millisecond)
		if !tt.HasText("Configuration") || !tt.HasText("Traffic") {
			t.Fatal("overview should show configuration and traffic")
		}

		click("View plans")
		if !tt.HasText("2 provider plans") || tt.HasText("Configuration") {
			t.Fatal("overview shortcut should open the plans section")
		}
		if !tt.HasText("127.0.0.1:8081") || tt.HasText("127.0.0.1:8080") {
			t.Fatal("the Gateway workspace should show only the Gateway listener")
		}
		click("Routes")
		if !tt.HasText("Configured routes") || tt.HasText("Targets in configuration order") {
			t.Fatal("routes should initially show collapsed rows")
		}
		click("Show route coding-main")
		if !tt.HasText("Targets in configuration order") || !tt.HasText("gpt-5-codex") {
			t.Fatal("expanded route should show its configured targets")
		}
		click("Hide route coding-main")
		if tt.HasText("Targets in configuration order") {
			t.Fatal("route details should collapse")
		}
		click("Usage")
		if !tt.HasText("Ranked by tokens") || !tt.HasText("16,700") {
			t.Fatal("usage should show formatted token totals")
		}
		if !tt.HasText("Ranked by tokens") || !tt.HasText("15,500") ||
			!tt.HasText("36%") || tt.HasText("glm-flash") {
			t.Fatal("narrow usage should show the plan ranking and totals with model details closed")
		}
		click("Show models for glm-coding")
		if !tt.HasText("2 models") || !tt.HasText("glm-flash") || !tt.HasText("3,500 tokens") || tt.HasText("gpt-5-codex") {
			t.Fatal("expanding a plan should show only its model breakdown")
		}
		click("Hide models for glm-coding")
		if tt.HasText("glm-flash") {
			t.Fatal("a collapsed plan should hide its model breakdown")
		}
		tt.SetSize(1280, 820)
		tt.Frame()
		rankedPlan, rankedOK := tt.Find("Show models for glm-coding")
		firstDetail, detailOK := tt.Find("glm-flash")
		if !rankedOK || !detailOK || firstDetail.X < rankedPlan.X+rankedPlan.W {
			t.Fatal("wide usage should show the top plan's details beside the ranking")
		}
		click("Show models for codex-sub")
		if !tt.HasText("gpt-5-codex") || tt.HasText("glm-flash") {
			t.Fatal("the detail panel should follow the selected ranking row")
		}
		tt.SetSize(860, 540)
		tt.Frame()
		click("Hide models for codex-sub")
		click("1h")
		oneHour := a.currentUsageRequest()
		if !tt.HasText("Loading 1h · Showing 24h") || tt.HasText("Range mismatch") ||
			!tt.HasText("16,700") || oneHour.window != "1h" {
			t.Fatal("range switch should keep the labeled previous totals while loading")
		}
		click("7d")
		week := a.currentUsageRequest()
		a.st.applyGateway(gwAnswers{usageRequest: oneHour})
		tt.Frame()
		if !tt.HasText("Loading 7d · Showing 24h") || tt.HasText("stale") || tt.HasText("Range mismatch") {
			t.Fatal("a failed request for an earlier selection should leave the new selection loading")
		}
		a.st.applyGateway(gwAnswers{usageRequest: week})
		tt.Frame()
		if tt.HasText("Loading 7d") || !tt.HasText("the latest usage request failed") || !tt.HasText("Range mismatch") {
			t.Fatal("a failed request for the current selection should show the warning")
		}

		click("24h")
		a.st.applyGateway(gwAnswers{usageOK: true, usageRequest: a.currentUsageRequest(), usage: a.st.gwUsage})
		tt.Frame()
		if !tt.HasText("24h (as answered)") || tt.HasText("Loading 24h") || tt.HasText("Range mismatch") {
			t.Fatal("a matching reply should clear the loading state")
		}
		click("Credentials")
		if !tt.HasText("Stored credentials") || !tt.HasText("glm_key") {
			t.Fatal("credentials should show stored names")
		}

		a.page = pageRequests
		a.reqSel = 0
		a.reqShownID = "req_gw_1"
		a.detailTab = 3
		tt.Frame()
		tt.Advance(200 * time.Millisecond)
	}
}

// The scroll body may be much taller than the window, but the sidebar and
// the top of the selected section must stay within the workspace.
func TestGatewayPlanColumnsKeepNaturalHeightsAndScroll(t *testing.T) {
	st := gwPlanStore(time.Now().Add(time.Minute))
	for range 4 {
		st.gwPlans[1].Windows = append(st.gwPlans[1].Windows, api.GatewayWindowUsage{
			Name: "Fixture window", Duration: "5h", Mode: "hard", MaxRequests: 100, RemainingRatio: 0.4,
		})
	}
	for i := range 18 {
		plan := st.gwPlans[0]
		plan.ID = fmt.Sprintf("fixture-plan-%02d", i)
		st.gwPlans = append(st.gwPlans, plan)
	}
	a := newAppForTest(st, 1)
	a.gwSection = gwPlans
	tt := ui.NewTester(a.view, 1280, 820)
	tt.Frame()
	check := func() (ui.Rect, ui.Rect) {
		t.Helper()
		nav, ok := tt.Find("Overview")
		if !ok || nav.Y < titleBarH || nav.Y > 200 {
			t.Fatalf("sidebar link should sit close to the top of the workspace, got %+v", nav)
		}
		first, ok := tt.Find("glm-coding")
		if !ok {
			t.Fatal("first provider card missing")
		}
		return nav, first
	}
	nav, first := check()
	second, ok := tt.Find("codex-sub")
	if !ok || first.Y != second.Y || first.X >= second.X || first.Y < titleBarH {
		t.Fatalf("first row should show two cards below the title bar, got %+v and %+v", first, second)
	}
	left, leftOK := tt.Find("fixture-plan-00")
	right, rightOK := tt.Find("fixture-plan-01")
	if !leftOK || !rightOK || left.X != first.X || right.X != second.X || left.Y >= right.Y-100 {
		t.Fatalf("the next cards should stack independently under unequal predecessors, got %+v and %+v", left, right)
	}
	tt.Scroll(800, 400, 0, 480)
	tt.Advance(time.Second)
	scrolledNav, ok := tt.Find("Overview")
	if !ok || nav != scrolledNav {
		t.Fatalf("sidebar should stay in place when cards scroll, got %+v then %+v", nav, scrolledNav)
	}
	if moved, ok := tt.Find("glm-coding"); ok && moved.Y >= first.Y {
		t.Fatalf("cards did not scroll, first card at %+v then %+v", first, moved)
	}
	tt.SetSize(860, 820)
	tt.Scroll(500, 300, 0, -2000)
	tt.Frame()
	first, firstOK := tt.Find("glm-coding")
	second, secondOK := tt.Find("codex-sub")
	if !firstOK || !secondOK || first.X != second.X || first.Y >= second.Y {
		t.Fatalf("narrow plans should keep a single column in data order, got %+v and %+v", first, second)
	}
}
