package codec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"sync"

	"github.com/lidessen/crabbro/internal/gateway/protocol"
)

// SignatureOrigin identifies the upstream that minted an opaque thinking
// signature or redacted-thinking blob. Provider is a credential-free endpoint
// identity (scheme, host, and path) so two plans on the same vendor host but
// different endpoints never match by accident.
type SignatureOrigin struct {
	PlanID   string
	Provider string
	Protocol string
	Model    string
}

func (o SignatureOrigin) matches(target SignatureOrigin) bool {
	if o.PlanID == "" || target.PlanID == "" || o.PlanID != target.PlanID {
		return false
	}
	if o.Provider == "" || target.Provider == "" || o.Provider != target.Provider {
		return false
	}
	if o.Protocol == "" || o.Protocol != target.Protocol {
		return false
	}
	return o.Model != "" && o.Model == target.Model
}

// CandidateSignatureOrigin derives the provenance identity for a routed
// candidate without retaining endpoint credentials or query parameters.
func CandidateSignatureOrigin(cand interface {
	CodecOrigin() (planID, baseURL, protocol, model string)
}) SignatureOrigin {
	planID, baseURL, protocol, model := cand.CodecOrigin()
	return SignatureOrigin{PlanID: planID, Provider: endpointIdentity(baseURL), Protocol: protocol, Model: model}
}

func endpointIdentity(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/")
}

// SignatureProvenance records which upstream minted each opaque thinking
// signature. It is deliberately in-memory and bounded: after a restart every
// signature is treated as unknown, which safely degrades to stripping signed
// thinking instead of forwarding a possibly foreign signature.
type SignatureProvenance struct {
	mu      sync.RWMutex
	entries map[string]SignatureOrigin
	max     int
}

// DefaultSignatureProvenanceEntries bounds the in-memory provenance map.
const DefaultSignatureProvenanceEntries = 16384

// NewSignatureProvenance creates an empty bounded provenance registry.
func NewSignatureProvenance() *SignatureProvenance {
	return &SignatureProvenance{
		entries: make(map[string]SignatureOrigin, 128),
		max:     DefaultSignatureProvenanceEntries,
	}
}

func signatureKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Record stores the origin of one opaque signature/blob.
func (p *SignatureProvenance) Record(value string, origin SignatureOrigin) {
	if p == nil || value == "" || origin.PlanID == "" || origin.Provider == "" || origin.Protocol == "" || origin.Model == "" {
		return
	}
	key := signatureKey(value)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil {
		p.entries = make(map[string]SignatureOrigin, 128)
	}
	if p.max <= 0 {
		p.max = DefaultSignatureProvenanceEntries
	}
	if len(p.entries) >= p.max {
		// A full map means the process has served a very large number of
		// signatures. Forget everything rather than growing without bound;
		// subsequent client history degrades safely to stripped thinking.
		p.entries = make(map[string]SignatureOrigin, 128)
	}
	p.entries[key] = origin
}

// Matches reports whether value is known to have been minted by target.
func (p *SignatureProvenance) Matches(value string, target SignatureOrigin) bool {
	if p == nil || value == "" {
		return false
	}
	key := signatureKey(value)
	p.mu.RLock()
	origin, ok := p.entries[key]
	p.mu.RUnlock()
	return ok && origin.matches(target)
}

// RecordResponse registers every thinking signature / redacted blob in a
// complete (non-streaming) upstream response.
func (p *SignatureProvenance) RecordResponse(proto string, origin SignatureOrigin, body []byte) {
	if p == nil || len(body) == 0 {
		return
	}
	switch proto {
	case protocol.ProtocolAnthropic:
		recordAnthropicSignatures(p, origin, body)
	case protocol.ProtocolGemini:
		recordGeminiSignatures(p, origin, body)
	}
}

func recordAnthropicSignatures(p *SignatureProvenance, origin SignatureOrigin, body []byte) {
	var doc struct {
		Content []struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
			Data      string `json:"data"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return
	}
	for _, block := range doc.Content {
		switch block.Type {
		case "thinking":
			p.Record(block.Signature, origin)
		case "redacted_thinking":
			p.Record(block.Data, origin)
		}
	}
}

func recordGeminiSignatures(p *SignatureProvenance, origin SignatureOrigin, body []byte) {
	var doc struct {
		Candidates []struct {
			Content *struct {
				Parts []gemPart `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return
	}
	for _, cand := range doc.Candidates {
		if cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			p.Record(part.ThoughtSignature, origin)
		}
	}
}

// NormalizeSameProtocolBody sanitizes a native protocol request body before it
// is forwarded to a candidate using the same wire protocol. Anthropic signed
// thinking and Gemini thought-signature parts are preserved only when their
// provenance is known to match the target plan/provider/model; otherwise the
// thinking is removed so a foreign signature can never reach the wrong
// upstream. All other request fields are left intact.
func NormalizeSameProtocolBody(body []byte, proto string, target SignatureOrigin, prov *SignatureProvenance) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	switch proto {
	case protocol.ProtocolAnthropic:
		if !bytes.Contains(body, []byte(`"thinking"`)) && !bytes.Contains(body, []byte(`"signature"`)) && !bytes.Contains(body, []byte(`"redacted_thinking"`)) {
			return body, nil
		}
		return normalizeAnthropicBody(body, target, prov)
	case protocol.ProtocolGemini:
		if !bytes.Contains(body, []byte(`"thought"`)) && !bytes.Contains(body, []byte(`"thoughtSignature"`)) {
			return body, nil
		}
		return normalizeGeminiBody(body, target, prov)
	default:
		return body, nil
	}
}

func normalizeAnthropicBody(body []byte, target SignatureOrigin, prov *SignatureProvenance) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return body, err
	}
	rawMessages, ok := top["messages"]
	if !ok {
		return body, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return body, nil
	}
	changed := false
	keptMessages := make([]map[string]json.RawMessage, 0, len(messages))
	for _, message := range messages {
		rawContent, ok := message["content"]
		if !ok {
			keptMessages = append(keptMessages, message)
			continue
		}
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(rawContent, &blocks); err != nil {
			// String content cannot carry signed thinking blocks.
			keptMessages = append(keptMessages, message)
			continue
		}
		kept := make([]map[string]json.RawMessage, 0, len(blocks))
		for _, block := range blocks {
			switch rawString(block["type"]) {
			case "thinking":
				if !prov.Matches(rawString(block["signature"]), target) {
					changed = true
					continue
				}
			case "redacted_thinking":
				if !prov.Matches(rawString(block["data"]), target) {
					changed = true
					continue
				}
			}
			kept = append(kept, block)
		}
		if len(kept) == 0 {
			// A thinking-only turn must not become an empty assistant/user
			// message: upstream Anthropic schemas reject empty content arrays.
			changed = true
			continue
		}
		if len(kept) != len(blocks) {
			encoded, err := json.Marshal(kept)
			if err != nil {
				return body, err
			}
			message["content"] = encoded
		}
		keptMessages = append(keptMessages, message)
	}
	if !changed {
		return body, nil
	}
	encodedMessages, err := json.Marshal(keptMessages)
	if err != nil {
		return body, err
	}
	top["messages"] = encodedMessages
	return json.Marshal(top)
}

func normalizeGeminiBody(body []byte, target SignatureOrigin, prov *SignatureProvenance) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return body, err
	}
	rawContents, ok := top["contents"]
	if !ok {
		return body, nil
	}
	var contents []map[string]json.RawMessage
	if err := json.Unmarshal(rawContents, &contents); err != nil {
		return body, nil
	}
	changed := false
	keptContents := make([]map[string]json.RawMessage, 0, len(contents))
	for _, content := range contents {
		rawParts, ok := content["parts"]
		if !ok {
			keptContents = append(keptContents, content)
			continue
		}
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(rawParts, &parts); err != nil {
			keptContents = append(keptContents, content)
			continue
		}
		kept := make([]map[string]json.RawMessage, 0, len(parts))
		contentChanged := false
		for _, part := range parts {
			signature := rawString(part["thoughtSignature"])
			thought := rawBool(part["thought"])
			if thought {
				// A thought part is only replayable when its signature is
				// proven to belong to this exact target. Unsigned / foreign
				// thought text degrades to nothing rather than becoming an
				// unsupported or poisoned model turn.
				if !prov.Matches(signature, target) {
					changed = true
					contentChanged = true
					continue
				}
			} else if signature != "" && !prov.Matches(signature, target) {
				// Function-call thought signatures are optional for the
				// native model turn but must not be forwarded cross-provider.
				delete(part, "thoughtSignature")
				changed = true
				contentChanged = true
			}
			kept = append(kept, part)
		}
		if len(kept) == 0 {
			changed = true
			continue
		}
		if contentChanged {
			encoded, err := json.Marshal(kept)
			if err != nil {
				return body, err
			}
			content["parts"] = encoded
		}
		keptContents = append(keptContents, content)
	}
	if !changed {
		return body, nil
	}
	encodedContents, err := json.Marshal(keptContents)
	if err != nil {
		return body, err
	}
	top["contents"] = encodedContents
	return json.Marshal(top)
}

// SignatureStreamTap consumes raw upstream SSE bytes and records signature
// provenance from Anthropic/Gemini stream events. It never buffers more than
// one line's worth of incomplete data and never logs or exposes the values.
type SignatureStreamTap struct {
	proto  string
	origin SignatureOrigin
	prov   *SignatureProvenance

	pending  []byte
	event    string
	data     []string
	thinking map[int]*strings.Builder
}

// NewSignatureStreamTap creates a non-blocking SSE provenance recorder.
func NewSignatureStreamTap(proto string, origin SignatureOrigin, prov *SignatureProvenance) *SignatureStreamTap {
	return &SignatureStreamTap{
		proto:    proto,
		origin:   origin,
		prov:     prov,
		thinking: map[int]*strings.Builder{},
	}
}

// Write implements io.Writer over raw bytes copied downstream.
func (t *SignatureStreamTap) Write(p []byte) (int, error) {
	if t == nil {
		return len(p), nil
	}
	t.pending = append(t.pending, p...)
	for {
		idx := bytes.IndexByte(t.pending, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(t.pending[:idx]), "\r")
		t.pending = t.pending[idx+1:]
		t.line(line)
	}
	return len(p), nil
}

// Close flushes a final event that was not terminated by a blank line.
func (t *SignatureStreamTap) Close() {
	if t == nil {
		return
	}
	if len(t.pending) > 0 {
		line := strings.TrimRight(string(t.pending), "\r")
		t.pending = nil
		t.line(line)
	}
	t.dispatch()
	t.flushThinking()
}

func (t *SignatureStreamTap) flushThinking() {
	for index, builder := range t.thinking {
		t.prov.Record(builder.String(), t.origin)
		delete(t.thinking, index)
	}
}

func (t *SignatureStreamTap) line(line string) {
	switch {
	case line == "":
		t.dispatch()
	case strings.HasPrefix(line, ":"):
		// SSE comment / keepalive.
	case strings.HasPrefix(line, "event:"):
		t.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		t.data = append(t.data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
}

func (t *SignatureStreamTap) dispatch() {
	event := t.event
	data := strings.Join(t.data, "\n")
	t.event = ""
	t.data = nil
	if data == "" || data == "[DONE]" {
		return
	}
	switch t.proto {
	case protocol.ProtocolAnthropic:
		t.observeAnthropic(event, data)
	case protocol.ProtocolGemini:
		t.observeGemini(data)
	}
}

func (t *SignatureStreamTap) observeAnthropic(event, data string) {
	var doc struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock *struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
			Data      string `json:"data"`
		} `json:"content_block"`
		Delta *struct {
			Type      string `json:"type"`
			Signature string `json:"signature"`
		} `json:"delta"`
	}
	if json.Unmarshal([]byte(data), &doc) != nil {
		return
	}
	typ := strings.TrimSpace(firstNonEmpty(doc.Type, event))
	switch typ {
	case "content_block_start":
		if doc.ContentBlock != nil {
			switch doc.ContentBlock.Type {
			case "thinking":
				t.prov.Record(doc.ContentBlock.Signature, t.origin)
			case "redacted_thinking":
				t.prov.Record(doc.ContentBlock.Data, t.origin)
			}
		}
	case "content_block_delta":
		if doc.Delta != nil && doc.Delta.Type == "signature_delta" && doc.Delta.Signature != "" {
			builder := t.thinking[doc.Index]
			if builder == nil {
				builder = &strings.Builder{}
				t.thinking[doc.Index] = builder
			}
			builder.WriteString(doc.Delta.Signature)
		}
	case "content_block_stop":
		if builder := t.thinking[doc.Index]; builder != nil {
			t.prov.Record(builder.String(), t.origin)
			delete(t.thinking, doc.Index)
		}
	case "message_stop":
		t.flushThinking()
	}
}

func (t *SignatureStreamTap) observeGemini(data string) {
	var doc struct {
		Candidates []struct {
			Content *struct {
				Parts []gemPart `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal([]byte(data), &doc) != nil {
		return
	}
	for _, cand := range doc.Candidates {
		if cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			t.prov.Record(part.ThoughtSignature, t.origin)
		}
	}
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func rawBool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	return false
}
