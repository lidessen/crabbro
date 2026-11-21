package codec

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/shared"
)

// ExtractUsageJSON reads token counters from a complete non-streaming JSON body.
func ExtractUsageJSON(proto string, body []byte) shared.TokenUsage {
	if len(body) == 0 {
		return shared.TokenUsage{}
	}
	switch proto {
	case config.ProtocolOpenAIChat:
		var doc struct {
			Usage *oaChatUsage `json:"usage"`
		}
		if json.Unmarshal(body, &doc) == nil && doc.Usage != nil {
			return doc.Usage.toTokenUsage()
		}
	case config.ProtocolOpenAIResponses, config.ProtocolOpenAIDecisions:
		var doc struct {
			Usage *oaResponsesUsage `json:"usage"`
		}
		if json.Unmarshal(body, &doc) == nil && doc.Usage != nil {
			return doc.Usage.toTokenUsage()
		}
	case config.ProtocolAnthropic:
		var doc struct {
			Usage *anthUsage `json:"usage"`
		}
		if json.Unmarshal(body, &doc) == nil && doc.Usage != nil {
			return doc.Usage.toTokenUsage()
		}
	case config.ProtocolGemini:
		var doc struct {
			UsageMetadata *gemUsage `json:"usageMetadata"`
		}
		if json.Unmarshal(body, &doc) == nil && doc.UsageMetadata != nil {
			return doc.UsageMetadata.toTokenUsage()
		}
	case config.ProtocolTypeSafeJev:
		var doc struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &doc) == nil && doc.Usage != nil {
			return shared.TokenUsage{
				InputTokens:  doc.Usage.InputTokens,
				OutputTokens: doc.Usage.OutputTokens,
			}
		}
	}
	return shared.TokenUsage{}
}

// UsageTap is a non-blocking, passive line scanner that inspects streaming SSE
// bytes for token usage without buffering or delaying downstream writes.
type UsageTap struct {
	proto string
	buf   []byte
	event string
	data  []byte
	usage shared.TokenUsage
}

// NewUsageTap returns a UsageTap configured for the given upstream wire protocol.
func NewUsageTap(proto string) *UsageTap {
	return &UsageTap{proto: proto}
}

// Write feeds raw SSE bytes into the tap. It always returns len(p), nil.
func (t *UsageTap) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	for {
		idx := bytes.IndexByte(t.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(bytes.TrimRight(t.buf[:idx], "\r"))
		t.buf = t.buf[idx+1:]
		t.processLine(line)
	}
	return len(p), nil
}

// Usage returns the accumulated token counters tapped so far.
func (t *UsageTap) Usage() shared.TokenUsage {
	if len(t.data) > 0 {
		t.flushEvent()
	}
	return t.usage
}

func (t *UsageTap) processLine(line string) {
	if line == "" {
		t.flushEvent()
		return
	}
	if strings.HasPrefix(line, ":") {
		return
	}
	if strings.HasPrefix(line, "event:") {
		t.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		return
	}
	if strings.HasPrefix(line, "data:") {
		d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if len(t.data) > 0 {
			t.data = append(t.data, '\n')
		}
		t.data = append(t.data, d...)
	}
}

func (t *UsageTap) flushEvent() {
	if len(t.data) == 0 {
		t.event = ""
		return
	}
	payload := t.data
	ev := t.event
	t.data = nil
	t.event = ""
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return
	}

	switch t.proto {
	case config.ProtocolOpenAIChat:
		var doc struct {
			Usage *oaChatUsage `json:"usage"`
		}
		if json.Unmarshal(payload, &doc) == nil && doc.Usage != nil {
			u := doc.Usage.toTokenUsage()
			mergeMaxUsage(&t.usage, u)
		}
	case config.ProtocolOpenAIResponses, config.ProtocolOpenAIDecisions:
		var doc struct {
			Type     string `json:"type"`
			Response *struct {
				Usage *oaResponsesUsage `json:"usage"`
			} `json:"response"`
			Usage *oaResponsesUsage `json:"usage"`
		}
		if json.Unmarshal(payload, &doc) == nil {
			if doc.Response != nil && doc.Response.Usage != nil {
				mergeMaxUsage(&t.usage, doc.Response.Usage.toTokenUsage())
			} else if doc.Usage != nil {
				mergeMaxUsage(&t.usage, doc.Usage.toTokenUsage())
			}
		}
	case config.ProtocolAnthropic:
		var doc struct {
			Type    string `json:"type"`
			Message *struct {
				Usage *anthUsage `json:"usage"`
			} `json:"message"`
			Usage *anthUsage `json:"usage"`
		}
		if json.Unmarshal(payload, &doc) == nil {
			if doc.Message != nil && doc.Message.Usage != nil {
				mergeMaxUsage(&t.usage, doc.Message.Usage.toTokenUsage())
			}
			if doc.Usage != nil {
				mergeMaxUsage(&t.usage, doc.Usage.toTokenUsage())
			}
		} else if ev == "message_delta" {
			var u anthUsage
			if json.Unmarshal(payload, &u) == nil {
				mergeMaxUsage(&t.usage, u.toTokenUsage())
			}
		}
	case config.ProtocolGemini:
		var doc struct {
			UsageMetadata *gemUsage `json:"usageMetadata"`
		}
		if json.Unmarshal(payload, &doc) == nil && doc.UsageMetadata != nil {
			mergeMaxUsage(&t.usage, doc.UsageMetadata.toTokenUsage())
		}
	}
}

// DetectSSEError inspects the first substantive SSE frame for fake-200 upstream
// error payloads (rate limits, quota exhaustion, overload errors).
func DetectSSEError(proto, eventName string, data []byte) (code, msg string, isErr bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return "", "", false
	}
	var env struct {
		Type     string `json:"type"`
		Code     string `json:"code"`
		Message  string `json:"message"`
		Response *struct {
			Status string `json:"status"`
			Error  *struct {
				Code    any    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"response"`
		Error *struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		if eventName == "error" {
			return "stream_error", string(trimmed), true
		}
		return "", "", false
	}
	if env.Error != nil {
		c := env.Error.Type
		if c == "" && env.Error.Code != nil {
			c = toString(env.Error.Code)
		}
		if c == "" {
			c = env.Error.Status
		}
		return c, env.Error.Message, true
	}
	if env.Type == "error" || eventName == "error" {
		c := env.Code
		if c == "" {
			c = env.Type
		}
		return c, env.Message, true
	}
	if env.Type == "response.failed" || (env.Response != nil && env.Response.Status == "failed") {
		c, m := "response_failed", "upstream response failed"
		if env.Response != nil && env.Response.Error != nil {
			if env.Response.Error.Code != nil {
				c = toString(env.Response.Error.Code)
			}
			if env.Response.Error.Message != "" {
				m = env.Response.Error.Message
			}
		}
		return c, m, true
	}
	if proto == config.ProtocolOpenAIChat && len(env.Choices) > 0 && env.Choices[0].FinishReason == "error" {
		return "upstream_error", "choice finish_reason=error", true
	}
	return "", "", false
}

func mergeMaxUsage(dst *shared.TokenUsage, src shared.TokenUsage) {
	if src.InputTokens > dst.InputTokens {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > dst.OutputTokens {
		dst.OutputTokens = src.OutputTokens
	}
	if src.CacheReadTokens > dst.CacheReadTokens {
		dst.CacheReadTokens = src.CacheReadTokens
	}
	if src.CacheWriteTokens > dst.CacheWriteTokens {
		dst.CacheWriteTokens = src.CacheWriteTokens
	}
	if src.ReasoningTokens > dst.ReasoningTokens {
		dst.ReasoningTokens = src.ReasoningTokens
	}
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

type oaChatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u oaChatUsage) toTokenUsage() shared.TokenUsage {
	out := shared.TokenUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

type oaResponsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (u oaResponsesUsage) toTokenUsage() shared.TokenUsage {
	out := shared.TokenUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
	}
	if u.InputTokensDetails != nil {
		out.CacheReadTokens = u.InputTokensDetails.CachedTokens
		out.CacheWriteTokens = u.InputTokensDetails.CacheWriteTokens
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

type anthUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	OutputTokensDetails      *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

func (u anthUsage) toTokenUsage() shared.TokenUsage {
	out := shared.TokenUsage{
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ThinkingTokens
	}
	return out
}

type gemUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
}

func (u gemUsage) toTokenUsage() shared.TokenUsage {
	return shared.TokenUsage{
		InputTokens:     u.PromptTokenCount,
		OutputTokens:    u.CandidatesTokenCount,
		CacheReadTokens: u.CachedContentTokenCount,
		ReasoningTokens: u.ThoughtsTokenCount,
	}
}
