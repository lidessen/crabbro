package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type oaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaRequest struct {
	Model          string         `json:"model"`
	Messages       []oaMessage    `json:"messages"`
	ResponseFormat map[string]any `json:"response_format"`
}

type oaResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			Refusal string `json:"refusal"`
		} `json:"message"`
	} `json:"choices"`
}

func (c *Client) findOpenAI(ctx context.Context, query string, flows []Flow) (FindResult, error) {
	if len(flows) > 32 {
		flows = flows[len(flows)-32:]
	}
	state, _ := json.Marshal(map[string]any{"query": query, "flows": flows})
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"hits": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"requestId": map[string]any{"type": "string"},
						"score":     map[string]any{"type": "number"},
						"reason":    map[string]any{"type": "string"},
					},
					"required":             []string{"requestId", "score", "reason"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"hits"},
		"additionalProperties": false,
	}
	var parsed struct {
		Hits []Hit `json:"hits"`
	}
	model, err := c.complete(ctx, "find_flows", schema,
		"You match captured HTTP flows to a query. Return only flows that match. score is 0 to 1. reason is one sentence.",
		string(state), &parsed)
	if err != nil {
		return FindResult{}, err
	}
	allowed := map[string]bool{}
	for _, f := range flows {
		allowed[f.ID] = true
	}
	out := FindResult{Model: model, Provider: "openai"}
	seen := map[string]bool{}
	for _, h := range parsed.Hits {
		if !allowed[h.RequestID] {
			return FindResult{}, fmt.Errorf("model returned unknown request id %q", h.RequestID)
		}
		if h.Score < 0 || h.Score > 1 {
			return FindResult{}, fmt.Errorf("score for %s is outside [0,1]", h.RequestID)
		}
		if strings.TrimSpace(h.Reason) == "" {
			return FindResult{}, fmt.Errorf("missing reason for %s", h.RequestID)
		}
		if h.Score < 0.5 || seen[h.RequestID] {
			continue
		}
		seen[h.RequestID] = true
		out.Hits = append(out.Hits, h)
	}
	sortHits(out.Hits)
	return out, nil
}

func (c *Client) draftOpenAI(ctx context.Context, description string, flow *Flow) (DraftResult, error) {
	payload := map[string]any{"description": description}
	if flow != nil {
		payload["flow"] = flow
	}
	state, _ := json.Marshal(payload)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string"},
			"action":      map[string]any{"type": "string", "enum": []string{"mockResponse", "block", "setRequestHeader", "rewriteURL", "tag"}},
			"explanation": map[string]any{"type": "string"},
			"script":      map[string]any{"type": "string"},
		},
		"required":             []string{"name", "action", "explanation", "script"},
		"additionalProperties": false,
	}
	var parsed DraftResult
	model, err := c.complete(ctx, "draft_rule", schema,
		"Write a chiling JavaScript rule. The default export is function rule(ctx) and returns {match, actions}. Each action is {type, params}. Use only mockResponse, block, setRequestHeader, rewriteURL, or tag.",
		string(state), &parsed)
	if err != nil {
		return DraftResult{}, err
	}
	if strings.TrimSpace(parsed.Name) == "" || strings.TrimSpace(parsed.Script) == "" || strings.TrimSpace(parsed.Explanation) == "" {
		return DraftResult{}, fmt.Errorf("draft response missing name, script, or explanation")
	}
	if !strings.Contains(parsed.Script, "function") {
		return DraftResult{}, fmt.Errorf("draft script has no function")
	}
	switch parsed.Action {
	case "mockResponse", "block", "setRequestHeader", "rewriteURL", "tag":
	default:
		return DraftResult{}, fmt.Errorf("draft action %q is not allowed", parsed.Action)
	}
	parsed.Model = model
	parsed.Provider = "openai"
	return parsed, nil
}

func (c *Client) complete(ctx context.Context, name string, schema map[string]any, system, user string, dest any) (string, error) {
	var resp oaResponse
	err := c.postJSON(ctx, c.cfg.BaseURL+"/chat/completions", oaRequest{
		Model: c.cfg.Model,
		Messages: []oaMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat: map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   name,
				"strict": true,
				"schema": schema,
			},
		},
	}, &resp)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("decision API returned no choices")
	}
	msg := resp.Choices[0].Message
	if msg.Refusal != "" {
		return "", fmt.Errorf("model refused the request")
	}
	if strings.TrimSpace(resp.Model) == "" {
		return "", fmt.Errorf("decision API response missing model")
	}
	if err := json.Unmarshal([]byte(msg.Content), dest); err != nil {
		return "", fmt.Errorf("structured output is not valid JSON: %w", err)
	}
	return resp.Model, nil
}

func sortHits(hits []Hit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score == hits[j].Score {
			return hits[i].RequestID < hits[j].RequestID
		}
		return hits[i].Score > hits[j].Score
	})
}

func scaffoldScript(description, action string, flow *Flow) string {
	host, path, method := "", "", ""
	if flow != nil {
		method = flow.Method
		if u := flow.URL; u != "" {
			// Keep the match simple: host and path are filled by the caller
			// through the URL string when present.
			host, path = splitURL(u)
		}
	}
	var b strings.Builder
	b.WriteString("export default function rule(ctx) {\n")
	fmt.Fprintf(&b, "  // %s\n", sanitizeComment(description))
	if host != "" {
		fmt.Fprintf(&b, "  if (ctx.request.host !== %q) return { match: false };\n", host)
	}
	if method != "" {
		fmt.Fprintf(&b, "  if (ctx.request.method !== %q) return { match: false };\n", method)
	}
	if path != "" {
		fmt.Fprintf(&b, "  if (ctx.request.path !== %q) return { match: false };\n", path)
	}
	b.WriteString("  return { match: true, actions: [")
	switch action {
	case "block":
		b.WriteString(`{ type: "block", params: { status: 403, body: "blocked" } }`)
	case "setRequestHeader":
		b.WriteString(`{ type: "setRequestHeader", params: { name: "X-Chiling", value: "1" } }`)
	case "rewriteURL":
		b.WriteString(`{ type: "rewriteURL", params: { url: "https://example.test/rewritten" } }`)
	case "tag":
		b.WriteString(`{ type: "tag", params: { tags: ["matched"] } }`)
	default:
		b.WriteString(`{ type: "mockResponse", params: { status: 200, headers: { "content-type": "application/json" }, body: "{\"ok\":true}" } }`)
	}
	b.WriteString("] };\n}\n")
	return b.String()
}

func splitURL(raw string) (host, path string) {
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return rest, "/"
	}
	host = rest[:slash]
	path = rest[slash:]
	if q := strings.Index(path, "?"); q >= 0 {
		path = path[:q]
	}
	return host, path
}

func sanitizeComment(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "*/", "")
	if len(s) > 180 {
		s = s[:180]
	}
	return s
}
