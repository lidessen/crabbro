package codec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
)

// PatchModelAndExtraBody performs a surgical top-level JSON patch on body:
// it rewrites "model" (except for Gemini where the model sits in the URL path)
// and injects absent defaults from extraBody, leaving all unknown vendor fields
// (cache_control, thinking, reasoning, etc.) intact.
func PatchModelAndExtraBody(body []byte, proto, targetModel string, extra map[string]any) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("invalid JSON object: %w", err)
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	if proto != config.ProtocolGemini && targetModel != "" {
		mb, err := json.Marshal(targetModel)
		if err != nil {
			return nil, err
		}
		obj["model"] = mb
	}
	for k, v := range extra {
		if _, exists := obj[k]; exists {
			continue
		}
		vb, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		obj[k] = vb
	}
	return json.Marshal(obj)
}

// BuildUpstreamURL resolves the upstream request URL from an endpoint's baseUrl,
// wire protocol, action, target model, and sanitized query string.
func BuildUpstreamURL(baseURL, proto, action, targetModel, cleanQuery string, stream bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid baseUrl %q", baseURL)
	}
	basePath := strings.TrimRight(u.Path, "/")
	hasNonRootPath := basePath != ""

	var suffix string
	switch proto {
	case config.ProtocolOpenAIChat:
		switch {
		case strings.HasSuffix(basePath, "/chat/completions"):
			suffix = basePath
		case hasNonRootPath:
			suffix = basePath + "/chat/completions"
		default:
			suffix = "/v1/chat/completions"
		}
	case config.ProtocolOpenAIResponses:
		switch {
		case strings.HasSuffix(basePath, "/responses"):
			suffix = basePath
		case hasNonRootPath:
			suffix = basePath + "/responses"
		default:
			suffix = "/v1/responses"
		}
	case config.ProtocolAnthropic:
		epTail := "/messages"
		if action == "count_tokens" {
			epTail = "/messages/count_tokens"
		}
		switch {
		case strings.HasSuffix(basePath, epTail):
			suffix = basePath
		case strings.HasSuffix(basePath, "/messages") && action == "count_tokens":
			suffix = basePath + "/count_tokens"
		case strings.HasSuffix(basePath, "/v1"):
			suffix = basePath + epTail
		default:
			suffix = basePath + "/v1" + epTail
		}
	case config.ProtocolGemini:
		gemAction := action
		if gemAction != "generateContent" && gemAction != "streamGenerateContent" {
			if stream {
				gemAction = "streamGenerateContent"
			} else {
				gemAction = "generateContent"
			}
		}
		cleanModel := strings.TrimPrefix(targetModel, "models/")
		modelPath := "/models/" + url.PathEscape(cleanModel) + ":" + gemAction
		switch {
		case strings.HasSuffix(basePath, "/models"):
			suffix = basePath + "/" + url.PathEscape(cleanModel) + ":" + gemAction
		case hasNonRootPath:
			suffix = basePath + modelPath
		default:
			suffix = "/v1beta" + modelPath
		}
	case config.ProtocolTypeSafeJev:
		switch {
		case strings.HasSuffix(basePath, "/systemone"):
			suffix = basePath
		case hasNonRootPath:
			suffix = basePath + "/systemone"
		default:
			suffix = "/v1/systemone"
		}
	default:
		return "", fmt.Errorf("unsupported protocol %q", proto)
	}

	u.Path = suffix
	q, _ := url.ParseQuery(cleanQuery)
	if u.RawQuery != "" {
		// Merge any base query parameters without overwriting sanitized client params.
		bq, _ := url.ParseQuery(u.RawQuery)
		for k, vs := range bq {
			if !q.Has(k) {
				q[k] = vs
			}
		}
	}
	if proto == config.ProtocolGemini && stream && !q.Has("alt") {
		q.Set("alt", "sse")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// InjectUpstreamAuth strips client auth headers, writes the plan credential using
// endpoint overrides or protocol defaults, and applies static endpoint headers.
func InjectUpstreamAuth(h http.Header, ep *config.EndpointSpec, proto, secretVal string) {
	if h == nil {
		return
	}
	extraStrip := ""
	if ep != nil && ep.AuthHeader != "" {
		extraStrip = ep.AuthHeader
	}
	cleaned := secret.StripAuthHeaders(h, extraStrip)
	for k := range h {
		delete(h, k)
	}
	for k, vs := range cleaned {
		h[k] = vs
	}

	headerName := "Authorization"
	prefix := "Bearer "
	switch proto {
	case config.ProtocolAnthropic:
		headerName = "x-api-key"
		prefix = ""
	case config.ProtocolGemini:
		headerName = "x-goog-api-key"
		prefix = ""
	}
	if ep != nil && ep.AuthHeader != "" {
		headerName = ep.AuthHeader
		prefix = ep.AuthPrefix
	} else if ep != nil && ep.AuthPrefix != "" {
		prefix = ep.AuthPrefix
	}
	if secretVal != "" {
		h.Set(headerName, prefix+secretVal)
	}

	if proto == config.ProtocolAnthropic {
		ver := "2023-06-01"
		if ep != nil && ep.APIVersion != "" {
			ver = ep.APIVersion
		}
		if h.Get("anthropic-version") == "" {
			h.Set("anthropic-version", ver)
		}
	}
	if ep != nil {
		for k, v := range ep.Headers {
			h.Set(k, v)
		}
	}
}
