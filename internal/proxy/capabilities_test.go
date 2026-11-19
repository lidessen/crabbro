package proxy

import (
	"crypto/tls"
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
	"github.com/lidessen/crabbro/internal/mitm"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"golang.org/x/net/http2"
)

func TestResponseRewriteAndDelay(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "hello secret")
	}))
	defer upstream.Close()

	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	script := `
version: 1
id: rewrite
on:
  request:
    path: /v1/profile
response:
  pipeline:
    - id: set_header
      uses: crabbro.set-response-header
      with:
        name: X-Crabbro
        value: rewritten
    - id: drop_header
      uses: crabbro.remove-response-header
      with:
        name: X-Upstream
    - id: rewrite_body
      uses: crabbro.rewrite-response-body
      with:
        find: secret
        replace: public
    - id: set_status
      uses: crabbro.set-response-status
      with:
        status: 201
    - id: slow_down
      uses: crabbro.delay
      with:
        milliseconds: 80
    - id: throttled
      uses: crabbro.throttle
      with:
        bytesPerSecond: 1000000
`
	if _, err := rs.Create("rewrite", 10, script); err != nil {
		t.Fatal(err)
	}
	pxy := New(":0", ring.NewRequestBuffer(8), rs, re)
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	start := time.Now()
	resp, err := client.Get(upstream.URL + "/v1/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if time.Since(start) < 70*time.Millisecond {
		t.Fatalf("delay not applied, took %s", time.Since(start))
	}
	if string(body) != "hello public" {
		t.Fatalf("body = %q", body)
	}
	if resp.Header.Get("X-Crabbro") != "rewritten" {
		t.Fatalf("X-Crabbro = %q", resp.Header.Get("X-Crabbro"))
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream") != "" {
		t.Fatalf("X-Upstream should be removed, got %q", resp.Header.Get("X-Upstream"))
	}
}

func TestWebSocketFrameRule(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		io.WriteString(bufrw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		bufrw.Flush()
		for {
			op, payload, _, err := readWSFrame(bufrw.Reader)
			if err != nil {
				return
			}
			if err := writeWSFrame(conn, op, payload, false); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	script := `
version: 1
id: frames
on:
  request:
    path: /ws
request:
  pipeline:
    - id: tag_ws
      uses: crabbro.tag
      with:
        tags: ["ws"]
frame:
  pipeline:
    - id: rewrite_frame
      run:
        runtime: goja
        script: |
          export default function(ctx) {
            if (ctx.frame.direction === "client" && ctx.frame.payload === "hello") {
              return { match: true, actions: [{ type: "replaceFrame", params: { payload: "rewritten" } }] };
            }
            return { match: false };
          }
`
	if _, err := rs.Create("frames", 10, script); err != nil {
		t.Fatal(err)
	}
	pxy := New(":0", ring.NewRequestBuffer(8), rs, re)
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()

	host := strings.TrimPrefix(upstream.URL, "http://")
	conn, err := net.Dial("tcp", pxy.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET http://%s/ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", host, host)
	header, err := readConnectHeader(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(header, "101") {
		t.Fatalf("handshake = %q", header)
	}
	if err := writeWSFrame(conn, 0x1, []byte("hello"), true); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, _, err := readWSFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "rewritten" {
		t.Fatalf("frame = %q", payload)
	}
}

func TestHTTP2MITM(t *testing.T) {
	var gotProto string
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Proto
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "h2-ok")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	ca, err := mitm.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	handler := mitm.NewMITMHandler(ca, 8)
	handler.SetEnabled(true)
	handler.Matcher.AddAllow("*")
	pxy := New(":0", ring.NewRequestBuffer(8), nil, nil)
	pxy.MITMHandler = handler
	pxy.MITMEnabled = true
	pxy.DialTLS = func(d *net.Dialer, network, addr string, config *tls.Config) (*tls.Conn, error) {
		config.InsecureSkipVerify = true
		if len(config.NextProtos) == 0 {
			config.NextProtos = []string{"h2", "http/1.1"}
		}
		return tls.DialWithDialer(d, network, addr, config)
	}
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()

	tr := &http.Transport{
		Proxy:             http.ProxyURL(mustParse(t, "http://"+pxy.Addr())),
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2"},
		},
	}
	if err := http2.ConfigureTransport(tr); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := client.Get(upstream.URL + "/check")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "h2-ok" {
		t.Fatalf("body = %q", body)
	}
	if resp.Proto != "HTTP/2.0" {
		t.Fatalf("client proto = %s, want HTTP/2.0", resp.Proto)
	}
	if gotProto != "HTTP/2.0" {
		t.Fatalf("upstream proto = %s, want HTTP/2.0", gotProto)
	}
	var intercepted *string
	for _, rec := range pxy.RingBuffer.List(0) {
		if strings.Contains(rec.URL, "/check") {
			if !rec.Intercepted {
				t.Fatal("request was not marked intercepted")
			}
			s := rec.URL
			intercepted = &s
		}
	}
	if intercepted == nil {
		t.Fatal("h2 exchange was not captured")
	}
}

func TestHistorySurvivesRestart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "stored")
	}))
	defer upstream.Close()

	path := filepath.Join(t.TempDir(), "history.db")
	hist, err := history.Open(path, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	pxy := New(":0", ring.NewRequestBuffer(8), nil, nil)
	pxy.History = hist
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	resp, err := client.Get(upstream.URL + "/keep")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	pxy.Stop()
	hist.Close()

	hist, err = history.Open(path, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer hist.Close()
	rows, err := hist.List(0)
	if err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, row := range rows {
		if strings.Contains(row.URL, "/keep") && row.Status == 200 {
			ok = true
			_, _, sb, err := hist.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if string(sb) != "stored" {
				t.Fatalf("stored body = %q", sb)
			}
		}
	}
	if !ok {
		t.Fatalf("history rows = %#v", rows)
	}
}
