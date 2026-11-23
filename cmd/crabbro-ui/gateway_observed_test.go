package main

import (
	"testing"

	"github.com/lidessen/crabbro/pkg/api"
)

// An empty plan answer must not be read as "no plans configured" in any
// direction of a mixed refresh: a kept empty answer beside a fresh enabled
// status, and a fresh empty answer beside a kept status, both describe an
// answer, not a config.
func TestGatewayEmptyPlansNeverClaimNoneConfigured(t *testing.T) {
	cases := []struct {
		name        string
		statusFetch gwFetch
		plansFetch  gwFetch
		enabled     bool
	}{
		{"stale empty plans beside fresh enabled status", gwFresh, gwStale, true},
		{"fresh empty plans beside stale enabled status", gwStale, gwFresh, true},
		{"fresh empty plans beside fresh enabled status", gwFresh, gwFresh, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := store{
				conn:          connConnected,
				gwStatusFetch: tc.statusFetch,
				gwPlansFetch:  tc.plansFetch,
				gwStatus:      api.GatewayStatusResponse{Enabled: tc.enabled, ListenAddr: "127.0.0.1:8081"},
				gwPlans:       []api.GatewayPlanStatus{},
			}
			tt := gwFrame(t, st)
			if tt.HasText("No plans loaded yet") {
				t.Error("an empty plan answer must not be read as none configured")
			}
			if tt.HasText("Plans unavailable: the gateway is off") {
				t.Error("the gateway is on here; the empty answer must not be blamed on it being off")
			}
			if !tt.HasText("does not prove none are configured") || !tt.HasText("No plans reported") {
				t.Error("the card should report an empty answer and decline to assert an empty config")
			}
		})
	}
}

// A kept empty plan answer alongside a fresh enabled status is the review's
// first case: the status answered on, the plans answer is old, and neither
// together proves the plan set is empty.
func TestGatewayStaleEmptyPlansWithFreshEnabledStatus(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwPlansFetch:  gwStale,
		gwStatus:      api.GatewayStatusResponse{Enabled: true, ListenAddr: "127.0.0.1:8081", PlanCount: 3},
		gwPlans:       []api.GatewayPlanStatus{},
	}
	tt := gwFrame(t, st)
	if !tt.HasText("the latest plans request failed") {
		t.Error("the kept empty plans answer should be marked as the stale answer it is")
	}
	if !tt.HasText("does not prove none are configured") {
		t.Error("a stale empty answer must not be upgraded into none configured")
	}
}

// A selected range the data has not caught up with must be labeled as a
// different window, in both directions: an older smaller range kept while a
// larger one is selected, and an older larger range kept while a smaller one
// is selected. Neither may pass as the selected range's own totals.
func TestGatewayUsageRangeMismatchIsLabeledBothDirections(t *testing.T) {
	cases := []struct {
		name     string
		winIdx   int // selected range
		asked    string
		reported string
	}{
		{"24h data kept while 1h is selected", 0, "24h", "24h"},
		{"1h data kept while 24h is selected", 1, "1h", "1h"},
		{"24h data kept while 7d is selected", 2, "24h", "24h"},
		{"7d data kept while 1h is selected", 0, "7d", "7d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := store{
				conn:         connConnected,
				gwUsageFetch: gwFresh,
				gwUsageAsked: tc.asked,
				gwUsage: api.GatewayUsageResponse{
					Window:  tc.reported,
					Totals:  api.GatewayUsageBucket{Requests: 9, TotalTokens: 500},
					Buckets: []api.GatewayUsageBucket{{Plan: "glm-coding", Model: "glm-4.7", Requests: 9}},
				},
			}
			tt := gwFrameApp(t, newAppForTest(st, tc.winIdx))
			if !tt.HasText("Range mismatch") {
				t.Error("data for a different range than selected must be labeled a mismatch")
			}
			if !tt.HasText("are not totals for the selected range") {
				t.Error("the mismatch note must say these are not the selected range's totals")
			}
			if tt.HasText("(as answered)") {
				t.Error("a mismatched range must not be presented as the answered selected range")
			}
		})
	}
}

// When the selected range and the answered range agree there is no mismatch
// to claim: the card names the range as answered.
func TestGatewayUsageMatchingRangeIsNotLabeledMismatch(t *testing.T) {
	st := store{
		conn:         connConnected,
		gwUsageFetch: gwFresh,
		gwUsageAsked: "24h",
		gwUsage: api.GatewayUsageResponse{
			Window:  "24h",
			Totals:  api.GatewayUsageBucket{Requests: 9, TotalTokens: 500},
			Buckets: []api.GatewayUsageBucket{{Plan: "glm-coding", Model: "glm-4.7", Requests: 9}},
		},
	}
	tt := gwFrameApp(t, newAppForTest(st, 1))
	if tt.HasText("Range mismatch") {
		t.Error("a matching range must not be labeled a mismatch")
	}
	if !tt.HasText("24h (as answered)") {
		t.Error("a matching range should be named as the answered range")
	}
}

// A failed refresh after the range changed keeps the old range's numbers and
// must say both: the answer is stale, and it is not the selected range.
func TestGatewayUsageStaleAfterRangeSwitchSaysBoth(t *testing.T) {
	st := store{conn: connConnected}
	st.applyGateway(gwAnswers{
		usageOK:      true,
		usageRequest: gwUsageRequest{window: "24h"},
		usage: api.GatewayUsageResponse{
			Window:  "24h",
			Totals:  api.GatewayUsageBucket{Requests: 5},
			Buckets: []api.GatewayUsageBucket{{Plan: "p", Model: "m", Requests: 5}},
		},
	})
	// The range moves to 1h and the next usage fetch fails: the 24h answer
	// is kept, now both stale and for a range that is not selected.
	st.applyGateway(gwAnswers{usageRequest: gwUsageRequest{window: "1h"}})
	if st.gwUsageAsked != "24h" {
		t.Error("a failed fetch must keep the range the kept answer was fetched for")
	}
	tt := gwFrameApp(t, newAppForTest(st, 0))
	if !tt.HasText("the latest usage request failed") {
		t.Error("the kept usage answer should be marked stale")
	}
	if !tt.HasText("Range mismatch") {
		t.Error("a kept answer for another range should also be labeled a range mismatch")
	}
}

// A disabled gateway answers the routes and usage endpoints with empty
// lists whatever the config and ledger hold: the cards must not read those
// placeholders as an empty config or as no usage.
func TestGatewayOffEmptyRoutesAndUsageDoNotProveNone(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: false},
		gwRoutesFetch: gwFresh,
		gwRoutes:      []api.GatewayRouteStatus{},
		gwUsageFetch:  gwFresh,
		gwUsageAsked:  "24h",
		gwUsage:       api.GatewayUsageResponse{Window: "24h", Buckets: []api.GatewayUsageBucket{}},
	}
	tt := gwFrameApp(t, newAppForTest(st, 1))
	if tt.HasText("No routes configured") {
		t.Error("an off gateway's empty routes answer must not be read as no routes configured")
	}
	if !tt.HasText("No routes reported: the gateway is off") {
		t.Error("the routes card should explain the empty answer by the gateway being off")
	}
	if !tt.HasText("No usage reported: the gateway is off") {
		t.Error("the usage card should explain the empty answer by the gateway being off")
	}
	if !tt.HasText("Whether any usage is recorded is not known from here") {
		t.Error("an off gateway's empty usage answer must not be read as no usage recorded")
	}
	if !tt.HasText("not known from here") {
		t.Error("an off gateway's empty answer leaves whether anything is configured unknown")
	}
}

// An off gateway's usage answer echoes the requested range only when it
// names one; when it does not, the card says the range is unnamed rather
// than invent one, and still refuses to infer an absence of usage.
func TestGatewayOffUsageWithUnnamedWindow(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: false},
		gwUsageFetch:  gwFresh,
		gwUsageAsked:  "24h",
		// An off answer that did not report which range it covers.
		gwUsage: api.GatewayUsageResponse{Window: "", Buckets: []api.GatewayUsageBucket{}},
	}
	tt := gwFrameApp(t, newAppForTest(st, 1))
	if !tt.HasText("No usage reported: the gateway is off") {
		t.Error("an off gateway's empty usage answer should be explained by the gateway being off")
	}
	if !tt.HasText("did not name the range") {
		t.Error("an off usage answer without a window should say the range is unnamed")
	}
}

// With the gateway on, an empty routes answer is still only an answer: the
// card names where routes live without asserting that directory is empty.
func TestGatewayEmptyRoutesOnEnabledGatewayDoesNotClaimEmptyConfig(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: true, ListenAddr: "127.0.0.1:8081"},
		gwRoutesFetch: gwFresh,
		gwRoutes:      []api.GatewayRouteStatus{},
	}
	tt := gwFrame(t, st)
	if tt.HasText("No routes configured") {
		t.Error("an empty routes answer must not be read as no routes configured")
	}
	if !tt.HasText("No routes reported in this answer") || !tt.HasText("does not prove none are configured") {
		t.Error("the routes card should report the empty answer and decline to assert an empty config")
	}
	if !tt.HasText("~/.crabbro/gateway/routes/") {
		t.Error("the routes card should still name where routes live")
	}
}

// An enabled gateway with an empty usage answer in the answered range may
// say only that: not that no usage was recorded anywhere.
func TestGatewayEmptyUsageOnEnabledGatewayScopesItsClaim(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: true, ListenAddr: "127.0.0.1:8081"},
		gwUsageFetch:  gwFresh,
		gwUsageAsked:  "24h",
		gwUsage:       api.GatewayUsageResponse{Window: "24h", Buckets: []api.GatewayUsageBucket{}},
	}
	tt := gwFrameApp(t, newAppForTest(st, 1))
	if !tt.HasText("No gateway usage buckets in the answered 24h range") {
		t.Error("an empty usage answer should be scoped to the answered range")
	}
	if !tt.HasText("does not prove no usage was recorded elsewhere") {
		t.Error("an empty usage answer must not be read as no usage recorded at all")
	}
}

// A kept empty plans answer that predates a fresh off status is not a fresh
// observation of the off gateway: the card must say the list is stale and was
// not freshly observed while the gateway is off, never blame it on the off
// state.
func TestGatewayStaleEmptyPlansBesideFreshOffStatus(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: false},
		gwPlansFetch:  gwStale,
		gwPlans:       []api.GatewayPlanStatus{},
	}
	tt := gwFrame(t, st)
	if !tt.HasText("the latest plans request failed") {
		t.Error("the kept plans answer should be marked stale")
	}
	if !tt.HasText("that empty list is stale and was not freshly observed while the gateway is off") {
		t.Error("a stale empty plans list beside a fresh off status must say it was not freshly observed while off")
	}
	if tt.HasText("Plans unavailable: the gateway is off") {
		t.Error("a stale kept empty list must not be explained by the current off state")
	}
}

// A kept empty routes answer beside a fresh off status was not freshly
// observed while the gateway is off and must say so instead of borrowing the
// off explanation.
func TestGatewayStaleEmptyRoutesBesideFreshOffStatus(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: false},
		gwRoutesFetch: gwStale,
		gwRoutes:      []api.GatewayRouteStatus{},
	}
	tt := gwFrame(t, st)
	if !tt.HasText("the latest routes request failed") {
		t.Error("the kept routes answer should be marked stale")
	}
	if !tt.HasText("that empty list is stale and routes were not freshly observed while the gateway is off") {
		t.Error("a stale empty routes list beside a fresh off status must say it was not freshly observed while off")
	}
	if tt.HasText("No routes reported: the gateway is off") {
		t.Error("a stale kept empty list must not be explained by the current off state")
	}
}

// A kept empty usage answer beside a fresh off status is a stale kept answer,
// not a fresh observation of the off gateway: zero buckets still prove no
// absence of recorded usage.
func TestGatewayStaleEmptyUsageBesideFreshOffStatus(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: false},
		gwUsageFetch:  gwStale,
		gwUsageAsked:  "24h",
		gwUsage:       api.GatewayUsageResponse{Window: "24h", Buckets: []api.GatewayUsageBucket{}},
	}
	tt := gwFrameApp(t, newAppForTest(st, 1))
	if !tt.HasText("the latest usage request failed") {
		t.Error("the kept usage answer should be marked stale")
	}
	if !tt.HasText("that empty list is a stale kept answer and was not freshly observed while the gateway is off") {
		t.Error("a stale empty usage answer beside a fresh off status must say it was not freshly observed while off")
	}
	if tt.HasText("No usage reported: the gateway is off") {
		t.Error("a stale kept empty list must not be explained by the current off state")
	}
}

// The selected range was requested, but the answer did not name the window it
// covers: agreement cannot be claimed, so the card says the range is
// unconfirmed rather than labeling a mismatch it cannot prove.
func TestGatewayUsageUnnamedWindowIsUnconfirmedNotMismatch(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: true, ListenAddr: "127.0.0.1:8081"},
		gwUsageFetch:  gwFresh,
		gwUsageAsked:  "24h",
		gwUsage: api.GatewayUsageResponse{
			Window:  "",
			Totals:  api.GatewayUsageBucket{Requests: 9, TotalTokens: 500},
			Buckets: []api.GatewayUsageBucket{{Plan: "glm-coding", Model: "glm-4.7", Requests: 9}},
		},
	}
	tt := gwFrameApp(t, newAppForTest(st, 1))
	if !tt.HasText("Range unconfirmed: 24h was selected and requested, but the response did not name the window it covers.") {
		t.Error("an unnamed answered window should be labeled unconfirmed")
	}
	if tt.HasText("Range mismatch") {
		t.Error("an unnamed window with the selected range asked for must not be called a mismatch")
	}
	if tt.HasText("(as answered)") {
		t.Error("an unnamed window must not be presented as the answered selected range")
	}
}

// An unconfirmed empty answer may not be read as no usage anywhere: the card
// says the window was unnamed and that zero buckets prove no absence.
func TestGatewayUsageUnconfirmedEmptyBucketsDoNotProveNone(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: true, ListenAddr: "127.0.0.1:8081"},
		gwUsageFetch:  gwFresh,
		gwUsageAsked:  "24h",
		gwUsage:       api.GatewayUsageResponse{Window: "", Buckets: []api.GatewayUsageBucket{}},
	}
	tt := gwFrameApp(t, newAppForTest(st, 1))
	if !tt.HasText("Range unconfirmed: 24h was selected and requested") {
		t.Error("an unnamed answered window should be labeled unconfirmed")
	}
	if !tt.HasText("No usage buckets in this answer; the window it covers was not named in the response, and zero buckets does not prove none are recorded elsewhere.") {
		t.Error("an unconfirmed empty answer must scope its claim to the unnamed window")
	}
	if tt.HasText("Range mismatch") {
		t.Error("an unnamed window with the selected range asked for must not be called a mismatch")
	}
}

// The selected range was never requested and the answer did not name its own
// window: neither side confirms the range, so the card states both facts
// rather than claiming the answer covers another named range.
func TestGatewayUsageUnnamedWindowFromAnotherRequestIsMismatch(t *testing.T) {
	st := store{
		conn:          connConnected,
		gwStatusFetch: gwFresh,
		gwStatus:      api.GatewayStatusResponse{Enabled: true, ListenAddr: "127.0.0.1:8081"},
		gwUsageFetch:  gwFresh,
		gwUsageAsked:  "7d",
		gwUsage: api.GatewayUsageResponse{
			Window:  "",
			Totals:  api.GatewayUsageBucket{Requests: 4, TotalTokens: 200},
			Buckets: []api.GatewayUsageBucket{{Plan: "glm-coding", Model: "glm-4.7", Requests: 4}},
		},
	}
	tt := gwFrameApp(t, newAppForTest(st, 1))
	if !tt.HasText("Range mismatch") {
		t.Error("a selected range that was never requested must be labeled a mismatch")
	}
	if !tt.HasText("came from a 7d request and did not name the window it covers") {
		t.Error("the mismatch note should name the requested range and the unnamed window")
	}
	if !tt.HasText("it is not confirmed as totals for the selected range") {
		t.Error("the mismatch note must say the numbers are not confirmed for the selected range")
	}
	if tt.HasText("Range unconfirmed") {
		t.Error("a selected range that was never requested must not be called merely unconfirmed")
	}
	if tt.HasText("these numbers are the last") {
		t.Error("an unnamed window must not be called the last answer for a named range")
	}
}
