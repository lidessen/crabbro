package rule

import (
	"strings"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/shared"
)

func newTestRequest() *shared.Request {
	return &shared.Request{
		Method: "POST",
		Scheme: "https",
		Host:   "api.example.com",
		Path:   "/v1/chat",
		Query:  "q=hello",
		URL:    "https://api.example.com/v1/chat?q=hello",
		RequestHeaders: shared.Headers{
			"Content-Type": "application/json",
		},
		ClientAddr: "127.0.0.1:54321",
	}
}

// yamlRun wraps a literal Goja script as the only run stage of a minimal
// YAML rule v1 document. Rule scripts are YAML documents now; the sandbox,
// context, timeout, and action-validation behavior these tests exercise is
// reached through a run stage. The script is trimmed, every line is
// right-trimmed, and each content line is re-indented under the block
// scalar (indent >= the "script:" key indent) so YAML block indentation
// detection is stable regardless of the script's own indentation.
func yamlRun(js string) string {
	const contentIndent = "          " // ten spaces; the script key sits at eight
	var b strings.Builder
	b.WriteString("version: 1\n")
	b.WriteString("id: t\n")
	b.WriteString("on:\n")
	b.WriteString("  request:\n")
	b.WriteString("    method: POST\n")
	b.WriteString("request:\n")
	b.WriteString("  pipeline:\n")
	b.WriteString("    - id: r\n")
	b.WriteString("      run:\n")
	b.WriteString("        runtime: goja\n")
	b.WriteString("        script: |\n")
	for _, line := range strings.Split(strings.TrimSpace(js), "\n") {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(contentIndent)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

func TestEvaluate_SimpleMatch(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			return { match: true, actions: [{ type: "tag", params: { tags: ["test"] } }] };
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true")
	}
	if result.Error != "" {
		t.Errorf("unexpected error: %s", result.Error)
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(result.Actions))
	}
	if result.Actions[0].Type != shared.ActionTag {
		t.Errorf("expected tag action, got %s", result.Actions[0].Type)
	}
}

func TestEvaluate_NoMatch(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			return { match: false };
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if result.Matched {
		t.Error("expected match=false")
	}
	if len(result.Actions) != 0 {
		t.Errorf("expected 0 actions, got %d", len(result.Actions))
	}
}

func TestEvaluate_ContextAvailable(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			if (ctx.request.host !== "api.example.com") return { match: false };
			if (ctx.request.method !== "POST") return { match: false };
			if (ctx.request.path !== "/v1/chat") return { match: false };
			return { match: true, actions: [{ type: "tag", params: { tags: ["verified"] } }] };
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true when context is correct")
	}
	if result.Error != "" {
		t.Errorf("unexpected error: %s", result.Error)
	}
}

func TestEvaluate_BlockAction(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			return { match: true, actions: [{ type: "block", params: { status: 403, body: "forbidden" } }] };
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true")
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(result.Actions))
	}
	if result.Actions[0].Type != shared.ActionBlock {
		t.Errorf("expected block action, got %s", result.Actions[0].Type)
	}
	if result.Actions[0].Params["status"] != 403 {
		t.Errorf("expected status=403, got %v", result.Actions[0].Params["status"])
	}
}

func TestEvaluate_MockResponseAction(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			return {
				match: true,
				actions: [{
					type: "mockResponse",
					params: { status: 200, body: JSON.stringify({ ok: true }) }
				}]
			};
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true")
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(result.Actions))
	}
	if result.Actions[0].Type != shared.ActionMockResponse {
		t.Errorf("expected mockResponse, got %s", result.Actions[0].Type)
	}
}

func TestEvaluate_SetRequestHeader(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			return {
				match: true,
				actions: [{ type: "setRequestHeader", params: { name: "X-Custom", value: "test" } }]
			};
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true")
	}
	if result.Actions[0].Params["name"] != "X-Custom" {
		t.Errorf("expected name=X-Custom, got %v", result.Actions[0].Params["name"])
	}
}

func TestEvaluate_RemoveRequestHeader(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			return {
				match: true,
				actions: [{ type: "removeRequestHeader", params: { name: "Authorization" } }]
			};
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true")
	}
	if result.Actions[0].Params["name"] != "Authorization" {
		t.Errorf("expected name=Authorization, got %v", result.Actions[0].Params["name"])
	}
}

func TestEvaluate_RewriteURL(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			return {
				match: true,
				actions: [{ type: "rewriteURL", params: { url: "https://other.example.com/new-path" } }]
			};
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true")
	}
	if result.Actions[0].Params["url"] != "https://other.example.com/new-path" {
		t.Errorf("unexpected URL, got %v", result.Actions[0].Params["url"])
	}
}

func TestEvaluate_AbortAction(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			return { match: true, actions: [{ type: "abort" }] };
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true")
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(result.Actions))
	}
	if result.Actions[0].Type != shared.ActionAbort {
		t.Errorf("expected abort action, got %s", result.Actions[0].Type)
	}
}

func TestEvaluate_ExportDefaultSyntax(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `export default function(ctx) {
		return { match: true, actions: [{ type: "tag", params: { tags: ["export-test"] } }] };
	}`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true with export default syntax")
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(result.Actions))
	}
	if result.Actions[0].Type != shared.ActionTag {
		t.Errorf("expected tag action, got %s", result.Actions[0].Type)
	}
}

func TestEvaluate_ExportDefaultArrow(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `export default (ctx) => {
		return { match: true, actions: [{ type: "tag", params: { tags: ["arrow"] } }] };
	}`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true with arrow function")
	}
	if len(result.Actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(result.Actions))
	}
}

func TestEvaluate_RequestError(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `function rule(ctx) { throw new Error("test error"); }`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if result.Matched {
		t.Error("expected match=false on error")
	}
	if result.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestEvaluate_InvalidScript(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	// Rule scripts are YAML rule v1 documents now. This text does not parse
	// as one, so dispatch rejects it before any stage runs.
	script := `this is not valid javascript @@@`
	result := e.Evaluate(script, req, "", shared.DefaultRuleTimeout)

	if result.Matched {
		t.Error("expected match=false on invalid script")
	}
	if result.Error == "" {
		t.Error("expected non-empty error message")
	}
	if !strings.Contains(result.Error, "crabbro rules migrate") {
		t.Errorf("expected the not-YAML error to mention crabbro rules migrate, got: %s", result.Error)
	}
}

func TestEvaluate_NoEntryPoint(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	// `var x = 1;` is not a YAML rule document either, so dispatch rejects it
	// the same way before any stage runs.
	script := `var x = 1;`
	result := e.Evaluate(script, req, "", shared.DefaultRuleTimeout)

	if result.Matched {
		t.Error("expected match=false when no entry point")
	}
	if result.Error == "" {
		t.Error("expected error about the script not being a YAML rule document")
	}
	if !strings.Contains(result.Error, "crabbro rules migrate") {
		t.Errorf("expected the not-YAML error to mention crabbro rules migrate, got: %s", result.Error)
	}
}

func TestEvaluate_Timeout(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `function rule(ctx) { while(true) {} }`
	result := e.Evaluate(yamlRun(script), req, "", 50*time.Millisecond)

	if result.Matched {
		t.Error("expected match=false on timeout")
	}
}

func TestEvaluate_SchemaValidationFails(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	// The run stage returns an invalid action type. ValidateOutput rejects it,
	// the stage fails, and the whole rule is fatal.
	script := `
		function rule(ctx) {
			return { match: true, actions: [{ type: "nonexistent", params: {} }] };
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if result.Matched {
		t.Error("expected match=false (invalid actions fail the rule)")
	}
	if result.Error == "" {
		t.Error("expected non-empty error for the failed run stage")
	}
	if result.SchemaValid {
		t.Error("expected schemaValid=false for unknown action type")
	}
	if len(result.SchemaErrs) == 0 {
		t.Error("expected schema errors")
	}
}

func TestEvaluate_BodyInContext(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			if (ctx.request.body === "hello") {
				return { match: true, actions: [{ type: "tag", params: { tags: ["has-body"] } }] };
			}
			return { match: false };
		}
	`
	result := e.Evaluate(yamlRun(script), req, "hello", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected match=true when body is available")
	}
}

func TestExecuteRule_ValidRequestContext(t *testing.T) {
	// Ensure that ctx.request has all expected fields.
	e := NewRuleEngine()
	req := newTestRequest()

	script := `
		function rule(ctx) {
			const r = ctx.request;
			const checks = [
				r.method === "POST",
				r.scheme === "https",
				r.host === "api.example.com",
				r.path === "/v1/chat",
				r.query === "q=hello",
				r.url === "https://api.example.com/v1/chat?q=hello",
				r.clientAddr === "127.0.0.1:54321",
				r.headers["Content-Type"] === "application/json",
			];
			return { match: checks.every(Boolean), actions: [] };
		}
	`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if !result.Matched {
		t.Error("expected all request context fields to be correct")
	}

}

func TestEvaluate_DurationMs(t *testing.T) {
	e := NewRuleEngine()
	req := newTestRequest()

	script := `function rule(ctx) { return { match: true, actions: [] }; }`
	result := e.Evaluate(yamlRun(script), req, "", shared.DefaultRuleTimeout)

	if result.DurationMs < 0 {
		t.Error("duration should be non-negative")
	}
}
