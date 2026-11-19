package ir

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DecisionToolName is the forced tool name used when emulating structured
// DecisionIR responses over Anthropic Messages.
const DecisionToolName = "submit_decision"

// DecisionToGenRequest converts a TypeSafe System One DecisionRequest into a
// structured-output GenRequest suitable for OpenAI, Gemini, or Anthropic.
func DecisionToGenRequest(req *DecisionRequest, targetModel string) (*GenRequest, error) {
	if req == nil || len(req.Questions) == 0 {
		return nil, fmt.Errorf("decision request requires at least one question")
	}
	qProps := make(map[string]any, len(req.Questions))
	qReq := make([]string, 0, len(req.Questions))

	qIDs := make([]string, 0, len(req.Questions))
	for id := range req.Questions {
		qIDs = append(qIDs, id)
	}
	sort.Strings(qIDs)

	for _, id := range qIDs {
		q := req.Questions[id]
		qReq = append(qReq, id)
		switch q.Type {
		case DecisionKindNoul:
			qProps[id] = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"noul": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				},
				"required":             []string{"noul"},
				"additionalProperties": false,
			}
		case DecisionKindChoice:
			choiceProp := map[string]any{"type": "string"}
			if opts := extractChoices(q.Criteria); len(opts) > 0 {
				choiceProp["enum"] = opts
			}
			qProps[id] = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"choice":     choiceProp,
					"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				},
				"required":             []string{"choice", "confidence"},
				"additionalProperties": false,
			}
		case DecisionKindScore:
			qProps[id] = map[string]any{
				"type": "object",
				"properties": map[string]any{
					"score":      map[string]any{"type": "number", "minimum": 0, "maximum": 1},
					"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
				},
				"required":             []string{"score", "confidence"},
				"additionalProperties": false,
			}
		default:
			return nil, fmt.Errorf("unsupported decision question type %q", q.Type)
		}
	}

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answers": map[string]any{
				"type":                 "object",
				"properties":           qProps,
				"required":             qReq,
				"additionalProperties": false,
			},
		},
		"required":             []string{"answers"},
		"additionalProperties": false,
	}

	payload, err := json.Marshal(map[string]any{
		"state":     req.State,
		"questions": req.Questions,
	})
	if err != nil {
		return nil, err
	}

	temp := 0.0
	return &GenRequest{
		Model:          targetModel,
		SchemaName:     DecisionToolName,
		ResponseSchema: schema,
		MaxTokens:      2048,
		Temperature:    &temp,
		Stream:         false,
		System: []ContentPart{{
			Kind: PartText,
			Text: "You are a structured decision engine. Evaluate the input state against every question and return ONLY JSON conforming strictly to the required schema.",
		}},
		Messages: []Message{{
			Role: "user",
			Parts: []ContentPart{{
				Kind: PartText,
				Text: string(payload),
			}},
		}},
	}, nil
}

// GenToDecisionResponse parses and strictly validates a generative response back
// into a DecisionResponse. Emulated responses always carry Calibrated=false.
func GenToDecisionResponse(req *DecisionRequest, gen *GenResponse) (*DecisionResponse, error) {
	if req == nil || gen == nil {
		return nil, fmt.Errorf("missing request or generative response")
	}
	rawJSON := extractDecisionJSON(gen)
	if rawJSON == "" {
		return nil, fmt.Errorf("emulated decision response contained no JSON payload")
	}

	var parsed struct {
		Answers map[string]struct {
			Noul       *float64 `json:"noul,omitempty"`
			Choice     string   `json:"choice,omitempty"`
			Score      *float64 `json:"score,omitempty"`
			Confidence *float64 `json:"confidence,omitempty"`
		} `json:"answers"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
		return nil, fmt.Errorf("emulated decision returned invalid JSON: %w", err)
	}
	if len(parsed.Answers) == 0 {
		return nil, fmt.Errorf("emulated decision response missing answers")
	}

	out := &DecisionResponse{
		Model:      gen.Model,
		Answers:    make(map[string]DecisionAnswer, len(req.Questions)),
		Usage:      gen.Usage,
		Calibrated: false,
		Mode:       "emulated",
	}

	for id, q := range req.Questions {
		rawAns, ok := parsed.Answers[id]
		if !ok {
			return nil, fmt.Errorf("emulated decision response missing answer %q", id)
		}
		ans := DecisionAnswer{Type: q.Type}
		switch q.Type {
		case DecisionKindNoul:
			if rawAns.Noul == nil || *rawAns.Noul < 0 || *rawAns.Noul > 1 {
				return nil, fmt.Errorf("emulated noul answer %q outside [0,1]", id)
			}
			v := *rawAns.Noul
			ans.Noul = &v
		case DecisionKindChoice:
			opts := extractChoices(q.Criteria)
			if strings.TrimSpace(rawAns.Choice) == "" {
				return nil, fmt.Errorf("emulated choice answer %q is empty", id)
			}
			if len(opts) > 0 && !containsString(opts, rawAns.Choice) {
				return nil, fmt.Errorf("emulated choice answer %q (%q) not in criteria", id, rawAns.Choice)
			}
			ans.Choice = rawAns.Choice
			if rawAns.Confidence != nil {
				if *rawAns.Confidence < 0 || *rawAns.Confidence > 1 {
					return nil, fmt.Errorf("emulated choice confidence %q outside [0,1]", id)
				}
				c := *rawAns.Confidence
				ans.Confidence = &c
			}
		case DecisionKindScore:
			if rawAns.Score == nil || *rawAns.Score < 0 || *rawAns.Score > 1 {
				return nil, fmt.Errorf("emulated score answer %q outside [0,1]", id)
			}
			v := *rawAns.Score
			ans.Score = &v
			if rawAns.Confidence != nil {
				if *rawAns.Confidence < 0 || *rawAns.Confidence > 1 {
					return nil, fmt.Errorf("emulated score confidence %q outside [0,1]", id)
				}
				c := *rawAns.Confidence
				ans.Confidence = &c
			}
		default:
			return nil, fmt.Errorf("unsupported decision question type %q", q.Type)
		}
		out.Answers[id] = ans
	}
	return out, nil
}

func extractDecisionJSON(gen *GenResponse) string {
	for _, p := range gen.Content {
		if p.Kind == PartToolUse && (p.ToolName == DecisionToolName || len(p.InputJSON) > 0) {
			if s := strings.TrimSpace(string(p.InputJSON)); s != "" {
				return s
			}
		}
	}
	for _, p := range gen.Content {
		if p.Kind == PartText {
			s := strings.TrimSpace(p.Text)
			if strings.HasPrefix(s, "```") {
				s = stripCodeFence(s)
			}
			if s != "" {
				return s
			}
		}
	}
	return ""
}

func stripCodeFence(s string) string {
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func extractChoices(criteria any) []string {
	switch c := criteria.(type) {
	case []string:
		return append([]string(nil), c...)
	case []any:
		out := make([]string, 0, len(c))
		for _, item := range c {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case map[string]string:
		out := make([]string, 0, len(c))
		for k := range c {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	case map[string]any:
		out := make([]string, 0, len(c))
		for k := range c {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	default:
		return nil
	}
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}
