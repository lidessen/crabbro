package codec

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/shared"
)

const defaultAnthropicMaxTokens = 4096

type anthropicCodec struct{}

func (anthropicCodec) DecodeRequest(body []byte, ingress IngressInfo) (*ir.GenRequest, error) {
	var raw struct {
		Model    string          `json:"model"`
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
		ToolChoice *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
		Thinking *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		MaxTokens     int      `json:"max_tokens"`
		Temperature   *float64 `json:"temperature"`
		StopSequences []string `json:"stop_sequences"`
		Stream        bool     `json:"stream"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode anthropic request: %w", err)
	}
	out := &ir.GenRequest{
		Model:         firstNonEmpty(raw.Model, ingress.RequestedModel),
		MaxTokens:     raw.MaxTokens,
		Temperature:   raw.Temperature,
		StopSequences: raw.StopSequences,
		Stream:        raw.Stream || ingress.Stream,
		SessionHint:   ingress.SessionHint,
	}
	if len(raw.System) > 0 {
		out.System = decodeAnthContent(raw.System)
	}
	if raw.Thinking != nil && raw.Thinking.Type == "enabled" {
		out.Thinking = &ir.ThinkingConfig{Enabled: true, BudgetTokens: raw.Thinking.BudgetTokens}
	}
	for _, t := range raw.Tools {
		out.Tools = append(out.Tools, ir.ToolDecl{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	if raw.ToolChoice != nil {
		out.ToolChoice = &ir.ToolChoice{Mode: raw.ToolChoice.Type, Name: raw.ToolChoice.Name}
	}
	for _, m := range raw.Messages {
		parts := decodeAnthContent(m.Content)
		if len(parts) > 0 {
			role := strings.ToLower(m.Role)
			if role != "assistant" {
				role = "user"
			}
			out.Messages = append(out.Messages, ir.Message{Role: role, Parts: parts})
		}
	}
	return out, nil
}

func decodeAnthContent(raw json.RawMessage) []ir.ContentPart {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []ir.ContentPart{{Kind: ir.PartText, Text: s}}
	}
	var blocks []struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
		Source    *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var parts []ir.ContentPart
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, ir.ContentPart{Kind: ir.PartText, Text: b.Text})
			}
		case "thinking":
			if b.Thinking != "" {
				parts = append(parts, ir.ContentPart{
					Kind:      ir.PartThinking,
					Text:      b.Thinking,
					Signature: b.Signature,
				})
			}
		case "image":
			if b.Source != nil {
				parts = append(parts, ir.ContentPart{
					Kind:      ir.PartImage,
					MediaType: b.Source.MediaType,
					Data:      b.Source.Data,
					URL:       b.Source.URL,
				})
			}
		case "tool_use":
			in := json.RawMessage(strings.TrimSpace(string(b.Input)))
			if len(in) == 0 {
				in = json.RawMessage("{}")
			}
			parts = append(parts, ir.ContentPart{
				Kind:      ir.PartToolUse,
				ToolID:    b.ID,
				ToolName:  b.Name,
				InputJSON: in,
			})
		case "tool_result":
			txt := ""
			if len(b.Content) > 0 {
				var cs string
				if b.Content[0] == '"' && json.Unmarshal(b.Content, &cs) == nil {
					txt = cs
				} else {
					txt = partsText(decodeAnthContent(b.Content))
				}
			}
			parts = append(parts, ir.ContentPart{
				Kind:    ir.PartToolResult,
				ToolID:  b.ToolUseID,
				Text:    txt,
				IsError: b.IsError,
			})
		}
	}
	return parts
}

func (anthropicCodec) EncodeRequest(req *ir.GenRequest) ([]byte, error) {
	maxTok := req.MaxTokens
	if maxTok <= 0 {
		maxTok = defaultAnthropicMaxTokens
	}

	// Merge consecutive messages of the same effective Anthropic role ("user" vs "assistant").
	type anthMsg struct {
		role   string
		blocks []map[string]any
	}
	var merged []anthMsg
	appendBlocks := func(role string, blks []map[string]any) {
		if len(blks) == 0 {
			return
		}
		if n := len(merged); n > 0 && merged[n-1].role == role {
			merged[n-1].blocks = append(merged[n-1].blocks, blks...)
			return
		}
		merged = append(merged, anthMsg{role: role, blocks: blks})
	}

	for _, m := range req.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		var blks []map[string]any
		for _, p := range m.Parts {
			switch p.Kind {
			case ir.PartText:
				blks = append(blks, map[string]any{"type": "text", "text": p.Text})
			case ir.PartThinking:
				// Only preserve thinking blocks in Anthropic history if signed natively by Anthropic.
				if p.Signature != "" {
					blks = append(blks, map[string]any{"type": "thinking", "thinking": p.Text, "signature": p.Signature})
				}
			case ir.PartImage:
				if p.Data != "" {
					blks = append(blks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": firstNonEmpty(p.MediaType, "image/png"),
							"data":       p.Data,
						},
					})
				} else if p.URL != "" {
					blks = append(blks, map[string]any{
						"type":   "image",
						"source": map[string]any{"type": "url", "url": p.URL},
					})
				}
			case ir.PartToolUse:
				blks = append(blks, map[string]any{
					"type":  "tool_use",
					"id":    p.ToolID,
					"name":  p.ToolName,
					"input": normalizeJSONObject(p.InputJSON),
				})
			case ir.PartToolResult:
				blk := map[string]any{
					"type":        "tool_result",
					"tool_use_id": p.ToolID,
					"content":     p.Text,
				}
				if p.IsError {
					blk["is_error"] = true
				}
				blks = append(blks, blk)
			}
		}
		appendBlocks(role, blks)
	}

	msgs := make([]map[string]any, 0, len(merged))
	for _, mm := range merged {
		msgs = append(msgs, map[string]any{"role": mm.role, "content": mm.blocks})
	}

	payload := map[string]any{
		"model":      req.Model,
		"max_tokens": maxTok,
		"messages":   msgs,
	}
	if len(req.System) > 0 {
		payload["system"] = partsText(req.System)
	}

	forcedToolChoice := false
	if len(req.ResponseSchema) > 0 && len(req.Tools) == 0 {
		toolName := firstNonEmpty(req.SchemaName, ir.DecisionToolName)
		payload["tools"] = []map[string]any{{
			"name":         toolName,
			"description":  "Submit the structured response matching the JSON schema.",
			"input_schema": req.ResponseSchema,
		}}
		payload["tool_choice"] = map[string]any{"type": "tool", "name": toolName}
		forcedToolChoice = true
	} else {
		if len(req.Tools) > 0 {
			tools := make([]map[string]any, 0, len(req.Tools))
			for _, t := range req.Tools {
				schema := t.Parameters
				if schema == nil {
					schema = map[string]any{"type": "object", "properties": map[string]any{}}
				}
				tools = append(tools, map[string]any{
					"name":         t.Name,
					"description":  t.Description,
					"input_schema": schema,
				})
			}
			payload["tools"] = tools
		}
		if req.ToolChoice != nil {
			switch req.ToolChoice.Mode {
			case "auto", "any", "none":
				payload["tool_choice"] = map[string]any{"type": req.ToolChoice.Mode}
				if req.ToolChoice.Mode == "any" {
					forcedToolChoice = true
				}
			case "tool":
				payload["tool_choice"] = map[string]any{"type": "tool", "name": req.ToolChoice.Name}
				forcedToolChoice = true
			}
		}
	}

	if req.Temperature != nil && !(req.Thinking != nil && req.Thinking.Enabled && !forcedToolChoice) {
		payload["temperature"] = *req.Temperature
	}
	if len(req.StopSequences) > 0 {
		payload["stop_sequences"] = req.StopSequences
	}
	// Anthropic rejects extended thinking combined with forced tool_choice.
	if req.Thinking != nil && req.Thinking.Enabled && !forcedToolChoice {
		budget := req.Thinking.BudgetTokens
		if budget <= 0 {
			budget = 1024
		}
		if maxTok <= budget {
			payload["max_tokens"] = budget + 1024
		}
		payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	if req.Stream {
		payload["stream"] = true
	}
	mergeExtraFields(payload, req.ExtraBody)
	return json.Marshal(payload)
}

func (anthropicCodec) DecodeResponse(body []byte) (*ir.GenResponse, error) {
	var doc struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		Usage *anthUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	out := &ir.GenResponse{
		ID:         doc.ID,
		Model:      doc.Model,
		StopReason: fromAnthStop(doc.StopReason),
	}
	if doc.Usage != nil {
		out.Usage = doc.Usage.toTokenUsage()
	}
	for _, b := range doc.Content {
		switch b.Type {
		case "thinking":
			out.Content = append(out.Content, ir.ContentPart{Kind: ir.PartThinking, Text: b.Thinking, Signature: b.Signature})
		case "text":
			out.Content = append(out.Content, ir.ContentPart{Kind: ir.PartText, Text: b.Text})
		case "tool_use":
			in := json.RawMessage(strings.TrimSpace(string(b.Input)))
			if len(in) == 0 {
				in = json.RawMessage("{}")
			}
			out.Content = append(out.Content, ir.ContentPart{
				Kind:      ir.PartToolUse,
				ToolID:    b.ID,
				ToolName:  b.Name,
				InputJSON: in,
			})
		}
	}
	return out, nil
}

func (anthropicCodec) EncodeResponse(resp *ir.GenResponse) ([]byte, error) {
	blocks := make([]map[string]any, 0, len(resp.Content))
	hasTools := false
	for _, p := range resp.Content {
		switch p.Kind {
		case ir.PartThinking:
			blk := map[string]any{"type": "thinking", "thinking": p.Text}
			if p.Signature != "" {
				blk["signature"] = p.Signature
			}
			blocks = append(blocks, blk)
		case ir.PartText:
			blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
		case ir.PartToolUse:
			hasTools = true
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    p.ToolID,
				"name":  p.ToolName,
				"input": normalizeJSONObject(p.InputJSON),
			})
		}
	}
	return json.Marshal(map[string]any{
		"id":            firstNonEmpty(resp.ID, "msg_crabbro"),
		"type":          "message",
		"role":          "assistant",
		"model":         resp.Model,
		"content":       blocks,
		"stop_reason":   toAnthStop(resp.StopReason, hasTools),
		"stop_sequence": nil,
		"usage":         encodeAnthUsage(resp.Usage),
	})
}

func (anthropicCodec) NewStreamDecoder() streamDecoder {
	return &anthStreamDec{blocks: map[int]string{}}
}

type anthStreamDec struct {
	started    bool
	ended      bool
	id, model  string
	usage      shared.TokenUsage
	stopReason string
	blocks     map[int]string
	toolIdx    map[int]int
	nextTool   int
}

func (d *anthStreamDec) Feed(evName, data string) ([]ir.StreamEvent, bool, error) {
	trim := strings.TrimSpace(data)
	if trim == "" {
		return nil, false, nil
	}
	var doc struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message *struct {
			ID    string     `json:"id"`
			Model string     `json:"model"`
			Usage *anthUsage `json:"usage"`
		} `json:"message"`
		ContentBlock *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *anthUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(trim), &doc); err != nil {
		return nil, false, nil
	}
	typ := firstNonEmpty(doc.Type, evName)
	if d.toolIdx == nil {
		d.toolIdx = map[int]int{}
	}
	var evs []ir.StreamEvent
	switch typ {
	case "message_start":
		if doc.Message != nil {
			d.id = doc.Message.ID
			d.model = doc.Message.Model
			if doc.Message.Usage != nil {
				mergeMaxUsage(&d.usage, doc.Message.Usage.toTokenUsage())
			}
		}
		if !d.started {
			d.started = true
			evs = append(evs, ir.StreamEvent{Kind: ir.EventStreamStart, ID: d.id, Model: d.model})
			if d.usage.Total() > 0 {
				uCopy := d.usage
				evs = append(evs, ir.StreamEvent{Kind: ir.EventUsageDelta, Usage: &uCopy})
			}
		}
	case "content_block_start":
		if doc.ContentBlock != nil {
			d.blocks[doc.Index] = doc.ContentBlock.Type
			if doc.ContentBlock.Type == "tool_use" {
				ti := d.nextTool
				d.nextTool++
				d.toolIdx[doc.Index] = ti
				evs = append(evs, ir.StreamEvent{
					Kind:     ir.EventToolCallStart,
					Index:    ti,
					ToolID:   doc.ContentBlock.ID,
					ToolName: doc.ContentBlock.Name,
				})
			}
		}
	case "content_block_delta":
		if doc.Delta != nil {
			switch doc.Delta.Type {
			case "text_delta":
				if doc.Delta.Text != "" {
					evs = append(evs, ir.StreamEvent{Kind: ir.EventTextDelta, Text: doc.Delta.Text})
				}
			case "thinking_delta":
				if doc.Delta.Thinking != "" {
					evs = append(evs, ir.StreamEvent{Kind: ir.EventThinkingDelta, Text: doc.Delta.Thinking})
				}
			case "input_json_delta":
				ti := d.toolIdx[doc.Index]
				if doc.Delta.PartialJSON != "" {
					evs = append(evs, ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: ti, Text: doc.Delta.PartialJSON})
				}
			}
		}
	case "content_block_stop":
		if d.blocks[doc.Index] == "tool_use" {
			evs = append(evs, ir.StreamEvent{Kind: ir.EventToolCallEnd, Index: d.toolIdx[doc.Index]})
		}
	case "message_delta":
		if doc.Delta != nil && doc.Delta.StopReason != "" {
			d.stopReason = fromAnthStop(doc.Delta.StopReason)
		}
		if doc.Usage != nil {
			mergeMaxUsage(&d.usage, doc.Usage.toTokenUsage())
			uCopy := d.usage
			evs = append(evs, ir.StreamEvent{Kind: ir.EventUsageDelta, Usage: &uCopy})
		}
	case "message_stop":
		evs = append(evs, d.finish()...)
		return evs, true, nil
	}
	return evs, false, nil
}

func (d *anthStreamDec) Flush() []ir.StreamEvent { return d.finish() }

func (d *anthStreamDec) finish() []ir.StreamEvent {
	if d.ended {
		return nil
	}
	d.ended = true
	uCopy := d.usage
	return []ir.StreamEvent{{
		Kind:       ir.EventStreamEnd,
		ID:         d.id,
		Model:      d.model,
		StopReason: firstNonEmpty(d.stopReason, "stop"),
		Usage:      &uCopy,
	}}
}

func (anthropicCodec) NewStreamEncoder(requestedModel string, dst io.Writer) streamEncoder {
	return &anthStreamEnc{
		model:    requestedModel,
		dst:      dst,
		openIdx:  -1,
		toolBIdx: map[int]int{},
	}
}

type anthStreamEnc struct {
	dst         io.Writer
	id, model   string
	sentStart   bool
	ended       bool
	nextIdx     int
	openIdx     int
	openKind    string // thinking | text | tool_use
	toolBIdx    map[int]int
	sawToolCall bool
	usage       shared.TokenUsage
}

func (e *anthStreamEnc) ensureStart() error {
	if e.sentStart {
		return nil
	}
	e.sentStart = true
	return writeSSE(e.dst, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            firstNonEmpty(e.id, "msg_crabbro"),
			"type":          "message",
			"role":          "assistant",
			"model":         e.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": e.usage.InputTokens, "output_tokens": 0},
		},
	})
}

func (e *anthStreamEnc) closeOpenBlock() error {
	if e.openIdx < 0 {
		return nil
	}
	idx := e.openIdx
	e.openIdx = -1
	e.openKind = ""
	return writeSSE(e.dst, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (e *anthStreamEnc) Emit(ev ir.StreamEvent) error {
	if ev.ID != "" && e.id == "" {
		e.id = ev.ID
	}
	if ev.Model != "" && e.model == "" {
		e.model = ev.Model
	}
	switch ev.Kind {
	case ir.EventStreamStart:
		return e.ensureStart()
	case ir.EventUsageDelta:
		if ev.Usage != nil {
			mergeMaxUsage(&e.usage, *ev.Usage)
		}
	case ir.EventThinkingDelta:
		if err := e.ensureStart(); err != nil {
			return err
		}
		if e.openKind != "thinking" {
			if err := e.closeOpenBlock(); err != nil {
				return err
			}
			e.openIdx = e.nextIdx
			e.nextIdx++
			e.openKind = "thinking"
			if err := writeSSE(e.dst, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         e.openIdx,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}); err != nil {
				return err
			}
		}
		return writeSSE(e.dst, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.openIdx,
			"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
		})
	case ir.EventTextDelta:
		if err := e.ensureStart(); err != nil {
			return err
		}
		if e.openKind != "text" {
			if err := e.closeOpenBlock(); err != nil {
				return err
			}
			e.openIdx = e.nextIdx
			e.nextIdx++
			e.openKind = "text"
			if err := writeSSE(e.dst, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         e.openIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
		}
		return writeSSE(e.dst, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.openIdx,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})
	case ir.EventToolCallStart:
		if err := e.ensureStart(); err != nil {
			return err
		}
		if err := e.closeOpenBlock(); err != nil {
			return err
		}
		e.sawToolCall = true
		e.openIdx = e.nextIdx
		e.nextIdx++
		e.openKind = "tool_use"
		e.toolBIdx[ev.Index] = e.openIdx
		return writeSSE(e.dst, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": e.openIdx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    ev.ToolID,
				"name":  ev.ToolName,
				"input": map[string]any{},
			},
		})
	case ir.EventToolCallDelta:
		bIdx, ok := e.toolBIdx[ev.Index]
		if !ok {
			bIdx = e.openIdx
		}
		return writeSSE(e.dst, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": bIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Text},
		})
	case ir.EventToolCallEnd:
		if bIdx, ok := e.toolBIdx[ev.Index]; ok && bIdx == e.openIdx {
			return e.closeOpenBlock()
		}
	case ir.EventStreamEnd:
		if ev.Usage != nil {
			mergeMaxUsage(&e.usage, *ev.Usage)
		}
		if err := e.ensureStart(); err != nil {
			return err
		}
		if err := e.closeOpenBlock(); err != nil {
			return err
		}
		e.ended = true
		if err := writeSSE(e.dst, "message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": toAnthStop(ev.StopReason, e.sawToolCall), "stop_sequence": nil},
			"usage": encodeAnthUsage(e.usage),
		}); err != nil {
			return err
		}
		return writeSSE(e.dst, "message_stop", map[string]any{"type": "message_stop"})
	}
	return nil
}

func (e *anthStreamEnc) Close() error {
	if e.ended {
		return nil
	}
	return e.Emit(ir.StreamEvent{Kind: ir.EventStreamEnd, StopReason: "stop", Usage: &e.usage})
}

func fromAnthStop(r string) string {
	switch r {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "max_tokens"
	default:
		return "stop"
	}
}

func toAnthStop(r string, hasTools bool) string {
	if hasTools || r == "tool_calls" {
		return "tool_use"
	}
	if r == "max_tokens" {
		return "max_tokens"
	}
	return "end_turn"
}

func encodeAnthUsage(u shared.TokenUsage) map[string]any {
	out := map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out["cache_read_input_tokens"] = u.CacheReadTokens
	}
	if u.CacheWriteTokens > 0 {
		out["cache_creation_input_tokens"] = u.CacheWriteTokens
	}
	return out
}
