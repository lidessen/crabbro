package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lidessen/crabbro/pkg/api"
)

// TestGatewayQuotasResetCommand covers `crabbro gateway quotas reset` flag parsing,
// request shaping, and sanitized rendering.
func TestGatewayQuotasResetCommand(t *testing.T) {
	var mu sync.Mutex
	var bodies []api.GatewayQuotaResetRequest
	handlers := map[string]http.HandlerFunc{
		"/v1/gateway/quotas/reset": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			var req api.GatewayQuotaResetRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("invalid body: %v", err)
			}
			mu.Lock()
			bodies = append(bodies, req)
			mu.Unlock()
			expires := time.Now().Add(90 * time.Minute).UTC()
			writeStatus(w, api.GatewayQuotaResetResponse{
				Plan:             req.Plan,
				Scope:            "fiveHour",
				DryRun:           req.DryRun,
				Redeemed:         !req.DryRun,
				CouponsAvailable: 1,
				CouponExpiresAt:  &expires,
			})
		},
	}
	server, cfg := newTestServer(handlers)
	defer server.Close()

	out := captureStdout(func() {
		if err := cmdGateway(cfg, []string{"quotas", "reset", "zcode", "--scope", "fiveHour", "--dry-run"}); err != nil {
			t.Errorf("dry-run command failed: %v", err)
		}
	})
	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	if last.Plan != "zcode" || last.Scope != "fiveHour" || !last.DryRun {
		t.Fatalf("request = %+v", last)
	}
	if !strings.Contains(out, `"scope":"fiveHour"`) || !strings.Contains(out, `"dryRun":true`) || !strings.Contains(out, `"redeemed":false`) {
		t.Fatalf("dry-run output = %q", out)
	}
	if strings.Contains(out, "recordId") {
		t.Fatalf("dry-run output leaked a coupon identifier: %q", out)
	}

	out = captureStdout(func() {
		if err := cmdGateway(cfg, []string{"quotas", "reset", "--pretty", "zcode"}); err != nil {
			t.Errorf("reset command failed: %v", err)
		}
	})
	mu.Lock()
	last = bodies[len(bodies)-1]
	mu.Unlock()
	if last.Scope != "" || last.DryRun {
		t.Fatalf("default request = %+v", last)
	}
	if !strings.Contains(out, "redeemed") || !strings.Contains(out, "coupons=1") || !strings.Contains(out, "scope=fiveHour") {
		t.Fatalf("reset output = %q", out)
	}

	if err := cmdGateway(cfg, []string{"quotas", "reset", "zcode", "--scope", "hourly"}); err == nil {
		t.Fatal("invalid scope accepted")
	}
	if err := cmdGateway(cfg, []string{"quotas", "reset"}); err == nil {
		t.Fatal("missing plan accepted")
	}
	if err := cmdGateway(cfg, []string{"quotas", "bogus"}); err == nil {
		t.Fatal("unknown quotas subcommand accepted")
	}
	if err := cmdGateway(cfg, []string{"quotas"}); err == nil {
		t.Fatal("missing quotas subcommand accepted")
	}
}
