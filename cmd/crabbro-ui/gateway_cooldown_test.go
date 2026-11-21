package main

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

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

// gwLastReasonText returns the rendered historical reason line, if any.
func gwLastReasonText(tt *ui.Tester) string {
	for _, text := range tt.Texts() {
		if strings.HasPrefix(text, "Last cooldown reason") {
			return text
		}
	}
	return ""
}

// gwUnsafeReason is shaped like an upstream body: an HTML dump with a private
// response, credential parameters, odd key prefixes, a fake API key, a Bearer
// token, an email, query tokens, newlines, and a large payload.
func gwUnsafeReason() string {
	return "<html><body>private response</body> sk-abc123456789012345 Bearer secret-token-value " +
		"user@example.com ?credential=example-secret xsk-example\n" +
		"https://upstream.example/v1/chat?api_key=leaked-token&key=secondary-leak " +
		strings.Repeat("<div>response dump</div>", 64) + "UPSTREAM-BODY-MARKER</body></html>"
}

// The UI boundary allows only the classifier's fixed summaries. A plan status
// answer carrying raw LastError text must render the fixed unclassified marker
// and never echo any body, credential, email, URL, or prompt fragment.
func TestGatewayPlansCardRejectsUnrecognizedLastErrorAtBoundary(t *testing.T) {
	st := gwPlanStore(time.Now().Add(90 * time.Second))
	st.gwPlans[0].LastError = strings.Join([]string{
		"<body>private response</body>",
		"?credential=example-secret",
		"xsk-example",
		"sk-abc123456789012345",
		"Bearer secret-token-value",
		"user@example.com",
	}, " ")
	st.gwPlans[1].LastError = gwUnsafeReason()

	tt := gwFrame(t, st)
	want := "Last cooldown reason (historical, time & model not reported): " + gwUnclassifiedReason
	reasons := 0
	for _, text := range tt.Texts() {
		for _, banned := range []string{
			"private response", "credential=example-secret", "xsk-example",
			"sk-abc123456789012345", "secret-token-value", "user@example.com",
			"api_key=leaked-token", "key=secondary-leak", "UPSTREAM-BODY-MARKER",
			"<html", "<body", "https://", "\n", "\r", "\t",
		} {
			if strings.Contains(text, banned) {
				t.Errorf("rendered text %q leaked %q", text, banned)
			}
		}
		for _, r := range text {
			if unicode.IsControl(r) {
				t.Errorf("rendered text %q contains control character %U", text, r)
			}
		}
		if !strings.HasPrefix(text, "Last cooldown reason") {
			continue
		}
		reasons++
		if text != want {
			t.Errorf("rendered reason = %q, want %q", text, want)
		}
		if n := utf8.RuneCountInString(text); n > 200 {
			t.Errorf("rendered reason is %d runes, want a bounded display: %q", n, text)
		}
	}
	if reasons != len(st.gwPlans) {
		t.Errorf("rendered %d historical reason lines, want %d", reasons, len(st.gwPlans))
	}
}

// The UI allowlist is closed: every classified summary passes unchanged, and
// anything else becomes the same fixed marker as the router's.
func TestGatewaySafeReasonAllowsOnlyClassifiedSummaries(t *testing.T) {
	allowed := []string{
		"rate limited (429)",
		"quota exhausted",
		"quota exhausted (429)",
		"payment or quota required (402)",
		"upstream auth error (401)",
		"upstream permission denied (403)",
		"model or endpoint not found",
		"model or endpoint not found (404)",
		"model capability or endpoint unavailable",
		"context length exceeded",
		"upstream timeout",
		"transport error",
		"mid-stream drop",
		"upstream SSE error",
		"upstream SSE error (rate_limit)",
		"upstream SSE error (quota_exhausted)",
		"upstream SSE error (auth_error)",
		"429 rate limited",
		"insufficient balance",
		"upstream invalid request (400)",
		"upstream rejected request (418)",
		"upstream server error (502)",
	}
	for _, reason := range allowed {
		if got := gwSafeReason(reason); got != reason {
			t.Errorf("gwSafeReason(%q) = %q, want the classified summary unchanged", reason, got)
		}
	}
	for _, reason := range []string{
		"<body>private response</body>",
		"?credential=example-secret",
		"xsk-example",
		"rate limited (429) please retry",
		gwUnsafeReason(),
	} {
		if got := gwSafeReason(reason); got != gwUnclassifiedReason {
			t.Errorf("gwSafeReason(%q) = %q, want %q", reason, got, gwUnclassifiedReason)
		}
	}
	if got := gwSafeReason(" \n\t\x00 "); got != "" {
		t.Errorf("blank reason = %q, want empty", got)
	}
}

// A reason that sanitizes to nothing must not render an empty labeled line.
func TestGatewayPlansCardSkipsBlankLastError(t *testing.T) {
	st := gwPlanStore(time.Now().Add(90 * time.Second))
	for i := range st.gwPlans {
		st.gwPlans[i].LastError = " \n\t\x00 "
	}
	tt := gwFrame(t, st)
	if reason := gwLastReasonText(tt); reason != "" {
		t.Errorf("blank LastError should not render a reason line, got %q", reason)
	}
}

func TestGatewayRemoteQuotaSummary(t *testing.T) {
	st := gwPlanStore(time.Now().Add(time.Minute))
	usage := 25.0
	st.gwPlans[0].RemoteQuota = &api.RemoteQuotaJSON{Balance: "$17.65", UsagePct: &usage, ResetsRemaining: 2, Stale: true}
	tt := gwFrame(t, st)
	if !tt.HasText("Remote quota (stale) · $17.65 · 25% used · 2 resets") {
		t.Fatal("remote quota summary missing")
	}
	if !tt.HasText("Refresh quotas") {
		t.Fatal("refresh action missing")
	}
}
