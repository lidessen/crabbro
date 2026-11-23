// Package provider invokes model providers without a daemon, local configuration,
// credential store, or usage ledger. The caller owns transport and credentials.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/lidessen/crabbro/internal/gateway/codec"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/gateway/protocol"
	"github.com/lidessen/crabbro/pkg/api"
)

const (
	OpenAIChat      = protocol.ProtocolOpenAIChat
	OpenAIResponses = protocol.ProtocolOpenAIResponses
	Anthropic       = protocol.ProtocolAnthropic
	Gemini          = protocol.ProtocolGemini
	SystemOne       = protocol.ProtocolTypeSafeJev
	OpenAIDecisions = protocol.ProtocolOpenAIDecisions
	GeminiDecisions = protocol.ProtocolGeminiDecisions
)

type (
	GenRequest       = ir.GenRequest
	GenResponse      = ir.GenResponse
	Message          = ir.Message
	ContentPart      = ir.ContentPart
	ToolDecl         = ir.ToolDecl
	ToolChoice       = ir.ToolChoice
	ThinkingConfig   = ir.ThinkingConfig
	StreamEvent      = ir.StreamEvent
	Usage            = api.TokenUsage
	DecisionRequest  = ir.DecisionRequest
	DecisionResponse = ir.DecisionResponse
	DecisionQuestion = ir.DecisionQuestion
	DecisionAnswer   = ir.DecisionAnswer
	DecisionAsset    = ir.DecisionAsset
)

const (
	PartText             = ir.PartText
	PartImage            = ir.PartImage
	PartThinking         = ir.PartThinking
	PartRedactedThinking = ir.PartRedactedThinking
	PartToolUse          = ir.PartToolUse
	PartToolResult       = ir.PartToolResult
	EventStreamStart     = ir.EventStreamStart
	EventThinkingDelta   = ir.EventThinkingDelta
	EventTextDelta       = ir.EventTextDelta
	EventToolCallStart   = ir.EventToolCallStart
	EventToolCallDelta   = ir.EventToolCallDelta
	EventToolCallEnd     = ir.EventToolCallEnd
	EventUsageDelta      = ir.EventUsageDelta
	EventStreamEnd       = ir.EventStreamEnd
	EventStreamError     = ir.EventStreamError
	DecisionKindNoul     = ir.DecisionKindNoul
	DecisionKindChoice   = ir.DecisionKindChoice
	DecisionKindScore    = ir.DecisionKindScore
)

// CredentialResolver resolves a reference on each request. An empty reference
// skips resolution; implementations are responsible for their own secret storage.
type CredentialResolver interface {
	Resolve(context.Context, string) (string, error)
}
type CredentialResolverFunc func(context.Context, string) (string, error)

func (f CredentialResolverFunc) Resolve(ctx context.Context, ref string) (string, error) {
	return f(ctx, ref)
}

// Endpoint describes the upstream. CredentialRef is never interpreted as a token.
type Endpoint struct {
	BaseURL       string
	Protocol      string
	Model         string
	CredentialRef string
	AuthHeader    string
	AuthPrefix    string
	APIVersion    string
	Headers       map[string]string
	ExtraBody     map[string]any
}

func (e Endpoint) CodecTarget() (string, string, map[string]any) {
	return e.Protocol, e.Model, e.ExtraBody
}
func (e Endpoint) CodecAuth() (string, string, string, map[string]string) {
	return e.AuthHeader, e.AuthPrefix, e.APIVersion, e.Headers
}

// Request contains the caller's wire-format JSON. Protocol selects that format;
// Stream asks for SSE, and Action optionally selects a protocol-specific method.
type Request struct {
	Protocol string
	Model    string
	Action   string
	Query    string
	Header   http.Header
	Body     []byte
	Stream   bool
}

// Response holds the status and translated wire body. For streaming calls Body
// contains only error responses; successful SSE is written to the supplied writer.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	Usage      Usage
}

// HTTPError reports a non-success upstream status. Response contains its original body.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return fmt.Sprintf("provider returned HTTP %d", e.StatusCode) }

// Client is safe for concurrent use if its HTTP client and resolver are safe.
type Client struct {
	HTTPClient *http.Client
	Resolver   CredentialResolver
}

// Do invokes an endpoint and buffers a non-streaming response.
func (c *Client) Do(ctx context.Context, endpoint Endpoint, req Request) (*Response, error) {
	if req.Stream {
		return nil, fmt.Errorf("streaming request requires DoStream")
	}
	return c.call(ctx, endpoint, req, nil)
}

// DoStream writes SSE incrementally and stops when the request context is cancelled.
func (c *Client) DoStream(ctx context.Context, endpoint Endpoint, req Request, dst io.Writer) (*Response, error) {
	if dst == nil {
		return nil, fmt.Errorf("stream destination is nil")
	}
	req.Stream = true
	return c.call(ctx, endpoint, req, dst)
}

func (c *Client) call(ctx context.Context, ep Endpoint, req Request, dst io.Writer) (*Response, error) {
	if ep.Model == "" {
		return nil, fmt.Errorf("endpoint model is required")
	}
	if req.Protocol == "" || ep.Protocol == "" {
		return nil, fmt.Errorf("request and endpoint protocols are required")
	}
	if req.Stream && (protocol.ProtocolFamily(req.Protocol) == protocol.FamilyDecision || protocol.ProtocolFamily(ep.Protocol) == protocol.FamilyDecision) {
		return nil, fmt.Errorf("decision streaming is not supported")
	}
	ingress := codec.IngressInfo{Protocol: req.Protocol, Family: protocol.ProtocolFamily(req.Protocol), RequestedModel: req.Model, Action: req.Action, Stream: req.Stream}
	var body []byte
	var state any
	var err error
	bridge := codec.NewBridge()
	same := req.Protocol == ep.Protocol
	if same {
		body, err = codec.PatchModelAndExtraBody(req.Body, ep.Protocol, ep.Model, ep.ExtraBody)
		if err == nil && ep.Protocol != Gemini && protocol.ProtocolFamily(ep.Protocol) != protocol.FamilyDecision {
			var obj map[string]json.RawMessage
			if err = json.Unmarshal(body, &obj); err == nil {
				if obj == nil {
					err = fmt.Errorf("request body must be a JSON object")
				} else {
					if req.Stream {
						obj["stream"] = json.RawMessage("true")
					} else {
						delete(obj, "stream")
					}
					body, err = json.Marshal(obj)
				}
			}
		}
		if err == nil {
			body, err = codec.NormalizeSameProtocolBody(body, ep.Protocol, codec.SignatureOrigin{}, nil)
		}
	} else {
		body, state, err = bridge.BuildUpstreamBody(ep, ingress, req.Body)
	}
	if err != nil {
		return nil, err
	}
	url, err := codec.BuildUpstreamURL(ep.BaseURL, ep.Protocol, req.Action, ep.Model, codec.CleanQuery(req.Query), req.Stream)
	if err != nil {
		return nil, err
	}
	var token string
	if ep.CredentialRef != "" {
		if c == nil || c.Resolver == nil {
			return nil, fmt.Errorf("credential resolver is required")
		}
		token, err = c.Resolver.Resolve(ctx, ep.CredentialRef)
		if err != nil {
			return nil, fmt.Errorf("resolve credential: %w", err)
		}
	}
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	up.Header = req.Header.Clone()
	if up.Header == nil {
		up.Header = make(http.Header)
	}
	up.Header.Del("Accept-Encoding")
	up.Header.Set("Content-Type", "application/json")
	codec.InjectUpstreamAuth(up.Header, ep, ep.Protocol, token)
	client := http.DefaultClient
	if c != nil && c.HTTPClient != nil {
		client = c.HTTPClient
	}
	res, err := client.Do(up)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	result := &Response{StatusCode: res.StatusCode, Header: res.Header.Clone()}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		result.Body, err = io.ReadAll(res.Body)
		if err != nil {
			return result, err
		}
		return result, &HTTPError{StatusCode: res.StatusCode}
	}
	if req.Stream {
		if same {
			tap := codec.NewUsageTap(ep.Protocol)
			writer := io.Writer(dst)
			if f, ok := dst.(http.Flusher); ok {
				writer = flushingWriter{Writer: dst, flusher: f}
			}
			_, err = io.Copy(io.MultiWriter(writer, tap), res.Body)
			result.Usage = tap.Usage()
		} else {
			flusher, _ := dst.(http.Flusher)
			result.Usage, err = bridge.TranslateStream(ep, ingress, state, nil, res.Body, dst, flusher)
		}
		return result, err
	}
	result.Body, err = io.ReadAll(res.Body)
	if err != nil {
		return result, err
	}
	if same {
		result.Usage = codec.ExtractUsageJSON(ep.Protocol, result.Body)
		return result, nil
	}
	result.StatusCode, result.Header, result.Body, result.Usage, err = bridge.TranslateResponse(ep, ingress, state, res.StatusCode, res.Header, result.Body)
	return result, err
}

// EncodeDecisionRequest and DecodeDecisionRequest operate on System One JSON.
func EncodeDecisionRequest(req *DecisionRequest) ([]byte, error) {
	return codec.EncodeDecisionRequest(req)
}
func DecodeDecisionRequest(body []byte) (*DecisionRequest, error) {
	return codec.DecodeDecisionRequest(body)
}
func EncodeDecisionResponse(resp *DecisionResponse) ([]byte, error) {
	return codec.EncodeDecisionResponse(resp)
}
func DecodeDecisionResponse(body []byte) (*DecisionResponse, error) {
	var resp DecisionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// OpenAI Decision and Gemini Decision codecs operate on the shared Decision IR.
func EncodeOpenAIDecisionRequest(req *DecisionRequest) ([]byte, error) {
	return codec.EncodeOpenAIDecisionsRequest(req)
}
func DecodeOpenAIDecisionRequest(body []byte) (*DecisionRequest, error) {
	return codec.DecodeOpenAIDecisionsRequest(body)
}
func EncodeOpenAIDecisionResponse(resp *DecisionResponse) ([]byte, error) {
	return codec.EncodeOpenAIDecisionsResponse(resp)
}
func DecodeOpenAIDecisionResponse(body []byte, req *DecisionRequest) (*DecisionResponse, error) {
	return codec.DecodeOpenAIDecisionsResponse(body, req)
}
func EncodeGeminiDecisionRequest(req *DecisionRequest, model string) ([]byte, error) {
	return codec.EncodeGeminiDecisionRequest(req, model)
}
func DecodeGeminiDecisionResponse(body []byte, req *DecisionRequest, model string) (*DecisionResponse, error) {
	return codec.DecodeGeminiDecisionResponse(body, req, model)
}

type flushingWriter struct {
	io.Writer
	flusher http.Flusher
}

func (w flushingWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 {
		w.flusher.Flush()
	}
	return n, err
}
