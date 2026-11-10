package main

import "testing"

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
      uses: chiling.mock-response
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
	if ph == nil || len(ph.Pipeline) != 1 || ph.Pipeline[0].stageUses() != "chiling.mock-response" {
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
