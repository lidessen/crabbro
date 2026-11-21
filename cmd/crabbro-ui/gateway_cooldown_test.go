package main

import (
	"context"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/crabbro/pkg/api"
)

// gwFrame renders the gateway page headlessly against st and returns the
// tester that drew it, for text assertions on the frame it holds.
func gwFrame(t *testing.T, st store) *ui.Tester {
	t.Helper()
	a := newApp(nil, context.Background())
	a.st = st
	tt := ui.NewTester(a.view, 1280, 820)
	a.page = pageGateway
	tt.Frame()
	return tt
}

// gwPlanStore is two plans on an enabled gateway: one out of cooldown that
// still carries its persistent last cooldown reason, and one cooling.
func gwPlanStore(until time.Time) store {
	return store{
		conn: connConnected,
		gwStatus: api.GatewayStatusResponse{
			Enabled:      true,
			ListenAddr:   "127.0.0.1:8081",
			PlanCount:    2,
			RouteCount:   1,
			HealthyPlans: 1,
			CoolingPlans: 1,
		},
		gwPlans: []api.GatewayPlanStatus{
			{
				ID:         "glm-coding",
				Enabled:    true,
				Credential: "glm_key",
				HasSecret:  true,
				Endpoints:  []string{"anthropic"},
				Models:     []string{"glm-4.7"},
				Healthy:    true,
				// Persistent: it outlives the cooldown it came from.
				LastError: "429 rate limited",
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
				LastError:     "quota exhausted",
			},
		},
	}
}

// The plan out of cooldown must say so about the plan, not claim the
// provider is healthy, and the count in the status card must agree.
func TestGatewayPlanNotCoolingIsNotProviderHealthy(t *testing.T) {
	tt := gwFrame(t, gwPlanStore(time.Now().Add(90*time.Second)))
	if !tt.HasText("Plan not cooling") {
		t.Error("a plan without plan-level cooldown should say it is the plan that is not cooling")
	}
	if tt.HasText("Healthy") {
		t.Error("no active plan cooldown is not a provider health verdict; the label Healthy must not appear")
	}
	if !tt.HasText("not cooling") {
		t.Error("the status card plan count should say not cooling, not healthy")
	}
}

// A cooling plan shows its remaining time, and one whose remaining time the
// API does not report says that instead of implying there is none.
func TestGatewayCoolingPlanShowsRemainingTimeOrItsAbsence(t *testing.T) {
	tt := gwFrame(t, gwPlanStore(time.Now().Add(90*time.Second)))
	if !tt.HasText("Plan cooling (") || !tt.HasText(" left)") {
		t.Error("a cooling plan should say Plan cooling with the remaining time left")
	}

	unreported := gwPlanStore(time.Time{})
	for i := range unreported.gwPlans {
		if !unreported.gwPlans[i].Healthy {
			unreported.gwPlans[i].CooldownUntil = nil
		}
	}
	tt = gwFrame(t, unreported)
	if !tt.HasText("Plan cooling (time not reported)") {
		t.Error("a cooling plan without a reported end time should say the time is not reported")
	}
}

// LastError is historical and persistent: it must be labeled as the last
// cooldown reason, with its missing time and model said, and it may sit
// beside a plan that is not cooling.
func TestGatewayLastErrorIsLabeledHistoricalLastCooldownReason(t *testing.T) {
	tt := gwFrame(t, gwPlanStore(time.Now().Add(90*time.Second)))
	if !tt.HasText("Last cooldown reason (historical, time & model not reported): 429 rate limited") {
		t.Error("LastError should be labeled as the historical last cooldown reason, with time and model said to be unreported")
	}
	if !tt.HasText("Plan not cooling") {
		t.Error("the labeled historical reason should still sit beside the plan that is not cooling")
	}
}

// The card must say model-level cooldowns are not reported, rather than
// let a plan-level not cooling imply every model is available.
func TestGatewayPlansCardNotesUnreportedModelCooldowns(t *testing.T) {
	tt := gwFrame(t, gwPlanStore(time.Now().Add(90*time.Second)))
	if !tt.HasText("model-level cooldowns are not reported") {
		t.Error("the plans card should say model-level cooldowns are not reported")
	}
}

// A gateway that is off answers with no plans: the card must call that
// unavailable, not repeat the staging hint meant for a gateway that is on.
func TestGatewayPlansCardDistinguishesOffFromNoneConfigured(t *testing.T) {
	off := store{conn: connConnected, gwStatus: api.GatewayStatusResponse{Enabled: false}}
	if tt := gwFrame(t, off); !tt.HasText("Plans unavailable: the gateway is off") || tt.HasText("No plans loaded yet") {
		t.Error("a disabled gateway should say plan state is unavailable, not that no plans are loaded")
	}

	none := store{
		conn:     connConnected,
		gwStatus: api.GatewayStatusResponse{Enabled: true},
		gwPlans:  []api.GatewayPlanStatus{},
	}
	if tt := gwFrame(t, none); !tt.HasText("No plans loaded yet") || tt.HasText("Plans unavailable") {
		t.Error("an enabled gateway with zero plans should keep the staging hint")
	}
}
