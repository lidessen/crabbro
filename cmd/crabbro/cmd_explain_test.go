package main

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/pkg/api"
)

func TestRulesDiagnoseFormatting(t *testing.T) {
	files := make([]rule.FileStatus, 21)
	files[0] = rule.FileStatus{ID: "sample", Live: true, Enabled: true, Diagnostics: []rule.Diagnostic{{Path: "$.match", Message: "invalid\nvalue"}}}
	var out bytes.Buffer
	printRulesDiagnose(&out, rulesDiagnoseResponse{Files: files})
	for _, want := range []string{"complete=false files=21", "sample live=true", "$.match: invalid value", "1 more files (use --json)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q missing %q", out.String(), want)
		}
	}
	for _, args := range [][]string{{"diagnose", "--bogus"}, {"diagnose", "extra"}} {
		if err := cmdRules(nil, args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestRulesDiagnose(t *testing.T) {
	server, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/rules/diagnose": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			fmt.Fprint(w, `{"complete":false,"files":[{"id":"sample","live":true,"enabled":true,"diagnostics":[{"path":"$.match","message":"invalid"}]}]}`)
		},
	})
	defer server.Close()
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"diagnose"}, []string{"complete=false files=1", "sample live=true", "$.match: invalid"}},
		{[]string{"diagnose", "--json"}, []string{`"complete":false`, `"id":"sample"`}},
	} {
		out := captureStdout(func() {
			if err := cmdRules(cfg, tc.args); err != nil {
				t.Errorf("%v: %v", tc.args, err)
			}
		})
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%v: output %q missing %q", tc.args, out, want)
			}
		}
	}
	for _, args := range [][]string{{"diagnose", "--bogus"}, {"diagnose", "extra"}} {
		if err := cmdRules(cfg, args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestGatewayWhyFormatting(t *testing.T) {
	until := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	trace := &api.GatewayTrace{
		Profile: "work", RouteID: "route", ChosenPlan: "second", ChosenModel: "model-b",
		RouteDecision: &api.RouteDecision{Resolution: "scored", Candidates: []api.CandidateDecision{{Plan: "first", Model: "model-a", Eligible: true, Scores: api.CandidateScores{Quality: .8, Cache: .7, Quota: .6, Total: .5}}}},
		Attempts:      []api.GatewayAttempt{{Plan: "first", Model: "model-a", Status: 200, ErrorKind: "rate_limit", ErrorMessage: "stream quota exceeded", CooldownApplied: "fiveHour"}, {Plan: "second", Model: "model-b", Status: 200}},
	}
	var out bytes.Buffer
	printGatewayWhy(&out, trace, []api.GatewayPlanStatus{{ID: "first", CooldownUntil: &until, CooldownScope: "fiveHour", RemoteQuota: &api.RemoteQuotaJSON{ResetsRemaining: 2}}, {ID: "second", Healthy: true}}, nil)
	for _, want := range []string{"profile=work route=route chosen=second/model-b", "quality=0.800 cache=0.700 quota=0.600", "reason=stream quota exceeded cooldown_applied=fiveHour", "peek-failover: origin not recorded", "current cooldown/coupon state", "cooldown_until=2026-11-25T12:00:00Z scope=fiveHour coupons=2", "plan=second healthy=true", "coupons=unknown"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q missing %q", out.String(), want)
		}
	}
}

func TestGatewayWhyCommand(t *testing.T) {
	trace := &api.GatewayTrace{ChosenPlan: "second", ChosenModel: "model-b"}
	server, cfg := newTestServer(map[string]http.HandlerFunc{
		"/v1/requests/req_1": func(w http.ResponseWriter, r *http.Request) {
			writeStatus(w, api.RequestDetailResponse{Request: api.Request{Gateway: trace}})
		},
		"/v1/gateway/plans": func(w http.ResponseWriter, r *http.Request) { writeStatus(w, api.GatewayPlanListResponse{}) },
	})
	defer server.Close()
	got := captureStdout(func() {
		if err := cmdGateway(cfg, []string{"why", "req_1"}); err != nil {
			t.Errorf("why: %v", err)
		}
	})
	if !strings.Contains(got, "chosen=second/model-b") {
		t.Errorf("command output = %q", got)
	}
	if err := cmdGateway(cfg, []string{"why"}); err == nil {
		t.Error("missing request ID accepted")
	}
}
