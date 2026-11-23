package main

import (
	"testing"

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
