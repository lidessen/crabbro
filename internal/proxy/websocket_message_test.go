package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/history"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/shared"
)

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
