package main

import (
	"testing"

	"github.com/egoist/mygo/ui"
	"github.com/lidessen/crabbro/pkg/api"
)

func TestGatewayUsageRetries(t *testing.T) {
	buckets := []api.GatewayUsageBucket{
		{Plan: "plan", Model: "first", Requests: 3, Retries: 1, TotalTokens: 100},
		{Plan: "plan", Model: "second", Requests: 4, Retries: 2, TotalTokens: 50},
	}
	plans := gwUsageByPlan(buckets)
	if len(plans) != 1 || plans[0].totals.Retries != 3 {
		t.Fatalf("plan retry totals = %+v", plans)
	}
	st := store{
		conn: connConnected, gwUsageFetch: gwFresh, gwUsageAsked: "24h",
		gwUsage: api.GatewayUsageResponse{
			Window: "24h", Totals: api.GatewayUsageBucket{Requests: 7, Retries: 3}, Buckets: buckets,
		},
	}
	tt := gwFrameApp(t, newAppForTest(st, 1))
	for _, want := range []string{"0 errors · 3 retries", "3 retries", "1 retry", "2 retries"} {
		if !tt.HasText(want) {
			t.Errorf("usage view missing %q", want)
		}
	}
	if got := gwRetries(0); got != "0 retries" {
		t.Errorf("zero retries = %q", got)
	}
}

func TestGatewayUsageMetricFitsMinimumWindow(t *testing.T) {
	tt := ui.NewTester(func(c *ui.Context) {
		p := applyPal(c)
		ui.Column(c).Width(614).Children(func() {
			gwUsageMetrics(c, p, api.GatewayUsageBucket{TotalTokens: 1234567})
		})
	}, 860, 540)
	tt.Frame()
	if bounds, ok := tt.Find("1,234,567"); !ok || bounds.H > 30 {
		t.Fatalf("token total should fit on one line: found=%v bounds=%+v", ok, bounds)
	}
}

func TestGatewayUsageAccessValue(t *testing.T) {
	for _, tc := range []struct {
		bucket api.GatewayUsageBucket
		want   string
	}{
		{api.GatewayUsageBucket{}, "0 tokens, 0 requests, 0 errors"},
		{api.GatewayUsageBucket{TotalTokens: 1234567, Requests: 1234, Errors: 1}, "1,234,567 tokens, 1,234 requests, 1 error"},
	} {
		if got := gwUsageAccessValue(tc.bucket); got != tc.want {
			t.Errorf("gwUsageAccessValue(%+v) = %q, want %q", tc.bucket, got, tc.want)
		}
	}
}
