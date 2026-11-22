package codec

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/gateway/router"
	"github.com/lidessen/crabbro/internal/shared"
)

type protocolCodec interface {
	DecodeRequest(body []byte, ingress IngressInfo) (*ir.GenRequest, error)
	EncodeRequest(req *ir.GenRequest) ([]byte, error)
	DecodeResponse(body []byte) (*ir.GenResponse, error)
	EncodeResponse(resp *ir.GenResponse) ([]byte, error)
	NewStreamDecoder() streamDecoder
	NewStreamEncoder(requestedModel string, dst io.Writer) streamEncoder
}

type streamDecoder interface {
	Feed(event, data string) ([]ir.StreamEvent, bool, error)
	Flush() []ir.StreamEvent
}

type streamEncoder interface {
	Emit(ev ir.StreamEvent) error
	Close() error
}

// Bridge translates between wire protocols and DecisionIR <-> GenIR using canonical IRs.
type Bridge struct{}

// NewBridge constructs a cross-protocol BridgeHandler.
func NewBridge() *Bridge { return &Bridge{} }

type bridgeState struct {
	origDecisionReq *ir.DecisionRequest
	ingressProto    string
	upstreamProto   string
	requestedModel  string
	targetModel     string
}

func codecFor(proto string) (protocolCodec, error) {
	switch proto {
	case config.ProtocolOpenAIChat:
		return oaChatCodec{}, nil
	case config.ProtocolOpenAIResponses:
		return oaResponsesCodec{}, nil
	case config.ProtocolAnthropic:
		return anthropicCodec{}, nil
	case config.ProtocolGemini:
		return geminiCodec{}, nil
	default:
		return nil, fmt.Errorf("no generative codec for protocol %q", proto)
	}
}

// BuildUpstreamBody converts an incoming HTTP or Decision request into the upstream
// target's native protocol JSON payload, stripping cross-provider vendor signatures.
func (b *Bridge) BuildUpstreamBody(cand router.Candidate, ingress IngressInfo, rawBody []byte) ([]byte, any, error) {
	if ingress.Action == "count_tokens" && cand.EndpointProto != config.ProtocolAnthropic {
		return nil, nil, fmt.Errorf("count_tokens is only supported by native anthropic targets")
	}
	st := &bridgeState{
		ingressProto:   ingress.Protocol,
		upstreamProto:  cand.EndpointProto,
		requestedModel: ingress.RequestedModel,
		targetModel:    cand.Target.Model,
	}

	var greq *ir.GenRequest
	var err error
	if ingress.Family == config.FamilyDecision {
		var dreq *ir.DecisionRequest
		var decErr error
		if ingress.Protocol == config.ProtocolOpenAIDecisions {
			dreq, decErr = DecodeOpenAIDecisionsRequest(rawBody)
		} else {
			dreq, decErr = DecodeDecisionRequest(rawBody)
		}
		if decErr != nil {
			return nil, nil, decErr
		}
		st.origDecisionReq = dreq
		if config.ProtocolFamily(cand.EndpointProto) == config.FamilyDecision {
			copyReq := *dreq
			copyReq.Model = cand.Target.Model
			var body []byte
			switch cand.EndpointProto {
			case config.ProtocolOpenAIDecisions:
				body, err = EncodeOpenAIDecisionsRequest(&copyReq)
			case config.ProtocolGeminiDecisions:
				body, err = EncodeGeminiDecisionRequest(&copyReq, cand.Target.Model)
			default:
				body, err = EncodeDecisionRequest(&copyReq)
			}
			if err != nil {
				return nil, nil, err
			}
			if cand.ModelSpec != nil {
				body, err = PatchModelAndExtraBody(body, cand.EndpointProto, cand.Target.Model, cand.ModelSpec.ExtraBody)
			}
			return body, st, err
		}
		greq, err = ir.DecisionToGenRequest(dreq, cand.Target.Model)
		if err != nil {
			return nil, nil, err
		}
	} else {
		inCodec, inErr := codecFor(ingress.Protocol)
		if inErr != nil {
			return nil, nil, inErr
		}
		greq, err = inCodec.DecodeRequest(rawBody, ingress)
		if err != nil {
			return nil, nil, err
		}
		greq.Model = cand.Target.Model
		greq.Stream = ingress.Stream
		if ingress.Protocol != cand.EndpointProto {
			stripForeignThinkingSignatures(greq)
		}
	}

	if cand.ModelSpec != nil && len(cand.ModelSpec.ExtraBody) > 0 {
		greq.ExtraBody = cand.ModelSpec.ExtraBody
	}
	upCodec, err := codecFor(cand.EndpointProto)
	if err != nil {
		return nil, nil, err
	}
	upBody, err := upCodec.EncodeRequest(greq)
	if err != nil {
		return nil, nil, err
	}
	return upBody, st, nil
}

// TranslateResponse decodes the upstream response and re-encodes it into the ingress format.
func (b *Bridge) TranslateResponse(cand router.Candidate, ingress IngressInfo, state any, status int, hdr http.Header, body []byte) (int, http.Header, []byte, shared.TokenUsage, error) {
	if st, ok := state.(*bridgeState); ok && st.origDecisionReq != nil && config.ProtocolFamily(cand.EndpointProto) == config.FamilyDecision {
		dresp, err := b.DecodeDecisionResponse(cand, state, body)
		if err != nil {
			return status, nil, nil, shared.TokenUsage{}, err
		}
		var out []byte
		if ingress.Protocol == config.ProtocolOpenAIDecisions {
			out, err = EncodeOpenAIDecisionsResponse(dresp)
		} else {
			out, err = EncodeDecisionResponse(dresp)
		}
		h := make(http.Header)
		h.Set("Content-Type", "application/json")
		return status, h, out, dresp.Usage, err
	}
	upCodec, err := codecFor(cand.EndpointProto)
	if err != nil {
		return status, nil, nil, shared.TokenUsage{}, err
	}
	gresp, err := upCodec.DecodeResponse(body)
	if err != nil {
		return status, nil, nil, shared.TokenUsage{}, err
	}
	if gresp.Model == "" {
		gresp.Model = firstNonEmpty(ingress.RequestedModel, cand.Target.Model)
	}

	outHdr := make(http.Header)
	outHdr.Set("Content-Type", "application/json")

	if st, ok := state.(*bridgeState); ok && st.origDecisionReq != nil {
		dresp, dErr := ir.GenToDecisionResponse(st.origDecisionReq, gresp)
		if dErr != nil {
			return status, nil, nil, gresp.Usage, dErr
		}
		var outBody []byte
		var eErr error
		if ingress.Protocol == config.ProtocolOpenAIDecisions {
			outBody, eErr = EncodeOpenAIDecisionsResponse(dresp)
		} else {
			outBody, eErr = EncodeDecisionResponse(dresp)
		}
		if eErr != nil {
			return status, nil, nil, gresp.Usage, eErr
		}
		return http.StatusOK, outHdr, outBody, gresp.Usage, nil
	}

	inCodec, err := codecFor(ingress.Protocol)
	if err != nil {
		return status, nil, nil, gresp.Usage, err
	}
	outBody, err := inCodec.EncodeResponse(gresp)
	if err != nil {
		return status, nil, nil, gresp.Usage, err
	}
	return status, outHdr, outBody, gresp.Usage, nil
}

// TranslateStream streams upstream SSE events through the upstream decoder -> GenIR -> ingress encoder.
func (b *Bridge) TranslateStream(cand router.Candidate, ingress IngressInfo, _ any, peeked []byte, src io.ReadCloser, dst io.Writer, flusher http.Flusher) (shared.TokenUsage, error) {
	upCodec, err := codecFor(cand.EndpointProto)
	if err != nil {
		return shared.TokenUsage{}, err
	}
	inCodec, err := codecFor(ingress.Protocol)
	if err != nil {
		return shared.TokenUsage{}, err
	}
	dec := upCodec.NewStreamDecoder()
	enc := inCodec.NewStreamEncoder(firstNonEmpty(ingress.RequestedModel, cand.Target.Model), dst)

	var totalUsage shared.TokenUsage
	emitAll := func(evs []ir.StreamEvent) error {
		for _, ev := range evs {
			if ev.Usage != nil {
				mergeMaxUsage(&totalUsage, *ev.Usage)
			}
			if err := enc.Emit(ev); err != nil {
				return err
			}
		}
		if flusher != nil && len(evs) > 0 {
			flusher.Flush()
		}
		return nil
	}

	reader := io.MultiReader(bytes.NewReader(peeked), src)
	if err := readSSEEvents(reader, func(event, data string) (bool, error) {
		evs, done, decErr := dec.Feed(event, data)
		if decErr != nil {
			return true, decErr
		}
		if err := emitAll(evs); err != nil {
			return true, err
		}
		return done, nil
	}); err != nil {
		return totalUsage, err
	}

	if err := emitAll(dec.Flush()); err != nil {
		return totalUsage, err
	}
	if err := enc.Close(); err != nil {
		return totalUsage, err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return totalUsage, nil
}

// EncodeDecision serializes a DecisionRequest for native TypeSafe Jev dispatch.
func (b *Bridge) EncodeDecision(dreq *ir.DecisionRequest) ([]byte, error) {
	return EncodeDecisionRequest(dreq)
}

// DecodeDecisionResponse converts an emulated generative response back into DecisionResponse.
func (b *Bridge) DecodeDecisionResponse(cand router.Candidate, state any, body []byte) (*ir.DecisionResponse, error) {
	st, ok := state.(*bridgeState)
	if !ok || st.origDecisionReq == nil {
		return nil, fmt.Errorf("missing original decision request state")
	}
	if cand.EndpointProto == config.ProtocolGeminiDecisions {
		return DecodeGeminiDecisionResponse(body, st.origDecisionReq, cand.Target.Model)
	}
	if cand.EndpointProto == config.ProtocolOpenAIDecisions {
		return DecodeOpenAIDecisionsResponse(body, st.origDecisionReq)
	}
	if cand.EndpointProto == config.ProtocolTypeSafeJev {
		var d ir.DecisionResponse
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		for k := range st.origDecisionReq.Questions {
			if _, ok := d.Answers[k]; !ok {
				return nil, fmt.Errorf("missing decision answer %q", k)
			}
		}
		d.Mode = "native"
		d.Calibrated = true
		d.Usage = ExtractUsageJSON(cand.EndpointProto, body)
		return &d, nil
	}
	upCodec, err := codecFor(cand.EndpointProto)
	if err != nil {
		return nil, err
	}
	gresp, err := upCodec.DecodeResponse(body)
	if err != nil {
		return nil, err
	}
	dresp, err := ir.GenToDecisionResponse(st.origDecisionReq, gresp)
	if err != nil {
		return nil, err
	}
	dresp.Calibrated = false
	dresp.Mode = "emulated"
	dresp.Usage = gresp.Usage
	return dresp, nil
}

func readSSEEvents(r io.Reader, fn func(event, data string) (done bool, err error)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var event string
	var dataLines []string
	dispatch := func() (bool, error) {
		if len(dataLines) == 0 {
			event = ""
			return false, nil
		}
		ev := event
		data := strings.Join(dataLines, "\n")
		event = ""
		dataLines = nil
		return fn(ev, data)
	}

	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			done, err := dispatch()
			if err != nil || done {
				return err
			}
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	_, err := dispatch()
	return err
}

func writeSSE(w io.Writer, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if event != "" {
		buf.WriteString("event: ")
		buf.WriteString(event)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(b)
	buf.WriteString("\n\n")
	_, err = w.Write(buf.Bytes())
	return err
}

func stripForeignThinkingSignatures(req *ir.GenRequest) {
	if req == nil {
		return
	}
	for i := range req.System {
		req.System[i].Signature = ""
		req.System[i].RedactedData = ""
	}
	for mi := range req.Messages {
		parts := req.Messages[mi].Parts
		kept := parts[:0]
		for pi := range parts {
			switch parts[pi].Kind {
			case ir.PartThinking:
				// The target protocol/provider cannot prove the signature
				// origin, so encoders must see unsigned thinking (and drop it
				// when the target cannot consume unsigned reasoning).
				parts[pi].Signature = ""
			case ir.PartRedactedThinking:
				// Never carry an opaque encrypted blob across protocols.
				continue
			}
			kept = append(kept, parts[pi])
		}
		req.Messages[mi].Parts = kept
	}
}

func mergeExtraFields(payload, extra map[string]any) {
	for k, v := range extra {
		if _, exists := payload[k]; !exists {
			payload[k] = v
		}
	}
}

func partsText(parts []ir.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == ir.PartText && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func imagePartFromURL(u string) ir.ContentPart {
	if strings.HasPrefix(u, "data:") {
		rest := strings.TrimPrefix(u, "data:")
		if semi := strings.Index(rest, ";base64,"); semi > 0 {
			return ir.ContentPart{
				Kind:      ir.PartImage,
				MediaType: rest[:semi],
				Data:      rest[semi+len(";base64,"):],
			}
		}
	}
	return ir.ContentPart{Kind: ir.PartImage, URL: u}
}

func imagePartToDataURL(p ir.ContentPart) string {
	if p.URL != "" {
		return p.URL
	}
	return "data:" + firstNonEmpty(p.MediaType, "image/png") + ";base64," + p.Data
}

func normalizeJSONObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil && m != nil {
		return m
	}
	return map[string]any{}
}

func parseStringOrSlice(v any) []string {
	switch s := v.(type) {
	case string:
		if s != "" {
			return []string{s}
		}
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	case []string:
		return s
	}
	return nil
}

func anyToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
