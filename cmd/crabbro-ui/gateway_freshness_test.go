package main

import (
	"testing"
	"time"

	"github.com/lidessen/crabbro/pkg/api"
)

// The store as the window opens, before any snapshot answered: every
// gateway endpoint unanswered. No card may assert a state from that, not
// even Disabled, which the zero value would otherwise read as.
func TestGatewayStatusIsUnknownUntilAnswered(t *testing.T) {
	tt := gwFrame(t, store{conn: connConnected})
	if !tt.HasText("Unknown") {
		t.Error("an unanswered status request should read as a Unknown badge")
	}
	if tt.HasText("Enabled") || tt.HasText("Disabled") {
		t.Error("without an answer neither on nor off may be asserted")
	}
	if tt.HasText("Listen addr") {
		t.Error("an unanswered status leaves no facts to show")
	}
	if tt.HasText("not cooling") {
		t.Error("an unanswered status leaves no plan counts to show")
	}
	if tt.HasText("No plans loaded yet") || tt.HasText("Plans unavailable") {
		t.Error("an unanswered plans request must not read as off or as an empty plan set")
	}
	if !tt.HasText("Plans unknown") {
		t.Error("the plans card should say its state is unknown")
	}
}

// A failed first fetch leaves every gateway section unknown, and the first
// answer that arrives is fresh.
func TestGatewayFailedFirstFetchStaysUnknown(t *testing.T) {
	st := store{conn: connConnected}
	st.applyGateway(gwAnswers{}) // every endpoint failed its first answer
	if st.gwStatusFetch != gwUnknown || st.gwPlansFetch != gwUnknown || st.gwRoutesFetch != gwUnknown ||
		st.gwUsageFetch != gwUnknown || st.gwSecretsFetch != gwUnknown {
		t.Error("a failed first fetch must leave every gateway section unknown, not off or empty")
	}

	st.applyGateway(gwAnswers{
		statusOK:  true,
		plansOK:   true,
		routesOK:  true,
		usageOK:   true,
		secretsOK: true,
	})
	if st.gwStatusFetch != gwFresh || st.gwPlansFetch != gwFresh || st.gwRoutesFetch != gwFresh ||
		st.gwUsageFetch != gwFresh || st.gwSecretsFetch != gwFresh {
		t.Error("an answered fetch should read fresh")
	}
}

// A round where only some endpoints answered keeps the failed sections'
// last-good data, marked stale, beside the answered ones read fresh: the
// sections never blend into one consistent-sounding snapshot.
func TestGatewayPartialRefreshKeepsLastGoodAsStale(t *testing.T) {
	st := store{conn: connConnected}
	st.applyGateway(gwAnswers{
		statusOK:  true,
		status:    api.GatewayStatusResponse{Enabled: true, ListenAddr: "127.0.0.1:8081", PlanCount: 2, HealthyPlans: 2},
		plansOK:   true,
		plans:     []api.GatewayPlanStatus{{ID: "kept-plan", Enabled: true}},
		routesOK:  true,
		routes:    []api.GatewayRouteStatus{{ID: "coding-main", Family: "generative"}},
		usageOK:   true,
		usage:     api.GatewayUsageResponse{Window: "24h", Totals: api.GatewayUsageBucket{Requests: 4}},
		secretsOK: true,
		secrets:   []api.SecretSummary{{Name: "glm_key"}},
	})

	// Only the plans endpoint answers this round.
	st.applyGateway(gwAnswers{
		plansOK: true,
		plans:   []api.GatewayPlanStatus{{ID: "fresh-plan", Enabled: true}},
	})
	if st.gwStatusFetch != gwStale {
		t.Error("a failed status fetch must mark the kept status stale")
	}
	if st.gwStatus.ListenAddr != "127.0.0.1:8081" {
		t.Error("a failed status fetch must keep the last-good status")
	}
	if st.gwPlansFetch != gwFresh || st.gwPlans[0].ID != "fresh-plan" {
		t.Error("an answered plans fetch must replace the plans and read fresh")
	}
	if st.gwRoutesFetch != gwStale || st.gwUsageFetch != gwStale || st.gwSecretsFetch != gwStale {
		t.Error("failed fetches must mark their kept sections stale, not fresh")
	}

	// Another fully failed round keeps the same kept data, still stale.
	st.applyGateway(gwAnswers{})
	if st.gwStatusFetch != gwStale || st.gwStatus.ListenAddr != "127.0.0.1:8081" {
		t.Error("a failed round after data exists must keep it marked stale")
	}
	if st.gwPlansFetch != gwStale || st.gwPlans[0].ID != "fresh-plan" {
		t.Error("a failed round must keep the last answered plans marked stale")
	}
}

// On the page, a kept status card and a fresh plans card each say what they
// are: the status card wears its staleness, the plans card does not borrow
// it, and neither sounds like one consistent snapshot.
func TestGatewayStaleAndFreshCardsAreSaidApart(t *testing.T) {
	st := gwPlanStore(time.Now().Add(90 * time.Second))
	st.gwStatusFetch = gwStale // the status request failed; its answer is kept
	st.gwPlansFetch = gwFresh  // the plans request answered
	st.gwRoutesFetch, st.gwUsageFetch, st.gwSecretsFetch = gwUnknown, gwUnknown, gwUnknown

	tt := gwFrame(t, st)
	if !tt.HasText("the latest status request failed") {
		t.Error("the status card must say its data is the kept last answer")
	}
	if !tt.HasText("stale") {
		t.Error("the status card header should carry the stale mark")
	}
	if tt.HasText("the latest plans request failed") {
		t.Error("the plans card answered; it must not wear the status card's staleness")
	}
	if !tt.HasText("glm-coding") {
		t.Error("the fresh plans should still be listed")
	}
	if tt.HasText("Plans unknown") {
		t.Error("the answered plans card must not read as unknown")
	}
}

// The plan counts must say configured and enabled for what each counts:
// PlanCount includes disabled plans, while not-cooling/cooling cover the
// enabled ones only.
func TestGatewayPlanCountsSayConfiguredApartFromEnabled(t *testing.T) {
	tt := gwFrame(t, gwPlanStore(time.Now().Add(90*time.Second)))
	if !tt.HasText("2 configured · 2 enabled (1 not cooling, 1 cooling)") {
		t.Error("the counts should say configured and enabled separately")
	}
	if tt.HasText("disabled") {
		t.Error("no plan is disabled here; the counts must not invent any")
	}

	// A third plan sits disabled in the config: the configured count says
	// three, the enabled counts still cover two.
	st := gwPlanStore(time.Now().Add(90 * time.Second))
	st.gwStatus.PlanCount = 3
	st.gwPlans = append(st.gwPlans, api.GatewayPlanStatus{
		ID:         "glm-disabled",
		Enabled:    false,
		Credential: "glm_key",
		HasSecret:  true,
		Endpoints:  []string{"anthropic"},
		Models:     []string{"glm-4.7"},
	})
	tt = gwFrame(t, st)
	if !tt.HasText("3 configured · 2 enabled (1 not cooling, 1 cooling) · 1 disabled") {
		t.Error("a disabled plan should be counted as configured and disabled, not as active")
	}
}

// A cooldown whose reported end time has passed must not be asserted as
// still active: say the window ended and the plan state is not refreshed.
func TestGatewayExpiredCooldownIsNotAssertedActive(t *testing.T) {
	st := gwPlanStore(time.Now().Add(-2 * time.Minute))
	tt := gwFrame(t, st)
	if tt.HasText("Plan cooling") {
		t.Error("a cooldown whose reported end has passed must not be asserted as still active")
	}
	if !tt.HasText("Cooldown ended") || !tt.HasText("ago (as last reported); plan state not refreshed") {
		t.Error("an expired cooldown should say the window ended and the plan state awaits a refresh")
	}
}
