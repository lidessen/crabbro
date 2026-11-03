package decision

import (
	"context"
	"fmt"
	"strings"
)

type jevQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type jevRequest struct {
	Model     string                 `json:"model"`
	State     any                    `json:"state"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevAnswer struct {
	Type       string             `json:"type"`
	Noul       *float64           `json:"noul,omitempty"`
	Choice     string             `json:"choice,omitempty"`
	Confidence *float64           `json:"confidence,omitempty"`
	Score      *float64           `json:"score,omitempty"`
	Probs      map[string]float64 `json:"probabilities,omitempty"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (c *Client) findJev(ctx context.Context, query string, flows []Flow) (FindResult, error) {
	if len(flows) > 32 {
		flows = flows[len(flows)-32:]
	}
	questions := make(map[string]jevQuestion, len(flows))
	ids := make(map[string]string, len(flows))
	for _, f := range flows {
		qid := "m_" + sanitizeID(f.ID)
		ids[qid] = f.ID
		questions[qid] = jevQuestion{
			Type:         "noul",
			Instructions: fmt.Sprintf("The user is looking for: %s. Does flow %s match that description?", query, f.ID),
			Criteria: map[string]string{
				"true":  "This flow is the one the query describes",
				"false": "This flow does not match the query",
			},
		}
	}
	var resp jevResponse
	err := c.postJSON(ctx, c.cfg.BaseURL+"/v1/systemone", jevRequest{
		Model:     c.cfg.Model,
		State:     map[string]any{"query": query, "flows": flows},
		Questions: questions,
	}, &resp)
	if err != nil {
		return FindResult{}, err
	}
	if err := validateJev(resp, questions); err != nil {
		return FindResult{}, err
	}
	result := FindResult{Model: resp.Model, Provider: "jev"}
	for qid, ans := range resp.Answers {
		if ans.Noul == nil || *ans.Noul < 0.5 {
			continue
		}
		id := ids[qid]
		result.Hits = append(result.Hits, Hit{
			RequestID: id,
			Score:     *ans.Noul,
			Reason:    fmt.Sprintf("jev noul %.2f for %q", *ans.Noul, query),
		})
	}
	sortHits(result.Hits)
	return result, nil
}

func (c *Client) draftJev(ctx context.Context, description string, flow *Flow) (DraftResult, error) {
	actions := []string{"mockResponse", "block", "setRequestHeader", "rewriteURL", "tag"}
	criteria := map[string]string{
		"mockResponse":     "Return a synthetic response and do not call upstream",
		"block":            "Reject the request with an error status",
		"setRequestHeader": "Add or replace a request header before forwarding",
		"rewriteURL":       "Send the request to a different URL",
		"tag":              "Only label the request",
	}
	q := map[string]jevQuestion{
		"action": {
			Type:         "choice",
			Instructions: "Which single rule action best fits the description?",
			Criteria:     criteria,
		},
	}
	state := map[string]any{"description": description}
	if flow != nil {
		state["flow"] = flow
	}
	var resp jevResponse
	if err := c.postJSON(ctx, c.cfg.BaseURL+"/v1/systemone", jevRequest{
		Model: c.cfg.Model, State: state, Questions: q,
	}, &resp); err != nil {
		return DraftResult{}, err
	}
	if err := validateJev(resp, q); err != nil {
		return DraftResult{}, err
	}
	ans := resp.Answers["action"]
	if !contains(actions, ans.Choice) {
		return DraftResult{}, fmt.Errorf("jev choice %q is not an allowed action", ans.Choice)
	}
	script := scaffoldScript(description, ans.Choice, flow)
	conf := 0.0
	if ans.Confidence != nil {
		conf = *ans.Confidence
	}
	return DraftResult{
		Name:        "draft-" + ans.Choice,
		Script:      script,
		Explanation: fmt.Sprintf("jev chose %s (confidence %.2f) for %q", ans.Choice, conf, description),
		Action:      ans.Choice,
		Model:       resp.Model,
		Provider:    "jev",
	}, nil
}

func validateJev(resp jevResponse, questions map[string]jevQuestion) error {
	if strings.TrimSpace(resp.Model) == "" {
		return fmt.Errorf("jev response missing model")
	}
	if len(resp.Answers) == 0 {
		return fmt.Errorf("jev response missing answers")
	}
	for id, q := range questions {
		ans, ok := resp.Answers[id]
		if !ok {
			return fmt.Errorf("jev response missing answer %q", id)
		}
		if ans.Type != q.Type {
			return fmt.Errorf("jev answer %q type %q, want %q", id, ans.Type, q.Type)
		}
		switch q.Type {
		case "noul":
			if ans.Noul == nil || *ans.Noul < 0 || *ans.Noul > 1 {
				return fmt.Errorf("jev answer %q has noul outside [0,1]", id)
			}
		case "choice":
			opts, _ := q.Criteria.(map[string]string)
			if ans.Choice == "" || (opts != nil && !containsMap(opts, ans.Choice)) {
				return fmt.Errorf("jev answer %q choice is not in criteria", id)
			}
			if ans.Confidence != nil && (*ans.Confidence < 0 || *ans.Confidence > 1) {
				return fmt.Errorf("jev answer %q confidence outside [0,1]", id)
			}
		default:
			return fmt.Errorf("unsupported jev question type %q", q.Type)
		}
	}
	return nil
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

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func containsMap(m map[string]string, v string) bool {
	_, ok := m[v]
	return ok
}
