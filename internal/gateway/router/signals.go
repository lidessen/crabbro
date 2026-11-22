package router

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/config"
)

const (
	PhaseDecisionCall = "DecisionCall"
	PhaseAuxSubcall   = "AuxSubcall"
	PhaseToolLoop     = "ToolLoop"
	PhaseColdStart    = "ColdStart"
	PhaseNewUserTurn  = "NewUserTurn"
)

// Signals contains zero-latency deterministic attributes extracted from an incoming request.
type Signals struct {
	Profile              string
	RouteOverride        string
	Protocol             string
	Family               string
	RequestedModel       string
	SessionKey           string
	TurnPhase            string
	MessageCount         int
	RequestedTier        string
	EstimatedInputTokens int
	NeedTools            bool
	NeedVision           bool
	NeedThinking         bool
	NeedStructuredOutput bool
	SummaryText          string
}

// ExtractSignals inspects headers and JSON payload bytes without calling any external model.
func ExtractSignals(profile, proto, family, requestedModel, sessionHint string, body []byte) Signals {
	sig := Signals{
		Profile:              profile,
		Protocol:             proto,
		Family:               family,
		RequestedModel:       requestedModel,
		RequestedTier:        inferTierFromModel(requestedModel),
		EstimatedInputTokens: max(1, int(float64(len(body))/3.5)),
	}
	if family == config.FamilyDecision || proto == config.ProtocolTypeSafeJev {
		sig.TurnPhase = PhaseDecisionCall
		sig.NeedStructuredOutput = true
		if sessionHint != "" {
			sig.SessionKey = sessionHint
		}
		return sig
	}

	var doc map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &doc)
	}

	var sysPrefix, firstUser, lastUser string
	var isToolLoop bool

	switch proto {
	case config.ProtocolAnthropic:
		sig.MessageCount, sysPrefix, firstUser, lastUser, isToolLoop = inspectAnthropic(doc, &sig)
	case config.ProtocolOpenAIChat:
		sig.MessageCount, sysPrefix, firstUser, lastUser, isToolLoop = inspectOpenAIChat(doc, &sig)
	case config.ProtocolOpenAIResponses:
		sig.MessageCount, sysPrefix, firstUser, lastUser, isToolLoop = inspectOpenAIResponses(doc, &sig)
	case config.ProtocolGemini:
		sig.MessageCount, sysPrefix, firstUser, lastUser, isToolLoop = inspectGemini(doc, &sig)
	}

	sig.SessionKey = deriveSessionKey(profile, proto, sessionHint, sysPrefix, firstUser)

	switch {
	case isToolLoop:
		sig.TurnPhase = PhaseToolLoop
	case !sig.NeedTools && sig.EstimatedInputTokens < 4000 && (sig.RequestedTier == config.TierFast || looksAuxPrompt(lastUser)):
		sig.TurnPhase = PhaseAuxSubcall
	case sig.MessageCount <= 1:
		sig.TurnPhase = PhaseColdStart
	default:
		sig.TurnPhase = PhaseNewUserTurn
	}

	sig.SummaryText = buildSummary(sysPrefix, lastUser, sig.NeedTools, sig.EstimatedInputTokens)
	return sig
}

func deriveSessionKey(profile, proto, hint, sysPrefix, firstUser string) string {
	if h := strings.TrimSpace(hint); h != "" {
		if len(h) <= 48 && !strings.ContainsAny(h, " \t\n") {
			return h
		}
		sum := sha256.Sum256([]byte(profile + "|" + proto + "|" + h))
		return "ses_" + hex.EncodeToString(sum[:8])
	}
	if sysPrefix == "" && firstUser == "" {
		return ""
	}
	if len(sysPrefix) > 1024 {
		sysPrefix = sysPrefix[:1024]
	}
	if len(firstUser) > 512 {
		firstUser = firstUser[:512]
	}
	sum := sha256.Sum256([]byte(profile + "|" + proto + "|" + sysPrefix + "|" + firstUser))
	return "ses_" + hex.EncodeToString(sum[:8])
}

func inferTierFromModel(model string) string {
	// Strip "gemini" before checking for "mini" so "gemini-*-pro" does not match as mini.
	m := strings.ReplaceAll(strings.ToLower(NormalizeModelName(model)), "gemini", "")
	switch {
	case strings.Contains(m, "haiku"), strings.Contains(m, "mini"), strings.Contains(m, "flash"), strings.Contains(m, "lite"), strings.Contains(m, "nano"):
		return config.TierFast
	case strings.Contains(m, "opus"), strings.Contains(m, "frontier"), strings.Contains(m, "max"), strings.Contains(m, "pro-high"):
		return config.TierFrontier
	default:
		return config.TierStrong
	}
}

func looksAuxPrompt(text string) bool {
	l := strings.ToLower(text)
	return strings.Contains(l, "summarize") ||
		strings.Contains(l, "write a concise title") ||
		strings.Contains(l, "generate a title") ||
		strings.Contains(l, "conversation title")
}

func buildSummary(sysPrefix, lastUser string, needTools bool, estTokens int) string {
	if len(sysPrefix) > 512 {
		sysPrefix = sysPrefix[:512]
	}
	if len(lastUser) > 1200 {
		lastUser = lastUser[:1200]
	}
	s := fmt.Sprintf("tools=%t estTokens=%d\nsystem: %s\nuser: %s", needTools, estTokens, sysPrefix, lastUser)
	if len(s) > 2048 {
		s = s[:2048]
	}
	return s
}

func inspectAnthropic(doc map[string]any, sig *Signals) (msgCount int, sysPrefix, firstUser, lastUser string, toolLoop bool) {
	if doc == nil {
		return
	}
	if tools, ok := doc["tools"].([]any); ok && len(tools) > 0 {
		sig.NeedTools = true
	}
	if _, ok := doc["thinking"]; ok {
		sig.NeedThinking = true
	}
	if sys := doc["system"]; sys != nil {
		sysPrefix = extractTextFromAny(sys)
	}
	msgs, _ := doc["messages"].([]any)
	msgCount = len(msgs)
	for i, raw := range msgs {
		m, _ := raw.(map[string]any)
		role, _ := m["role"].(string)
		content := m["content"]
		if hasImageBlock(content) {
			sig.NeedVision = true
		}
		txt := extractTextFromAny(content)
		if role == "user" {
			if firstUser == "" {
				firstUser = txt
			}
			lastUser = txt
		}
		if i == len(msgs)-1 && hasAnthropicToolResult(content) {
			toolLoop = true
		}
	}
	return
}

func inspectOpenAIChat(doc map[string]any, sig *Signals) (msgCount int, sysPrefix, firstUser, lastUser string, toolLoop bool) {
	if doc == nil {
		return
	}
	if tools, ok := doc["tools"].([]any); ok && len(tools) > 0 {
		sig.NeedTools = true
	}
	if _, ok := doc["reasoning_effort"]; ok {
		sig.NeedThinking = true
	}
	if _, ok := doc["response_format"]; ok {
		sig.NeedStructuredOutput = true
	}
	msgs, _ := doc["messages"].([]any)
	msgCount = len(msgs)
	for i, raw := range msgs {
		m, _ := raw.(map[string]any)
		role, _ := m["role"].(string)
		content := m["content"]
		if hasImageBlock(content) {
			sig.NeedVision = true
		}
		txt := extractTextFromAny(content)
		switch role {
		case "system", "developer":
			if sysPrefix == "" {
				sysPrefix = txt
			}
		case "user":
			if firstUser == "" {
				firstUser = txt
			}
			lastUser = txt
		}
		if i == len(msgs)-1 && role == "tool" {
			toolLoop = true
		}
	}
	return
}

func inspectOpenAIResponses(doc map[string]any, sig *Signals) (msgCount int, sysPrefix, firstUser, lastUser string, toolLoop bool) {
	if doc == nil {
		return
	}
	if tools, ok := doc["tools"].([]any); ok && len(tools) > 0 {
		sig.NeedTools = true
	}
	if _, ok := doc["reasoning"]; ok {
		sig.NeedThinking = true
	}
	if textCfg, ok := doc["text"].(map[string]any); ok {
		if _, ok := textCfg["format"]; ok {
			sig.NeedStructuredOutput = true
		}
	}
	if inst, ok := doc["instructions"].(string); ok {
		sysPrefix = inst
	}
	switch inp := doc["input"].(type) {
	case string:
		msgCount = 1
		firstUser, lastUser = inp, inp
	case []any:
		msgCount = len(inp)
		for i, raw := range inp {
			m, _ := raw.(map[string]any)
			typ, _ := m["type"].(string)
			role, _ := m["role"].(string)
			content := m["content"]
			if hasImageBlock(content) || typ == "input_image" {
				sig.NeedVision = true
			}
			txt := extractTextFromAny(content)
			if role == "user" || (typ == "message" && role == "user") {
				if firstUser == "" {
					firstUser = txt
				}
				lastUser = txt
			}
			if i == len(inp)-1 && typ == "function_call_output" {
				toolLoop = true
			}
		}
	}
	return
}

func inspectGemini(doc map[string]any, sig *Signals) (msgCount int, sysPrefix, firstUser, lastUser string, toolLoop bool) {
	if doc == nil {
		return
	}
	if tools, ok := doc["tools"].([]any); ok && len(tools) > 0 {
		sig.NeedTools = true
	}
	if si, ok := doc["systemInstruction"]; ok {
		sysPrefix = extractTextFromAny(si)
	}
	if gc, ok := doc["generationConfig"].(map[string]any); ok {
		if _, ok := gc["thinkingConfig"]; ok {
			sig.NeedThinking = true
		}
		if _, ok := gc["responseSchema"]; ok {
			sig.NeedStructuredOutput = true
		}
	}
	contents, _ := doc["contents"].([]any)
	msgCount = len(contents)
	for i, raw := range contents {
		m, _ := raw.(map[string]any)
		role, _ := m["role"].(string)
		parts, _ := m["parts"].([]any)
		if hasImageBlock(parts) {
			sig.NeedVision = true
		}
		txt := extractTextFromAny(parts)
		if role == "user" || role == "" {
			if firstUser == "" {
				firstUser = txt
			}
			lastUser = txt
		}
		if i == len(contents)-1 {
			for _, pr := range parts {
				if pm, ok := pr.(map[string]any); ok {
					if _, ok := pm["functionResponse"]; ok {
						toolLoop = true
					}
				}
			}
		}
	}
	return
}

func hasAnthropicToolResult(content any) bool {
	blocks, ok := content.([]any)
	if !ok {
		return false
	}
	for _, raw := range blocks {
		if m, ok := raw.(map[string]any); ok {
			if m["type"] == "tool_result" {
				return true
			}
		}
	}
	return false
}

func hasImageBlock(v any) bool {
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if hasImageBlock(item) {
				return true
			}
		}
	case map[string]any:
		typ, _ := t["type"].(string)
		if typ == "image" || typ == "image_url" || typ == "input_image" {
			return true
		}
		if _, ok := t["inlineData"]; ok {
			return true
		}
		if _, ok := t["parts"]; ok {
			return hasImageBlock(t["parts"])
		}
	}
	return false
}

func extractTextFromAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, item := range t {
			s := extractTextFromAny(item)
			if s != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(s)
				if b.Len() > 1500 {
					break
				}
			}
		}
		return b.String()
	case map[string]any:
		if s, ok := t["text"].(string); ok {
			return s
		}
		if parts, ok := t["parts"]; ok {
			return extractTextFromAny(parts)
		}
	}
	return ""
}
