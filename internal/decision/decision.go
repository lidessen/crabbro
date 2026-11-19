// Package decision asks a configured gateway decision route to judge captured
// flows, draft YAML v1 rule pipelines, or answer plugin decision prompts.
package decision

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/shared"
)

// ErrNotConfigured is returned when no enabled gateway decision route is available.
var ErrNotConfigured = errors.New("decision API is not configured")

// Invoker decouples decision orchestration from concrete gateway HTTP/execution internals.
type Invoker interface {
	InvokeDecision(ctx context.Context, routeID string, req *ir.DecisionRequest) (*ir.DecisionResponse, error)
	DecisionStatus(routeID string) (configured bool, provider, model, routeName string)
}

// Flow is one captured exchange shown to the decision model. Bodies are previews.
type Flow struct {
	ID           string `json:"id"`
	Method       string `json:"method"`
	URL          string `json:"url"`
	Status       int    `json:"status,omitempty"`
	RequestBody  string `json:"requestBody,omitempty"`
	ResponseBody string `json:"responseBody,omitempty"`
}

// Hit is one flow the model judged relevant.
type Hit struct {
	RequestID string  `json:"requestId"`
	Score     float64 `json:"score"`
	Reason    string  `json:"reason"`
}

// FindResult is the validated search result.
type FindResult struct {
	Hits     []Hit  `json:"hits"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

// DraftResult is a validated YAML rule scaffold selected by the decision model.
type DraftResult struct {
	Name        string `json:"name"`
	Script      string `json:"script"`
	Explanation string `json:"explanation"`
	Action      string `json:"action"`
	Model       string `json:"model"`
	Provider    string `json:"provider"`
}

// Status is the public configuration view of the decision route.
type Status struct {
	Configured bool   `json:"configured"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Route      string `json:"route,omitempty"`
}

// Client invokes a configured gateway decision route.
type Client struct {
	invoker Invoker
	routeID string
}

// New constructs a Client routed through invoker for routeID (default "decision").
func New(invoker Invoker, routeID string) *Client {
	if strings.TrimSpace(routeID) == "" {
		routeID = "decision"
	}
	return &Client{invoker: invoker, routeID: routeID}
}

// Status reports whether the backing gateway decision route is configured.
func (c *Client) Status() Status {
	if c == nil || c.invoker == nil {
		return Status{Configured: false}
	}
	ok, provider, model, route := c.invoker.DecisionStatus(c.routeID)
	return Status{
		Configured: ok,
		Provider:   provider,
		Model:      model,
		Route:      route,
	}
}

// Find ranks flows against a natural-language query via the gateway decision route.
func (c *Client) Find(ctx context.Context, query string, flows []Flow) (FindResult, error) {
	st := c.Status()
	if !st.Configured {
		return FindResult{}, ErrNotConfigured
	}
	if strings.TrimSpace(query) == "" {
		return FindResult{}, fmt.Errorf("query is required")
	}
	if len(flows) == 0 {
		return FindResult{Model: st.Model, Provider: st.Provider}, nil
	}
	if len(flows) > 32 {
		flows = flows[len(flows)-32:]
	}

	questions := make(map[string]ir.DecisionQuestion, len(flows))
	ids := make(map[string]string, len(flows))
	for i, f := range flows {
		qid := fmt.Sprintf("m_%d_%s", i, sanitizeID(f.ID))
		ids[qid] = f.ID
		questions[qid] = ir.DecisionQuestion{
			Type:         ir.DecisionKindNoul,
			Instructions: fmt.Sprintf("The user is looking for: %s. Does flow %s match that description?", query, f.ID),
			Criteria: map[string]string{
				"true":  "This flow matches the query",
				"false": "This flow does not match the query",
			},
		}
	}

	resp, err := c.invoker.InvokeDecision(ctx, c.routeID, &ir.DecisionRequest{
		Model:     st.Model,
		State:     map[string]any{"query": query, "flows": flows},
		Questions: questions,
	})
	if err != nil {
		return FindResult{}, err
	}

	out := FindResult{
		Model:    firstNonEmpty(resp.Model, st.Model),
		Provider: providerWithMode(st.Provider, resp.Mode),
	}
	for qid, ans := range resp.Answers {
		if ans.Noul == nil || *ans.Noul < 0.5 {
			continue
		}
		origID, ok := ids[qid]
		if !ok {
			continue
		}
		out.Hits = append(out.Hits, Hit{
			RequestID: origID,
			Score:     *ans.Noul,
			Reason:    fmt.Sprintf("decision noul %.2f for %q", *ans.Noul, query),
		})
	}
	sort.Slice(out.Hits, func(i, j int) bool {
		if out.Hits[i].Score == out.Hits[j].Score {
			return out.Hits[i].RequestID < out.Hits[j].RequestID
		}
		return out.Hits[i].Score > out.Hits[j].Score
	})
	return out, nil
}

// Draft selects an appropriate rule action via the gateway decision route and
// generates a validated YAML v1 rule pipeline.
func (c *Client) Draft(ctx context.Context, description string, flow *Flow) (DraftResult, error) {
	st := c.Status()
	if !st.Configured {
		return DraftResult{}, ErrNotConfigured
	}
	if strings.TrimSpace(description) == "" {
		return DraftResult{}, fmt.Errorf("description is required")
	}

	allowedActions := []string{"mockResponse", "block", "setRequestHeader", "rewriteURL", "tag"}
	criteria := map[string]string{
		"mockResponse":     "Return a synthetic response without calling upstream",
		"block":            "Reject the matching request with an error status",
		"setRequestHeader": "Add or replace a request header before forwarding",
		"rewriteURL":       "Rewrite the destination URL",
		"tag":              "Attach metadata tags to matching exchanges",
	}
	state := map[string]any{"description": description}
	if flow != nil {
		state["flow"] = flow
	}
	resp, err := c.invoker.InvokeDecision(ctx, c.routeID, &ir.DecisionRequest{
		Model: st.Model,
		State: state,
		Questions: map[string]ir.DecisionQuestion{
			"action": {
				Type:         ir.DecisionKindChoice,
				Instructions: "Which single rule action best matches the description?",
				Criteria:     criteria,
			},
		},
	})
	if err != nil {
		return DraftResult{}, err
	}
	ans, ok := resp.Answers["action"]
	if !ok || !containsString(allowedActions, ans.Choice) {
		return DraftResult{}, fmt.Errorf("decision choice %q is not an allowed rule action", ans.Choice)
	}

	script := scaffoldYAMLRule(description, ans.Choice, flow)
	if diags := rule.Validate(script, true, nil); len(diags) > 0 {
		return DraftResult{}, fmt.Errorf("drafted YAML rule invalid: %s", diags[0].Message)
	}

	conf := 0.0
	if ans.Confidence != nil {
		conf = *ans.Confidence
	}
	return DraftResult{
		Name:        "draft-" + ans.Choice,
		Script:      script,
		Explanation: fmt.Sprintf("decision (%s) chose %s (confidence %.2f) for %q", firstNonEmpty(resp.Mode, "native"), ans.Choice, conf, description),
		Action:      ans.Choice,
		Model:       firstNonEmpty(resp.Model, st.Model),
		Provider:    providerWithMode(st.Provider, resp.Mode),
	}, nil
}

func scaffoldYAMLRule(description, action string, flow *Flow) string {
	var host, path, method string
	if flow != nil {
		method = flow.Method
		if u, err := url.Parse(flow.URL); err == nil {
			host = u.Hostname()
			path = u.Path
		}
	}
	note := strings.ReplaceAll(description, "\n", " ")
	if len(note) > 180 {
		note = note[:180]
	}
	if strings.TrimSpace(note) == "" {
		note = "drafted via gateway decision"
	}
	return rule.ScaffoldWithNote(&shared.Request{
		Method: method,
		Host:   host,
		Path:   path,
	}, action, `{"ok":true}`, "application/json", note)
}

func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "flow"
	}
	return b.String()
}

func containsString(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

func providerWithMode(provider, mode string) string {
	if mode != "" && provider != "" {
		return provider + ":" + mode
	}
	return firstNonEmpty(provider, mode)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
