package codec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
)

type geminiDecisionQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// geminiDecisionOptions preserves slice order and sorts map keys, matching the
// ordering used by the OpenAI decision codec for map-based criteria.
func geminiDecisionOptions(criteria any) []decisionOption {
	switch v := criteria.(type) {
	case map[string]string, []string:
		return decisionOptions(v, true)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		result := make([]decisionOption, 0, len(keys))
		for _, k := range keys {
			desc := anyToString(v[k])
			if obj, ok := v[k].(map[string]any); ok {
				desc = firstNonEmpty(anyToString(obj["description"]), anyToString(obj["label"]), anyToString(obj["value"]))
			}
			result = append(result, decisionOption{Label: k, Description: desc})
		}
		return result
	}
	b, err := json.Marshal(criteria)
	if err != nil {
		return nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(b, &entries) != nil {
		return nil
	}
	var result []decisionOption
	for _, entry := range entries {
		var opt decisionOption
		if json.Unmarshal(entry, &opt) == nil {
			result = append(result, opt)
		}
	}
	return result
}

func EncodeGeminiDecisionRequest(req *ir.DecisionRequest, model string) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("decision request is nil")
	}
	text, image, hasImage, err := decisionImage(req)
	if err != nil {
		return nil, err
	}
	input, ok := req.State.(string)
	if !ok {
		b, err := json.Marshal(req.State)
		if err != nil {
			return nil, err
		}
		input = string(b)
	}
	var wireInput any = input
	if hasImage {
		wireInput = []any{map[string]any{"type": "text", "text": text}, map[string]any{"type": "image", "data": image, "mime_type": "image/png"}}
	}
	questions := make(map[string]geminiDecisionQuestion, len(req.Questions))
	for name, q := range req.Questions {
		w := geminiDecisionQuestion{Type: q.Type, Instructions: q.Instructions}
		switch q.Type {
		case ir.DecisionKindNoul:
		case ir.DecisionKindChoice:
			choices := map[string]string{}
			for _, o := range geminiDecisionOptions(q.Criteria) {
				key := firstNonEmpty(o.Value, o.Label)
				choices[key] = firstNonEmpty(o.Description, key)
			}
			w.Criteria = choices
		case ir.DecisionKindScore:
			levels := []string{}
			for _, o := range geminiDecisionOptions(q.Criteria) {
				levels = append(levels, firstNonEmpty(o.Description, o.Label, o.Value))
			}
			w.Criteria = levels
		default:
			return nil, fmt.Errorf("unsupported decision question type %q", q.Type)
		}
		questions[name] = w
	}
	return json.Marshal(struct {
		Model     string                            `json:"model"`
		Input     any                               `json:"input"`
		Questions map[string]geminiDecisionQuestion `json:"questions"`
	}{model, wireInput, questions})
}

func DecodeGeminiDecisionResponse(body []byte, orig *ir.DecisionRequest, model string) (*ir.DecisionResponse, error) {
	var wire struct {
		Answers map[string]struct {
			Type          string             `json:"type"`
			Noul          *float64           `json:"noul"`
			Choice        string             `json:"choice"`
			Score         *float64           `json:"score"`
			Confidence    *float64           `json:"confidence"`
			Probabilities map[string]float64 `json:"probabilities"`
			Legend        map[string]string  `json:"legend"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, err
	}
	result := &ir.DecisionResponse{Model: model, Answers: map[string]ir.DecisionAnswer{}, Usage: ExtractUsageJSON(config.ProtocolGeminiDecisions, body), Mode: "native", Calibrated: true}
	for name, a := range wire.Answers {
		if a.Type != ir.DecisionKindNoul && a.Type != ir.DecisionKindChoice && a.Type != ir.DecisionKindScore {
			return nil, fmt.Errorf("unknown answer type %q", a.Type)
		}
		probs := a.Probabilities
		if a.Type == ir.DecisionKindScore && probs != nil {
			labels := []string{}
			if orig != nil {
				if q, ok := orig.Questions[name]; ok {
					for _, o := range geminiDecisionOptions(q.Criteria) {
						labels = append(labels, firstNonEmpty(o.Label, o.Value, o.Description))
					}
				}
			}
			mapped := make(map[string]float64, len(probs))
			keys := make([]string, 0, len(probs))
			for k := range probs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				label := a.Legend[k]
				if i, err := strconv.Atoi(k); err == nil && i >= 0 && i < len(labels) && labels[i] != "" {
					label = labels[i]
				}
				if label == "" {
					label = k
				}
				mapped[label] = probs[k]
			}
			probs = mapped
		}
		result.Answers[name] = ir.DecisionAnswer{Type: a.Type, Noul: a.Noul, Choice: a.Choice, Score: a.Score, Confidence: a.Confidence, Probabilities: probs}
	}
	if orig != nil {
		for k, q := range orig.Questions {
			a, ok := result.Answers[k]
			if !ok {
				return nil, fmt.Errorf("missing decision answer %q", k)
			}
			if a.Type != q.Type {
				return nil, fmt.Errorf("decision answer %q has wrong type", k)
			}
		}
	}
	return result, nil
}
