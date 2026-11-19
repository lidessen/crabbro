package codec

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/shared"
)

type geminiCodec struct{}

func (geminiCodec) DecodeRequest(body []byte, ingress IngressInfo) (*ir.GenRequest, error) {
	var raw struct {
		SystemInstruction *struct {
			Parts []gemPart `json:"parts"`
		} `json:"systemInstruction"`
		Contents []struct {
			Role  string    `json:"role"`
			Parts []gemPart `json:"parts"`
		} `json:"contents"`
		Tools []struct {
			FunctionDeclarations []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"functionDeclarations"`
		} `json:"tools"`
		ToolConfig *struct {
			FunctionCallingConfig *struct {
				Mode                 string   `json:"mode"`
				AllowedFunctionNames []string `json:"allowedFunctionNames"`
			} `json:"functionCallingConfig"`
		} `json:"toolConfig"`
		GenerationConfig *struct {
			MaxOutputTokens int            `json:"maxOutputTokens"`
			Temperature     *float64       `json:"temperature"`
			StopSequences   []string       `json:"stopSequences"`
			ResponseSchema  map[string]any `json:"responseSchema"`
			ThinkingConfig  *struct {
				ThinkingBudget int    `json:"thinkingBudget"`
				ThinkingLevel  string `json:"thinkingLevel"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode gemini request: %w", err)
	}
	out := &ir.GenRequest{
		Model:       ingress.RequestedModel,
		Stream:      ingress.Stream,
		SessionHint: ingress.SessionHint,
	}
	if raw.SystemInstruction != nil {
		for _, p := range raw.SystemInstruction.Parts {
			if p.Text != "" {
				out.System = append(out.System, ir.ContentPart{Kind: ir.PartText, Text: p.Text})
			}
		}
	}
	if raw.GenerationConfig != nil {
		out.MaxTokens = raw.GenerationConfig.MaxOutputTokens
		out.Temperature = raw.GenerationConfig.Temperature
		out.StopSequences = raw.GenerationConfig.StopSequences
		if raw.GenerationConfig.ResponseSchema != nil {
			out.SchemaName = "response"
			out.ResponseSchema = raw.GenerationConfig.ResponseSchema
		}
		if tc := raw.GenerationConfig.ThinkingConfig; tc != nil && (tc.ThinkingBudget > 0 || tc.ThinkingLevel != "") {
			out.Thinking = &ir.ThinkingConfig{
				Enabled:      true,
				BudgetTokens: tc.ThinkingBudget,
				Effort:       strings.ToLower(tc.ThinkingLevel),
			}
		}
	}
	for _, tg := range raw.Tools {
		for _, fd := range tg.FunctionDeclarations {
			out.Tools = append(out.Tools, ir.ToolDecl{
				Name:        fd.Name,
				Description: fd.Description,
				Parameters:  fd.Parameters,
			})
		}
	}
	if raw.ToolConfig != nil && raw.ToolConfig.FunctionCallingConfig != nil {
		fc := raw.ToolConfig.FunctionCallingConfig
		switch strings.ToUpper(fc.Mode) {
		case "AUTO":
			out.ToolChoice = &ir.ToolChoice{Mode: "auto"}
		case "NONE":
			out.ToolChoice = &ir.ToolChoice{Mode: "none"}
		case "ANY":
			if len(fc.AllowedFunctionNames) == 1 {
				out.ToolChoice = &ir.ToolChoice{Mode: "tool", Name: fc.AllowedFunctionNames[0]}
			} else {
				out.ToolChoice = &ir.ToolChoice{Mode: "any"}
			}
		}
	}

	nameToID := map[string]string{}
	for mi, c := range raw.Contents {
		role := "user"
		if strings.EqualFold(c.Role, "model") {
			role = "assistant"
		}
		var parts []ir.ContentPart
		for pi, p := range c.Parts {
			switch {
			case p.Thought && p.Text != "":
				parts = append(parts, ir.ContentPart{Kind: ir.PartThinking, Text: p.Text})
			case p.Text != "":
				parts = append(parts, ir.ContentPart{Kind: ir.PartText, Text: p.Text})
			case p.InlineData != nil:
				parts = append(parts, ir.ContentPart{
					Kind:      ir.PartImage,
					MediaType: p.InlineData.MimeType,
					Data:      p.InlineData.Data,
				})
			case p.FunctionCall != nil:
				id := firstNonEmpty(p.FunctionCall.ID, fmt.Sprintf("call_gem_%d_%d", mi, pi))
				nameToID[p.FunctionCall.Name] = id
				args, _ := json.Marshal(p.FunctionCall.Args)
				if len(args) == 0 || string(args) == "null" {
					args = json.RawMessage("{}")
				}
				parts = append(parts, ir.ContentPart{
					Kind:      ir.PartToolUse,
					ToolID:    id,
					ToolName:  p.FunctionCall.Name,
					InputJSON: args,
				})
			case p.FunctionResponse != nil:
				id := firstNonEmpty(p.FunctionResponse.ID, nameToID[p.FunctionResponse.Name], p.FunctionResponse.Name)
				parts = append(parts, ir.ContentPart{
					Kind:     ir.PartToolResult,
					ToolID:   id,
					ToolName: p.FunctionResponse.Name,
					Text:     anyToString(p.FunctionResponse.Response),
				})
			}
		}
		if len(parts) > 0 {
			out.Messages = append(out.Messages, ir.Message{Role: role, Parts: parts})
		}
	}
	return out, nil
}

type gemPart struct {
	Text             string `json:"text,omitempty"`
	Thought          bool   `json:"thought,omitempty"`
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	InlineData       *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData,omitempty"`
	FunctionCall *struct {
		ID   string         `json:"id,omitempty"`
		Name string         `json:"name"`
		Args map[string]any `json:"args,omitempty"`
	} `json:"functionCall,omitempty"`
	FunctionResponse *struct {
		ID       string `json:"id,omitempty"`
		Name     string `json:"name"`
		Response any    `json:"response"`
	} `json:"functionResponse,omitempty"`
}

func (geminiCodec) EncodeRequest(req *ir.GenRequest) ([]byte, error) {
	idToName := map[string]string{}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if p.Kind == ir.PartToolUse && p.ToolID != "" && p.ToolName != "" {
				idToName[p.ToolID] = p.ToolName
			}
		}
	}

	var contents []map[string]any
	appendContent := func(role string, parts []map[string]any) {
		if len(parts) == 0 {
			return
		}
		if n := len(contents); n > 0 && contents[n-1]["role"] == role {
			prev := contents[n-1]["parts"].([]map[string]any)
			contents[n-1]["parts"] = append(prev, parts...)
			return
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}

	for _, m := range req.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		var gparts []map[string]any
		for _, p := range m.Parts {
			switch p.Kind {
			case ir.PartText:
				gparts = append(gparts, map[string]any{"text": p.Text})
			case ir.PartImage:
				if p.Data != "" {
					gparts = append(gparts, map[string]any{
						"inlineData": map[string]any{
							"mimeType": firstNonEmpty(p.MediaType, "image/png"),
							"data":     p.Data,
						},
					})
				} else if p.URL != "" {
					gparts = append(gparts, map[string]any{"text": p.URL})
				}
			case ir.PartToolUse:
				gparts = append(gparts, map[string]any{
					"functionCall": map[string]any{
						"name": p.ToolName,
						"args": normalizeJSONObject(p.InputJSON),
					},
				})
			case ir.PartToolResult:
				fnName := firstNonEmpty(p.ToolName, idToName[p.ToolID], p.ToolID)
				respObj := map[string]any{"result": p.Text}
				if p.IsError {
					respObj = map[string]any{"error": p.Text}
				}
				gparts = append(gparts, map[string]any{
					"functionResponse": map[string]any{
						"name":     fnName,
						"response": respObj,
					},
				})
			}
		}
		appendContent(role, gparts)
	}

	payload := map[string]any{"contents": contents}
	if len(req.System) > 0 {
		payload["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": partsText(req.System)}},
		}
	}
	if len(req.Tools) > 0 {
		decls := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			decl := map[string]any{"name": t.Name}
			if t.Description != "" {
				decl["description"] = t.Description
			}
			if t.Parameters != nil {
				decl["parameters"] = stripAdditionalProperties(t.Parameters)
			}
			decls = append(decls, decl)
		}
		payload["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	if req.ToolChoice != nil {
		cfg := map[string]any{}
		switch req.ToolChoice.Mode {
		case "auto":
			cfg["mode"] = "AUTO"
		case "none":
			cfg["mode"] = "NONE"
		case "any":
			cfg["mode"] = "ANY"
		case "tool":
			cfg["mode"] = "ANY"
			if req.ToolChoice.Name != "" {
				cfg["allowedFunctionNames"] = []string{req.ToolChoice.Name}
			}
		}
		if len(cfg) > 0 {
			payload["toolConfig"] = map[string]any{"functionCallingConfig": cfg}
		}
	}

	genCfg := map[string]any{}
	if req.MaxTokens > 0 {
		genCfg["maxOutputTokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		genCfg["temperature"] = *req.Temperature
	}
	if len(req.StopSequences) > 0 {
		genCfg["stopSequences"] = req.StopSequences
	}
	if len(req.ResponseSchema) > 0 {
		genCfg["responseMimeType"] = "application/json"
		genCfg["responseSchema"] = stripAdditionalProperties(req.ResponseSchema)
	}
	if req.Thinking != nil && req.Thinking.Enabled && req.Thinking.BudgetTokens > 0 {
		genCfg["thinkingConfig"] = map[string]any{
			"thinkingBudget":  req.Thinking.BudgetTokens,
			"includeThoughts": true,
		}
	}
	if len(genCfg) > 0 {
		payload["generationConfig"] = genCfg
	}
	mergeExtraFields(payload, req.ExtraBody)
	return json.Marshal(payload)
}

func (geminiCodec) DecodeResponse(body []byte) (*ir.GenResponse, error) {
	var doc struct {
		ModelVersion string `json:"modelVersion"`
		Candidates   []struct {
			FinishReason string `json:"finishReason"`
			Content      *struct {
				Parts []gemPart `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata *gemUsage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode gemini response: %w", err)
	}
	out := &ir.GenResponse{
		Model:      doc.ModelVersion,
		StopReason: "stop",
	}
	if doc.UsageMetadata != nil {
		out.Usage = doc.UsageMetadata.toTokenUsage()
	}
	hasTools := false
	if len(doc.Candidates) > 0 {
		c := doc.Candidates[0]
		if c.Content != nil {
			for i, p := range c.Content.Parts {
				switch {
				case p.Thought && p.Text != "":
					out.Content = append(out.Content, ir.ContentPart{Kind: ir.PartThinking, Text: p.Text})
				case p.Text != "":
					out.Content = append(out.Content, ir.ContentPart{Kind: ir.PartText, Text: p.Text})
				case p.FunctionCall != nil:
					hasTools = true
					args, _ := json.Marshal(p.FunctionCall.Args)
					if len(args) == 0 || string(args) == "null" {
						args = json.RawMessage("{}")
					}
					out.Content = append(out.Content, ir.ContentPart{
						Kind:      ir.PartToolUse,
						ToolID:    firstNonEmpty(p.FunctionCall.ID, fmt.Sprintf("call_gem_%d", i)),
						ToolName:  p.FunctionCall.Name,
						InputJSON: args,
					})
				}
			}
		}
		out.StopReason = fromGemStop(c.FinishReason, hasTools)
	}
	return out, nil
}

func (geminiCodec) EncodeResponse(resp *ir.GenResponse) ([]byte, error) {
	parts := make([]map[string]any, 0, len(resp.Content))
	hasTools := false
	for _, p := range resp.Content {
		switch p.Kind {
		case ir.PartThinking:
			parts = append(parts, map[string]any{"thought": true, "text": p.Text})
		case ir.PartText:
			parts = append(parts, map[string]any{"text": p.Text})
		case ir.PartToolUse:
			hasTools = true
			parts = append(parts, map[string]any{
				"functionCall": map[string]any{
					"name": p.ToolName,
					"args": normalizeJSONObject(p.InputJSON),
				},
			})
		}
	}
	return json.Marshal(map[string]any{
		"candidates": []map[string]any{{
			"index":        0,
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": toGemStop(resp.StopReason, hasTools),
		}},
		"usageMetadata": encodeGemUsage(resp.Usage),
		"modelVersion":  resp.Model,
	})
}

func (geminiCodec) NewStreamDecoder() streamDecoder {
	return &gemStreamDec{}
}

type gemStreamDec struct {
	started     bool
	ended       bool
	model       string
	usage       shared.TokenUsage
	stopReason  string
	nextToolIdx int
}

func (d *gemStreamDec) Feed(_, data string) ([]ir.StreamEvent, bool, error) {
	trim := strings.TrimSpace(data)
	if trim == "" || trim == "[DONE]" {
		return nil, false, nil
	}
	var doc struct {
		ModelVersion string `json:"modelVersion"`
		Candidates   []struct {
			FinishReason string `json:"finishReason"`
			Content      *struct {
				Parts []gemPart `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata *gemUsage `json:"usageMetadata"`
	}
	if err := json.Unmarshal([]byte(trim), &doc); err != nil {
		return nil, false, nil
	}
	if doc.ModelVersion != "" {
		d.model = doc.ModelVersion
	}
	var evs []ir.StreamEvent
	if !d.started {
		d.started = true
		evs = append(evs, ir.StreamEvent{Kind: ir.EventStreamStart, Model: d.model})
	}
	if len(doc.Candidates) > 0 {
		c := doc.Candidates[0]
		if c.Content != nil {
			for _, p := range c.Content.Parts {
				switch {
				case p.Thought && p.Text != "":
					evs = append(evs, ir.StreamEvent{Kind: ir.EventThinkingDelta, Text: p.Text})
				case p.Text != "":
					evs = append(evs, ir.StreamEvent{Kind: ir.EventTextDelta, Text: p.Text})
				case p.FunctionCall != nil:
					idx := d.nextToolIdx
					d.nextToolIdx++
					id := firstNonEmpty(p.FunctionCall.ID, fmt.Sprintf("call_gem_%d", idx))
					args, _ := json.Marshal(p.FunctionCall.Args)
					if len(args) == 0 || string(args) == "null" {
						args = []byte("{}")
					}
					evs = append(evs,
						ir.StreamEvent{Kind: ir.EventToolCallStart, Index: idx, ToolID: id, ToolName: p.FunctionCall.Name},
						ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: idx, Text: string(args)},
						ir.StreamEvent{Kind: ir.EventToolCallEnd, Index: idx},
					)
				}
			}
		}
		if c.FinishReason != "" {
			d.stopReason = fromGemStop(c.FinishReason, d.nextToolIdx > 0)
		}
	}
	if doc.UsageMetadata != nil {
		mergeMaxUsage(&d.usage, doc.UsageMetadata.toTokenUsage())
		uCopy := d.usage
		evs = append(evs, ir.StreamEvent{Kind: ir.EventUsageDelta, Usage: &uCopy})
	}
	if len(doc.Candidates) > 0 && doc.Candidates[0].FinishReason != "" {
		evs = append(evs, d.finish()...)
		return evs, true, nil
	}
	return evs, false, nil
}

func (d *gemStreamDec) Flush() []ir.StreamEvent { return d.finish() }

func (d *gemStreamDec) finish() []ir.StreamEvent {
	if d.ended {
		return nil
	}
	d.ended = true
	sr := firstNonEmpty(d.stopReason, "stop")
	if d.nextToolIdx > 0 && sr == "stop" {
		sr = "tool_calls"
	}
	uCopy := d.usage
	return []ir.StreamEvent{{
		Kind:       ir.EventStreamEnd,
		Model:      d.model,
		StopReason: sr,
		Usage:      &uCopy,
	}}
}

func (geminiCodec) NewStreamEncoder(requestedModel string, dst io.Writer) streamEncoder {
	return &gemStreamEnc{
		model:    requestedModel,
		dst:      dst,
		toolName: map[int]string{},
		toolArgs: map[int]*strings.Builder{},
		flushed:  map[int]bool{},
	}
}

type gemStreamEnc struct {
	dst         io.Writer
	model       string
	ended       bool
	sawToolCall bool
	toolName    map[int]string
	toolArgs    map[int]*strings.Builder
	flushed     map[int]bool
	usage       shared.TokenUsage
}

func (e *gemStreamEnc) Emit(ev ir.StreamEvent) error {
	if ev.Model != "" && e.model == "" {
		e.model = ev.Model
	}
	switch ev.Kind {
	case ir.EventThinkingDelta:
		return writeSSE(e.dst, "", map[string]any{
			"candidates": []map[string]any{{
				"index":   0,
				"content": map[string]any{"role": "model", "parts": []map[string]any{{"thought": true, "text": ev.Text}}},
			}},
		})
	case ir.EventTextDelta:
		return writeSSE(e.dst, "", map[string]any{
			"candidates": []map[string]any{{
				"index":   0,
				"content": map[string]any{"role": "model", "parts": []map[string]any{{"text": ev.Text}}},
			}},
		})
	case ir.EventToolCallStart:
		e.sawToolCall = true
		e.toolName[ev.Index] = ev.ToolName
		e.toolArgs[ev.Index] = &strings.Builder{}
	case ir.EventToolCallDelta:
		if b, ok := e.toolArgs[ev.Index]; ok {
			b.WriteString(ev.Text)
		}
	case ir.EventToolCallEnd:
		return e.flushTool(ev.Index)
	case ir.EventUsageDelta:
		if ev.Usage != nil {
			mergeMaxUsage(&e.usage, *ev.Usage)
		}
	case ir.EventStreamEnd:
		if ev.Usage != nil {
			mergeMaxUsage(&e.usage, *ev.Usage)
		}
		var indices []int
		for idx := range e.toolName {
			indices = append(indices, idx)
		}
		sort.Ints(indices)
		for _, idx := range indices {
			if err := e.flushTool(idx); err != nil {
				return err
			}
		}
		e.ended = true
		return writeSSE(e.dst, "", map[string]any{
			"candidates": []map[string]any{{
				"index":        0,
				"content":      map[string]any{"role": "model", "parts": []any{}},
				"finishReason": toGemStop(ev.StopReason, e.sawToolCall),
			}},
			"usageMetadata": encodeGemUsage(e.usage),
			"modelVersion":  e.model,
		})
	}
	return nil
}

func (e *gemStreamEnc) flushTool(idx int) error {
	if e.flushed[idx] {
		return nil
	}
	name, ok := e.toolName[idx]
	if !ok {
		return nil
	}
	e.flushed[idx] = true
	var args json.RawMessage
	if b := e.toolArgs[idx]; b != nil {
		args = json.RawMessage(strings.TrimSpace(b.String()))
	}
	return writeSSE(e.dst, "", map[string]any{
		"candidates": []map[string]any{{
			"index": 0,
			"content": map[string]any{
				"role": "model",
				"parts": []map[string]any{{
					"functionCall": map[string]any{
						"name": name,
						"args": normalizeJSONObject(args),
					},
				}},
			},
		}},
	})
}

func (e *gemStreamEnc) Close() error {
	if e.ended {
		return nil
	}
	return e.Emit(ir.StreamEvent{Kind: ir.EventStreamEnd, StopReason: "stop", Usage: &e.usage})
}

func stripAdditionalProperties(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "additionalProperties" {
				continue
			}
			out[k] = stripAdditionalProperties(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = stripAdditionalProperties(item)
		}
		return out
	default:
		return v
	}
}

func fromGemStop(reason string, hasTools bool) string {
	if hasTools {
		return "tool_calls"
	}
	switch strings.ToUpper(reason) {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "BLOCKLIST":
		return "content_filter"
	default:
		return "stop"
	}
}

func toGemStop(reason string, hasTools bool) string {
	switch reason {
	case "max_tokens":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	default:
		_ = hasTools
		return "STOP"
	}
}

func encodeGemUsage(u shared.TokenUsage) map[string]any {
	out := map[string]any{
		"promptTokenCount":     u.InputTokens,
		"candidatesTokenCount": u.OutputTokens,
		"totalTokenCount":      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out["cachedContentTokenCount"] = u.CacheReadTokens
	}
	if u.ReasoningTokens > 0 {
		out["thoughtsTokenCount"] = u.ReasoningTokens
	}
	return out
}
