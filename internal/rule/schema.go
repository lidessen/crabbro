package rule

import (
	"fmt"
	"net/url"

	"github.com/lidessen/chiling/internal/shared"
)

// MaxMockResponseBodyLen is the maximum allowed size for a mockResponse body.
const MaxMockResponseBodyLen = 64 * 1024

// MaxSerializedResultLen is the maximum allowed size for the serialized rule
// result (the return value of the default function).
const MaxSerializedResultLen = 64 * 1024

// ruleOutput is the deserialized result of a rule evaluation.
type ruleOutput struct {
	Match   bool         `json:"match"`
	Actions []ruleAction `json:"actions,omitempty"`
}

type ruleAction struct {
	Type    string         `json:"type"`
	Params  map[string]any `json:"params,omitempty"`
	Status  any            `json:"status,omitempty"`
	Headers map[string]any `json:"headers,omitempty"`
	Body    any            `json:"body,omitempty"`
	Tags    any            `json:"tags,omitempty"`
	Name    any            `json:"name,omitempty"`
	Value   any            `json:"value,omitempty"`
	URL     any            `json:"url,omitempty"`
}

// liftFlatParams copies design-doc flat action fields into params when the
// script did not nest them. Both shapes are accepted.
func (a *ruleAction) liftFlatParams() {
	if a.Params == nil {
		a.Params = map[string]any{}
	}
	lift := func(key string, v any) {
		if v == nil {
			return
		}
		if _, exists := a.Params[key]; exists {
			return
		}
		a.Params[key] = v
	}
	lift("status", a.Status)
	if len(a.Headers) > 0 {
		lift("headers", a.Headers)
	}
	lift("body", a.Body)
	lift("tags", a.Tags)
	lift("name", a.Name)
	lift("value", a.Value)
	lift("url", a.URL)
}

// ValidateOutput validates a rule evaluation result and converts it into
// validated shared.RuleAction values. It checks:
//   - match is present and boolean
//   - if match=true, actions is present and non-empty
//   - each action has a valid type
//   - per-type parameter validation as defined in the design doc
func ValidateOutput(out ruleOutput) ([]shared.RuleAction, []string) {
	var errs []string

	if !out.Match {
		return nil, nil
	}

	if len(out.Actions) == 0 {
		return nil, []string{"match is true but no actions provided"}
	}

	validated := make([]shared.RuleAction, 0, len(out.Actions))

	for i, a := range out.Actions {
		a.liftFlatParams()
		v, err := validateAction(a)
		if err != nil {
			errs = append(errs, fmt.Sprintf("actions[%d]: %v", i, err))
			continue
		}
		validated = append(validated, v)
	}

	// All-or-nothing: a single invalid action discards the whole result.
	if len(errs) > 0 {
		return nil, errs
	}

	terminals := 0
	for _, a := range validated {
		switch a.Type {
		case shared.ActionBlock, shared.ActionMockResponse, shared.ActionAbort:
			terminals++
		}
	}
	if terminals > 1 {
		return nil, []string{"multiple terminal actions (block, mockResponse, abort); none applied"}
	}

	return validated, nil
}

func validateAction(a ruleAction) (shared.RuleAction, error) {
	actionType := shared.RuleActionType(a.Type)

	switch actionType {
	case shared.ActionTag:
		return validateTag(a.Params)
	case shared.ActionBlock:
		return validateBlock(a.Params)
	case shared.ActionMockResponse:
		return validateMockResponse(a.Params)
	case shared.ActionSetRequestHeader:
		return validateSetRequestHeader(a.Params)
	case shared.ActionRemoveRequestHeader:
		return validateRemoveRequestHeader(a.Params)
	case shared.ActionAbort:
		return validateAbort(a.Params)
	case shared.ActionRewriteURL:
		return validateRewriteURL(a.Params)
	default:
		return shared.RuleAction{}, fmt.Errorf("unknown action type %q", a.Type)
	}
}

func validateTag(params map[string]any) (shared.RuleAction, error) {
	tagsRaw, ok := params["tags"]
	if !ok {
		return shared.RuleAction{}, fmt.Errorf("tag action requires params.tags")
	}
	tagsIface, ok := tagsRaw.([]any)
	if !ok {
		return shared.RuleAction{}, fmt.Errorf("tag action params.tags must be an array of strings")
	}
	if len(tagsIface) == 0 {
		return shared.RuleAction{}, fmt.Errorf("tag action params.tags must not be empty")
	}
	tags := make([]string, 0, len(tagsIface))
	for _, t := range tagsIface {
		s, ok := t.(string)
		if !ok {
			return shared.RuleAction{}, fmt.Errorf("tag action params.tags values must be strings")
		}
		tags = append(tags, s)
	}
	return shared.RuleAction{
		Type: shared.ActionTag,
		Params: map[string]any{
			"tags": tags,
		},
	}, nil
}

func validateBlock(params map[string]any) (shared.RuleAction, error) {
	status, err := extractIntParam(params, "status", 100, 599)
	if err != nil {
		return shared.RuleAction{}, fmt.Errorf("block action: %v", err)
	}

	result := shared.RuleAction{
		Type: shared.ActionBlock,
		Params: map[string]any{
			"status": status,
		},
	}

	if body, ok := params["body"]; ok {
		s, ok := body.(string)
		if !ok {
			return shared.RuleAction{}, fmt.Errorf("block action params.body must be a string")
		}
		result.Params["body"] = s
	}

	return result, nil
}

func validateMockResponse(params map[string]any) (shared.RuleAction, error) {
	status, err := extractIntParam(params, "status", 100, 599)
	if err != nil {
		return shared.RuleAction{}, fmt.Errorf("mockResponse action: %v", err)
	}

	result := shared.RuleAction{
		Type: shared.ActionMockResponse,
		Params: map[string]any{
			"status": status,
		},
	}

	if headers, ok := params["headers"]; ok {
		hMap, ok := headers.(map[string]any)
		if !ok {
			return shared.RuleAction{}, fmt.Errorf("mockResponse action params.headers must be an object with string values")
		}
		validHeaders := make(map[string]string, len(hMap))
		for k, v := range hMap {
			s, ok := v.(string)
			if !ok {
				return shared.RuleAction{}, fmt.Errorf("mockResponse action params.headers values must be strings")
			}
			validHeaders[k] = s
		}
		result.Params["headers"] = validHeaders
	}

	if body, ok := params["body"]; ok {
		s, ok := body.(string)
		if !ok {
			return shared.RuleAction{}, fmt.Errorf("mockResponse action params.body must be a string")
		}
		if len(s) > MaxMockResponseBodyLen {
			return shared.RuleAction{}, fmt.Errorf("mockResponse action params.body exceeds max size (%d bytes)", MaxMockResponseBodyLen)
		}
		result.Params["body"] = s
	}

	return result, nil
}

func validateSetRequestHeader(params map[string]any) (shared.RuleAction, error) {
	name, err := extractStringParam(params, "name")
	if err != nil {
		return shared.RuleAction{}, fmt.Errorf("setRequestHeader action: %v", err)
	}
	if name == "" {
		return shared.RuleAction{}, fmt.Errorf("setRequestHeader action params.name must not be empty")
	}

	value, err := extractStringParam(params, "value")
	if err != nil {
		return shared.RuleAction{}, fmt.Errorf("setRequestHeader action: %v", err)
	}

	return shared.RuleAction{
		Type: shared.ActionSetRequestHeader,
		Params: map[string]any{
			"name":  name,
			"value": value,
		},
	}, nil
}

func validateRemoveRequestHeader(params map[string]any) (shared.RuleAction, error) {
	name, err := extractStringParam(params, "name")
	if err != nil {
		return shared.RuleAction{}, fmt.Errorf("removeRequestHeader action: %v", err)
	}
	if name == "" {
		return shared.RuleAction{}, fmt.Errorf("removeRequestHeader action params.name must not be empty")
	}

	return shared.RuleAction{
		Type: shared.ActionRemoveRequestHeader,
		Params: map[string]any{
			"name": name,
		},
	}, nil
}

func validateAbort(params map[string]any) (shared.RuleAction, error) {
	return shared.RuleAction{
		Type:   shared.ActionAbort,
		Params: nil,
	}, nil
}

func validateRewriteURL(params map[string]any) (shared.RuleAction, error) {
	urlStr, err := extractStringParam(params, "url")
	if err != nil {
		return shared.RuleAction{}, fmt.Errorf("rewriteURL action: %v", err)
	}
	parsed, err := url.Parse(urlStr)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return shared.RuleAction{}, fmt.Errorf("rewriteURL action params.url must be a valid HTTP(S) URL")
	}
	if parsed.User != nil {
		return shared.RuleAction{}, fmt.Errorf("rewriteURL action params.url must not include userinfo")
	}
	return shared.RuleAction{
		Type: shared.ActionRewriteURL,
		Params: map[string]any{
			"url": urlStr,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Validation helpers
// ---------------------------------------------------------------------------

func extractIntParam(params map[string]any, key string, min, max int) (int, error) {
	raw, ok := params[key]
	if !ok {
		return 0, fmt.Errorf("missing required param %q", key)
	}
	f, ok := toFloat64(raw)
	if !ok {
		return 0, fmt.Errorf("param %q must be a number", key)
	}
	v := int(f)
	if v < min || v > max {
		return 0, fmt.Errorf("param %q=%d out of range [%d, %d]", key, v, min, max)
	}
	return v, nil
}

func extractStringParam(params map[string]any, key string) (string, error) {
	raw, ok := params[key]
	if !ok {
		return "", fmt.Errorf("missing required param %q", key)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("param %q must be a string", key)
	}
	return s, nil
}

// toFloat64 converts a JSON-number-like value to float64 for range checking.
// Goja returns int64 for small integers and float64 for floats, so we need
// to handle both.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}
