package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/scrub"
	"github.com/lidessen/crabbro/internal/shared"
)

const (
	maxWSMessageBytes     = 1 << 20
	maxWSCapturedMessages = 100
	maxWSPreviewBytes     = 1024
)

func isWebSocketUpgrade(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func restoreWebSocketHeaders(dst, src http.Header) {
	for _, name := range []string{
		"Upgrade", "Connection", "Sec-WebSocket-Key", "Sec-WebSocket-Version",
		"Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions",
	} {
		if vs := src.Values(name); len(vs) > 0 {
			dst.Del(name)
			for _, v := range vs {
				dst.Add(name, v)
			}
		}
	}
}

func (p *Proxy) websocketRoundTrip(ex *Exchange) (*http.Response, error) {
	inspecting := ex.captureStarted || len(ex.matched) > 0
	if inspecting {
		ex.outbound.Header.Del("Sec-WebSocket-Extensions")
	}
	host := ex.outbound.URL.Host
	if host == "" {
		host = ex.outbound.Host
	}
	var conn net.Conn
	var err error
	d := &net.Dialer{Timeout: 10 * time.Second}
	if ex.outbound.URL.Scheme == "https" {
		cfg := &tls.Config{ServerName: normalizeHost(host), NextProtos: []string{"http/1.1"}}
		if p.DialTLS != nil {
			conn, err = p.DialTLS(d, "tcp", host, cfg)
		} else {
			conn, err = tls.DialWithDialer(d, "tcp", host, cfg)
		}
	} else {
		conn, err = d.DialContext(ex.ctx, "tcp", host)
	}
	if err != nil {
		ex.failureStage = "dial"
		return nil, err
	}
	ex.outbound.RequestURI = ""
	if err := ex.outbound.Write(conn); err != nil {
		conn.Close()
		ex.failureStage = "upstream_request"
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, ex.outbound)
	if err != nil {
		conn.Close()
		ex.failureStage = "upstream_response"
		return nil, err
	}
	wrapped := &bufferedConn{r: br, c: conn}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		if inspecting && resp.Header.Get("Sec-WebSocket-Extensions") != "" {
			conn.Close()
			ex.failureStage = "upstream_response"
			return nil, fmt.Errorf("upstream negotiated an unsolicited websocket extension")
		}
		ex.upgradeConn = wrapped
		resp.Body = io.NopCloser(strings.NewReader(""))
		return resp, nil
	}
	// Non-upgrade response: the buffered reader inside ReadResponse consumed
	// the headers. The body is resp.Body, which still references the conn.
	resp.Body = &connBody{ReadCloser: resp.Body, conn: wrapped}
	return resp, nil
}

type connBody struct {
	io.ReadCloser
	conn net.Conn
}

func (c *connBody) Close() error {
	err := c.ReadCloser.Close()
	c.conn.Close()
	return err
}

func (p *Proxy) bridgeWebSocket(w http.ResponseWriter, ex *Exchange, resp *http.Response) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		ex.upgradeConn.Close()
		ex.failureStage = "websocket"
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		p.complete(ex.record)
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		ex.upgradeConn.Close()
		ex.failureStage = "websocket"
		p.complete(ex.record)
		return
	}
	defer client.Close()
	defer ex.upgradeConn.Close()

	status := resp.Status
	if status == "" {
		status = "101 Switching Protocols"
	}
	fmt.Fprintf(client, "HTTP/1.1 %s\r\n", status)
	resp.Header.Write(client)
	io.WriteString(client, "\r\n")
	if brw.Reader.Buffered() > 0 {
		extra, _ := brw.Reader.Peek(brw.Reader.Buffered())
		ex.clientPreface = append([]byte(nil), extra...)
	}

	ex.record.Status = http.StatusSwitchingProtocols
	_, flat := headerFields(resp.Header)
	ex.record.ResponseHeaders = flat
	if len(ex.traces) > 0 {
		ex.record.RuleTrace = append([]shared.RuleTrace(nil), ex.traces...)
	}
	p.emit(shared.EventResponseHeaders, ex.record.ID, nil)

	observations := &wsObservations{}
	ctx, cancel := context.WithCancel(ex.ctx)
	defer cancel()
	ex.ctx = ctx
	toClient := &wsWriter{dst: client}
	toUpstream := &wsWriter{dst: ex.upgradeConn, mask: true}
	var offloads sync.WaitGroup
	errc := make(chan struct{}, 2)
	go func() {
		if ex.captureStarted || len(ex.matched) > 0 {
			p.relayFramesWithWriters(ex, ex.clientReader(client), toUpstream, toClient, "client", observations, &offloads)
		} else {
			_, _ = io.Copy(ex.upgradeConn, ex.clientReader(client))
		}
		errc <- struct{}{}
	}()
	go func() {
		if ex.captureStarted || len(ex.matched) > 0 {
			p.relayFramesWithWriters(ex, ex.upgradeConn, toClient, toUpstream, "server", observations, &offloads)
		} else {
			_, _ = io.Copy(client, ex.upgradeConn)
		}
		errc <- struct{}{}
	}()
	<-errc
	cancel()
	client.Close()
	ex.upgradeConn.Close()
	<-errc
	offloads.Wait()
	ex.record.WebSocketMessages = observations.messages
	ex.record.WebSocketMessagesTruncated = observations.truncated

	now := time.Now()
	ex.record.CompletedAt = &now
	ex.record.DurationMs = now.Sub(ex.started).Milliseconds()
	if len(ex.traces) > 0 {
		ex.record.RuleTrace = append([]shared.RuleTrace(nil), ex.traces...)
	}
	p.emit(shared.EventResponseCompleted, ex.record.ID, map[string]any{"status": 101, "websocket": true})
	p.complete(ex.record)
}

func (ex *Exchange) clientReader(c net.Conn) io.Reader {
	if len(ex.clientPreface) == 0 {
		return c
	}
	return io.MultiReader(bytes.NewReader(ex.clientPreface), c)
}

type wsObservations struct {
	mu        sync.Mutex
	messages  []shared.WebSocketMessage
	truncated bool
}

// wsWriter serializes complete frames, including their headers and payloads.
type wsWriter struct {
	mu   sync.Mutex
	dst  io.Writer
	mask bool
}

func (w *wsWriter) write(opcode byte, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return writeWSFrame(w.dst, opcode, payload, w.mask)
}

func (p *Proxy) relayFrames(ex *Exchange, src io.Reader, dst io.Writer, direction string, mask bool, observations *wsObservations) {
	var offloads sync.WaitGroup
	p.relayFramesWithWriters(ex, src, &wsWriter{dst: dst, mask: mask}, nil, direction, observations, &offloads)
	offloads.Wait()
}

func (p *Proxy) relayFramesWithWriters(ex *Exchange, src io.Reader, dst, reverse *wsWriter, direction string, observations *wsObservations, offloads *sync.WaitGroup) {
	var message []byte
	var messageOpcode byte
	fragmented := false
	for {
		opcode, payload, fin, err := readWSFrame(src)
		if err != nil {
			return
		}
		switch opcode {
		case 0x8, 0x9, 0xa:
			if !fin || len(payload) > 125 || dst.write(opcode, payload) != nil {
				return
			}
			continue
		case 0x1, 0x2:
			if fragmented {
				return
			}
			messageOpcode = opcode
			message = message[:0]
		case 0x0:
			if !fragmented {
				return
			}
		default:
			return
		}
		if len(payload) > maxWSMessageBytes-len(message) {
			return
		}
		message = append(message, payload...)
		fragmented = !fin
		if !fin {
			continue
		}
		original := message
		out, drop, terminal, traces := p.rewriteFrame(ex, direction, opcodeName(messageOpcode), original, true)
		if ex.captureStarted {
			observation := makeWSObservation(ex, direction, messageOpcode, original, drop, traces)
			observations.mu.Lock()
			if len(observations.messages) < maxWSCapturedMessages {
				observations.messages = append(observations.messages, observation)
			} else {
				observations.truncated = true
			}
			p.emit(shared.EventWebSocketMessage, ex.record.ID, observation)
			observations.mu.Unlock()
		}
		if terminal != nil && reverse != nil {
			if terminal.Type == shared.ActionRespondFrame {
				if reverse.write(1, []byte(terminal.Params["payload"].(string))) != nil {
					return
				}
			} else {
				payload := append([]byte(nil), out...)
				action := *terminal
				offloads.Add(1)
				go func() {
					defer offloads.Done()
					if err := p.offloadWebSocket(ex, payload, action, reverse); err != nil && ex.ctx.Err() == nil {
						_ = reverse.write(1, []byte(`{"type":"error","error":{"message":"gateway offload failed"}}`))
					}
				}()
			}
		}
		if drop {
			continue
		}
		if len(out) > maxWSMessageBytes || dst.write(messageOpcode, out) != nil {
			return
		}
	}
}

// offloadWebSocket constructs an isolated HTTP ingress request for each message.
// No subscription request metadata is passed to the gateway.
func (p *Proxy) offloadWebSocket(ex *Exchange, payload []byte, action shared.RuleAction, client *wsWriter) error {
	if ex.outbound == nil || p.GatewayOffloader == nil {
		return fmt.Errorf("gateway unavailable")
	}
	source := ex.outbound.Clone(ex.ctx)
	source.Method = http.MethodPost
	source.Header = ex.sourceHeader.Clone()
	source.URL = ex.sourceURL
	source.Body = io.NopCloser(bytes.NewReader(payload))
	protocol, _ := action.Params["protocol"].(string)
	model, _ := action.Params["model"].(string)
	clean, err := buildOffloadRequest(source, protocol, model, shared.DefaultMaxGatewayRequestBody)
	if err != nil {
		return err
	}
	resp, err := p.GatewayOffloader.Offload(clean)
	if err != nil {
		return err
	}
	if resp == nil || resp.Body == nil {
		return fmt.Errorf("empty gateway response")
	}
	closed := make(chan struct{})
	stopClose := context.AfterFunc(ex.ctx, func() {
		_ = resp.Body.Close()
		close(closed)
	})
	defer func() {
		if stopClose() {
			_ = resp.Body.Close()
		} else {
			<-closed
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gateway rejected request")
	}
	if strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return writeWSSSE(resp.Body, client)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxWSMessageBytes+1))
	if err != nil || len(body) > maxWSMessageBytes || !json.Valid(body) {
		return fmt.Errorf("invalid gateway response")
	}
	return client.write(1, body)
}

// Each SSE data event becomes one logical text message. Comments and the
// transport-level [DONE] sentinel do not represent WebSocket messages.
func writeWSSSE(body io.Reader, client *wsWriter) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), maxWSMessageBytes+1)
	var data []string
	size := 0
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data, size = nil, 0
		if payload == "[DONE]" {
			return nil
		}
		if !json.Valid([]byte(payload)) {
			return fmt.Errorf("invalid gateway event")
		}
		return client.write(1, []byte(payload))
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			size += len(value) + 1
			if size > maxWSMessageBytes {
				return fmt.Errorf("gateway event too large")
			}
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

type bufferedConn struct {
	r *bufio.Reader
	c net.Conn
}

func (b *bufferedConn) Read(p []byte) (int, error)  { return b.r.Read(p) }
func (b *bufferedConn) Write(p []byte) (int, error) { return b.c.Write(p) }
func (b *bufferedConn) Close() error                { return b.c.Close() }
func (b *bufferedConn) LocalAddr() net.Addr         { return b.c.LocalAddr() }
func (b *bufferedConn) RemoteAddr() net.Addr        { return b.c.RemoteAddr() }
func (b *bufferedConn) SetDeadline(t time.Time) error {
	return b.c.SetDeadline(t)
}
func (b *bufferedConn) SetReadDeadline(t time.Time) error {
	return b.c.SetReadDeadline(t)
}
func (b *bufferedConn) SetWriteDeadline(t time.Time) error {
	return b.c.SetWriteDeadline(t)
}

func opcodeName(op byte) string {
	switch op {
	case 0x1:
		return "text"
	case 0x2:
		return "binary"
	case 0x8:
		return "close"
	case 0x9:
		return "ping"
	case 0xA:
		return "pong"
	default:
		return fmt.Sprintf("%d", op)
	}
}

func readWSFrame(r io.Reader) (opcode byte, payload []byte, fin bool, err error) {
	hdr := make([]byte, 2)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return 0, nil, false, err
	}
	if hdr[0]&0x70 != 0 {
		return 0, nil, false, fmt.Errorf("unexpected websocket extension bits")
	}
	fin = hdr[0]&0x80 != 0
	opcode = hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	ln := int(hdr[1] & 0x7f)
	switch ln {
	case 126:
		ext := make([]byte, 2)
		if _, err = io.ReadFull(r, ext); err != nil {
			return 0, nil, false, err
		}
		ln = int(ext[0])<<8 | int(ext[1])
	case 127:
		ext := make([]byte, 8)
		if _, err = io.ReadFull(r, ext); err != nil {
			return 0, nil, false, err
		}
		var n uint64
		for _, b := range ext {
			n = n<<8 | uint64(b)
		}
		if n > maxWSMessageBytes {
			return 0, nil, false, fmt.Errorf("websocket frame too large")
		}
		ln = int(n)
	}
	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(r, maskKey[:]); err != nil {
			return 0, nil, false, err
		}
	}
	payload = make([]byte, ln)
	if _, err = io.ReadFull(r, payload); err != nil {
		return 0, nil, false, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return opcode, payload, fin, nil
}

func writeWSFrame(w io.Writer, opcode byte, payload []byte, mask bool) error {
	header := []byte{0x80 | opcode}
	n := len(payload)
	var first byte
	switch {
	case n < 126:
		first = byte(n)
	case n < 65536:
		first = 126
	default:
		first = 127
	}
	if mask {
		first |= 0x80
	}
	header = append(header, first)
	if n >= 126 && n < 65536 {
		header = append(header, byte(n>>8), byte(n))
	} else if n >= 65536 {
		var ext [8]byte
		nn := uint64(n)
		for i := 7; i >= 0; i-- {
			ext[i] = byte(nn)
			nn >>= 8
		}
		header = append(header, ext[:]...)
	}
	out := payload
	if mask {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return err
		}
		header = append(header, key[:]...)
		out = make([]byte, len(payload))
		for i := range payload {
			out[i] = payload[i] ^ key[i%4]
		}
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(out)
	return err
}

// rewriteFrame applies matched frame rules to one complete message.
func (p *Proxy) rewriteFrame(ex *Exchange, direction, opcode string, payload []byte, fin bool) ([]byte, bool, *shared.RuleAction, []shared.RuleTrace) {
	if len(ex.matched) == 0 || p.RuleEngine == nil {
		return payload, false, nil, nil
	}
	current := string(payload)
	dropped := false
	var terminal *shared.RuleAction
	var traces []shared.RuleTrace
	for _, rl := range ex.matched {
		if dropped {
			break
		}
		view := &shared.Request{}
		if ex.record != nil {
			*view = *ex.record
		}
		phase := &rule.Phase{Frame: &shared.FrameView{
			Direction: direction, Opcode: opcode, Payload: current, Fin: fin,
		}}
		result := p.RuleEngine.EvaluatePhase(rl.Script, view, "", phase, shared.DefaultRuleTimeout)
		if !result.Matched || !result.SchemaValid || result.Error != "" {
			continue
		}
		for _, a := range result.Actions {
			switch a.Type {
			case shared.ActionDropFrame:
				dropped = true
			case shared.ActionRespondFrame, shared.ActionGatewayOffload:
				if direction == "client" {
					dropped = true
					action := a
					terminal = &action
				}
			case shared.ActionReplaceFrame:
				if s, ok := a.Params["payload"].(string); ok {
					current = s
				} else if b64, ok := a.Params["payloadBase64"].(string); ok {
					if raw, err := base64.StdEncoding.DecodeString(b64); err == nil {
						current = string(raw)
					}
				}
			}
		}
		if len(traces) < 32 {
			// Action parameters can contain message credentials; retain only types.
			actions := make([]shared.RuleAction, 0, len(result.Actions))
			for _, action := range result.Actions {
				if len(actions) == 32 {
					break
				}
				actions = append(actions, shared.RuleAction{Type: action.Type})
			}
			traces = append(traces, shared.RuleTrace{
				RuleID: rl.ID, RuleVersion: rl.Version, Phase: "frame",
				Matched: true, Actions: actions, DurationMs: result.DurationMs,
			})
		}
	}
	return []byte(current), dropped, terminal, traces
}

func makeWSObservation(ex *Exchange, direction string, opcode byte, payload []byte, drop bool, traces []shared.RuleTrace) shared.WebSocketMessage {
	obs := shared.WebSocketMessage{Direction: direction, Opcode: opcodeName(opcode), PayloadBytes: len(payload), Dropped: drop, RuleTrace: traces}
	if opcode != 1 || !utf8.Valid(payload) {
		obs.PayloadPreview = "[binary payload omitted]"
		return obs
	}
	secrets := make(map[string]struct{})
	for name, values := range ex.sourceHeader {
		if sensitiveOffloadKey(name) || strings.EqualFold(name, "proxy-authorization") || strings.EqualFold(name, "x-api-key") || strings.EqualFold(name, "sec-websocket-protocol") {
			for _, value := range values {
				addOffloadSecret(secrets, value)
				if i := strings.IndexAny(value, " \t"); i >= 0 {
					addOffloadSecret(secrets, strings.TrimSpace(value[i:]))
				}
				if strings.EqualFold(name, "cookie") {
					for _, pair := range strings.Split(value, ";") {
						if _, v, ok := strings.Cut(pair, "="); ok {
							addOffloadSecret(secrets, strings.Trim(strings.TrimSpace(v), `"`))
						}
					}
				}
			}
		}
	}
	if ex.sourceURL != nil {
		for key, values := range ex.sourceURL.Query() {
			if sensitiveOffloadKey(key) || strings.EqualFold(key, "key") {
				for _, value := range values {
					addOffloadSecret(secrets, value)
				}
			}
		}
	}
	var value any
	validJSON := json.Unmarshal(payload, &value) == nil
	if validJSON {
		collectWSSecrets(secrets, value)
	}
	values := make([]string, 0, len(secrets))
	for value := range secrets {
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	preview := string(payload)
	if validJSON {
		clean := redactWSText(cleanOffloadValue(value, values))
		if data, err := json.Marshal(clean); err == nil {
			preview = string(data)
		} else {
			preview = scrub.RedactedPlaceholder
		}
	} else {
		// Free-form credential-bearing text has no reliable field boundaries.
		preview = redactWSText(preview).(string)
		for _, secret := range values {
			preview = strings.ReplaceAll(preview, secret, scrub.RedactedPlaceholder)
		}
	}
	if len(preview) > maxWSPreviewBytes {
		preview = preview[:maxWSPreviewBytes]
		for !utf8.ValidString(preview) {
			preview = preview[:len(preview)-1]
		}
		obs.Truncated = true
	}
	obs.PayloadPreview = preview
	return obs
}

func collectWSSecrets(secrets map[string]struct{}, value any) {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			collectWSSecrets(secrets, item)
		}
	case map[string]any:
		for key, item := range v {
			if sensitiveOffloadKey(key) || strings.EqualFold(key, "key") || strings.EqualFold(key, "auth") {
				collectOffloadSecrets(secrets, item)
			} else {
				collectWSSecrets(secrets, item)
			}
		}
	}
}

func redactWSText(value any) any {
	switch v := value.(type) {
	case string:
		lower := strings.ToLower(v)
		for _, marker := range []string{"token", "authorization", "credential", "secret", "password", "api_key", "api-key", "apikey", "cookie", "bearer ", "subscription", "key=", "key:", `"key"`, "auth=", "auth:"} {
			if strings.Contains(lower, marker) {
				return scrub.RedactedPlaceholder
			}
		}
		return v
	case []any:
		for i, item := range v {
			v[i] = redactWSText(item)
		}
	case map[string]any:
		for key, item := range v {
			v[key] = redactWSText(item)
		}
	}
	return value
}
