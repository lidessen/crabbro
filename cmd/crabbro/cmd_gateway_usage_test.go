package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/pkg/api"
)

func TestGatewayUsagePrettyRetries(t *testing.T) {
	server, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/gateway/usage": func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, api.GatewayUsageResponse{
				Window: "24h", Totals: api.GatewayUsageBucket{Requests: 3, Errors: 1, Retries: 2, AvgTokensPerSec: 42.5, AvgDurationMs: 300},
				Buckets: []api.GatewayUsageBucket{
					{Plan: "a", Model: "one", Requests: 2, Retries: 2, AvgTokensPerSec: 42.5},
					{Plan: "b", Model: "two", Requests: 1},
				},
			})
		},
	})
	defer server.Close()
	out := captureStdout(func() {
		if err := cmdGateway(cfg, []string{"usage", "--pretty"}); err != nil {
			t.Errorf("gateway usage: %v", err)
		}
	})
	for _, want := range []string{"total_requests=3 errors=1 retries=2", "requests=2 errors=0 retries=2", "requests=1 errors=0 retries=0", "avg_duration=300ms avg_tok_per_sec=42.5", "tok_per_sec=—", "tok_per_sec=42.5"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}
