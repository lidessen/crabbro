package codec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/gateway/protocol"
)

type decisionOption struct {
	Value       string `json:"value,omitempty"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

func (o *decisionOption) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		o.Value = s
		o.Label = s
		return nil
	}
	type plain decisionOption
	return json.Unmarshal(b, (*plain)(o))
}

type decisionQuestion struct {
	Type         string           `json:"type"`
	Name         string           `json:"name"`
	Instructions string           `json:"instructions"`
	Choices      []decisionOption `json:"choices,omitempty"`
	Levels       []decisionOption `json:"levels,omitempty"`
}
type decisionWireRequest struct {
	Model          string             `json:"model"`
	Input          any                `json:"input"`
	Questions      []decisionQuestion `json:"questions"`
	PromptCacheKey string             `json:"prompt_cache_key,omitempty"`
}
type decisionWireAnswer struct {
	Type          string                `json:"type"`
	Name          string                `json:"name"`
	Probability   *float64              `json:"probability,omitempty"`
	Choice        string                `json:"choice,omitempty"`
	Confidence    *float64              `json:"confidence,omitempty"`
	Score         *float64              `json:"score,omitempty"`
	Probabilities decisionProbabilities `json:"probabilities,omitempty"`
}
type decisionProbabilities map[string]float64

type decisionProbability struct {
	Value       any     `json:"value"`
	Label       string  `json:"label,omitempty"`
	Probability float64 `json:"probability"`
}

func (p *decisionProbabilities) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '[' {
		return json.Unmarshal(b, (*map[string]float64)(p))
	}
	var entries []struct {
		Value       json.RawMessage `json:"value"`
		Label       string          `json:"label"`
		Probability float64         `json:"probability"`
	}
	if err := json.Unmarshal(b, &entries); err != nil {
		return err
	}
	result := make(decisionProbabilities, len(entries))
	for _, entry := range entries {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(entry.Value))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("invalid probability value: %w", err)
		}
		var key string
		switch v := value.(type) {
		case string:
			key = v
		case json.Number:
			key = v.String()
		default:
			return fmt.Errorf("invalid probability value type %T", value)
		}
		if entry.Label != "" {
			key = entry.Label
		}
		result[key] = entry.Probability
	}
	*p = result
	return nil
}

func (a decisionWireAnswer) MarshalJSON() ([]byte, error) {
	type wireAnswer decisionWireAnswer
	var probabilities []decisionProbability
	keys := make([]string, 0, len(a.Probabilities))
	for key := range a.Probabilities {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		entry := decisionProbability{Value: key, Probability: a.Probabilities[key]}
		if a.Type == ir.DecisionKindScore {
			entry.Value, entry.Label = i, key
		}
		probabilities = append(probabilities, entry)
	}
	return json.Marshal(struct {
		wireAnswer
		Probabilities []decisionProbability `json:"probabilities,omitempty"`
	}{wireAnswer: wireAnswer(a), Probabilities: probabilities})
}

type decisionWireResponse struct {
	Model   string               `json:"model"`
	Answers []decisionWireAnswer `json:"answers"`
	Usage   any                  `json:"usage,omitempty"`
}

func decisionOptions(criteria any, score bool) []decisionOption {
	var out []decisionOption
	add := func(k, desc string) {
		o := decisionOption{Description: desc}
		if score {
			o.Label = k
		} else {
			o.Value = k
		}
		out = append(out, o)
	}
	switch v := criteria.(type) {
	case map[string]string:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(k, v[k])
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(k, anyToString(v[k]))
		}
	case []string:
		for _, s := range v {
			add(s, "")
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				add(s, "")
			}
		}
	}
	return out
}
func EncodeOpenAIDecisionsRequest(req *ir.DecisionRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("decision request is nil")
	}
	text, image, hasImage, err := decisionImage(req)
	if err != nil {
		return nil, err
	}
	input := req.State
	if _, ok := input.(string); !ok {
		b, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		input = string(b)
	}
	if hasImage {
		input = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "UI state:\n" + text}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + image}}}}
	}
	w := decisionWireRequest{Model: req.Model, Input: input, PromptCacheKey: req.PromptCacheKey}
	keys := make([]string, 0, len(req.Questions))
	for k := range req.Questions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		q := req.Questions[k]
		x := decisionQuestion{Name: k, Instructions: q.Instructions, Type: q.Type}
		switch q.Type {
		case ir.DecisionKindNoul:
			x.Type = "predicate"
		case ir.DecisionKindChoice:
			x.Choices = decisionOptions(q.Criteria, false)
		case ir.DecisionKindScore:
			x.Levels = decisionOptions(q.Criteria, true)
		default:
			return nil, fmt.Errorf("unsupported decision question type %q", q.Type)
		}
		w.Questions = append(w.Questions, x)
	}
	return json.Marshal(w)
}
func DecodeOpenAIDecisionsRequest(body []byte) (*ir.DecisionRequest, error) {
	var w decisionWireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	if len(w.Questions) == 0 {
		return nil, fmt.Errorf("openai-decisions request requires questions")
	}
	r := &ir.DecisionRequest{Model: w.Model, State: w.Input, PromptCacheKey: w.PromptCacheKey, Questions: map[string]ir.DecisionQuestion{}}
	for _, q := range w.Questions {
		if q.Name == "" {
			return nil, fmt.Errorf("decision question requires name")
		}
		if _, ok := r.Questions[q.Name]; ok {
			return nil, fmt.Errorf("duplicate decision question %q", q.Name)
		}
		x := ir.DecisionQuestion{ID: q.Name, Type: q.Type, Instructions: q.Instructions}
		switch q.Type {
		case "predicate":
			x.Type = ir.DecisionKindNoul
		case "choice", "score":
			m := map[string]string{}
			opts := q.Choices
			if q.Type == "score" {
				opts = q.Levels
			}
			for _, o := range opts {
				k := o.Value
				if q.Type == "score" {
					k = o.Label
				}
				m[k] = o.Description
			}
			x.Criteria = m
		default:
			return nil, fmt.Errorf("unsupported decision question type %q", q.Type)
		}
		r.Questions[q.Name] = x
	}
	return r, nil
}
func DecodeOpenAIDecisionsResponse(body []byte, req *ir.DecisionRequest) (*ir.DecisionResponse, error) {
	var w decisionWireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	r := &ir.DecisionResponse{Model: w.Model, Answers: map[string]ir.DecisionAnswer{}, Usage: ExtractUsageJSON(protocol.ProtocolOpenAIDecisions, body), Mode: "native", Calibrated: true}
	for _, a := range w.Answers {
		if a.Type == "refusal" {
			return nil, fmt.Errorf("decision question %q refused", a.Name)
		}
		kind := a.Type
		if kind == "predicate" {
			kind = ir.DecisionKindNoul
		}
		if kind != ir.DecisionKindNoul && kind != ir.DecisionKindChoice && kind != ir.DecisionKindScore {
			return nil, fmt.Errorf("unknown answer type %q", a.Type)
		}
		r.Answers[a.Name] = ir.DecisionAnswer{Type: kind, Noul: a.Probability, Choice: a.Choice, Confidence: a.Confidence, Score: a.Score, Probabilities: a.Probabilities}
	}
	if req != nil {
		for k, q := range req.Questions {
			a, ok := r.Answers[k]
			if !ok {
				return nil, fmt.Errorf("missing decision answer %q", k)
			}
			if a.Type != q.Type {
				return nil, fmt.Errorf("decision answer %q has wrong type", k)
			}
		}
	}
	return r, nil
}
func EncodeOpenAIDecisionsResponse(r *ir.DecisionResponse) ([]byte, error) {
	if r == nil {
		return nil, fmt.Errorf("decision response is nil")
	}
	w := decisionWireResponse{Model: r.Model, Usage: map[string]any{"input_tokens": r.Usage.InputTokens, "output_tokens": r.Usage.OutputTokens, "total_tokens": r.Usage.Total(), "input_tokens_details": map[string]int64{"cached_tokens": r.Usage.CacheReadTokens, "cache_write_tokens": r.Usage.CacheWriteTokens}, "output_tokens_details": map[string]int64{"reasoning_tokens": r.Usage.ReasoningTokens}}}
	keys := make([]string, 0, len(r.Answers))
	for k := range r.Answers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := r.Answers[k]
		kind := a.Type
		if kind == ir.DecisionKindNoul {
			kind = "predicate"
		}
		w.Answers = append(w.Answers, decisionWireAnswer{Type: kind, Name: k, Probability: a.Noul, Choice: a.Choice, Score: a.Score, Confidence: a.Confidence, Probabilities: a.Probabilities})
	}
	return json.Marshal(w)
}
