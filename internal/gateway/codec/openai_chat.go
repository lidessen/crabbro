package codec

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/shared"
)

type oaChatCodec struct{}

type oaChatReqWire struct {
	Model               string          `json:"model"`
	Messages            []oaChatMessage `json:"messages"`
	Tools               []oaChatTool    `json:"tools,omitempty"`
	ToolChoice          any             `json:"tool_choice,omitempty"`
	MaxTokens           int             `json:"max_tokens,omitempty"`
	MaxCompletionTokens int             `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	Stop                any             `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       map[string]any  `json:"stream_options,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	ResponseFormat      *oaChatRespFmt  `json:"response_format,omitempty"`
}

type oaChatRespFmt struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string         `json:"name"`
		Strict bool           `json:"strict,omitempty"`
		Schema map[string]any `json:"schema"`
	} `json:"json_schema,omitempty"`
}

type oaChatMessage struct {
	Role             string           `json:"role"`
	Content          any              `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	Reasoning        string           `json:"reasoning,omitempty"`
	ToolCalls        []oaChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string           `json:"tool_call_id,omitempty"`
}

type oaChatToolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaChatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	} `json:"function"`
}

func (oaChatCodec) DecodeRequest(body []byte, ingress IngressInfo) (*ir.GenRequest, error) {
	var raw oaChatReqWire
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode openai-chat request: %w", err)
	}
	out := &ir.GenRequest{
		Model:       raw.Model,
		Temperature: raw.Temperature,
		Stream:      raw.Stream || ingress.Stream,
		SessionHint: ingress.SessionHint,
	}
	if raw.MaxTokens > 0 {
		out.MaxTokens = raw.MaxTokens
	} else if raw.MaxCompletionTokens > 0 {
		out.MaxTokens = raw.MaxCompletionTokens
	}
	if out.Model == "" {
		out.Model = ingress.RequestedModel
	}
	out.StopSequences = parseStringOrSlice(raw.Stop)
	if raw.ReasoningEffort != "" {
		out.Thinking = &ir.ThinkingConfig{Enabled: true, Effort: raw.ReasoningEffort}
	}
	if raw.ResponseFormat != nil {
		switch raw.ResponseFormat.Type {
		case "json_schema":
			if raw.ResponseFormat.JSONSchema != nil {
				out.SchemaName = raw.ResponseFormat.JSONSchema.Name
				out.ResponseSchema = raw.ResponseFormat.JSONSchema.Schema
			}
		case "json_object":
			out.ResponseSchema = map[string]any{"type": "object"}
		}
	}

	for _, t := range raw.Tools {
		out.Tools = append(out.Tools, ir.ToolDecl{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	if raw.ToolChoice != nil {
		switch tc := raw.ToolChoice.(type) {
		case string:
			mode := tc
			if mode == "required" {
				mode = "any"
			}
			out.ToolChoice = &ir.ToolChoice{Mode: mode}
		case map[string]any:
			if fn, ok := tc["function"].(map[string]any); ok {
				name, _ := fn["name"].(string)
				out.ToolChoice = &ir.ToolChoice{Mode: "tool", Name: name}
			}
		}
	}

	for _, m := range raw.Messages {
		role := strings.ToLower(m.Role)
		switch role {
		case "system", "developer":
			parts := decodeOAChatContent(m.Content)
			out.System = append(out.System, parts...)
		case "tool":
			parts := decodeOAChatContent(m.Content)
			txt := partsText(parts)
			out.Messages = append(out.Messages, ir.Message{
				Role: "tool",
				Parts: []ir.ContentPart{{
					Kind:   ir.PartToolResult,
					ToolID: m.ToolCallID,
					Text:   txt,
				}},
			})
		case "assistant":
			var parts []ir.ContentPart
			if rtxt := firstNonEmpty(m.ReasoningContent, m.Reasoning); rtxt != "" {
				parts = append(parts, ir.ContentPart{Kind: ir.PartThinking, Text: rtxt})
			}
			parts = append(parts, decodeOAChatContent(m.Content)...)
			for _, tc := range m.ToolCalls {
				args := json.RawMessage(strings.TrimSpace(tc.Function.Arguments))
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				parts = append(parts, ir.ContentPart{
					Kind:      ir.PartToolUse,
					ToolID:    tc.ID,
					ToolName:  tc.Function.Name,
					InputJSON: args,
				})
			}
			if len(parts) > 0 {
				out.Messages = append(out.Messages, ir.Message{Role: "assistant", Parts: parts})
			}
		default:
			parts := decodeOAChatContent(m.Content)
			if len(parts) > 0 {
				out.Messages = append(out.Messages, ir.Message{Role: "user", Parts: parts})
			}
		}
	}
	return out, nil
}

func decodeOAChatContent(c any) []ir.ContentPart {
	switch v := c.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []ir.ContentPart{{Kind: ir.PartText, Text: v}}
	case []any:
		var out []ir.ContentPart
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "text", "input_text", "output_text":
				if txt, _ := m["text"].(string); txt != "" {
					out = append(out, ir.ContentPart{Kind: ir.PartText, Text: txt})
				}
			case "image_url":
				var urlStr string
				switch iu := m["image_url"].(type) {
				case string:
					urlStr = iu
				case map[string]any:
					urlStr, _ = iu["url"].(string)
				}
				if urlStr != "" {
					out = append(out, imagePartFromURL(urlStr))
				}
			}
		}
		return out
	}
	return nil
}

func (oaChatCodec) EncodeRequest(req *ir.GenRequest) ([]byte, error) {
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if len(req.System) > 0 {
		msgs = append(msgs, map[string]any{
			"role":    "system",
			"content": partsText(req.System),
		})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			for _, p := range m.Parts {
				if p.Kind == ir.PartToolResult {
					msgs = append(msgs, map[string]any{
						"role":         "tool",
						"tool_call_id": p.ToolID,
						"content":      p.Text,
					})
				}
			}
		case "assistant":
			var txtParts []string
			var toolCalls []map[string]any
			for _, p := range m.Parts {
				switch p.Kind {
				case ir.PartText:
					txtParts = append(txtParts, p.Text)
				case ir.PartToolUse:
					args := strings.TrimSpace(string(p.InputJSON))
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, map[string]any{
						"id":   p.ToolID,
						"type": "function",
						"function": map[string]any{
							"name":      p.ToolName,
							"arguments": args,
						},
					})
				}
			}
			msgObj := map[string]any{"role": "assistant"}
			if len(txtParts) > 0 {
				msgObj["content"] = strings.Join(txtParts, "")
			} else if len(toolCalls) > 0 {
				msgObj["content"] = nil
			}
			if len(toolCalls) > 0 {
				msgObj["tool_calls"] = toolCalls
			}
			msgs = append(msgs, msgObj)
		default:
			var toolResults []ir.ContentPart
			var userParts []ir.ContentPart
			for _, p := range m.Parts {
				if p.Kind == ir.PartToolResult {
					toolResults = append(toolResults, p)
				} else {
					userParts = append(userParts, p)
				}
			}
			for _, tr := range toolResults {
				msgs = append(msgs, map[string]any{
					"role":         "tool",
					"tool_call_id": tr.ToolID,
					"content":      tr.Text,
				})
			}
			if len(userParts) > 0 {
				msgs = append(msgs, map[string]any{
					"role":    "user",
					"content": encodeOAUserContent(userParts),
				})
			}
		}
	}

	payload := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if t.Parameters != nil {
				fn["parameters"] = t.Parameters
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		payload["tools"] = tools
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Mode {
		case "any":
			payload["tool_choice"] = "required"
		case "auto", "none":
			payload["tool_choice"] = req.ToolChoice.Mode
		case "tool":
			payload["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": req.ToolChoice.Name},
			}
		}
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if len(req.StopSequences) > 0 {
		payload["stop"] = req.StopSequences
	}
	if req.Thinking != nil && req.Thinking.Enabled && req.Thinking.Effort != "" {
		payload["reasoning_effort"] = req.Thinking.Effort
	}
	if len(req.ResponseSchema) > 0 {
		payload["response_format"] = map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   firstNonEmpty(req.SchemaName, "response"),
				"strict": true,
				"schema": req.ResponseSchema,
			},
		}
	}
	if req.Stream {
		payload["stream"] = true
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	mergeExtraFields(payload, req.ExtraBody)
	return json.Marshal(payload)
}

func encodeOAUserContent(parts []ir.ContentPart) any {
	if len(parts) == 1 && parts[0].Kind == ir.PartText {
		return parts[0].Text
	}
	arr := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Kind {
		case ir.PartText:
			arr = append(arr, map[string]any{"type": "text", "text": p.Text})
		case ir.PartImage:
			arr = append(arr, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": imagePartToDataURL(p)},
			})
		}
	}
	return arr
}

func (oaChatCodec) DecodeResponse(body []byte) (*ir.GenResponse, error) {
	var doc struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string        `json:"finish_reason"`
			Message      oaChatMessage `json:"message"`
		} `json:"choices"`
		Usage *oaChatUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode openai-chat response: %w", err)
	}
	out := &ir.GenResponse{
		ID:         doc.ID,
		Model:      doc.Model,
		StopReason: "stop",
	}
	if doc.Usage != nil {
		out.Usage = doc.Usage.toTokenUsage()
	}
	if len(doc.Choices) > 0 {
		ch := doc.Choices[0]
		out.StopReason = fromOAChatStop(ch.FinishReason)
		if rtxt := firstNonEmpty(ch.Message.ReasoningContent, ch.Message.Reasoning); rtxt != "" {
			out.Content = append(out.Content, ir.ContentPart{Kind: ir.PartThinking, Text: rtxt})
		}
		out.Content = append(out.Content, decodeOAChatContent(ch.Message.Content)...)
		for _, tc := range ch.Message.ToolCalls {
			args := json.RawMessage(strings.TrimSpace(tc.Function.Arguments))
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out.Content = append(out.Content, ir.ContentPart{
				Kind:      ir.PartToolUse,
				ToolID:    tc.ID,
				ToolName:  tc.Function.Name,
				InputJSON: args,
			})
		}
	}
	return out, nil
}

func (oaChatCodec) EncodeResponse(resp *ir.GenResponse) ([]byte, error) {
	var thinking, textBuf strings.Builder
	var toolCalls []map[string]any
	for _, p := range resp.Content {
		switch p.Kind {
		case ir.PartThinking:
			thinking.WriteString(p.Text)
		case ir.PartText:
			textBuf.WriteString(p.Text)
		case ir.PartToolUse:
			args := strings.TrimSpace(string(p.InputJSON))
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   p.ToolID,
				"type": "function",
				"function": map[string]any{
					"name":      p.ToolName,
					"arguments": args,
				},
			})
		}
	}
	msg := map[string]any{"role": "assistant"}
	if textBuf.Len() > 0 || len(toolCalls) == 0 {
		msg["content"] = textBuf.String()
	} else {
		msg["content"] = nil
	}
	if thinking.Len() > 0 {
		msg["reasoning_content"] = thinking.String()
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return json.Marshal(map[string]any{
		"id":     firstNonEmpty(resp.ID, "chatcmpl-crabbro"),
		"object": "chat.completion",
		"model":  resp.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": toOAChatStop(resp.StopReason, len(toolCalls) > 0),
		}},
		"usage": encodeOAChatUsage(resp.Usage),
	})
}

func (oaChatCodec) NewStreamDecoder() streamDecoder {
	return &oaChatStreamDec{toolSeen: map[int]bool{}}
}

type oaChatStreamDec struct {
	started    bool
	ended      bool
	id, model  string
	usage      shared.TokenUsage
	stopReason string
	toolSeen   map[int]bool
}

func (d *oaChatStreamDec) Feed(_, data string) ([]ir.StreamEvent, bool, error) {
	trim := strings.TrimSpace(data)
	if trim == "[DONE]" {
		return d.finish(), true, nil
	}
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *oaChatUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(trim), &chunk); err != nil {
		return nil, false, nil
	}
	var evs []ir.StreamEvent
	if chunk.ID != "" {
		d.id = chunk.ID
	}
	if chunk.Model != "" {
		d.model = chunk.Model
	}
	if !d.started {
		d.started = true
		evs = append(evs, ir.StreamEvent{Kind: ir.EventStreamStart, ID: d.id, Model: d.model})
	}
	if chunk.Usage != nil {
		u := chunk.Usage.toTokenUsage()
		mergeMaxUsage(&d.usage, u)
		uCopy := d.usage
		evs = append(evs, ir.StreamEvent{Kind: ir.EventUsageDelta, Usage: &uCopy})
	}
	if len(chunk.Choices) > 0 {
		ch := chunk.Choices[0]
		if rtxt := firstNonEmpty(ch.Delta.ReasoningContent, ch.Delta.Reasoning); rtxt != "" {
			evs = append(evs, ir.StreamEvent{Kind: ir.EventThinkingDelta, Text: rtxt})
		}
		if ch.Delta.Content != "" {
			evs = append(evs, ir.StreamEvent{Kind: ir.EventTextDelta, Text: ch.Delta.Content})
		}
		for _, tc := range ch.Delta.ToolCalls {
			if !d.toolSeen[tc.Index] {
				d.toolSeen[tc.Index] = true
				evs = append(evs, ir.StreamEvent{
					Kind:     ir.EventToolCallStart,
					Index:    tc.Index,
					ToolID:   firstNonEmpty(tc.ID, fmt.Sprintf("call_%d", tc.Index)),
					ToolName: tc.Function.Name,
				})
			}
			if tc.Function.Arguments != "" {
				evs = append(evs, ir.StreamEvent{
					Kind:  ir.EventToolCallDelta,
					Index: tc.Index,
					Text:  tc.Function.Arguments,
				})
			}
		}
		if ch.FinishReason != "" {
			d.stopReason = fromOAChatStop(ch.FinishReason)
		}
	}
	return evs, false, nil
}

func (d *oaChatStreamDec) Flush() []ir.StreamEvent { return d.finish() }

func (d *oaChatStreamDec) finish() []ir.StreamEvent {
	if d.ended {
		return nil
	}
	d.ended = true
	var evs []ir.StreamEvent
	for idx := range d.toolSeen {
		evs = append(evs, ir.StreamEvent{Kind: ir.EventToolCallEnd, Index: idx})
	}
	uCopy := d.usage
	sr := firstNonEmpty(d.stopReason, "stop")
	if len(d.toolSeen) > 0 && sr == "stop" {
		sr = "tool_calls"
	}
	evs = append(evs, ir.StreamEvent{
		Kind:       ir.EventStreamEnd,
		ID:         d.id,
		Model:      d.model,
		StopReason: sr,
		Usage:      &uCopy,
	})
	return evs
}

func (oaChatCodec) NewStreamEncoder(requestedModel string, dst io.Writer) streamEncoder {
	return &oaChatStreamEnc{model: requestedModel, dst: dst}
}

type oaChatStreamEnc struct {
	dst         io.Writer
	id, model   string
	sentStart   bool
	ended       bool
	sawToolCall bool
	usage       shared.TokenUsage
}

func (e *oaChatStreamEnc) Emit(ev ir.StreamEvent) error {
	if ev.ID != "" && e.id == "" {
		e.id = ev.ID
	}
	if ev.Model != "" && e.model == "" {
		e.model = ev.Model
	}
	switch ev.Kind {
	case ir.EventStreamStart:
		e.sentStart = true
		return writeSSE(e.dst, "", map[string]any{
			"id":     firstNonEmpty(e.id, "chatcmpl-crabbro"),
			"object": "chat.completion.chunk",
			"model":  e.model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{"role": "assistant"},
			}},
		})
	case ir.EventThinkingDelta:
		return writeSSE(e.dst, "", e.deltaChunk(map[string]any{"reasoning_content": ev.Text}))
	case ir.EventTextDelta:
		return writeSSE(e.dst, "", e.deltaChunk(map[string]any{"content": ev.Text}))
	case ir.EventToolCallStart:
		e.sawToolCall = true
		return writeSSE(e.dst, "", e.deltaChunk(map[string]any{
			"tool_calls": []map[string]any{{
				"index": ev.Index,
				"id":    ev.ToolID,
				"type":  "function",
				"function": map[string]any{
					"name":      ev.ToolName,
					"arguments": "",
				},
			}},
		}))
	case ir.EventToolCallDelta:
		return writeSSE(e.dst, "", e.deltaChunk(map[string]any{
			"tool_calls": []map[string]any{{
				"index": ev.Index,
				"function": map[string]any{
					"arguments": ev.Text,
				},
			}},
		}))
	case ir.EventUsageDelta:
		if ev.Usage != nil {
			mergeMaxUsage(&e.usage, *ev.Usage)
		}
	case ir.EventStreamEnd:
		if ev.Usage != nil {
			mergeMaxUsage(&e.usage, *ev.Usage)
		}
		e.ended = true
		if err := writeSSE(e.dst, "", map[string]any{
			"id":     firstNonEmpty(e.id, "chatcmpl-crabbro"),
			"object": "chat.completion.chunk",
			"model":  e.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": toOAChatStop(ev.StopReason, e.sawToolCall),
			}},
			"usage": encodeOAChatUsage(e.usage),
		}); err != nil {
			return err
		}
		_, err := io.WriteString(e.dst, "data: [DONE]\n\n")
		return err
	}
	return nil
}

func (e *oaChatStreamEnc) Close() error {
	if e.ended {
		return nil
	}
	return e.Emit(ir.StreamEvent{Kind: ir.EventStreamEnd, StopReason: "stop", Usage: &e.usage})
}

func (e *oaChatStreamEnc) deltaChunk(delta map[string]any) map[string]any {
	return map[string]any{
		"id":     firstNonEmpty(e.id, "chatcmpl-crabbro"),
		"object": "chat.completion.chunk",
		"model":  e.model,
		"choices": []map[string]any{{
			"index": 0,
			"delta": delta,
		}},
	}
}

func fromOAChatStop(r string) string {
	switch r {
	case "tool_calls", "function_call":
		return "tool_calls"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

func toOAChatStop(r string, hasTools bool) string {
	if hasTools || r == "tool_calls" {
		return "tool_calls"
	}
	switch r {
	case "max_tokens":
		return "length"
	case "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

func encodeOAChatUsage(u shared.TokenUsage) map[string]any {
	out := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		out["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.ReasoningTokens}
	}
	return out
}
