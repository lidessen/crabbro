package main

import (
	"context"
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
					{Plan: "glm-coding", Model: "glm-4.7"},
					{Plan: "codex-sub", Model: "gpt-5-codex"},
				},
			},
		},
		gwUsage: api.GatewayUsageResponse{
			Window: "24h",
			Totals: api.GatewayUsageBucket{Requests: 14, InputTokens: 9000, OutputTokens: 3200, CacheReadTokens: 4500, TotalTokens: 16700, CacheHitRatio: 0.5, AvgTTFTMs: 180},
			Buckets: []api.GatewayUsageBucket{
				{Plan: "glm-coding", Model: "glm-4.7", Requests: 14, TotalTokens: 16700, CacheHitRatio: 0.5, AvgTTFTMs: 180},
			},
		},
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

	tt := ui.NewTester(a.view, 1280, 820)
	for _, dark := range []bool{false, true} {
		tt.SetDark(dark)
		a.page = pageGateway
		tt.Frame()
		tt.Advance(200 * time.Millisecond)

		a.page = pageRequests
		a.reqSel = 0
		a.reqShownID = "req_gw_1"
		a.detailTab = 3
		tt.Frame()
		tt.Advance(200 * time.Millisecond)
	}
}
