package proxy

import (
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

// ruleActions is the accumulated result of evaluating all enabled rules
// against a single request.
type ruleActions struct {
	Traces       []shared.RuleTrace
	Tags         []string
	SetHeaders   map[string]string // header → value
	RemoveHeader []string
	BlockAction  *shared.RuleAction // set if a block action matched (first wins)
	MockAction   *shared.RuleAction // set if a mockResponse action matched (last wins)
	AbortAction  bool               // set if any abort action matched
	RewriteURL   *string            // set if any rewriteURL action matched (last wins)
}

// evaluateRules evaluates all enabled rules against the request and returns
// the accumulated actions. Actions are resolved by "last matching rule wins"
// for conflicting dimensions.
func (p *Proxy) evaluateRules(req *shared.Request, body string, timeout time.Duration) ruleActions {
	result := ruleActions{
		SetHeaders: make(map[string]string),
	}

	if p.RuleStore == nil || p.RuleEngine == nil {
		return result
	}

	rules := p.RuleStore.EnabledRules()

	for _, r := range rules {
		evalResult := p.RuleEngine.Evaluate(r.Script, req, body, timeout)

		trace := shared.RuleTrace{
			RuleID:     r.ID,
			Matched:    evalResult.Matched,
			DurationMs: evalResult.DurationMs,
			Error:      evalResult.Error,
		}

		if evalResult.Matched && len(evalResult.Actions) > 0 {
			trace.Actions = evalResult.Actions
			// Apply actions: last matching rule wins for conflicts.
			for _, action := range evalResult.Actions {
				result.applyAction(action)
			}
		}

		result.Traces = append(result.Traces, trace)

		// Stop evaluating if any rule aborted.
		if result.AbortAction {
			break
		}
	}

	// Fill in the duration for all traces from the engine results.
	return result
}

// applyAction applies a single validated action to the accumulated result.
func (a *ruleActions) applyAction(action shared.RuleAction) {
	switch action.Type {
	case shared.ActionTag:
		if params, ok := action.Params["tags"].([]string); ok {
			a.Tags = append(a.Tags, params...)
		} else if params, ok := action.Params["tags"].([]any); ok {
			for _, t := range params {
				if s, ok := t.(string); ok {
					a.Tags = append(a.Tags, s)
				}
			}
		}

	case shared.ActionSetRequestHeader:
		if name, ok := action.Params["name"].(string); ok && name != "" {
			if value, ok := action.Params["value"].(string); ok {
				a.SetHeaders[name] = value
			}
		}

	case shared.ActionRemoveRequestHeader:
		if name, ok := action.Params["name"].(string); ok && name != "" {
			a.RemoveHeader = append(a.RemoveHeader, name)
		}

	case shared.ActionBlock:
		a.BlockAction = &action

	case shared.ActionMockResponse:
		a.MockAction = &action

	case shared.ActionAbort:
		a.AbortAction = true

	case shared.ActionRewriteURL:
		if url, ok := action.Params["url"].(string); ok && url != "" {
			a.RewriteURL = &url
		}
	}
}
