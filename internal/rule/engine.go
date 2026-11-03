package rule

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/lidessen/chiling/internal/shared"
)

// RuleEngine executes JavaScript rules in isolated Goja VMs.
//
// The engine creates a fresh VM per evaluation, sandboxes it, injects the
// request context as a global `ctx` object, runs the script, and validates
// the output. No runtime state is shared across evaluations.
//
// The JS context object shape is:
//
//	ctx = { request: { method, scheme, host, path, query, url,
//	    headers: { "Header-Name": "value" }, body, clientAddr } }
type RuleEngine struct{}

// Result is the validated outcome of a single rule evaluation.
type Result struct {
	Matched     bool                `json:"matched"`
	Actions     []shared.RuleAction `json:"actions,omitempty"`
	DurationMs  int64               `json:"durationMs"`
	Error       string              `json:"error,omitempty"`
	SchemaValid bool                `json:"schemaValid"`
	SchemaErrs  []string            `json:"schemaErrors,omitempty"`
}

// NewRuleEngine creates a RuleEngine.
func NewRuleEngine() *RuleEngine {
	return &RuleEngine{}
}

// Evaluate executes a rule script against the given request snapshot and
// returns the validated result.
//
// The script should export a default function that receives a context object
// with the request details. The function must return { match: bool, actions?: [] }.
//
// script   — the JavaScript source code.
// req      — the captured HTTP request metadata.
// body     — the request body content (empty string if unavailable).
// timeout  — per-rule execution deadline; 0 means no timeout.
func (e *RuleEngine) Evaluate(script string, req *shared.Request, body string, timeout time.Duration) Result {
	start := time.Now()

	vm := goja.New()
	setupSandbox(vm)

	// Build the immutable JS context and inject as global.
	ctx := buildContext(req, body)
	if err := vm.Set("ctx", ctx); err != nil {
		return evalError(start, fmt.Sprintf("failed to set context: %v", err))
	}

	// Preprocess script to handle ES module export syntax.
	processedScript := preprocessScript(script)

	// Schedule interrupt-based timeout.
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			vm.Interrupt("rule execution timeout")
		})
	}

	// Run the script.
	_, err := vm.RunString(processedScript)
	if err != nil {
		if timer != nil {
			timer.Stop()
		}
		return evalError(start, fmt.Sprintf("script error: %v", err))
	}

	// Locate the rule function in the global scope.
	ruleFn := getRuleFunction(vm)
	if ruleFn == nil {
		if timer != nil {
			timer.Stop()
		}
		return evalError(start, "rule entry point not found: define __rule__ or rule function")
	}

	// Call the rule function with the context.
	ctxVal := vm.ToValue(ctx)
	resultVal, err := ruleFn(goja.Undefined(), ctxVal)
	if timer != nil {
		timer.Stop()
	}
	if err != nil {
		return evalError(start, fmt.Sprintf("rule execution error: %v", err))
	}

	// Serialize result to JSON for size check.
	resultJSON, err := json.Marshal(resultVal.Export())
	if err != nil {
		return evalError(start, fmt.Sprintf("result serialization error: %v", err))
	}
	if len(resultJSON) > MaxSerializedResultLen {
		return evalError(start, fmt.Sprintf("rule result exceeds max size (%d bytes)", MaxSerializedResultLen))
	}

	// Deserialize into ruleOutput for validation.
	var out ruleOutput
	if err := json.Unmarshal(resultJSON, &out); err != nil {
		return evalError(start, fmt.Sprintf("result format error: %v", err))
	}
	for i := range out.Actions {
		out.Actions[i].liftFlatParams()
	}

	// Validate the output against action schemas.
	actions, errs := ValidateOutput(out)
	durationMs := time.Since(start).Milliseconds()

	return Result{
		Matched:     out.Match,
		Actions:     actions,
		DurationMs:  durationMs,
		SchemaValid: len(errs) == 0,
		SchemaErrs:  errs,
		Error:       errString(errs),
	}
}

// preprocessScript converts ES module export syntax to a form Goja can
// evaluate. Goja does not support ES module syntax natively, so we
// transform export default declarations into variable assignments.
//
//	export default function(ctx) { ... }     → var __rule__ = function(ctx) { ... }
//	export default function name(ctx) {...}  → var __rule__ = function name(ctx) {...}
//	export default (ctx) => { ... }          → var __rule__ = (ctx) => { ... }
//	export default <expr>                    → var __rule__ = <expr>
//
// Scripts without export default are used as-is; the engine looks for a
// top-level function named __rule__ or rule.
func preprocessScript(script string) string {
	s := strings.TrimSpace(script)
	if !strings.HasPrefix(s, "export default") {
		return s
	}
	body := strings.TrimPrefix(s, "export default ")
	return "var __rule__ = " + body
}

// getRuleFunction looks up the rule entry point in the Goja runtime.
// It tries "__rule__" first, then "rule" as a fallback.
func getRuleFunction(vm *goja.Runtime) goja.Callable {
	for _, name := range []string{"__rule__", "rule"} {
		if v := vm.Get(name); v != nil && !goja.IsUndefined(v) && v != goja.Null() {
			if fn, ok := goja.AssertFunction(v); ok {
				return fn
			}
		}
	}
	return nil
}

// setupSandbox configures a Goja runtime with security constraints.
//
// Goja's default runtime is already isolated from the filesystem, network,
// and process environment. This function adds console.log as a safe no-op.
func setupSandbox(vm *goja.Runtime) {
	_ = vm.Set("console", map[string]any{
		"log": func(goja.FunctionCall) goja.Value {
			return goja.Undefined()
		},
	})
}

// buildContext constructs the JS context object from a Request and optional body.
// Returns a map so Goja creates the exact property structure we need.
func buildContext(req *shared.Request, body string) map[string]any {
	headers := make(map[string]any)
	for k, v := range req.RequestHeaders {
		headers[k] = v
	}
	return map[string]any{
		"request": map[string]any{
			"method":     req.Method,
			"scheme":     req.Scheme,
			"host":       req.Host,
			"path":       req.Path,
			"query":      req.Query,
			"url":        req.URL,
			"headers":    headers,
			"body":       body,
			"clientAddr": req.ClientAddr,
		},
	}
}

// evalError constructs a Result for a failed evaluation.
func evalError(start time.Time, errMsg string) Result {
	return Result{
		Matched:     false,
		DurationMs:  time.Since(start).Milliseconds(),
		Error:       errMsg,
		SchemaValid: false,
	}
}

// errString joins schema validation errors into a single error string.
func errString(errs []string) string {
	if len(errs) == 0 {
		return ""
	}
	return strings.Join(errs, "; ")
}
