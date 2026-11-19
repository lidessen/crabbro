package codec

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/shared"
)

type oaResponsesCodec struct{}

func (oaResponsesCodec) DecodeRequest(body []byte, ingress IngressInfo) (*ir.GenRequest, error) {
	var raw struct {
		Model           string          `json:"model"`
		Instructions    string          `json:"instructions"`
		Input           json.RawMessage `json:"input"`
		Tools           []struct {
			Type        string         `json:"type"`
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Parameters  map[string]any `json:"parameters"`
		} `json:"tools"`
		ToolChoice      any      `json:"tool_choice"`
		MaxOutputTokens int      `json:"max_output_tokens"`
		Temperature     *float64 `json:"temperature"`
		Stream          bool     `json:"stream"`
		Reasoning       *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Text *struct {
			Format *struct {
				Type   string         `json:"type"`
				Name   string         `json:"name"`
				Schema map[string]any `json:"schema"`
			} `json:"format"`
		} `json:"text"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode openai-responses request: %w", err)
	}
	out := &ir.GenRequest{
		Model:       firstNonEmpty(raw.Model, ingress.RequestedModel),
		MaxTokens:   raw.MaxOutputTokens,
		Temperature: raw.Temperature,
		Stream:      raw.Stream || ingress.Stream,
		SessionHint: ingress.SessionHint,
	}
	if raw.Instructions != "" {
		out.System = append(out.System, ir.ContentPart{Kind: ir.PartText, Text: raw.Instructions})
	}
	if raw.Reasoning != nil && raw.Reasoning.Effort != "" {
		out.Thinking = &ir.ThinkingConfig{Enabled: true, Effort: raw.Reasoning.Effort}
	}
	if raw.Text != nil && raw.Text.Format != nil && raw.Text.Format.Type == "json_schema" {
		out.SchemaName = raw.Text.Format.Name
		out.ResponseSchema = raw.Text.Format.Schema
	}
	for _, t := range raw.Tools {
		out.Tools = append(out.Tools, ir.ToolDecl{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.Parameters,
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
			if name, _ := tc["name"].(string); name != "" {
				out.ToolChoice = &ir.ToolChoice{Mode: "tool", Name: name}
			}
		}
	}

	var inputStr string
	if len(raw.Input) > 0 && raw.Input[0] == '"' && json.Unmarshal(raw.Input, &inputStr) == nil {
		if inputStr != "" {
			out.Messages = append(out.Messages, ir.Message{
				Role:  "user",
				Parts: []ir.ContentPart{{Kind: ir.PartText, Text: inputStr}},
			})
		}
		return out, nil
	}

	var items []map[string]any
	if len(raw.Input) > 0 && json.Unmarshal(raw.Input, &items) == nil {
		for _, item := range items {
			typ, _ := item["type"].(string)
			switch typ {
			case "function_call":
				callID, _ := item["call_id"].(string)
				name, _ := item["name"].(string)
				args, _ := item["arguments"].(string)
				if strings.TrimSpace(args) == "" {
					args = "{}"
				}
				part := ir.ContentPart{
					Kind:      ir.PartToolUse,
					ToolID:    callID,
					ToolName:  name,
					InputJSON: json.RawMessage(args),
				}
				if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == "assistant" {
					out.Messages[n-1].Parts = append(out.Messages[n-1].Parts, part)
				} else {
					out.Messages = append(out.Messages, ir.Message{Role: "assistant", Parts: []ir.ContentPart{part}})
				}
			case "function_call_output":
				callID, _ := item["call_id"].(string)
				outStr := anyToString(item["output"])
				out.Messages = append(out.Messages, ir.Message{
					Role: "tool",
					Parts: []ir.ContentPart{{
						Kind:   ir.PartToolResult,
						ToolID: callID,
						Text:   outStr,
					}},
				})
			default:
				role, _ := item["role"].(string)
				parts := decodeOAResponsesParts(item["content"])
				switch strings.ToLower(role) {
				case "system", "developer":
					out.System = append(out.System, parts...)
				case "assistant":
					if len(parts) > 0 {
						out.Messages = append(out.Messages, ir.Message{Role: "assistant", Parts: parts})
					}
				default:
					if len(parts) > 0 {
						out.Messages = append(out.Messages, ir.Message{Role: "user", Parts: parts})
					}
				}
			}
		}
	}
	return out, nil
}

func decodeOAResponsesParts(c any) []ir.ContentPart {
	switch v := c.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []ir.ContentPart{{Kind: ir.PartText, Text: v}}
	case []any:
		var out []ir.ContentPart
		for _, raw := range v {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "input_text", "output_text", "text":
				if txt, _ := m["text"].(string); txt != "" {
					out = append(out, ir.ContentPart{Kind: ir.PartText, Text: txt})
				}
			case "input_image":
				if u, _ := m["image_url"].(string); u != "" {
					out = append(out, imagePartFromURL(u))
				}
			}
		}
		return out
	}
	return nil
}

func (oaResponsesCodec) EncodeRequest(req *ir.GenRequest) ([]byte, error) {
	items := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "tool":
			for _, p := range m.Parts {
				if p.Kind == ir.PartToolResult {
					items = append(items, map[string]any{
						"type":    "function_call_output",
						"call_id": p.ToolID,
						"output":  p.Text,
					})
				}
			}
		case "assistant":
			var txt strings.Builder
			for _, p := range m.Parts {
				switch p.Kind {
				case ir.PartText:
					txt.WriteString(p.Text)
				case ir.PartToolUse:
					if txt.Len() > 0 {
						items = append(items, map[string]any{
							"type":    "message",
							"role":    "assistant",
							"content": []map[string]any{{"type": "output_text", "text": txt.String()}},
						})
						txt.Reset()
					}
					args := strings.TrimSpace(string(p.InputJSON))
					if args == "" {
						args = "{}"
					}
					items = append(items, map[string]any{
						"type":      "function_call",
						"call_id":   p.ToolID,
						"name":      p.ToolName,
						"arguments": args,
					})
				}
			}
			if txt.Len() > 0 {
				items = append(items, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": []map[string]any{{"type": "output_text", "text": txt.String()}},
				})
			}
		default:
			var toolResults []ir.ContentPart
			var userContent []map[string]any
			for _, p := range m.Parts {
				switch p.Kind {
				case ir.PartToolResult:
					toolResults = append(toolResults, p)
				case ir.PartText:
					userContent = append(userContent, map[string]any{"type": "input_text", "text": p.Text})
				case ir.PartImage:
					userContent = append(userContent, map[string]any{"type": "input_image", "image_url": imagePartToDataURL(p)})
				}
			}
			for _, tr := range toolResults {
				items = append(items, map[string]any{
					"type":    "function_call_output",
					"call_id": tr.ToolID,
					"output":  tr.Text,
				})
			}
			if len(userContent) > 0 {
				items = append(items, map[string]any{
					"type":    "message",
					"role":    "user",
					"content": userContent,
				})
			}
		}
	}

	payload := map[string]any{
		"model": req.Model,
		"input": items,
	}
	if len(req.System) > 0 {
		payload["instructions"] = partsText(req.System)
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool := map[string]any{"type": "function", "name": t.Name}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			if t.Parameters != nil {
				tool["parameters"] = t.Parameters
			}
			tools = append(tools, tool)
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
			payload["tool_choice"] = map[string]any{"type": "function", "name": req.ToolChoice.Name}
		}
	}
	if req.MaxTokens > 0 {
		payload["max_output_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.Thinking != nil && req.Thinking.Enabled && req.Thinking.Effort != "" {
		payload["reasoning"] = map[string]any{"effort": req.Thinking.Effort}
	}
	if len(req.ResponseSchema) > 0 {
		payload["text"] = map[string]any{
			"format": map[string]any{
				"type":   "json_schema",
				"name":   firstNonEmpty(req.SchemaName, "response"),
				"strict": true,
				"schema": req.ResponseSchema,
			},
		}
	}
	if req.Stream {
		payload["stream"] = true
	}
	mergeExtraFields(payload, req.ExtraBody)
	return json.Marshal(payload)
}

func (oaResponsesCodec) DecodeResponse(body []byte) (*ir.GenResponse, error) {
	var doc struct {
		ID                string `json:"id"`
		Model             string `json:"model"`
		Status            string `json:"status"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Output []struct {
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Arguments string `json:"arguments"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage *oaResponsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode openai-responses response: %w", err)
	}
	out := &ir.GenResponse{
		ID:         doc.ID,
		Model:      doc.Model,
		StopReason: "stop",
	}
	if doc.Usage != nil {
		out.Usage = doc.Usage.toTokenUsage()
	}
	hasTools := false
	for _, item := range doc.Output {
		switch item.Type {
		case "reasoning":
			for _, s := range item.Summary {
				if s.Text != "" {
					out.Content = append(out.Content, ir.ContentPart{Kind: ir.PartThinking, Text: s.Text})
				}
			}
		case "message":
			for _, c := range item.Content {
				if c.Text != "" {
					out.Content = append(out.Content, ir.ContentPart{Kind: ir.PartText, Text: c.Text})
				}
			}
		case "function_call":
			hasTools = true
			args := json.RawMessage(strings.TrimSpace(item.Arguments))
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			out.Content = append(out.Content, ir.ContentPart{
				Kind:      ir.PartToolUse,
				ToolID:    item.CallID,
				ToolName:  item.Name,
				InputJSON: args,
			})
		}
	}
	switch {
	case hasTools:
		out.StopReason = "tool_calls"
	case doc.IncompleteDetails != nil && doc.IncompleteDetails.Reason == "max_output_tokens":
		out.StopReason = "max_tokens"
	case doc.IncompleteDetails != nil && doc.IncompleteDetails.Reason == "content_filter":
		out.StopReason = "content_filter"
	}
	return out, nil
}

func (oaResponsesCodec) EncodeResponse(resp *ir.GenResponse) ([]byte, error) {
	var output []map[string]any
	var textBuf, thinkBuf strings.Builder
	for _, p := range resp.Content {
		switch p.Kind {
		case ir.PartThinking:
			thinkBuf.WriteString(p.Text)
		case ir.PartText:
			textBuf.WriteString(p.Text)
		case ir.PartToolUse:
			args := strings.TrimSpace(string(p.InputJSON))
			if args == "" {
				args = "{}"
			}
			output = append(output, map[string]any{
				"id":        "fc_" + p.ToolID,
				"type":      "function_call",
				"status":    "completed",
				"call_id":   p.ToolID,
				"name":      p.ToolName,
				"arguments": args,
			})
		}
	}
	if thinkBuf.Len() > 0 {
		output = append([]map[string]any{{
			"id":      "rs_crabbro",
			"type":    "reasoning",
			"summary": []map[string]any{{"type": "summary_text", "text": thinkBuf.String()}},
		}}, output...)
	}
	if textBuf.Len() > 0 || len(output) == 0 {
		output = append(output, map[string]any{
			"id":     "msg_crabbro",
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": textBuf.String(),
			}},
		})
	}
	res := map[string]any{
		"id":     firstNonEmpty(resp.ID, "resp_crabbro"),
		"object": "response",
		"model":  resp.Model,
		"status": "completed",
		"output": output,
		"usage":  encodeOAResponsesUsage(resp.Usage),
	}
	if resp.StopReason == "max_tokens" {
		res["status"] = "incomplete"
		res["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return json.Marshal(res)
}

func (oaResponsesCodec) NewStreamDecoder() streamDecoder {
	return &oaRespStreamDec{
		toolCalls: map[string]int{},
		argSent:   map[int]bool{},
	}
}

type oaRespStreamDec struct {
	started     bool
	ended       bool
	id, model   string
	usage       shared.TokenUsage
	stopReason  string
	toolCalls   map[string]int
	argSent     map[int]bool
	nextToolIdx int
}

func (d *oaRespStreamDec) Feed(evName, data string) ([]ir.StreamEvent, bool, error) {
	trim := strings.TrimSpace(data)
	if trim == "" || trim == "[DONE]" {
		return nil, false, nil
	}
	var doc struct {
		Type        string `json:"type"`
		Delta       string `json:"delta"`
		ItemID      string `json:"item_id"`
		OutputIndex int    `json:"output_index"`
		Response    *struct {
			ID                string            `json:"id"`
			Model             string            `json:"model"`
			Usage             *oaResponsesUsage `json:"usage"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
		Item *struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(trim), &doc); err != nil {
		return nil, false, nil
	}
	typ := firstNonEmpty(doc.Type, evName)
	var evs []ir.StreamEvent
	ensureStart := func() {
		if !d.started {
			d.started = true
			evs = append(evs, ir.StreamEvent{Kind: ir.EventStreamStart, ID: d.id, Model: d.model})
		}
	}

	switch typ {
	case "response.created":
		if doc.Response != nil {
			d.id = firstNonEmpty(doc.Response.ID, d.id)
			d.model = firstNonEmpty(doc.Response.Model, d.model)
		}
		ensureStart()
	case "response.output_text.delta":
		ensureStart()
		if doc.Delta != "" {
			evs = append(evs, ir.StreamEvent{Kind: ir.EventTextDelta, Text: doc.Delta})
		}
	case "response.reasoning_summary_text.delta":
		ensureStart()
		if doc.Delta != "" {
			evs = append(evs, ir.StreamEvent{Kind: ir.EventThinkingDelta, Text: doc.Delta})
		}
	case "response.output_item.added":
		ensureStart()
		if doc.Item != nil && doc.Item.Type == "function_call" {
			idx := d.nextToolIdx
			d.nextToolIdx++
			if doc.Item.ID != "" {
				d.toolCalls[doc.Item.ID] = idx
			}
			evs = append(evs, ir.StreamEvent{
				Kind:     ir.EventToolCallStart,
				Index:    idx,
				ToolID:   firstNonEmpty(doc.Item.CallID, doc.Item.ID, fmt.Sprintf("call_%d", idx)),
				ToolName: doc.Item.Name,
			})
		}
	case "response.function_call_arguments.delta":
		ensureStart()
		idx, ok := d.toolCalls[doc.ItemID]
		if !ok {
			idx = d.nextToolIdx
			d.nextToolIdx++
			if doc.ItemID != "" {
				d.toolCalls[doc.ItemID] = idx
			}
		}
		if doc.Delta != "" {
			d.argSent[idx] = true
			evs = append(evs, ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: idx, Text: doc.Delta})
		}
	case "response.output_item.done":
		if doc.Item != nil && doc.Item.Type == "function_call" {
			idx, ok := d.toolCalls[doc.Item.ID]
			if !ok {
				idx = d.nextToolIdx
				d.nextToolIdx++
				evs = append(evs, ir.StreamEvent{
					Kind:     ir.EventToolCallStart,
					Index:    idx,
					ToolID:   firstNonEmpty(doc.Item.CallID, doc.Item.ID, fmt.Sprintf("call_%d", idx)),
					ToolName: doc.Item.Name,
				})
			}
			if !d.argSent[idx] && doc.Item.Arguments != "" {
				d.argSent[idx] = true
				evs = append(evs, ir.StreamEvent{Kind: ir.EventToolCallDelta, Index: idx, Text: doc.Item.Arguments})
			}
			evs = append(evs, ir.StreamEvent{Kind: ir.EventToolCallEnd, Index: idx})
		}
	case "response.completed", "response.incomplete":
		if doc.Response != nil {
			if doc.Response.Usage != nil {
				mergeMaxUsage(&d.usage, doc.Response.Usage.toTokenUsage())
				uCopy := d.usage
				evs = append(evs, ir.StreamEvent{Kind: ir.EventUsageDelta, Usage: &uCopy})
			}
			if doc.Response.IncompleteDetails != nil && doc.Response.IncompleteDetails.Reason == "max_output_tokens" {
				d.stopReason = "max_tokens"
			}
		}
		evs = append(evs, d.finish()...)
		return evs, true, nil
	}
	return evs, false, nil
}

func (d *oaRespStreamDec) Flush() []ir.StreamEvent { return d.finish() }

func (d *oaRespStreamDec) finish() []ir.StreamEvent {
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
		ID:         d.id,
		Model:      d.model,
		StopReason: sr,
		Usage:      &uCopy,
	}}
}

func (oaResponsesCodec) NewStreamEncoder(requestedModel string, dst io.Writer) streamEncoder {
	return &oaRespStreamEnc{
		model:    requestedModel,
		dst:      dst,
		toolIDs:  map[int]string{},
		toolName: map[int]string{},
		toolArgs: map[int]*strings.Builder{},
	}
}

type oaRespStreamEnc struct {
	dst        io.Writer
	id, model  string
	sentStart  bool
	sentText   bool
	ended      bool
	usage      shared.TokenUsage
	textBuf    strings.Builder
	toolIDs    map[int]string
	toolName   map[int]string
	toolArgs   map[int]*strings.Builder
}

func (e *oaRespStreamEnc) ensureStart() error {
	if e.sentStart {
		return nil
	}
	e.sentStart = true
	return writeSSE(e.dst, "response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     firstNonEmpty(e.id, "resp_crabbro"),
			"object": "response",
			"model":  e.model,
			"status": "in_progress",
		},
	})
}

func (e *oaRespStreamEnc) Emit(ev ir.StreamEvent) error {
	if ev.ID != "" && e.id == "" {
		e.id = ev.ID
	}
	if ev.Model != "" && e.model == "" {
		e.model = ev.Model
	}
	switch ev.Kind {
	case ir.EventStreamStart:
		return e.ensureStart()
	case ir.EventThinkingDelta:
		if err := e.ensureStart(); err != nil {
			return err
		}
		return writeSSE(e.dst, "response.reasoning_summary_text.delta", map[string]any{
			"type":  "response.reasoning_summary_text.delta",
			"delta": ev.Text,
		})
	case ir.EventTextDelta:
		if err := e.ensureStart(); err != nil {
			return err
		}
		if !e.sentText {
			e.sentText = true
			_ = writeSSE(e.dst, "response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": 0,
				"item": map[string]any{
					"id":      "msg_crabbro",
					"type":    "message",
					"role":    "assistant",
					"content": []any{},
				},
			})
		}
		e.textBuf.WriteString(ev.Text)
		return writeSSE(e.dst, "response.output_text.delta", map[string]any{
			"type":         "response.output_text.delta",
			"item_id":      "msg_crabbro",
			"output_index": 0,
			"delta":        ev.Text,
		})
	case ir.EventToolCallStart:
		if err := e.ensureStart(); err != nil {
			return err
		}
		e.toolIDs[ev.Index] = ev.ToolID
		e.toolName[ev.Index] = ev.ToolName
		e.toolArgs[ev.Index] = &strings.Builder{}
		return writeSSE(e.dst, "response.output_item.added", map[string]any{
			"type": "response.output_item.added",
			"item": map[string]any{
				"id":      fmt.Sprintf("fc_%d", ev.Index),
				"type":    "function_call",
				"call_id": ev.ToolID,
				"name":    ev.ToolName,
			},
		})
	case ir.EventToolCallDelta:
		if b, ok := e.toolArgs[ev.Index]; ok {
			b.WriteString(ev.Text)
		}
		return writeSSE(e.dst, "response.function_call_arguments.delta", map[string]any{
			"type":    "response.function_call_arguments.delta",
			"item_id": fmt.Sprintf("fc_%d", ev.Index),
			"delta":   ev.Text,
		})
	case ir.EventToolCallEnd:
		args := "{}"
		if b, ok := e.toolArgs[ev.Index]; ok && b.Len() > 0 {
			args = b.String()
		}
		return writeSSE(e.dst, "response.output_item.done", map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{
				"id":        fmt.Sprintf("fc_%d", ev.Index),
				"type":      "function_call",
				"call_id":   e.toolIDs[ev.Index],
				"name":      e.toolName[ev.Index],
				"arguments": args,
				"status":    "completed",
			},
		})
	case ir.EventUsageDelta:
		if ev.Usage != nil {
			mergeMaxUsage(&e.usage, *ev.Usage)
		}
	case ir.EventStreamEnd:
		if ev.Usage != nil {
			mergeMaxUsage(&e.usage, *ev.Usage)
		}
		if err := e.ensureStart(); err != nil {
			return err
		}
		e.ended = true
		var outputs []map[string]any
		if e.sentText {
			outputs = append(outputs, map[string]any{
				"id":     "msg_crabbro",
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{{
					"type": "output_text",
					"text": e.textBuf.String(),
				}},
			})
		}
		for idx, id := range e.toolIDs {
			args := "{}"
			if b, ok := e.toolArgs[idx]; ok && b.Len() > 0 {
				args = b.String()
			}
			outputs = append(outputs, map[string]any{
				"id":        fmt.Sprintf("fc_%d", idx),
				"type":      "function_call",
				"call_id":   id,
				"name":      e.toolName[idx],
				"arguments": args,
				"status":    "completed",
			})
		}
		return writeSSE(e.dst, "response.completed", map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":     firstNonEmpty(e.id, "resp_crabbro"),
				"object": "response",
				"model":  e.model,
				"status": "completed",
				"output": outputs,
				"usage":  encodeOAResponsesUsage(e.usage),
			},
		})
	}
	return nil
}

func (e *oaRespStreamEnc) Close() error {
	if e.ended {
		return nil
	}
	return e.Emit(ir.StreamEvent{Kind: ir.EventStreamEnd, StopReason: "stop", Usage: &e.usage})
}

func encodeOAResponsesUsage(u shared.TokenUsage) map[string]any {
	out := map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"total_tokens":  u.InputTokens + u.OutputTokens,
	}
	if u.CacheReadTokens > 0 {
		out["input_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		out["output_tokens_details"] = map[string]any{"reasoning_tokens": u.ReasoningTokens}
	}
	return out
}
