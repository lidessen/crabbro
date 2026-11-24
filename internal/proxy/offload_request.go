package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
)

// buildOffloadRequest constructs a gateway ingress request without inheriting
// the subscription request's URL, headers, host, or transport metadata.
// Only protocol fields explicitly listed below are retained.
func buildOffloadRequest(src *http.Request, adapter, model string, maxBody int64) (*http.Request, error) {
	if src == nil || src.Method != http.MethodPost || src.Body == nil || maxBody <= 0 {
		return nil, errors.New("invalid offload request")
	}
	if enc := strings.TrimSpace(src.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, errors.New("unsupported request content encoding")
	}
	if strings.TrimSpace(model) == "" || strings.TrimSpace(model) != model {
		return nil, errors.New("invalid offload model")
	}
	var path string
	var allowed []string
	switch adapter {
	case "responses":
		path = "/v1/responses"
		allowed = []string{"input", "instructions", "tools", "tool_choice", "parallel_tool_calls", "text", "reasoning", "max_output_tokens", "temperature", "top_p", "stream", "truncation"}
	case "anthropic-messages":
		path = "/v1/messages"
		allowed = []string{"messages", "system", "max_tokens", "tools", "tool_choice", "temperature", "top_p", "top_k", "stop_sequences", "stream", "thinking"}
	default:
		return nil, errors.New("unsupported offload adapter")
	}
	raw, err := io.ReadAll(io.LimitReader(src.Body, maxBody+1))
	if err != nil {
		return nil, errors.New("failed reading offload body")
	}
	if int64(len(raw)) > maxBody {
		return nil, errors.New("offload body exceeds limit")
	}
	var source map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&source); err != nil || source == nil {
		return nil, errors.New("invalid offload JSON object")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid offload JSON object")
	}
	// Values in discarded fields can also be repeated in otherwise permitted
	// fields. Remove those copies as well as the fields themselves.
	secrets := make(map[string]struct{})
	for _, values := range src.Header {
		for _, value := range values {
			addOffloadSecret(secrets, value)
			if fields := strings.Fields(value); len(fields) == 2 && strings.EqualFold(fields[0], "bearer") {
				addOffloadSecret(secrets, fields[1])
			}
		}
	}
	if src.URL != nil {
		for _, values := range src.URL.Query() {
			for _, value := range values {
				addOffloadSecret(secrets, value)
			}
		}
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = true
	}
	for name, value := range source {
		if !allowedSet[name] {
			if sensitiveOffloadKey(name) || name == "model" {
				collectOffloadSecrets(secrets, value)
			}
		} else {
			collectNestedOffloadSecrets(secrets, value)
		}
	}
	values := make([]string, 0, len(secrets))
	for value := range secrets {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	clean := make(map[string]any, len(allowed)+1)
	for _, name := range allowed {
		if value, ok := source[name]; ok {
			clean[name] = cleanOffloadValue(value, values)
		}
	}
	clean["model"] = model
	body, err := json.Marshal(clean)
	if err != nil {
		return nil, errors.New("invalid offload JSON fields")
	}
	if int64(len(body)) > maxBody {
		return nil, errors.New("offload body exceeds limit")
	}
	req, err := http.NewRequestWithContext(src.Context(), http.MethodPost, "http://gateway.internal"+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("failed creating offload request")
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func addOffloadSecret(secrets map[string]struct{}, value string) {
	if value != "" {
		secrets[value] = struct{}{}
	}
}

func collectOffloadSecrets(secrets map[string]struct{}, value any) {
	switch v := value.(type) {
	case string:
		addOffloadSecret(secrets, v)
	case []any:
		for _, item := range v {
			collectOffloadSecrets(secrets, item)
		}
	case map[string]any:
		for _, item := range v {
			collectOffloadSecrets(secrets, item)
		}
	}
}

func sensitiveOffloadKey(key string) bool {
	key = strings.ToLower(key)
	key = strings.NewReplacer("_", "", "-", "", ".", "").Replace(key)
	if key == "maxtokens" || key == "maxoutputtokens" {
		return false
	}
	for _, fragment := range []string{"metadata", "subscription", "account", "credential", "authorization", "apikey", "token", "cookie", "secret", "password"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func collectNestedOffloadSecrets(secrets map[string]struct{}, value any) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			collectNestedOffloadSecrets(secrets, item)
		}
	case map[string]any:
		for key, item := range v {
			if sensitiveOffloadKey(key) {
				collectOffloadSecrets(secrets, item)
			} else {
				collectNestedOffloadSecrets(secrets, item)
			}
		}
	}
}

func cleanOffloadValue(value any, secrets []string) any {
	switch v := value.(type) {
	case string:
		for _, secret := range secrets {
			v = strings.ReplaceAll(v, secret, "[REDACTED]")
		}
		return v
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cleanOffloadValue(item, secrets)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if !sensitiveOffloadKey(key) {
				cleanKey := cleanOffloadValue(key, secrets).(string)
				out[cleanKey] = cleanOffloadValue(item, secrets)
			}
		}
		return out
	default:
		return value
	}
}
