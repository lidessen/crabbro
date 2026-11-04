package rule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

var builtinPhase = map[string]string{
	"chiling.tag":                    "request",
	"chiling.set-request-header":     "request",
	"chiling.remove-request-header":  "request",
	"chiling.rewrite-url":            "request",
	"chiling.mock-response":          "request",
	"chiling.block":                  "request",
	"chiling.abort":                  "request",
	"chiling.set-response-header":    "response",
	"chiling.remove-response-header": "response",
	"chiling.set-response-status":    "response",
	"chiling.rewrite-response-body":  "response",
	"chiling.delay":                  "response",
	"chiling.throttle":               "response",
	"chiling.replace-frame":          "frame",
	"chiling.drop-frame":             "frame",
	"chiling.read-body":              "any",
	"chiling.log":                    "any",
	"chiling.metric":                 "any",
	"chiling.notify":                 "any",
	"chiling.tee":                    "any",
}

func builtinEffect(uses string) string {
	switch uses {
	case "chiling.read-body", "chiling.log", "chiling.metric", "chiling.notify", "chiling.tee":
		return "observe"
	default:
		return "mutate"
	}
}

func (e *RuleEngine) evalDocument(doc *Document, req *shared.Request, body string, phase *Phase, timeout time.Duration) Result {
	start := time.Now()
	finish := func(r Result) Result {
		r.Format = "yaml"
		if r.DurationMs == 0 {
			r.DurationMs = time.Since(start).Milliseconds()
		}
		return r
	}
	if req == nil {
		req = &shared.Request{}
	}
	if sem := semanticDiagnostics(doc, true, nil); len(sem) > 0 {
		return finish(Result{Error: sem[0].Error(), SchemaValid: false, SchemaErrs: diagStrings(sem)})
	}
	kind := "request"
	if phase != nil && phase.Frame != nil {
		kind = "frame"
	} else if phase != nil && phase.HasResponse {
		kind = "response"
	}
	if doc.On.Request == nil || !doc.On.Request.matches(req.Method, req.Host, req.Path) {
		return finish(Result{SchemaValid: true})
	}
	var pipeline []Stage
	switch kind {
	case "request":
		if doc.Request == nil {
			return finish(Result{Matched: true, SchemaValid: true})
		}
		pipeline = doc.Request.Pipeline
	case "response":
		if doc.Response == nil {
			return finish(Result{SchemaValid: true})
		}
		pipeline = doc.Response.Pipeline
	case "frame":
		if doc.Frame == nil {
			return finish(Result{SchemaValid: true})
		}
		pipeline = doc.Frame.Pipeline
	}
	scope := e.baseScope(req, body, phase)
	vars, err := e.resolveVars(doc.Vars, scope)
	if err != nil {
		return finish(Result{Error: err.Error(), SchemaValid: false})
	}
	scope["vars"] = vars
	limit := timeout
	if limit <= 0 {
		limit = doc.defaultTimeout()
	}
	pr := e.runPipeline(pipeline, scope, req, body, phase, kind, limit, false)
	res := Result{
		Matched:     true,
		Actions:     pr.actions,
		Stages:      pr.stages,
		Vars:        vars,
		SchemaValid: pr.fatal == nil,
		DurationMs:  time.Since(start).Milliseconds(),
	}
	if pr.fatal != nil {
		res.Matched = false
		res.Actions = nil
		res.Error = pr.fatal.Error()
		res.SchemaErrs = []string{pr.fatal.Error()}
		return finish(res)
	}
	if pr.sawRun && !pr.sawPositive && !pr.sawStep {
		res.Matched = false
		res.Actions = nil
	}
	return finish(res)
}

func diagStrings(d []Diagnostic) []string {
	out := make([]string, len(d))
	for i, x := range d {
		out[i] = x.Error()
	}
	return out
}

func (d *Document) defaultTimeout() time.Duration {
	if d == nil || d.Timeout == "" {
		return shared.DefaultRuleTimeout
	}
	n := 0
	unit := ""
	fmt.Sscanf(d.Timeout, "%d%s", &n, &unit)
	if n <= 0 {
		return shared.DefaultRuleTimeout
	}
	var t time.Duration
	if unit == "s" {
		t = time.Duration(n) * time.Second
	} else {
		t = time.Duration(n) * time.Millisecond
	}
	if t > 2*time.Second {
		return 2 * time.Second
	}
	return t
}

func (e *RuleEngine) baseScope(req *shared.Request, body string, phase *Phase) map[string]any {
	headers := map[string]any{}
	if req != nil {
		for k, v := range req.RequestHeaders {
			headers[k] = v
		}
	}
	scope := map[string]any{
		"request": map[string]any{
			"id":      string(req.ID),
			"method":  req.Method,
			"scheme":  req.Scheme,
			"host":    req.Host,
			"path":    req.Path,
			"query":   req.Query,
			"url":     req.URL,
			"headers": headers,
			"body":    body,
		},
		"pipeline": map[string]any{},
		"vars":     map[string]any{},
		"hook":     map[string]any{"outputs": map[string]any{}},
	}
	if phase != nil && phase.HasResponse {
		rh := map[string]any{}
		for k, v := range phase.ResponseHeaders {
			rh[k] = v
		}
		scope["response"] = map[string]any{
			"status":        phase.ResponseStatus,
			"headers":       rh,
			"body":          phase.ResponseBody,
			"bodyTruncated": phase.ResponseBodyTruncated,
		}
	}
	if phase != nil && phase.Frame != nil {
		scope["frame"] = map[string]any{
			"direction": phase.Frame.Direction,
			"opcode":    phase.Frame.Opcode,
			"payload":   phase.Frame.Payload,
			"fin":       phase.Frame.Fin,
		}
	}
	return scope
}

func (e *RuleEngine) resolveVars(vars map[string]any, scope map[string]any) (map[string]any, error) {
	if len(vars) == 0 {
		return map[string]any{}, nil
	}
	if err := checkVars(vars); err != nil {
		return nil, err
	}
	resolved := map[string]any{}
	pending := map[string]any{}
	for k, v := range vars {
		pending[k] = v
	}
	for len(pending) > 0 {
		progress := false
		for name, val := range pending {
			scope["vars"] = resolved
			if !refsReady(val, resolved) {
				continue
			}
			got, err := expandValue(val, scope, true)
			if err != nil {
				return nil, fmt.Errorf("vars")
			}
			resolved[name] = got
			delete(pending, name)
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("vars cycle")
		}
	}
	raw, err := json.Marshal(resolved)
	if err != nil || len(raw) > maxVarsBytes {
		return nil, fmt.Errorf("vars")
	}
	scope["vars"] = resolved
	return resolved, nil
}

func refsReady(val any, resolved map[string]any) bool {
	s, ok := val.(string)
	if !ok {
		return true
	}
	for _, body := range exprBodies(s) {
		e, err := parseExpr(body)
		if err != nil {
			return true
		}
		var paths [][]string
		exprRefs(e, &paths)
		for _, p := range paths {
			if len(p) >= 2 && p[0] == "vars" {
				if _, ok := resolved[p[1]]; !ok {
					return false
				}
			}
		}
	}
	return true
}

type pipeOut struct {
	actions     []shared.RuleAction
	stages      []shared.StageTrace
	fatal       error
	sawRun      bool
	sawPositive bool
	sawStep     bool
	stop        bool
}

func (e *RuleEngine) runPipeline(stages []Stage, scope map[string]any, req *shared.Request, body string, phase *Phase, kind string, timeout time.Duration, nested bool) pipeOut {
	var out pipeOut
	for _, st := range stages {
		if out.stop || out.fatal != nil {
			break
		}
		switch {
		case st.Select != nil:
			e.runSelect(&out, st, scope, req, body, phase, kind, timeout)
		case st.Parallel != nil:
			e.runParallel(&out, st, scope, req, body, phase, kind, timeout)
		default:
			e.runAction(&out, st, scope, req, body, phase, kind, timeout, false)
		}
	}
	return out
}

func (e *RuleEngine) bindSecrets(scope map[string]any, plugin, instance string, redacted *[]string) {
	scope["__secrets"] = func(name string) (string, error) {
		if e.Runtime == nil {
			return "", fmt.Errorf("condition error")
		}
		v, err := e.Runtime.Secret(plugin, instance, name)
		if err != nil || v == "" {
			return "", fmt.Errorf("condition error")
		}
		*redacted = append(*redacted, v)
		return v, nil
	}
}

func (e *RuleEngine) runSelect(out *pipeOut, st Stage, scope map[string]any, req *shared.Request, body string, phase *Phase, kind string, timeout time.Duration) {
	if st.If != "" {
		ok, err := evalCond(st.If, scope)
		if err != nil {
			out.fatal = fmt.Errorf("condition error")
			out.stages = append(out.stages, shared.StageTrace{ID: st.ID, Error: "condition error"})
			return
		}
		if !ok {
			out.stages = append(out.stages, shared.StageTrace{ID: st.ID, Skipped: true, SkipReason: "if"})
			return
		}
	}
	chosen := -1
	for i, c := range st.Select.Cases {
		ok, err := evalCond(c.If, scope)
		if err != nil {
			out.fatal = fmt.Errorf("condition error")
			out.stages = append(out.stages, shared.StageTrace{ID: st.ID, Error: "condition error"})
			return
		}
		if ok {
			chosen = i
			break
		}
	}
	tr := shared.StageTrace{ID: st.ID}
	var arm []Stage
	if chosen >= 0 {
		tr.Case = st.Select.Cases[chosen].Name
		arm = st.Select.Cases[chosen].Pipeline
		for i, c := range st.Select.Cases {
			if i == chosen {
				continue
			}
			out.stages = append(out.stages, shared.StageTrace{ID: st.ID, Case: c.Name, Skipped: true, SkipReason: "unselected"})
		}
		if st.Select.Default != nil {
			name := st.Select.Default.Name
			if name == "" {
				name = "default"
			}
			out.stages = append(out.stages, shared.StageTrace{ID: st.ID, Case: name, Skipped: true, SkipReason: "unselected"})
		}
	} else if st.Select.Default != nil {
		name := st.Select.Default.Name
		if name == "" {
			name = "default"
		}
		tr.Case = name
		arm = st.Select.Default.Pipeline
		for _, c := range st.Select.Cases {
			out.stages = append(out.stages, shared.StageTrace{ID: st.ID, Case: c.Name, Skipped: true, SkipReason: "unselected"})
		}
	} else {
		tr.Skipped = true
		tr.SkipReason = "unselected"
		out.stages = append(out.stages, tr)
		for _, c := range st.Select.Cases {
			out.stages = append(out.stages, shared.StageTrace{ID: st.ID, Case: c.Name, Skipped: true, SkipReason: "unselected"})
		}
		return
	}
	// The choice is recorded before any arm stage runs.
	out.stages = append(out.stages, tr)
	if st.ID != "" {
		publish(scope, st.ID, map[string]any{"case": tr.Case})
	}
	armOut := e.runPipeline(arm, scope, req, body, phase, kind, timeout, true)
	out.stages = append(out.stages, armOut.stages...)
	out.actions = append(out.actions, armOut.actions...)
	out.sawRun = out.sawRun || armOut.sawRun
	out.sawPositive = out.sawPositive || armOut.sawPositive
	out.sawStep = out.sawStep || armOut.sawStep
	out.stop = out.stop || armOut.stop
	if armOut.fatal != nil {
		out.fatal = armOut.fatal
	}
}

func (e *RuleEngine) runParallel(out *pipeOut, st Stage, scope map[string]any, req *shared.Request, body string, phase *Phase, kind string, timeout time.Duration) {
	if st.If != "" {
		ok, err := evalCond(st.If, scope)
		if err != nil {
			out.stages = append(out.stages, shared.StageTrace{ID: st.ID, Error: "condition error"})
			return
		}
		if !ok {
			out.stages = append(out.stages, shared.StageTrace{ID: st.ID, Skipped: true, SkipReason: "if"})
			return
		}
	}
	limit := timeout
	if st.Parallel.TimeoutMs > 0 {
		limit = time.Duration(st.Parallel.TimeoutMs) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var wg sync.WaitGroup
	traces := make([]shared.StageTrace, len(st.Parallel.Stages))
	for i, child := range st.Parallel.Stages {
		wg.Add(1)
		go func(i int, child Stage) {
			defer wg.Done()
			local := cloneScope(scope)
			var sub pipeOut
			done := make(chan struct{})
			go func() {
				e.runAction(&sub, child, local, req, body, phase, kind, limit, true)
				close(done)
			}()
			select {
			case <-done:
			case <-ctx.Done():
				sub.stages = append(sub.stages, shared.StageTrace{ID: child.ID, Uses: child.Uses, Error: "timeout"})
			}
			if len(sub.stages) == 0 {
				traces[i] = shared.StageTrace{ID: child.ID, Uses: child.Uses}
			} else {
				traces[i] = sub.stages[0]
				if traces[i].Error == "" && len(sub.stages) > 1 {
					traces[i] = sub.stages[len(sub.stages)-1]
				}
			}
			if sub.fatal != nil {
				traces[i].Error = "parallel error"
			}
			// Parallel outputs stay on the trace. They are not published.
			traces[i].Outputs = redactMap(traces[i].Outputs, nil)
		}(i, child)
	}
	wg.Wait()
	out.stages = append(out.stages, traces...)
	out.sawStep = true
}

func (e *RuleEngine) runAction(out *pipeOut, st Stage, scope map[string]any, req *shared.Request, body string, phase *Phase, kind string, timeout time.Duration, inParallel bool) {
	tr := shared.StageTrace{ID: st.ID, Uses: st.Uses}
	if st.If != "" {
		ok, err := evalCond(st.If, scope)
		if err != nil {
			out.fatal = fmt.Errorf("condition error")
			tr.Error = "condition error"
			out.stages = append(out.stages, tr)
			return
		}
		if !ok {
			tr.Skipped = true
			tr.SkipReason = "if"
			out.stages = append(out.stages, tr)
			return
		}
	}
	limit := timeout
	if st.TimeoutMs > 0 {
		limit = time.Duration(st.TimeoutMs) * time.Millisecond
		if limit > 2*time.Second {
			limit = 2 * time.Second
		}
	}
	inst := "local"
	if st.Instance != "" {
		inst = st.Instance
	}
	plugin := ""
	if isPluginUses(st.Uses) {
		plugin = pluginID(st.Uses)
	}
	var secrets []string
	e.bindSecrets(scope, plugin, inst, &secrets)
	if st.Instance != "" {
		v, err := expandValue(st.Instance, scope, false)
		if err != nil {
			e.failStage(out, tr, "invalid instance", st.ContinueOnError || inParallel)
			return
		}
		s, ok := v.(string)
		if !ok || s == "" {
			e.failStage(out, tr, "invalid instance", st.ContinueOnError || inParallel)
			return
		}
		inst = s
	}
	with, err := expandMap(st.With, scope, false)
	if err != nil {
		e.failStage(out, tr, "invalid expression", st.ContinueOnError || inParallel)
		return
	}
	env, err := expandMap(st.Env, scope, false)
	if err != nil {
		e.failStage(out, tr, "invalid expression", st.ContinueOnError || inParallel)
		return
	}
	for _, v := range env {
		switch v.(type) {
		case string, bool, float64, int, int64, json.Number:
		default:
			if v != nil {
				e.failStage(out, tr, "invalid env", st.ContinueOnError || inParallel)
				return
			}
		}
	}

	if len(st.Middleware) > 0 && !inParallel {
		for _, mw := range st.Middleware {
			if e.Runtime == nil {
				e.failStage(out, tr, "plugin unavailable", st.ContinueOnError)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			res, err := e.Runtime.Invoke(ctx, InvokeRequest{
				Plugin: mw.Plugin, Instance: mw.Instance, Capability: mw.Name,
				Kind: "middleware", With: with, Env: env, Payload: payloadOf(scope), Timeout: limit,
			})
			cancel()
			if err != nil {
				e.failStage(out, tr, "middleware failed", st.ContinueOnError)
				return
			}
			out.actions = append(out.actions, res.Actions...)
			out.sawStep = true
		}
	}

	switch {
	case st.Run != nil:
		out.sawRun = true
		extra := map[string]any{
			"vars":     scope["vars"],
			"pipeline": scope["pipeline"],
			"env":      env,
			"with":     with,
		}
		ph := phase
		if ph == nil {
			ph = &Phase{}
		}
		cp := *ph
		cp.Extra = extra
		raw, outputs, err := e.callScript(st.Run.Script, req, body, &cp, limit)
		if err != nil {
			e.failStage(out, tr, "run failed", st.ContinueOnError || inParallel)
			return
		}
		if !raw.Match {
			tr.Skipped = true
			tr.SkipReason = "run"
			out.stages = append(out.stages, tr)
			return
		}
		out.sawPositive = true
		var actions []shared.RuleAction
		if len(raw.Actions) > 0 {
			var errs []string
			actions, errs = ValidateOutput(raw)
			if len(errs) > 0 {
				e.failStage(out, tr, "invalid actions", st.ContinueOnError || inParallel)
				return
			}
		}
		tr.Effect = "mutate"
		tr.Outputs = redactMap(outputs, secrets)
		out.stages = append(out.stages, tr)
		if !inParallel {
			out.actions = append(out.actions, actions...)
			if st.ID != "" && outputs != nil {
				publish(scope, st.ID, outputs)
			}
			if hasTerminal(actions) {
				out.stop = true
			}
		}
		out.sawStep = true
	case strings.HasPrefix(st.Uses, "chiling."):
		actions, outputs, effect, err := builtinCall(st.Uses, with, scope, kind)
		if err != nil {
			e.failStage(out, tr, "invalid action", st.ContinueOnError || inParallel)
			return
		}
		if inParallel && effect != "observe" {
			tr.Error = "parallel stages must be observe-only"
			out.stages = append(out.stages, tr)
			return
		}
		tr.Effect = effect
		tr.Outputs = redactMap(outputs, secrets)
		out.stages = append(out.stages, tr)
		out.sawStep = true
		if effect != "observe" && !inParallel {
			out.actions = append(out.actions, actions...)
			if hasTerminal(actions) {
				out.stop = true
			}
		}
		if st.ID != "" && !inParallel {
			publish(scope, st.ID, outputs)
		}
	default:
		if e.Runtime == nil || !e.Runtime.HasInstance(plugin, inst) {
			e.failStage(out, tr, "instance is not installed", st.ContinueOnError || inParallel)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		res, err := e.Runtime.Invoke(ctx, InvokeRequest{
			Plugin: plugin, Instance: inst, Capability: capabilityName(st.Uses),
			Kind: "uses", With: with, Env: env, Payload: payloadOf(scope), Timeout: limit,
			ExchangeID: string(req.ID),
		})
		cancel()
		if err != nil {
			e.failStage(out, tr, "plugin failed", st.ContinueOnError || inParallel)
			return
		}
		effect := res.Effect
		if effect == "" {
			effect = "mutate"
		}
		if inParallel && effect != "observe" {
			tr.Error = "parallel stages must be observe-only"
			out.stages = append(out.stages, tr)
			return
		}
		tr.Effect = effect
		tr.Outputs = redactMap(res.Outputs, secrets)
		out.stages = append(out.stages, tr)
		out.sawStep = true
		if !inParallel && effect != "observe" {
			out.actions = append(out.actions, res.Actions...)
			if hasTerminal(res.Actions) {
				out.stop = true
			}
		}
		if st.ID != "" && !inParallel {
			publish(scope, st.ID, res.Outputs)
		}
	}
}

func (e *RuleEngine) failStage(out *pipeOut, tr shared.StageTrace, msg string, cont bool) {
	tr.Error = msg
	out.stages = append(out.stages, tr)
	if !cont {
		out.fatal = fmt.Errorf("%s", msg)
	}
}

func hasTerminal(actions []shared.RuleAction) bool {
	for _, a := range actions {
		switch a.Type {
		case shared.ActionMockResponse, shared.ActionBlock, shared.ActionAbort:
			return true
		}
	}
	return false
}

func publish(scope map[string]any, id string, outputs map[string]any) {
	pipe, _ := scope["pipeline"].(map[string]any)
	if pipe == nil {
		pipe = map[string]any{}
		scope["pipeline"] = pipe
	}
	if outputs == nil {
		outputs = map[string]any{}
	}
	pipe[id] = map[string]any{"outputs": outputs}
}

func payloadOf(scope map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"request", "response", "frame", "vars"} {
		if v, ok := scope[k]; ok {
			out[k] = v
		}
	}
	return out
}

func expandMap(m map[string]any, scope map[string]any, embed bool) (map[string]any, error) {
	if m == nil {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	for k, v := range m {
		got, err := expandAny(v, scope, embed)
		if err != nil {
			return nil, err
		}
		out[k] = got
	}
	return out, nil
}

func expandAny(v any, scope map[string]any, embed bool) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		return expandMap(t, scope, embed)
	case []any:
		arr := make([]any, len(t))
		for i, c := range t {
			got, err := expandAny(c, scope, embed)
			if err != nil {
				return nil, err
			}
			arr[i] = got
		}
		return arr, nil
	default:
		return expandValue(v, scope, embed)
	}
}

func cloneScope(scope map[string]any) map[string]any {
	raw, err := json.Marshal(cleanScope(scope))
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func cleanScope(scope map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range scope {
		if strings.HasPrefix(k, "__") {
			continue
		}
		out[k] = v
	}
	return out
}

func redactMap(m map[string]any, secrets []string) map[string]any {
	if m == nil {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	s := string(raw)
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]any{}
	}
	return out
}

func builtinCall(uses string, with map[string]any, scope map[string]any, phase string) ([]shared.RuleAction, map[string]any, string, error) {
	ph, ok := builtinPhase[uses]
	if !ok {
		return nil, nil, "", fmt.Errorf("unknown builtin")
	}
	if ph != "any" && ph != phase {
		return nil, nil, "", fmt.Errorf("wrong phase")
	}
	effect := builtinEffect(uses)
	if effect == "observe" {
		return nil, observeOutputs(uses, with, scope), effect, nil
	}
	action, err := builtinAction(uses, with)
	if err != nil {
		return nil, nil, effect, err
	}
	return []shared.RuleAction{action}, map[string]any{"ok": true}, effect, nil
}

func observeOutputs(uses string, with map[string]any, scope map[string]any) map[string]any {
	switch uses {
	case "chiling.log", "chiling.notify":
		msg, _ := with["message"].(string)
		return map[string]any{"message": msg}
	case "chiling.metric":
		name, _ := with["name"].(string)
		return map[string]any{"name": name}
	case "chiling.read-body", "chiling.tee":
		body := ""
		truncated := false
		if req, ok := scope["request"].(map[string]any); ok {
			body, _ = req["body"].(string)
		}
		if resp, ok := scope["response"].(map[string]any); ok {
			if b, ok := resp["body"].(string); ok && b != "" {
				body = b
			}
			truncated, _ = resp["bodyTruncated"].(bool)
		}
		if len(body) > shared.MaxBodySize {
			body = body[:shared.MaxBodySize]
			truncated = true
		}
		return map[string]any{"bytes": body, "truncated": truncated}
	default:
		return map[string]any{}
	}
}

func builtinAction(uses string, with map[string]any) (shared.RuleAction, error) {
	params := map[string]any{}
	for k, v := range with {
		params[k] = v
	}
	var typ shared.RuleActionType
	switch uses {
	case "chiling.tag":
		typ = shared.ActionTag
	case "chiling.set-request-header":
		typ = shared.ActionSetRequestHeader
	case "chiling.remove-request-header":
		typ = shared.ActionRemoveRequestHeader
	case "chiling.rewrite-url":
		typ = shared.ActionRewriteURL
	case "chiling.mock-response":
		typ = shared.ActionMockResponse
	case "chiling.block":
		typ = shared.ActionBlock
	case "chiling.abort":
		typ = shared.ActionAbort
	case "chiling.set-response-header":
		typ = shared.ActionSetResponseHeader
	case "chiling.remove-response-header":
		typ = shared.ActionRemoveResponseHeader
	case "chiling.set-response-status":
		typ = shared.ActionSetResponseStatus
	case "chiling.rewrite-response-body":
		typ = shared.ActionRewriteResponseBody
	case "chiling.delay":
		typ = shared.ActionDelay
	case "chiling.throttle":
		typ = shared.ActionThrottle
	case "chiling.replace-frame":
		typ = shared.ActionReplaceFrame
	case "chiling.drop-frame":
		typ = shared.ActionDropFrame
	default:
		return shared.RuleAction{}, fmt.Errorf("unknown builtin")
	}
	return validateAction(ruleAction{Type: string(typ), Params: params})
}
