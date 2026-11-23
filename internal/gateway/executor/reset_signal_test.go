package executor

import (
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/router"
)

// TestApplyFailureRaisesAutoResetSignal verifies that quota exhaustion and
// rate-limit cooling raise the auto-reset hook while other failure classes do not.
func TestApplyFailureRaisesAutoResetSignal(t *testing.T) {
	health := router.NewHealthStore()
	rtr := router.New(nil, router.NewSessionCacheStore(8), health, nil)
	e := &Executor{Router: rtr}

	var raised []string
	e.AutoResetSignal = func(planID string) { raised = append(raised, planID) }

	cand := router.Candidate{Plan: &config.Plan{ID: "zcode"}, Target: config.Target{Model: "glm-4.7"}}
	cases := []struct {
		kind string
		want bool
	}{
		{ErrKindQuotaExhausted, true},
		{ErrKindRateLimit, true},
		{ErrKindAuthError, false},
		{ErrKindUpstreamUnavailable, false},
		{ErrKindClientBadRequest, false},
	}
	for _, tc := range cases {
		raised = nil
		e.applyFailure(cand, FailureClass{Kind: tc.kind, CooldownScope: "plan", CooldownDur: time.Minute, Summary: "classified"}, 3, "route", "session", 429, nil)
		if (len(raised) == 1) != tc.want {
			t.Fatalf("kind %s: raised = %v, want signal = %t", tc.kind, raised, tc.want)
		}
		if len(raised) == 1 && raised[0] != "zcode" {
			t.Fatalf("signal carried plan %q", raised[0])
		}
	}

	// Without a hook the failure path stays inert.
	e.AutoResetSignal = nil
	e.applyFailure(cand, FailureClass{Kind: ErrKindQuotaExhausted, CooldownScope: "plan", CooldownDur: time.Minute}, 3, "route", "session", 429, nil)
}
