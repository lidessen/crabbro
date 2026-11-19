package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/shared"
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

	errc := make(chan struct{}, 2)
	go func() {
		p.relayFrames(ex, ex.clientReader(client), ex.upgradeConn, "client", true)
		errc <- struct{}{}
	}()
	go func() {
		p.relayFrames(ex, ex.upgradeConn, client, "server", false)
		errc <- struct{}{}
	}()
	<-errc
	client.Close()
	ex.upgradeConn.Close()
	<-errc

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

func (p *Proxy) relayFrames(ex *Exchange, src io.Reader, dst io.Writer, direction string, mask bool) {
	for {
		opcode, payload, fin, err := readWSFrame(src)
		if err != nil {
			return
		}
		if opcode == 0x1 || opcode == 0x2 {
			var drop bool
			payload, drop = p.rewriteFrame(ex, direction, opcodeName(opcode), payload, fin)
			if drop {
				continue
			}
		}
		if err := writeWSFrame(dst, opcode, payload, mask); err != nil {
			return
		}
	}
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
		if n > 1<<20 {
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

// applyFrameRules is completed below once matched rules are available on the proxy call.
func (p *Proxy) rewriteFrame(ex *Exchange, direction, opcode string, payload []byte, fin bool) ([]byte, bool) {
	if len(ex.matched) == 0 || p.RuleEngine == nil {
		return payload, false
	}
	current := string(payload)
	dropped := false
	frameTraces := 0
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
		if frameTraces < 32 {
			ex.traces = append(ex.traces, shared.RuleTrace{
				RuleID: rl.ID, RuleVersion: rl.Version, Phase: "frame",
				Matched: true, Actions: result.Actions, DurationMs: result.DurationMs,
			})
			frameTraces++
		}
	}
	if dropped {
		return nil, true
	}
	return []byte(current), false
}
