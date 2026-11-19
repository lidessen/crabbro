package rule

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/shared"
)

func TestYAMLSelectParallelAndRun(t *testing.T) {
	script := `
version: 1
id: decode-login
on:
  request:
    method: POST
    path: /v1/login
vars:
  note: "login"
request:
  pipeline:
    - id: decoded
      run:
        runtime: goja
        script: |
          export default function(ctx) {
            return { match: true, outputs: { kind: "password", ok: true } };
          }
    - id: route
      select:
        cases:
          - name: password
            if: pipeline.decoded.outputs.kind == "password"
            pipeline:
              - uses: crabbro.tag
                with:
                  tags: ["password-login"]
          - name: sso
            if: pipeline.decoded.outputs.kind == "sso"
            pipeline:
              - uses: crabbro.set-request-header
                with:
                  name: X-Login-Kind
                  value: sso
        default:
          pipeline:
            - uses: crabbro.tag
              with:
                tags: ["other-login"]
    - parallel:
        timeoutMs: 200
        stages:
          - id: logged
            uses: crabbro.log
            with:
              message: decoded-login
          - id: counted
            uses: crabbro.metric
            with:
              name: login_decoded
`
	eng := NewRuleEngine()
	res := eng.Evaluate(script, &shared.Request{Method: "POST", Path: "/v1/login"}, "", shared.DefaultRuleTimeout)
	if res.Error != "" || !res.Matched {
		t.Fatalf("result error=%q matched=%v stages=%+v", res.Error, res.Matched, res.Stages)
	}
	if len(res.Actions) != 1 || res.Actions[0].Type != shared.ActionTag {
		t.Fatalf("actions = %+v", res.Actions)
	}
	sawLog := false
	for _, st := range res.Stages {
		if st.Uses == "crabbro.log" && !st.Skipped {
			sawLog = true
		}
		if st.ID == "logged" && st.Outputs != nil {
			if _, ok := st.Outputs["message"]; ok {
				sawLog = true
			}
		}
	}
	if !sawLog {
		t.Fatalf("parallel log missing: %+v", res.Stages)
	}
	// A later stage must not be able to read a parallel output. Validation rejects it.
	bad := strings.Replace(script, "name: login_decoded", "name: \"${{ pipeline.logged.outputs.message }}\"", 1)
	badRes := eng.Evaluate(bad, &shared.Request{Method: "POST", Path: "/v1/login"}, "", shared.DefaultRuleTimeout)
	if badRes.Error == "" {
		t.Fatal("expected parallel output reference to fail validation")
	}
}

func TestYAMLMissAndConditionError(t *testing.T) {
	script := `
version: 1
id: only-post
on:
  request:
    method: POST
    path: /v1/login
request:
  pipeline:
    - uses: crabbro.tag
      with:
        tags: ["x"]
`
	eng := NewRuleEngine()
	res := eng.Evaluate(script, &shared.Request{Method: "GET", Path: "/v1/login"}, "", 0)
	if res.Matched || res.Error != "" {
		t.Fatalf("miss = %+v", res)
	}
}

func TestPerFileIsolation(t *testing.T) {
	dir := t.TempDir()
	good := []byte("version: 1\nid: good-rule\non:\n  request:\n    path: /ok\nrequest:\n  pipeline:\n    - uses: crabbro.tag\n      with:\n        tags: [a]\n")
	bad := []byte("version: 1\nid: wrong-name\non:\n  request: {}\nrequest:\n  pipeline:\n    - uses: crabbro.tag\n      with:\n        tags: [a]\n")
	if err := os.WriteFile(filepath.Join(dir, "good-rule.yaml"), good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad-rule.yaml"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good-rule.yaml.draft"), good, 0o600); err != nil {
		t.Fatal(err)
	}
	d := OpenDir(dir)
	rules, diags, complete := d.Load()
	if complete {
		t.Fatal("expected incomplete because one file is invalid")
	}
	if len(rules) != 1 || rules[0].ID != "good-rule" {
		t.Fatalf("rules = %+v diags=%+v", rules, diags)
	}
	prev := d.Preview("GET", "example.test", "/ok")
	if len(prev.Matched) != 1 || prev.Matched[0] != "good-rule" {
		t.Fatalf("preview = %+v", prev)
	}
	if err := d.Commit("good-rule"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "good-rule.yaml.draft")); !os.IsNotExist(err) {
		t.Fatalf("draft still present: %v", err)
	}
}

func TestMigrateRoundTrip(t *testing.T) {
	js := "export default function(ctx) { return { match: true, actions: [{ type: 'tag', params: { tags: ['migrated'] } }] }; }\n"
	yaml, err := Migrate("migrated", js)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(yaml, "${{") {
		t.Fatal("migration expanded an expression into the script")
	}
	res := NewRuleEngine().Evaluate(yaml, &shared.Request{Method: "GET", Path: "/"}, "", shared.DefaultRuleTimeout)
	if res.Error != "" || !res.Matched || len(res.Actions) != 1 {
		t.Fatalf("migrated = %+v script=%s", res, yaml)
	}
}

func TestScaffoldIsYAML(t *testing.T) {
	out := Scaffold(&shared.Request{Method: "POST", Host: "api.example.com:443", Path: "/v1/login"}, "mock", `{"ok":true}`, "application/json")
	// The scaffold must emit a YAML rule v1 document; IsYAMLRule is gone now
	// that the legacy JavaScript format is removed.
	if !strings.HasPrefix(strings.TrimSpace(out), "version:") || !strings.Contains(out, "crabbro.mock-response") || !strings.Contains(out, "/v1/login") {
		t.Fatalf("scaffold = %s", out)
	}
	res := NewRuleEngine().Evaluate(out, &shared.Request{Method: "POST", Host: "api.example.com", Path: "/v1/login"}, "", 0)
	if !res.Matched || res.Error != "" {
		t.Fatalf("scaffold eval = %+v", res)
	}
}
