package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/history"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/shared"
)

func TestWebSocketConcurrentBidirectionalWriters(t *testing.T) {
	for _, masked := range []bool{false, true} {
		t.Run(fmt.Sprintf("masked=%v", masked), func(t *testing.T) {
			var output bytes.Buffer
			writer := &wsWriter{dst: &slowWebSocketWriter{dst: &output}, mask: masked}
			var wg sync.WaitGroup
			for n := 0; n < 4; n++ {
				wg.Add(1)
				go func(n int) {
					defer wg.Done()
					for i := 0; i < 50; i++ {
						if err := writer.write(1, []byte(fmt.Sprintf("%d:%d", n, i))); err != nil {
							t.Error(err)
							return
						}
					}
				}(n)
			}
			wg.Wait()
			seen := map[string]bool{}
			for output.Len() > 0 {
				op, data, fin, err := readWSFrame(&output)
				if err != nil || op != 1 || !fin || seen[string(data)] {
					t.Fatalf("corrupt frame: %d %q %v %v", op, data, fin, err)
				}
				seen[string(data)] = true
			}
			if len(seen) != 200 {
				t.Fatalf("got %d frames", len(seen))
			}
		})
	}
}

type slowWebSocketWriter struct{ dst *bytes.Buffer }

func (w *slowWebSocketWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		w.dst.WriteByte(b)
		time.Sleep(time.Microsecond)
	}
	return len(p), nil
}

func TestWebSocketRelayConcurrentInjection(t *testing.T) {
	var clientInput, serverInput, toClient, toUpstream bytes.Buffer
	for i := 0; i < 20; i++ {
		appendTestWSFrame(t, &clientInput, 1, "intercept", true)
		appendTestWSFrame(t, &serverInput, 1, "server", true)
	}
	clientWriter := &wsWriter{dst: &slowWebSocketWriter{dst: &toClient}}
	upstreamWriter := &wsWriter{dst: &slowWebSocketWriter{dst: &toUpstream}, mask: true}
	ex := &Exchange{record: &shared.Request{Path: "/ws"}, matched: []*shared.Rule{wsTerminalRule("    - uses: crabbro.respond-frame\n      with:\n        payload: reply\n")}}
	p := &Proxy{RuleEngine: rule.NewRuleEngine()}
	var wg sync.WaitGroup
	var offloads sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		p.relayFramesWithWriters(ex, &clientInput, upstreamWriter, clientWriter, "client", &wsObservations{}, &offloads)
	}()
	go func() {
		defer wg.Done()
		p.relayFramesWithWriters(ex, &serverInput, clientWriter, upstreamWriter, "server", &wsObservations{}, &offloads)
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if err := upstreamWriter.write(1, []byte("injected")); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	offloads.Wait()
	for _, tc := range []struct {
		name   string
		frames *bytes.Buffer
		want   map[string]int
	}{
		{"client", &toClient, map[string]int{"server": 20, "reply": 20}},
		{"upstream", &toUpstream, map[string]int{"injected": 20}},
	} {
		for tc.frames.Len() > 0 {
			op, payload, fin, err := readWSFrame(tc.frames)
			if err != nil || op != 1 || !fin {
				t.Fatalf("%s: corrupt frame: %d %q %v %v", tc.name, op, payload, fin, err)
			}
			tc.want[string(payload)]--
		}
		for payload, count := range tc.want {
			if count != 0 {
				t.Errorf("%s: %q count differs by %d", tc.name, payload, count)
			}
		}
	}
}

func wsTerminalRule(stage string) *shared.Rule {
	return &shared.Rule{ID: "terminal", Version: 1, Script: "version: 1\nid: terminal\non:\n  request:\n    path: /ws\nframe:\n  pipeline:\n" + stage}
}

func runWebSocketTerminal(t *testing.T, stage, message string, offloader GatewayOffloader) (string, []string) {
	t.Helper()
	var input, upstream, client bytes.Buffer
	cut := len(message) / 2
	appendTestWSFrame(t, &input, 1, message[:cut], false)
	appendTestWSFrame(t, &input, 0, message[cut:], true)
	req, err := http.NewRequestWithContext(context.Background(), "GET", "https://subscription.example/ws?key=synthetic-query", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer synthetic-auth")
	req.Header.Set("Cookie", "session=synthetic-cookie")
	req.Header.Set("X-Subscription", "synthetic-header")
	ex := &Exchange{ctx: context.Background(), outbound: req, sourceURL: req.URL, sourceHeader: req.Header.Clone(), record: &shared.Request{Path: "/ws"}, matched: []*shared.Rule{wsTerminalRule(stage)}}
	p := &Proxy{RuleEngine: rule.NewRuleEngine(), GatewayOffloader: offloader}
	var offloads sync.WaitGroup
	p.relayFramesWithWriters(ex, &input, &wsWriter{dst: &upstream, mask: true}, &wsWriter{dst: &client}, "client", &wsObservations{}, &offloads)
	offloads.Wait()
	var messages []string
	for client.Len() > 0 {
		op, payload, fin, err := readWSFrame(&client)
		if err != nil || op != 1 || !fin {
			t.Fatalf("client frame: %d %q %v %v", op, payload, fin, err)
		}
		messages = append(messages, string(payload))
	}
	return upstream.String(), messages
}

func TestWebSocketSyntheticReplyNotForwarded(t *testing.T) {
	upstream, messages := runWebSocketTerminal(t, "    - uses: crabbro.respond-frame\n      with:\n        payload: synthetic-reply\n", "intercepted", nil)
	if upstream != "" || len(messages) != 1 || messages[0] != "synthetic-reply" {
		t.Fatalf("upstream %q, client %q", upstream, messages)
	}
}

func TestWebSocketGatewayOffload(t *testing.T) {
	stage := "    - uses: crabbro.gateway-offload\n      with:\n        protocol: responses\n        model: local/model\n"
	message := `{"type":"response.create","model":"subscription-model","stream":true,"input":[{"content":"synthetic-auth synthetic-cookie synthetic-query synthetic-header synthetic-token","token":"synthetic-token"}]}`
	for _, tc := range []struct {
		name, contentType, body string
		failure                 bool
		want                    []string
	}{
		{"json", "application/json", `{"id":"response-1"}`, false, []string{`{"id":"response-1"}`}},
		{"sse", "text/event-stream; charset=utf-8", "event: response.created\ndata: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.completed\"}\n\ndata: [DONE]\n\n", false, []string{`{"type":"response.created"}`, `{"type":"response.completed"}`}},
		{"failure", "", "", true, []string{`{"type":"error","error":{"message":"gateway offload failed"}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			upstream, got := runWebSocketTerminal(t, stage, message, offloadFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				all := r.URL.String() + fmt.Sprint(r.Header) + string(body)
				for _, secret := range []string{"synthetic-auth", "synthetic-cookie", "synthetic-query", "synthetic-header", "synthetic-token", "subscription-model"} {
					if strings.Contains(all, secret) {
						t.Errorf("credential reached gateway: %s", secret)
					}
				}
				if r.URL.Path != "/v1/responses" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || !strings.Contains(string(body), `"model":"local/model"`) {
					t.Errorf("unexpected isolated request: %s %s", r.URL, body)
				}
				if tc.failure {
					return nil, errors.New("private gateway failure")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}))
			if !called || upstream != "" || fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("called %v, upstream %q, client %q", called, upstream, got)
			}
		})
	}
}

func TestWebSocketGatewayOffloadStreamsBeforeCompletion(t *testing.T) {
	events, sendEvent := io.Pipe()
	client, sendClient := io.Pipe()
	t.Cleanup(func() { events.Close(); sendEvent.Close(); client.Close(); sendClient.Close() })
	req, err := http.NewRequestWithContext(context.Background(), "GET", "https://subscription.example/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	ex := &Exchange{ctx: context.Background(), outbound: req, sourceURL: req.URL, sourceHeader: req.Header}
	p := &Proxy{GatewayOffloader: offloadFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: events}, nil
	})}
	done := make(chan error, 1)
	go func() {
		done <- p.offloadWebSocket(ex, []byte(`{"input":"hello","stream":true}`), shared.RuleAction{Params: map[string]any{"protocol": "responses", "model": "local/model"}}, &wsWriter{dst: sendClient})
	}()
	go func() { _, _ = io.WriteString(sendEvent, "data: {\"type\":\"response.created\"}\n\n") }()
	received := make(chan string, 1)
	go func() {
		_, data, _, err := readWSFrame(client)
		if err == nil {
			received <- string(data)
		}
	}()
	select {
	case data := <-received:
		if data != `{"type":"response.created"}` {
			t.Fatalf("event = %q", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event was not delivered before stream completion")
	}
	_ = sendEvent.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketGatewayOffloadBridge(t *testing.T) {
	upstreamResult := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstreamResult <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		if err == nil {
			err = rw.Flush()
		}
		if err != nil {
			upstreamResult <- err
			return
		}
		op, payload, _, err := readWSFrame(rw.Reader)
		if err != nil || op != 9 || string(payload) != "marker" {
			upstreamResult <- fmt.Errorf("unexpected upstream frame %d %q: %v", op, payload, err)
			return
		}
		upstreamResult <- writeWSFrame(conn, 10, payload, false)
	}))
	defer upstream.Close()
	store := rule.NewRuleStore()
	script := wsTerminalRule("    - uses: crabbro.gateway-offload\n      with:\n        protocol: responses\n        model: local/model\n").Script
	if _, err := store.Create("terminal", 10, script); err != nil {
		t.Fatal(err)
	}
	p := New("127.0.0.1:0", ring.NewRequestBuffer(8), store, rule.NewRuleEngine())
	p.GatewayOffloader = offloadFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"local"}`))}, nil
	})
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	host := strings.TrimPrefix(upstream.URL, "http://")
	fmt.Fprintf(conn, "GET http://%s/ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", host, host)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	if err := writeWSFrame(conn, 1, []byte(`{"type":"response.create","input":"hello"}`), true); err != nil {
		t.Fatal(err)
	}
	op, payload, _, err := readWSFrame(reader)
	if err != nil || op != 1 || string(payload) != `{"id":"local"}` {
		t.Fatalf("client frame: %d %q %v", op, payload, err)
	}
	if err := writeWSFrame(conn, 9, []byte("marker"), true); err != nil {
		t.Fatal(err)
	}
	op, payload, _, err = readWSFrame(reader)
	if err != nil || op != 10 || string(payload) != "marker" {
		t.Fatalf("pong: %d %q %v", op, payload, err)
	}
	if err := <-upstreamResult; err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketGatewayOffloadStalledSSEAllowsPing(t *testing.T) {
	upstreamResult := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstreamResult <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, err = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		if err == nil {
			err = rw.Flush()
		}
		if err == nil {
			var op byte
			var payload []byte
			op, payload, _, err = readWSFrame(rw.Reader)
			if err == nil && (op != 9 || string(payload) != "marker") {
				err = fmt.Errorf("unexpected upstream frame %d %q", op, payload)
			}
			if err == nil {
				err = writeWSFrame(conn, 10, payload, false)
			}
		}
		upstreamResult <- err
		if err == nil {
			_, _, _, _ = readWSFrame(rw.Reader)
		}
	}))
	defer upstream.Close()
	store := rule.NewRuleStore()
	script := wsTerminalRule("    - uses: crabbro.gateway-offload\n      with:\n        protocol: responses\n        model: local/model\n").Script
	if _, err := store.Create("terminal", 10, script); err != nil {
		t.Fatal(err)
	}
	events, sendEvent := io.Pipe()
	defer sendEvent.Close()
	canceled := make(chan struct{})
	p := New("127.0.0.1:0", ring.NewRequestBuffer(8), store, rule.NewRuleEngine())
	p.GatewayOffloader = offloadFunc(func(r *http.Request) (*http.Response, error) {
		go func() { <-r.Context().Done(); close(canceled) }()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: events}, nil
	})
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	host := strings.TrimPrefix(upstream.URL, "http://")
	fmt.Fprintf(conn, "GET http://%s/ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", host, host)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	if err := writeWSFrame(conn, 1, []byte(`{"input":"hello","stream":true}`), true); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.WriteString(sendEvent, "data: {\"type\":\"response.created\"}\n\n") }()
	op, payload, _, err := readWSFrame(reader)
	if err != nil || op != 1 || string(payload) != `{"type":"response.created"}` {
		t.Fatalf("event: %d %q %v", op, payload, err)
	}
	if err := writeWSFrame(conn, 9, []byte("marker"), true); err != nil {
		t.Fatal(err)
	}
	op, payload, _, err = readWSFrame(reader)
	if err != nil || op != 10 || string(payload) != "marker" {
		t.Fatalf("pong while SSE remains open: %d %q %v", op, payload, err)
	}
	if err := <-upstreamResult; err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("offload context not canceled when client closed")
	}
	closed := make(chan error, 1)
	go func() {
		for {
			if _, err := io.WriteString(sendEvent, "data: pending\n"); err != nil {
				closed <- err
				return
			}
		}
	}()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("SSE response body remained open")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SSE response body was not closed")
	}
}

func appendTestWSFrame(t *testing.T, out *bytes.Buffer, opcode byte, payload string, fin bool) {
	t.Helper()
	var frame bytes.Buffer
	if err := writeWSFrame(&frame, opcode, []byte(payload), false); err != nil {
		t.Fatal(err)
	}
	b := frame.Bytes()
	if !fin {
		b[0] &^= 0x80
	}
	out.Write(b)
}

func wsTestRule() *shared.Rule {
	return &shared.Rule{ID: "message", Version: 1, Script: `
version: 1
id: message
on:
  request:
    path: /ws
frame:
  pipeline:
    - id: rewrite
      run:
        runtime: goja
        script: |
          export default function(ctx) {
            if (ctx.frame.payload === "hello") return { match: true, actions: [{type: "replaceFrame", params: {payload: "rewritten"}}] };
            if (ctx.frame.payload === "discard") return { match: true, actions: [{type: "dropFrame"}] };
            return { match: false };
          }
`}
}

func TestWebSocketFragmentedMessagesAndControlFrames(t *testing.T) {
	var input, output bytes.Buffer
	appendTestWSFrame(t, &input, 1, "hel", false)
	appendTestWSFrame(t, &input, 9, "ping", true)
	appendTestWSFrame(t, &input, 10, "pong", true)
	appendTestWSFrame(t, &input, 0, "lo", true)
	appendTestWSFrame(t, &input, 1, "dis", false)
	appendTestWSFrame(t, &input, 0, "card", true)
	appendTestWSFrame(t, &input, 2, "bin", false)
	appendTestWSFrame(t, &input, 0, "ary", true)
	p := &Proxy{RuleEngine: rule.NewRuleEngine()}
	ex := &Exchange{record: &shared.Request{Path: "/ws"}, matched: []*shared.Rule{wsTestRule()}, captureStarted: true}
	observations := &wsObservations{}
	p.relayFrames(ex, &input, &output, "client", false, observations)
	for _, want := range []struct {
		op      byte
		payload string
	}{{9, "ping"}, {10, "pong"}, {1, "rewritten"}, {2, "binary"}} {
		op, data, fin, err := readWSFrame(&output)
		if err != nil || op != want.op || string(data) != want.payload || !fin {
			t.Fatalf("frame: %d %q %v %v", op, data, fin, err)
		}
	}
	if output.Len() != 0 {
		t.Fatal("unexpected output after dropped message")
	}
	if len(observations.messages) != 3 || !observations.messages[1].Dropped || len(observations.messages[0].RuleTrace) != 1 {
		t.Fatalf("observations = %+v", observations.messages)
	}
}

func TestWebSocketControlFramesForwardedBeforeMessageCompletes(t *testing.T) {
	source, send := io.Pipe()
	receive, destination := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Proxy{}).relayFrames(&Exchange{record: &shared.Request{}}, source, destination, "client", false, &wsObservations{})
	}()
	t.Cleanup(func() { send.Close(); source.Close(); receive.Close(); destination.Close(); <-done })
	var frames bytes.Buffer
	appendTestWSFrame(t, &frames, 1, "partial", false)
	appendTestWSFrame(t, &frames, 9, "ping", true)
	go func() { _, _ = send.Write(frames.Bytes()) }()
	got := make(chan string, 1)
	go func() {
		op, payload, _, err := readWSFrame(receive)
		if err == nil && op == 9 {
			got <- string(payload)
		}
	}()
	select {
	case payload := <-got:
		if payload != "ping" {
			t.Fatalf("ping payload = %q", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ping blocked behind unfinished message")
	}
}

func TestWebSocketDelayedCloseHandshake(t *testing.T) {
	serverResult := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"); err == nil {
			err = rw.Flush()
		}
		if err != nil {
			serverResult <- err
			return
		}
		op, payload, fin, err := readWSFrame(rw.Reader)
		if err != nil || op != 0x8 || !fin || string(payload) != "client close" {
			serverResult <- fmt.Errorf("client close: opcode %d, payload %q, fin %v, error %v", op, payload, fin, err)
			return
		}
		time.Sleep(100 * time.Millisecond)
		serverResult <- writeWSFrame(conn, 0x8, []byte("server close"), false)
	}))
	defer upstream.Close()

	p := New(":0", ring.NewRequestBuffer(8), nil, nil)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	host := strings.TrimPrefix(upstream.URL, "http://")
	fmt.Fprintf(conn, "GET http://%s/ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", host, host)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	if err := writeWSFrame(conn, 0x8, []byte("client close"), true); err != nil {
		t.Fatal(err)
	}
	op, payload, fin, err := readWSFrame(reader)
	if err != nil || op != 0x8 || !fin || string(payload) != "server close" {
		t.Fatalf("server close: opcode %d, payload %q, fin %v, error %v", op, payload, fin, err)
	}
	if err := <-serverResult; err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketMessageSizeLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
		want bool
	}{{"at limit", maxWSMessageBytes, true}, {"over limit", maxWSMessageBytes + 1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			var input, output bytes.Buffer
			appendTestWSFrame(t, &input, 2, strings.Repeat("x", maxWSMessageBytes/2), false)
			appendTestWSFrame(t, &input, 0, strings.Repeat("x", tc.size-maxWSMessageBytes/2), true)
			(&Proxy{}).relayFrames(&Exchange{record: &shared.Request{}}, &input, &output, "server", false, &wsObservations{})
			if (output.Len() > 0) != tc.want {
				t.Fatalf("output size = %d", output.Len())
			}
		})
	}
}

func TestWebSocketCaptureBoundsAndRedaction(t *testing.T) {
	var input, output bytes.Buffer
	for i := 0; i < maxWSCapturedMessages+1; i++ {
		appendTestWSFrame(t, &input, 1, strings.Repeat("x", maxWSPreviewBytes+50), true)
	}
	ex := &Exchange{record: &shared.Request{}, captureStarted: true}
	observations := &wsObservations{}
	(&Proxy{}).relayFrames(ex, &input, &output, "server", false, observations)
	if len(observations.messages) != maxWSCapturedMessages || !observations.truncated || !observations.messages[0].Truncated || len(observations.messages[0].PayloadPreview) > maxWSPreviewBytes {
		t.Fatal("capture limits not enforced")
	}
	obs := makeWSObservation(ex, "client", 1, []byte(`{"token":"short","nested":{"password":"private-password"},"text":"short private-password"}`), false, nil)
	if strings.Contains(obs.PayloadPreview, "short") || strings.Contains(obs.PayloadPreview, "private-password") {
		t.Fatalf("unredacted JSON preview: %s", obs.PayloadPreview)
	}
}

func TestWebSocketRejectsUnsolicitedExtension(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n")
		rw.Flush()
	}))
	defer upstream.Close()
	req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Extensions", "permessage-deflate")
	ex := &Exchange{ctx: context.Background(), outbound: req, captureStarted: true}
	if _, err := (&Proxy{}).websocketRoundTrip(ex); err == nil || ex.failureStage != "upstream_response" {
		t.Fatalf("unsolicited extension: %v, stage %q", err, ex.failureStage)
	}
}

func TestWebSocketHandshakeCaptureHistoryAndEvents(t *testing.T) {
	extensions := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		extensions <- r.Header.Get("Sec-WebSocket-Extensions")
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		rw.Flush()
		op, payload, _, err := readWSFrame(rw.Reader)
		if err != nil {
			return
		}
		_ = writeWSFrame(conn, op, payload, false)
	}))
	defer upstream.Close()
	p := New(":0", ring.NewRequestBuffer(8), nil, nil)
	hist, err := history.Open(filepath.Join(t.TempDir(), "history.db"), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	p.DefaultSink().History = hist
	events := p.DefaultSink().EventHub.Subscribe()
	defer p.DefaultSink().EventHub.Unsubscribe(events)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	host := strings.TrimPrefix(upstream.URL, "http://")
	fmt.Fprintf(conn, "GET http://%s/ws?access_token=synthetic-query HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate\r\nAuthorization: Bearer synthetic-header\r\n\r\n", host, host)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("handshake: %v %v", resp, err)
	}
	if ext := <-extensions; ext != "" {
		t.Fatalf("extension offered upstream: %q", ext)
	}
	payload := `{"type":"subscribe","token":"synthetic-payload","message":"synthetic-header synthetic-query synthetic-payload"}`
	if err := writeWSFrame(conn, 1, []byte(payload), true); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, echoed, _, err := readWSFrame(reader)
	if err != nil || string(echoed) != payload {
		t.Fatalf("echo = %q, %v", echoed, err)
	}
	var id shared.RequestID
	var seen int
	deadline := time.After(5 * time.Second)
	for seen < 2 {
		select {
		case event := <-events:
			if event.Type != shared.EventWebSocketMessage {
				continue
			}
			id = event.RequestID
			seen++
			blob, _ := json.Marshal(event.Payload)
			for _, secret := range []string{"synthetic-header", "synthetic-query", "synthetic-payload"} {
				if bytes.Contains(blob, []byte(secret)) {
					t.Fatal("secret in event")
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for message events")
		}
	}
	conn.Close()
	for {
		select {
		case event := <-events:
			if event.Type == shared.EventResponseCompleted {
				goto complete
			}
		case <-deadline:
			t.Fatal("timed out waiting for completion")
		}
	}
complete:
	// Completion is emitted immediately before persistence; synchronize through history.
	for i := 0; i < 100; i++ {
		rec, _, _, err := hist.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if rec != nil {
			if len(rec.WebSocketMessages) != 2 || rec.WebSocketMessages[0].Direction != "client" || rec.WebSocketMessages[1].Direction != "server" {
				t.Fatalf("history messages: %+v", rec.WebSocketMessages)
			}
			blob, _ := json.Marshal(rec.WebSocketMessages)
			for _, secret := range []string{"synthetic-header", "synthetic-query", "synthetic-payload"} {
				if bytes.Contains(blob, []byte(secret)) {
					t.Fatal("secret in history messages")
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("history not persisted")
}
