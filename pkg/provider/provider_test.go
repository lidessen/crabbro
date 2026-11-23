package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/pkg/provider"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fakeServer struct {
	url    string
	client *http.Client
}

func mockServer(handler http.Handler) fakeServer {
	return fakeServer{url: "http://provider.example", client: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}}
}

func TestPassthroughAndAuth(t *testing.T) {
	for _, tc := range []struct{ proto, path, header, value string }{
		{provider.OpenAIChat, "/v1/chat/completions", "Authorization", "Bearer test-token"},
		{provider.OpenAIResponses, "/v1/responses", "Authorization", "Bearer test-token"},
		{provider.Anthropic, "/v1/messages", "X-Api-Key", "test-token"},
		{provider.Gemini, "/v1beta/models/target:generateContent", "X-Goog-Api-Key", "test-token"},
	} {
		t.Run(tc.proto, func(t *testing.T) {
			n := 0
			srv := mockServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Header.Get(tc.header) != tc.value {
					t.Errorf("path or auth mismatch: %s %s", r.URL.Path, r.Header.Get(tc.header))
				}
				if r.Header.Get("Proxy-Authorization") != "" || r.URL.Query().Get("key") != "" {
					t.Error("client credential forwarded")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if tc.proto != provider.Gemini && body["model"] != "target" {
					t.Errorf("model = %v", body["model"])
				}
				if body["vendor_extra"] != "retained" {
					t.Errorf("lost passthrough field: %v", body)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"usage":{"prompt_tokens":7,"completion_tokens":2},"untouched":true}`)
			}))

			c := provider.Client{HTTPClient: srv.client, Resolver: provider.CredentialResolverFunc(func(_ context.Context, ref string) (string, error) {
				n++
				if ref != "secret://example" {
					t.Errorf("ref %s", ref)
				}
				return "test-token", nil
			})}
			ep := provider.Endpoint{BaseURL: srv.url, Protocol: tc.proto, Model: "target", CredentialRef: "secret://example"}
			req := provider.Request{Protocol: tc.proto, Body: []byte(`{"model":"old","vendor_extra":"retained"}`), Query: "key=client-value&trace=1", Header: http.Header{"Proxy-Authorization": []string{"client-value"}}}
			for range 2 {
				res, err := c.Do(context.Background(), ep, req)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(res.Body, []byte(`"untouched":true`)) {
					t.Fatal(string(res.Body))
				}
			}
			if n != 2 {
				t.Errorf("resolver called %d times", n)
			}
		})
	}
}

func TestCrossProtocolAndSignatureStripping(t *testing.T) {
	srv := mockServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "foreign-signature") || !strings.Contains(string(b), "hello") || !strings.Contains(string(b), `"model":"target"`) {
			t.Errorf("translated request: %s", b)
		}
		io.WriteString(w, `{"id":"chat-1","model":"target","choices":[{"index":0,"message":{"role":"assistant","content":"world"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	}))

	c := provider.Client{HTTPClient: srv.client}
	res, err := c.Do(context.Background(), provider.Endpoint{BaseURL: srv.url, Protocol: provider.OpenAIChat, Model: "target"}, provider.Request{Protocol: provider.Anthropic, Model: "alias", Body: []byte(`{"model":"alias","max_tokens":32,"messages":[{"role":"user","content":[{"type":"thinking","thinking":"hidden","signature":"foreign-signature"},{"type":"text","text":"hello"}]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Body), "world") || res.Usage.InputTokens != 5 || res.Usage.OutputTokens != 3 {
		t.Fatalf("response %s usage %+v", res.Body, res.Usage)
	}
}

func TestStream(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "passthrough", false: "translated"}[same], func(t *testing.T) {
			const sse = "data: {\"id\":\"x\",\"model\":\"target\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\ndata: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"
			srv := mockServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if !bytes.Contains(b, []byte(`"stream":true`)) {
					t.Errorf("stream flag absent: %s", b)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, sse)
			}))

			proto := provider.Anthropic
			body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
			if same {
				proto = provider.OpenAIChat
				body = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
			}
			var out bytes.Buffer
			c := provider.Client{HTTPClient: srv.client}
			res, err := c.DoStream(context.Background(), provider.Endpoint{BaseURL: srv.url, Protocol: provider.OpenAIChat, Model: "target"}, provider.Request{Protocol: proto, Body: []byte(body)}, &out)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "hello") || res.Usage.InputTokens != 4 {
				t.Fatalf("output %s usage %+v", out.String(), res.Usage)
			}
			if same && out.String() != sse {
				t.Fatal("passthrough changed SSE")
			}
			if !same && !strings.Contains(out.String(), "event: message_start") {
				t.Fatal(out.String())
			}
		})
	}
}

func TestErrorsAndCancellation(t *testing.T) {
	sentinel := errors.New("resolver failure")
	c := provider.Client{Resolver: provider.CredentialResolverFunc(func(context.Context, string) (string, error) { return "", sentinel })}
	ep := provider.Endpoint{BaseURL: "http://example.invalid", Protocol: provider.OpenAIChat, Model: "m", CredentialRef: "ref"}
	req := provider.Request{Protocol: provider.OpenAIChat, Body: []byte(`{}`)}
	if _, err := c.Do(context.Background(), ep, req); !errors.Is(err, sentinel) {
		t.Fatalf("resolver error %v", err)
	}
	ep.CredentialRef = ""
	c.HTTPClient = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"busy"}`))}, nil
	})}
	res, err := c.Do(context.Background(), ep, req)
	var httpErr *provider.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 429 || !strings.Contains(string(res.Body), "busy") {
		t.Fatalf("response %v error %v", res, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.HTTPClient = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	if _, err = c.Do(ctx, ep, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation %v", err)
	}
}

func TestDecisionCodecsAndBridge(t *testing.T) {
	req := &provider.DecisionRequest{Model: "m", State: "hello", Questions: map[string]provider.DecisionQuestion{"q": {Type: provider.DecisionKindNoul, Instructions: "judge"}}}
	b, err := provider.EncodeDecisionRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.DecodeDecisionRequest(b); err != nil {
		t.Fatal(err)
	}
	c := provider.Client{HTTPClient: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"model":"target"`)) {
			t.Errorf("model: %s", body)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"x","choices":[{"message":{"role":"assistant","content":"{\"answers\":{\"q\":{\"noul\":0.8}}}"},"finish_reason":"stop"}]}`))}, nil
	})}}
	res, err := c.Do(context.Background(), provider.Endpoint{BaseURL: "http://example.invalid", Protocol: provider.OpenAIChat, Model: "target"}, provider.Request{Protocol: provider.SystemOne, Body: b})
	if err != nil {
		t.Fatal(err)
	}
	var d provider.DecisionResponse
	if err = json.Unmarshal(res.Body, &d); err != nil || len(d.Answers) != 1 {
		t.Fatalf("decision %s: %v", res.Body, err)
	}
}

func TestSameProtocolStripsUnverifiedSignaturesAndCustomAuth(t *testing.T) {
	client := provider.Client{HTTPClient: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("foreign-signature")) || bytes.Contains(body, []byte("encrypted-blob")) {
			t.Errorf("opaque signature forwarded: %s", body)
		}
		if r.Header.Get("X-Custom-Auth") != "Token test-token" || r.Header.Get("Authorization") != "" || r.Header.Get("Anthropic-Version") != "custom-version" {
			t.Errorf("custom authentication headers: %v", r.Header)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}, Resolver: provider.CredentialResolverFunc(func(context.Context, string) (string, error) { return "test-token", nil })}
	_, err := client.Do(context.Background(), provider.Endpoint{BaseURL: "http://provider.example", Protocol: provider.Anthropic, Model: "target", CredentialRef: "ref", AuthHeader: "X-Custom-Auth", AuthPrefix: "Token ", APIVersion: "custom-version"}, provider.Request{Protocol: provider.Anthropic, Header: http.Header{"Authorization": []string{"incoming"}, "X-Custom-Auth": []string{"incoming"}}, Body: []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"text","signature":"foreign-signature"},{"type":"redacted_thinking","data":"encrypted-blob"},{"type":"text","text":"answer"}]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestStreamWriterError(t *testing.T) {
	c := provider.Client{HTTPClient: &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: hello\n\n"))}, nil
	})}}
	_, err := c.DoStream(context.Background(), provider.Endpoint{BaseURL: "http://provider.example", Protocol: provider.OpenAIChat, Model: "m"}, provider.Request{Protocol: provider.OpenAIChat, Body: []byte(`{}`)}, failingWriter{})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writer error: %v", err)
	}
}

func TestRequestTransformAndAuth(t *testing.T) {
	path, val := "/custom", "model"
	srv := mockServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom" || r.URL.Query().Get("mode") != "model" || r.URL.Query().Has("remove") {
			t.Errorf("URL %s", r.URL)
		}
		if r.Header.Get("Anthropic-Beta") != "" || r.Header.Get("X-Stainless-Lang") != "" || r.Header.Get("X-Order") != "model" || r.Header.Get("X-Custom-Auth") != "fixture-credential" || len(r.Header.Values("X-Custom-Auth")) != 1 {
			t.Errorf("headers incorrect")
		}
		b, _ := io.ReadAll(r.Body)
		if r.ContentLength != int64(len(b)) || !bytes.Contains(b, []byte(`"new":"model"`)) || bytes.Contains(b, []byte(`"old"`)) || bytes.Contains(b, []byte(`"drop"`)) {
			t.Errorf("body %s length %d", b, r.ContentLength)
		}
		io.WriteString(w, `{}`)
	}))
	c := provider.Client{HTTPClient: srv.client, Resolver: provider.CredentialResolverFunc(func(context.Context, string) (string, error) { return "fixture-credential", nil })}
	ep := provider.Endpoint{BaseURL: srv.url, Protocol: provider.Anthropic, Model: "target", CredentialRef: "ref", AuthHeader: "X-Custom-Auth", Headers: map[string]string{"Anthropic-Beta": "beta"}, RequestTransform: provider.RequestTransform{StripHeaders: []string{"anthropic-beta", "x-stainless-*"}, Path: &path, Query: map[string]*string{"mode": &val, "remove": nil}, RenameBodyFields: map[string]string{"old": "new"}, DropBodyFields: []string{"drop"}, SetHeaders: map[string]string{"X-Order": "endpoint"}}, ModelRequestTransform: provider.RequestTransform{SetHeaders: map[string]string{"X-Order": "model"}, SetBodyFields: map[string]any{"new": "model"}}}
	_, err := c.Do(context.Background(), ep, provider.Request{Protocol: provider.Anthropic, Header: http.Header{"X-Stainless-Lang": {"go"}, "x-custom-auth": {"client-credential"}}, Query: "remove=1", Body: []byte(`{"old":"endpoint","drop":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	ep.ModelRequestTransform.SetHeaders = map[string]string{"x-custom-auth": "bad"}
	if _, err = c.Do(context.Background(), ep, provider.Request{Protocol: provider.Anthropic, Body: []byte(`{}`)}); err == nil {
		t.Fatal("custom auth override accepted")
	}
}
