package rule

import (
	"context"
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
type RuleEngine struct {
	// Runtime calls plugins. Nil means a plugin uses stage fails the rule.
	Runtime Runtime
}

// Result is the validated outcome of a single rule evaluation.
type Result struct {
	Matched     bool                `json:"matched"`
	Actions     []shared.RuleAction `json:"actions,omitempty"`
	DurationMs  int64               `json:"durationMs"`
	Error       string              `json:"error,omitempty"`
	SchemaValid bool                `json:"schemaValid"`
	SchemaErrs  []string            `json:"schemaErrors,omitempty"`
	// Format is "yaml" or "js". The proxy runs a YAML response pipeline on a
	// synthetic response; a JavaScript rule is evaluated again instead.
	Format string              `json:"format,omitempty"`
	Stages []shared.StageTrace `json:"stages,omitempty"`
	Vars   map[string]any      `json:"vars,omitempty"`
}

// Runtime is the plugin host a YAML pipeline calls for uses, tools, and secrets.
type Runtime interface {
	Invoke(ctx context.Context, req InvokeRequest) (InvokeResult, error)
	Secret(plugin, instance, name string) (string, error)
	HasInstance(plugin, instance string) bool
}

// InvokeRequest is one plugin call. Kind is "uses", "tool", "hook", or "middleware".
type InvokeRequest struct {
	Plugin     string
	Instance   string
	Capability string
	Kind       string
	With       map[string]any
	Env        map[string]any
	Payload    map[string]any
	Timeout    time.Duration
	ExchangeID string
}

// InvokeResult is the validated plugin reply.
type InvokeResult struct {
	Outputs map[string]any
	Actions []shared.RuleAction
	Effect  string
	Match   bool
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
// Phase is an optional response or WebSocket frame passed to a later evaluation.
type Phase struct {
	HasResponse           bool
	ResponseStatus        int
	ResponseHeaders       map[string]string
	ResponseBody          string
	ResponseBodyTruncated bool
	Frame                 *shared.FrameView
	// Extra is copied onto the Goja context for a run stage (vars, pipeline, env).
	Extra map[string]any
}

// EvaluatePhase is Evaluate with a response view or a WebSocket frame.
func (e *RuleEngine) EvaluatePhase(script string, req *shared.Request, body string, phase *Phase, timeout time.Duration) Result {
	return e.dispatch(script, req, body, phase, timeout)
}

func (e *RuleEngine) Evaluate(script string, req *shared.Request, body string, timeout time.Duration) Result {
	return e.dispatch(script, req, body, nil, timeout)
}

func (e *RuleEngine) dispatch(script string, req *shared.Request, body string, phase *Phase, timeout time.Duration) Result {
	if IsYAMLRule(script) {
		doc, err := ParseYAML(script)
		if err != nil {
			return evalError(time.Now(), err.Error())
		}
		if diags := schemaDiagnostics(script); len(diags) > 0 {
			return Result{
				Format: "yaml", DurationMs: 0, SchemaValid: false,
				Error: diags[0].Error(), SchemaErrs: diagStrings(diags),
			}
		}
		return e.evalDocument(doc, req, body, phase, timeout)
	}
	res := e.evaluate(script, req, body, phase, timeout)
	res.Format = "js"
	return res
}

func (e *RuleEngine) evaluate(script string, req *shared.Request, body string, phase *Phase, timeout time.Duration) Result {
	start := time.Now()
	out, outputs, err := e.callScript(script, req, body, phase, timeout)
	if err != nil {
		return evalError(start, err.Error())
	}
	_ = outputs
	actions, errs := ValidateOutput(out)
	return Result{
		Matched:     out.Match,
		Actions:     actions,
		DurationMs:  time.Since(start).Milliseconds(),
		SchemaValid: len(errs) == 0,
		SchemaErrs:  errs,
		Error:       errString(errs),
	}
}

// callScript runs a Goja script and returns the raw result. The script text
// is not an expression expansion site.
func (e *RuleEngine) callScript(script string, req *shared.Request, body string, phase *Phase, timeout time.Duration) (ruleOutput, map[string]any, error) {
	vm := goja.New()
	setupSandbox(vm)
	ctx := buildContext(req, body, phase)
	if err := vm.Set("ctx", ctx); err != nil {
		return ruleOutput{}, nil, fmt.Errorf("failed to set context: %v", err)
	}
	processedScript := preprocessScript(script)
	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			vm.Interrupt("rule execution timeout")
		})
		defer timer.Stop()
	}
	if _, err := vm.RunString(processedScript); err != nil {
		return ruleOutput{}, nil, fmt.Errorf("script error: %v", err)
	}
	ruleFn := getRuleFunction(vm)
	if ruleFn == nil {
		return ruleOutput{}, nil, fmt.Errorf("rule entry point not found: define __rule__ or rule function")
	}
	resultVal, err := ruleFn(goja.Undefined(), vm.ToValue(ctx))
	if err != nil {
		return ruleOutput{}, nil, fmt.Errorf("rule execution error: %v", err)
	}
	exported := resultVal.Export()
	resultJSON, err := json.Marshal(exported)
	if err != nil {
		return ruleOutput{}, nil, fmt.Errorf("result serialization error: %v", err)
	}
	if len(resultJSON) > MaxSerializedResultLen {
		return ruleOutput{}, nil, fmt.Errorf("rule result exceeds max size (%d bytes)", MaxSerializedResultLen)
	}
	var out ruleOutput
	if err := json.Unmarshal(resultJSON, &out); err != nil {
		return ruleOutput{}, nil, fmt.Errorf("result format error: %v", err)
	}
	for i := range out.Actions {
		out.Actions[i].liftFlatParams()
	}
	var outputs map[string]any
	if m, ok := exported.(map[string]any); ok {
		if o, ok := m["outputs"].(map[string]any); ok {
			outputs = o
		}
	}
	return out, outputs, nil
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
func buildContext(req *shared.Request, body string, phase *Phase) map[string]any {
	headers := make(map[string]any)
	if req != nil {
		for k, v := range req.RequestHeaders {
			headers[k] = v
		}
	}
	ctx := map[string]any{
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
	if phase != nil && phase.HasResponse {
		rh := make(map[string]any, len(phase.ResponseHeaders))
		for k, v := range phase.ResponseHeaders {
			rh[k] = v
		}
		ctx["response"] = map[string]any{
			"status":        phase.ResponseStatus,
			"headers":       rh,
			"body":          phase.ResponseBody,
			"bodyTruncated": phase.ResponseBodyTruncated,
		}
	}
	if phase != nil && phase.Frame != nil {
		ctx["frame"] = map[string]any{
			"direction": phase.Frame.Direction,
			"opcode":    phase.Frame.Opcode,
			"payload":   phase.Frame.Payload,
			"fin":       phase.Frame.Fin,
		}
	}
	if phase != nil {
		for k, v := range phase.Extra {
			ctx[k] = v
		}
	}
	return ctx
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
