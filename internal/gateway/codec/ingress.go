// Package codec implements wire-protocol parsing, same-protocol Fast Path
// patching, streaming UsageTap/error detection, and cross-protocol IR bridges
// across openai-chat, openai-responses, anthropic, gemini, and typesafe-jev.
package codec

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
)

var profilePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// IngressInfo describes the normalized wire metadata of an incoming gateway call.
type IngressInfo struct {
	Profile        string
	Protocol       string // openai-chat | openai-responses | anthropic | gemini | typesafe-jev | models
	Family         string // generative | decision
	Action         string // chat | responses | messages | count_tokens | generateContent | streamGenerateContent | systemone | models
	RequestedModel string
	Stream         bool
	SessionHint    string
	CleanPath      string
	CleanQuery     string
}

// ParseIngress parses the HTTP path (including optional /p/{profile} prefix),
// query parameters, headers, and top-level JSON metadata into IngressInfo.
func ParseIngress(r *http.Request, body []byte) (IngressInfo, error) {
	if r == nil || r.URL == nil {
		return IngressInfo{}, fmt.Errorf("missing request URL")
	}
	rawPath := r.URL.Path
	var profile string
	cleanPath := rawPath
	if strings.HasPrefix(rawPath, "/p/") {
		rest := strings.TrimPrefix(rawPath, "/p/")
		idx := strings.IndexByte(rest, '/')
		if idx <= 0 {
			return IngressInfo{}, fmt.Errorf("invalid profile path %q", rawPath)
		}
		profile = rest[:idx]
		if !profilePattern.MatchString(profile) {
			return IngressInfo{}, fmt.Errorf("invalid profile name %q", profile)
		}
		cleanPath = rest[idx:]
	}

	// Scrub sensitive query parameters (e.g. Gemini ?key=...).
	cleanQuery := scrubRawQuery(r.URL.RawQuery)

	info := IngressInfo{
		Profile:     profile,
		CleanPath:   cleanPath,
		CleanQuery:  cleanQuery,
		SessionHint: strings.TrimSpace(r.Header.Get("X-Crabbro-Session")),
	}

	if r.Method == http.MethodGet && cleanPath == "/v1/models" {
		info.Protocol = "models"
		info.Action = "models"
		return info, nil
	}
	if r.Method != http.MethodPost {
		return IngressInfo{}, fmt.Errorf("unsupported method %s for %s", r.Method, cleanPath)
	}

	switch {
	case cleanPath == "/v1/chat/completions":
		info.Protocol = config.ProtocolOpenAIChat
		info.Family = config.FamilyGenerative
		info.Action = "chat"
	case cleanPath == "/v1/responses":
		info.Protocol = config.ProtocolOpenAIResponses
		info.Family = config.FamilyGenerative
		info.Action = "responses"
	case cleanPath == "/v1/messages":
		info.Protocol = config.ProtocolAnthropic
		info.Family = config.FamilyGenerative
		info.Action = "messages"
	case cleanPath == "/v1/messages/count_tokens":
		info.Protocol = config.ProtocolAnthropic
		info.Family = config.FamilyGenerative
		info.Action = "count_tokens"
	case cleanPath == "/v1/systemone":
		info.Protocol = config.ProtocolTypeSafeJev
		info.Family = config.FamilyDecision
		info.Action = "systemone"
	case strings.HasPrefix(cleanPath, "/v1beta/models/"):
		tail := strings.TrimPrefix(cleanPath, "/v1beta/models/")
		colon := strings.LastIndexByte(tail, ':')
		if colon <= 0 || colon >= len(tail)-1 {
			return IngressInfo{}, fmt.Errorf("invalid gemini endpoint %q", cleanPath)
		}
		info.Protocol = config.ProtocolGemini
		info.Family = config.FamilyGenerative
		info.RequestedModel = tail[:colon]
		info.Action = tail[colon+1:]
		switch info.Action {
		case "generateContent":
			if q, _ := url.ParseQuery(cleanQuery); q.Get("alt") == "sse" {
				info.Stream = true
			}
		case "streamGenerateContent":
			info.Stream = true
		default:
			return IngressInfo{}, fmt.Errorf("unsupported gemini action %q", info.Action)
		}
	default:
		return IngressInfo{}, fmt.Errorf("unknown gateway endpoint %q", cleanPath)
	}

	if len(body) > 0 {
		var meta struct {
			Model              string `json:"model"`
			Stream             bool   `json:"stream"`
			PromptCacheKey     string `json:"prompt_cache_key"`
			PreviousResponseID string `json:"previous_response_id"`
			Metadata           struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(body, &meta); err != nil {
			return IngressInfo{}, fmt.Errorf("invalid JSON request body: %w", err)
		}
		if info.RequestedModel == "" {
			info.RequestedModel = strings.TrimSpace(meta.Model)
		}
		if meta.Stream {
			info.Stream = true
		}
		if info.SessionHint == "" {
			switch {
			case strings.TrimSpace(meta.Metadata.UserID) != "":
				info.SessionHint = strings.TrimSpace(meta.Metadata.UserID)
			case strings.TrimSpace(meta.PromptCacheKey) != "":
				info.SessionHint = strings.TrimSpace(meta.PromptCacheKey)
			case strings.TrimSpace(meta.PreviousResponseID) != "":
				info.SessionHint = strings.TrimSpace(meta.PreviousResponseID)
			}
		}
	}
	return info, nil
}

// ScrubbedDisplayURL returns a safe URL string with sensitive query params removed.
func ScrubbedDisplayURL(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	raw := scheme + "://" + host + r.URL.RequestURI()
	return secret.ScrubURL(raw)
}

func scrubRawQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	for k := range q {
		switch strings.ToLower(k) {
		case "key", "api_key", "apikey", "access_token":
			q.Del(k)
		}
	}
	return q.Encode()
}
