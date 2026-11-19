package main

import (
	"strings"
	"testing"

	"github.com/lidessen/crabbro/pkg/api"
)

const sampleRule = `# keep me
version: 1
id: mock-login
description: Mock the login call
priority: 100
on:
  request:
    method: POST
    host: auth.example.com
    path: /login
request:
  pipeline:
    - id: respond
      uses: crabbro.mock-response
      with:
        status: 200
        body: '{"on":true}'
`

func TestParseRuleDoc(t *testing.T) {
	doc, err := parseRuleDoc(sampleRule)
	if err != nil {
		t.Fatal(err)
	}
	if m := doc.On.Request; m == nil || m.Method != "POST" || m.Host != "auth.example.com" || m.Path != "/login" {
		t.Errorf("match = %+v", doc.On.Request)
	}
	ph := doc.phaseOf("request")
	if ph == nil || len(ph.Pipeline) != 1 || ph.Pipeline[0].stageUses() != "crabbro.mock-response" {
		t.Fatalf("pipeline = %+v", ph)
	}
	if got := paramString(ph.Pipeline[0].With["body"]); got != `{"on":true}` {
		t.Errorf("body = %q", got)
	}
}

// A field the model does not know is a parse error, so the overview never
// shows a partial reading of a rule: the Code view takes it instead.
func TestParseRuleDocRejectsUnknownFields(t *testing.T) {
	if _, err := parseRuleDoc("version: 1\nid: r\nbogus: true\n"); err == nil {
		t.Error("unknown field parsed")
	}
}

func TestFoldTraceKeepsOneStepPerFinishedRequest(t *testing.T) {
	in := []api.Activity{
		{ID: "act_5", Kind: "request_captured", RequestID: "req_2"}, // still in flight
		{ID: "act_4", Kind: "request_completed", RequestID: "req_1"},
		{ID: "act_3", Kind: "rule_applied", RequestID: "req_1", RuleID: "rule_1"},
		{ID: "act_2", Kind: "request_captured", RequestID: "req_1"},
		{ID: "act_1", Kind: "session_started"},
	}
	got := foldTrace(in)
	var ids []string
	for _, it := range got {
		ids = append(ids, it.ID)
	}
	if want := "act_5 act_4 act_3 act_1"; strings.Join(ids, " ") != want {
		t.Fatalf("foldTrace kept %v, want %s", ids, want)
	}
}

func TestLatencyLabel(t *testing.T) {
	for _, tc := range []struct {
		r    api.RequestSummary
		want string
	}{
		{api.RequestSummary{}, "—"},               // in flight
		{api.RequestSummary{Status: 200}, "<1ms"}, // mocked: done in under a millisecond
		{api.RequestSummary{Status: 200, DurationMs: 61}, "61ms"},
		{api.RequestSummary{Status: 200, DurationMs: 1500}, "1.5s"},
	} {
		if got := latencyLabel(tc.r); got != tc.want {
			t.Errorf("latencyLabel(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

func TestRetentionLabel(t *testing.T) {
	for in, want := range map[string]string{"168h0m0s": "7 days", "24h0m0s": "1 day", "90m0s": "90m0s", "": ""} {
		if got := retentionLabel(in); got != want {
			t.Errorf("retentionLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
