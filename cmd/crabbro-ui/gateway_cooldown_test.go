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
	return gwFrameApp(t, newAppForTest(st, 1))
}

// newAppForTest builds an app holding st with the usage range selected by
// winIdx, the range mirrored as the sync goroutine would have it after a
// frame. The selected range is view state, so it is set on the app, not the
// store.
func newAppForTest(st store, winIdx int) *app {
	a := newApp(nil, context.Background())
	a.st = st
	a.page = pageGateway
	a.gwUsageWinIdx = winIdx
	a.mirrorView()
	return a
}

// gwFrameApp draws one gateway page frame for a.
func gwFrameApp(t *testing.T, a *app) *ui.Tester {
	t.Helper()
	tt := ui.NewTester(a.view, 1280, 820)
	tt.Frame()
	return tt
}

// gwPlanStore is two plans on an enabled gateway: one out of cooldown that
// still carries its persistent last cooldown reason, and one cooling. The
// store reads as one after a successful snapshot: every endpoint answered.
func gwPlanStore(until time.Time) store {
	return store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwPlansFetch:  gwFresh,
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
// unavailable, not claim an empty plan set. Only a fresh status answer may
// say the gateway is off, and an empty list on a gateway that is on still
// proves nothing about the config.
func TestGatewayPlansCardDistinguishesOffFromNoneConfigured(t *testing.T) {
	off := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwPlansFetch:  gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: false},
		// The off gateway answers the plans request with an empty list.
		gwPlans: []api.GatewayPlanStatus{},
	}
	if tt := gwFrame(t, off); !tt.HasText("Plans unavailable: the gateway is off") || tt.HasText("No plans reported in this answer") {
		t.Error("a disabled gateway should say plan state is unavailable, not comment on whether plans are configured")
	}

	none := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwPlansFetch:  gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: true},
		gwPlans:       []api.GatewayPlanStatus{},
	}
	tt := gwFrame(t, none)
	if !tt.HasText("No plans reported in this answer") || tt.HasText("Plans unavailable") {
		t.Error("an enabled gateway with an empty answer should report the empty answer, not that the gateway is off")
	}
	if !tt.HasText("does not prove none are configured") {
		t.Error("an empty plan answer must not be read as an empty plan config")
	}
	if !tt.HasText("~/.crabbro/gateway/plans/") {
		t.Error("the card should still name where plans live, without asserting that directory is empty")
	}
}
