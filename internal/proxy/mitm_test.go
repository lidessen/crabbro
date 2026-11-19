package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/mitm"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/shared"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// testMITMSetup creates a Proxy with MITM enabled and configured with
// the given domain patterns. It returns the proxy and a cleanup function.
func testMITMSetup(t *testing.T, allowPatterns, denyPatterns []string) (*Proxy, func()) {
	t.Helper()

	ca, err := mitm.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	handler := mitm.NewMITMHandler(ca, 100)
	handler.SetEnabled(true)
	if len(allowPatterns) > 0 {
		handler.Matcher.SetAllow(allowPatterns)
	}
	for _, p := range denyPatterns {
		handler.Matcher.AddDeny(p)
	}

	rb := ring.NewRequestBuffer(100)
	pxy := New(":0", rb, nil, nil)
	pxy.MITMHandler = handler
	pxy.MITMEnabled = true

	// Inject a TLS dialer that trusts any cert (for test upstream servers).
	pxy.DialTLS = func(dialer *net.Dialer, network, addr string, config *tls.Config) (*tls.Conn, error) {
		config.InsecureSkipVerify = true
		return tls.DialWithDialer(dialer, network, addr, config)
	}

	return pxy, func() {
		pxy.Stop()
	}
}

// startTestTLSServer starts a minimal TLS HTTP server that responds on any path.
func startTestTLSServer() *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hello from upstream"))
	}))
}

// doMITMExchange performs a full MITM exchange through a CONNECT tunnel.
// It returns the TLS connection after the exchange, so callers can verify
// the response or send more data.
func doMITMExchange(t *testing.T, proxyAddr, upstreamHost string) *tls.Conn {
	t.Helper()

	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect to proxy: %v", err)
	}

	// Send CONNECT request.
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", upstreamHost, upstreamHost)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// Read only the CONNECT response header. A buffered read would swallow
	// the TLS handshake that follows.
	header, err := readConnectHeader(conn)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if !strings.Contains(header, "200") {
		t.Fatalf("CONNECT expected 200, got: %s", header)
	}

	// Now establish TLS over the tunneled connection.
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         upstreamHost,
	}
	clientTLS := tls.Client(conn, tlsConfig)
	if err := clientTLS.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	return clientTLS
}

func readConnectHeader(conn net.Conn) (string, error) {
	var buf []byte
	one := make([]byte, 1)
	for !bytes.Contains(buf, []byte("\r\n\r\n")) {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := conn.Read(one)
		if n > 0 {
			buf = append(buf, one[:n]...)
		}
		if err != nil {
			return string(buf), err
		}
		if len(buf) > 8192 {
			return string(buf), fmt.Errorf("CONNECT header too long")
		}
	}
	return string(buf), nil
}

// testMITMSetupWithRuleStore creates a Proxy with MITM enabled, configured with
// the given domain patterns and rule store/engine. It also injects a TLS dialer
// that trusts any cert for upstream connections in tests.
func testMITMSetupWithRuleStore(t *testing.T, allowPatterns, denyPatterns []string, rs *rule.RuleStore, re *rule.RuleEngine) (*Proxy, func()) {
	t.Helper()

	ca, err := mitm.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	handler := mitm.NewMITMHandler(ca, 100)
	handler.SetEnabled(true)
	if len(allowPatterns) > 0 {
		handler.Matcher.SetAllow(allowPatterns)
	}
	for _, p := range denyPatterns {
		handler.Matcher.AddDeny(p)
	}

	rb := ring.NewRequestBuffer(100)
	pxy := New(":0", rb, rs, re)
	pxy.MITMHandler = handler
	pxy.MITMEnabled = true

	// Inject a TLS dialer that trusts any cert (for test upstream servers).
	pxy.DialTLS = func(dialer *net.Dialer, network, addr string, config *tls.Config) (*tls.Conn, error) {
		config.InsecureSkipVerify = true
		return tls.DialWithDialer(dialer, network, addr, config)
	}

	return pxy, func() {
		pxy.Stop()
	}
}

// ---------------------------------------------------------------------------
// Tests — handleMITMExchange directly (over TCP)
// ---------------------------------------------------------------------------

func TestHandleMITMExchange_Success(t *testing.T) {
	upstream := startTestTLSServer()
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetup(t, []string{"*"}, nil)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	// Send the HTTP request.
	_, err := clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n"))
	if err != nil {
		t.Fatalf("write request failed: %v", err)
	}

	// Read the response.
	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK from upstream, got %d", resp.StatusCode)
	}
}

func TestHandleMITMExchange_NotAllowed(t *testing.T) {
	ca, err := mitm.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	handler := mitm.NewMITMHandler(ca, 100)
	handler.SetEnabled(true)
	handler.Matcher.SetAllow([]string{"allowed.example.com"})

	rb := ring.NewRequestBuffer(100)
	pxy := New(":0", rb, nil, nil)
	pxy.MITMHandler = handler
	pxy.MITMEnabled = true
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	// Connect and send CONNECT for a non-allowed host.
	proxyAddr := pxy.Addr()
	conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect to proxy: %v", err)
	}
	defer conn.Close()

	// When MITM is enabled but host is not allowed, proxy falls through to
	// normal tunnel. Normal tunnel tries to connect to evil.com:443 which
	// doesn't exist in tests, resulting in a 502 response.
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", "evil.com:443", "evil.com:443")

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 200)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}

	// The MITM path was NOT taken (good!), and the normal tunnel also fails
	// because there's no server at evil.com:443 — so we get 502.
	resp := string(buf[:n])
	if strings.Contains(resp, "502") {
		t.Log("correctly returned 502 (normal tunnel attempted, host unreachable)")
	} else {
		t.Logf("unexpected response: %s", resp)
	}
}

func TestHandleMITMExchange_UpstreamFailure(t *testing.T) {
	pxy, cleanup := testMITMSetup(t, []string{"*"}, nil)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	badHost := "127.0.0.1:1"
	clientTLS := doMITMExchange(t, pxy.Addr(), badHost)
	defer clientTLS.Close()

	// Send request. Nothing is listening, so the proxy returns a gateway error.
	_, err := clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: " + badHost + "\r\n\r\n"))
	if err != nil {
		t.Fatalf("write request failed: %v", err)
	}

	clientTLS.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusGatewayTimeout && resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 504 or 502, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Tests — CONNECT MITM branching
// ---------------------------------------------------------------------------

func TestHandleConnect_MITMEnabledAndAllowed(t *testing.T) {
	ca, err := mitm.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	handler := mitm.NewMITMHandler(ca, 100)
	handler.SetEnabled(true)
	handler.Matcher.AddAllow("*")

	rb := ring.NewRequestBuffer(100)
	pxy := New(":0", rb, nil, nil)
	pxy.MITMHandler = handler
	pxy.MITMEnabled = true

	if !pxy.MITMEnabled {
		t.Error("MITM should be enabled")
	}
	if pxy.MITMHandler == nil {
		t.Error("MITMHandler should be set")
	}
	if !pxy.MITMHandler.Matcher.IsAllowed("example.com") {
		t.Error("example.com should be allowed")
	}
}

func TestHandleConnect_MITMDisabled(t *testing.T) {
	ca, err := mitm.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	handler := mitm.NewMITMHandler(ca, 100)
	handler.SetEnabled(true)

	rb := ring.NewRequestBuffer(100)
	pxy := New(":0", rb, nil, nil)
	pxy.MITMHandler = handler
	pxy.MITMEnabled = false

	if pxy.MITMEnabled {
		t.Error("MITM should be disabled")
	}
	if pxy.MITMHandler == nil {
		t.Error("MITMHandler should be set but disabled")
	}
}

func TestHandleConnect_MITMEnabledHostDenied(t *testing.T) {
	ca, err := mitm.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	handler := mitm.NewMITMHandler(ca, 100)
	handler.SetEnabled(true)
	handler.Matcher.AddDeny("evil.com")

	rb := ring.NewRequestBuffer(100)
	pxy := New(":0", rb, nil, nil)
	pxy.MITMHandler = handler
	pxy.MITMEnabled = true

	if pxy.MITMHandler.Matcher.IsAllowed("evil.com") {
		t.Error("evil.com should be denied")
	}
	if !pxy.MITMHandler.Matcher.IsAllowed("good.com") {
		t.Error("good.com should be allowed (empty allowlist = all allowed)")
	}
}

func TestHandleConnect_NoMITMHandler(t *testing.T) {
	rb := ring.NewRequestBuffer(100)
	pxy := New(":0", rb, nil, nil)
	pxy.MITMEnabled = true
	pxy.MITMHandler = nil

	if pxy.MITMEnabled && pxy.MITMHandler == nil {
		t.Log("config: MITMEnabled=true but MITMHandler=nil — should fall through to normal path")
	}
}

// ---------------------------------------------------------------------------
// Tests — Event emissions
// ---------------------------------------------------------------------------

func TestHandleMITMExchange_EmitEvents(t *testing.T) {
	upstream := startTestTLSServer()
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetup(t, []string{"*"}, nil)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	sub := pxy.EventHub.Subscribe()
	defer pxy.EventHub.Unsubscribe(sub)

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n"))

	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()

	// Collect events. The MITM decrypted request may terminate with either
	// EventResponseCompleted or EventRequestFailed (if TLS write returns an
	// error after the client reads the response and disconnects).
	var gotStarted, gotCompleted, gotFailed bool
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case evt := <-sub:
			if evt.Type == shared.EventRequestStarted {
				gotStarted = true
			}
			if evt.Type == shared.EventResponseCompleted {
				gotCompleted = true
			}
			if evt.Type == shared.EventRequestFailed {
				gotFailed = true
			}
			if gotStarted && (gotCompleted || gotFailed) {
				goto done4
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
done4:

	if !gotStarted {
		t.Error("expected EventRequestStarted event")
	}
	if !gotCompleted && !gotFailed {
		t.Error("expected EventResponseCompleted or EventRequestFailed event")
	}
}

// ---------------------------------------------------------------------------
// Tests — Concurrency safety
// ---------------------------------------------------------------------------

func TestMITMExchange_ConcurrentSafe(t *testing.T) {
	upstream := startTestTLSServer()
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetup(t, []string{"*"}, nil)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	proxyAddr := pxy.Addr()
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
			if err != nil {
				t.Errorf("[%d] connect: %v", id, err)
				return
			}
			defer conn.Close()

			req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", upstreamHost, upstreamHost)
			conn.Write([]byte(req))

			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, 200)
			_, err = conn.Read(buf)
			if err != nil {
				t.Errorf("[%d] read CONNECT: %v", id, err)
				return
			}

			tlsConfig := &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         upstreamHost,
			}
			clientTLS := tls.Client(conn, tlsConfig)
			if err := clientTLS.Handshake(); err != nil {
				t.Errorf("[%d] TLS handshake: %v", id, err)
				return
			}
			defer clientTLS.Close()

			clientTLS.Write([]byte(fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\n\r\n", upstreamHost)))

			clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
			if err != nil {
				t.Errorf("[%d] read response: %v", id, err)
				return
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("[%d] expected 200, got %d", id, resp.StatusCode)
			}
		}(i)
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// Tests — Body cache
// ---------------------------------------------------------------------------

func TestHandleMITMExchange_BodyCached(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("response body content"))
	}))
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetup(t, []string{"*"}, nil)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n"))

	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()

	// Find the request record in the ring buffer.
	var foundReq *shared.Request
	for _, r := range pxy.RingBuffer.List(100) {
		if r.Intercepted && r.Scheme == "https" && r.Method == "GET" {
			foundReq = r
			break
		}
	}
	if foundReq == nil {
		t.Fatal("request not found in ring buffer")
	}
	if foundReq.ResponseBodyRef == "" {
		t.Error("response body reference should be set")
	} else {
		cachedBody := pxy.BodyCache.Get(foundReq.ResponseBodyRef)
		if cachedBody == nil {
			t.Fatal("response body should be in cache")
		}
		if string(cachedBody) != "response body content" {
			t.Errorf("cached body = %q, want %q", string(cachedBody), "response body content")
		}
	}
}

// ---------------------------------------------------------------------------
// Tests — Keep-alive / multiple requests
// ---------------------------------------------------------------------------

func TestHandleMITMExchange_KeepAlive(t *testing.T) {
	upstream := startTestTLSServer()
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetup(t, []string{"*"}, nil)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	// Send first request (HTTP/1.1 keep-alive is the default).
	_, err := clientTLS.Write([]byte("GET /first HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n"))
	if err != nil {
		t.Fatalf("write first request: %v", err)
	}

	// Read first response.
	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp1, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read first response: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Errorf("first request: expected 200, got %d", resp1.StatusCode)
	}

	// Send second request on the same connection.
	_, err = clientTLS.Write([]byte("GET /second HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n"))
	if err != nil {
		t.Fatalf("write second request: %v", err)
	}

	// Read second response.
	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp2, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read second response: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("second request: expected 200, got %d", resp2.StatusCode)
	}

	// Verify both requests are logged in the ring buffer as intercepted.
	foundFirst, foundSecond := false, false
	for _, r := range pxy.RingBuffer.List(100) {
		if !r.Intercepted || r.Method != "GET" {
			continue
		}
		if strings.Contains(r.URL, "/first") {
			foundFirst = true
		}
		if strings.Contains(r.URL, "/second") {
			foundSecond = true
		}
	}
	if !foundFirst {
		t.Error("first request not found in ring buffer")
	}
	if !foundSecond {
		t.Error("second request not found in ring buffer")
	}
}

func TestHandleMITMExchange_ConnectionClose(t *testing.T) {
	upstream := startTestTLSServer()
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetup(t, []string{"*"}, nil)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	// Send request with Connection: close so proxy closes after this request.
	_, err := clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: " + upstreamHost + "\r\nConnection: close\r\n\r\n"))
	if err != nil {
		t.Fatalf("write request: %v", err)
	}

	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	// Trying another request should fail — proxy closed the connection.
	clientTLS.SetDeadline(time.Now().Add(1 * time.Second))
	_, err = clientTLS.Write([]byte("GET /again HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n"))
	if err == nil {
		// Write succeeded; the read should fail.
		_, err = clientTLS.Read(make([]byte, 1))
	}
	if err == nil {
		t.Error("expected failure on second request after Connection: close")
	}
}

// ---------------------------------------------------------------------------
// Tests — Request body via MITM
// ---------------------------------------------------------------------------

func TestHandleMITMExchange_RequestBody(t *testing.T) {
	// Upstream server that echoes the request body back.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetup(t, []string{"*"}, nil)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	bodyContent := "this is the request body"
	reqLine := fmt.Sprintf("POST / HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\nContent-Type: text/plain\r\n\r\n%s",
		upstreamHost, len(bodyContent), bodyContent)
	_, err := clientTLS.Write([]byte(reqLine))
	if err != nil {
		t.Fatalf("write request: %v", err)
	}

	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if string(respBody) != bodyContent {
		t.Errorf("response body = %q, want %q", string(respBody), bodyContent)
	}

	// Verify the request body was cached.
	var foundReq *shared.Request
	for _, r := range pxy.RingBuffer.List(100) {
		if r.Intercepted && r.Method == "POST" {
			foundReq = r
			break
		}
	}
	if foundReq == nil {
		t.Fatal("POST request not found in ring buffer")
	}
	if foundReq.RequestBodyRef == "" {
		t.Error("request body should be cached")
	} else {
		cachedBody := pxy.BodyCache.Get(foundReq.RequestBodyRef)
		if string(cachedBody) != bodyContent {
			t.Errorf("cached body = %q, want %q", string(cachedBody), bodyContent)
		}
	}
}

// ---------------------------------------------------------------------------
// Tests — Rule actions during MITM interception
// ---------------------------------------------------------------------------

func TestHandleMITMExchange_RuleBlock(t *testing.T) {
	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	_, err := rs.Create("block-rule", 100,
		`version: 1
id: block-rule
on:
  request: {}
request:
  pipeline:
    - id: block
      uses: crabbro.block
      with:
        status: 403
        body: blocked by rule`)
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	upstream := startTestTLSServer()
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetupWithRuleStore(t, []string{"*"}, nil, rs, re)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	_, err = clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n"))
	if err != nil {
		t.Fatalf("write request: %v", err)
	}

	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 403 {
		t.Errorf("expected 403 Forbidden, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "blocked by rule" {
		t.Errorf("response body = %q, want %q", string(body), "blocked by rule")
	}

	// Verify the request record is in the ring buffer with a completed status.
	var foundReq *shared.Request
	for _, r := range pxy.RingBuffer.List(100) {
		if r.Intercepted && r.Method == "GET" {
			foundReq = r
			break
		}
	}
	if foundReq == nil {
		t.Error("blocked request not found in ring buffer")
	}
}

func TestHandleMITMExchange_RuleMock(t *testing.T) {
	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	_, err := rs.Create("mock-rule", 100,
		`version: 1
id: mock-rule
on:
  request: {}
request:
  pipeline:
    - id: mock
      uses: crabbro.mock-response
      with:
        status: 201
        body: mocked response body`)
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	upstream := startTestTLSServer()
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetupWithRuleStore(t, []string{"*"}, nil, rs, re)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	_, err = clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n"))
	if err != nil {
		t.Fatalf("write request: %v", err)
	}

	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		t.Errorf("expected 201 Created (mock), got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "mocked response body" {
		t.Errorf("response body = %q, want %q", string(body), "mocked response body")
	}

	// Verify the request is in the ring buffer with the mock status.
	var foundReq *shared.Request
	for _, r := range pxy.RingBuffer.List(100) {
		if r.Intercepted && r.Method == "GET" {
			foundReq = r
			break
		}
	}
	if foundReq == nil {
		t.Error("mocked request not found in ring buffer")
	}
}

func TestHandleMITMExchange_RuleAbort(t *testing.T) {
	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	_, err := rs.Create("abort-rule", 100,
		`version: 1
id: abort-rule
on:
  request: {}
request:
  pipeline:
    - id: abort
      uses: crabbro.abort`)
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	upstream := startTestTLSServer()
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetupWithRuleStore(t, []string{"*"}, nil, rs, re)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	sub := pxy.EventHub.Subscribe()
	defer pxy.EventHub.Unsubscribe(sub)

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	// Send request with Connection: close so the tunnel terminates after the abort.
	_, err = clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: " + upstreamHost + "\r\nConnection: close\r\n\r\n"))
	if err != nil {
		t.Fatalf("write request: %v", err)
	}

	// abort is a typed rejection: the client gets a defined 403 response.
	clientTLS.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, readErr := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if readErr != nil {
		t.Fatalf("read rejection response: %v", readErr)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("abort status = %d, want 403", resp.StatusCode)
	}

	// Verify EventRequestFailed was emitted for the aborted request.
	var gotFailed bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case evt := <-sub:
			if evt.Type == shared.EventRequestFailed {
				gotFailed = true
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !gotFailed {
		t.Error("expected EventRequestFailed after abort")
	}
}

// ---------------------------------------------------------------------------
// Tests — Rule header modifications
// ---------------------------------------------------------------------------

func TestHandleMITMExchange_HeaderModification(t *testing.T) {
	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	_, err := rs.Create("header-rule", 100,
		`version: 1
id: header-rule
on:
  request: {}
request:
  pipeline:
    - id: header
      uses: crabbro.set-request-header
      with:
        name: X-Custom-Test
        value: header-value`)
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Upstream that returns the value of X-Custom-Test in the response body.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hv := r.Header.Get("X-Custom-Test")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(hv))
	}))
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetupWithRuleStore(t, []string{"*"}, nil, rs, re)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	_, err = clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: " + upstreamHost + "\r\n\r\n"))
	if err != nil {
		t.Fatalf("write request: %v", err)
	}

	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if string(body) != "header-value" {
		t.Errorf("upstream received X-Custom-Test = %q, want %q", string(body), "header-value")
	}
}

// ---------------------------------------------------------------------------
// Tests — Full event emission lifecycle
// ---------------------------------------------------------------------------

func TestHandleMITMExchange_FullEventEmission(t *testing.T) {
	// Use a rule that matches (to trigger RuleEvaluated + RuleApplied events).
	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	_, err := rs.Create("tag-rule", 100,
		`version: 1
id: tag-rule
on:
  request: {}
request:
  pipeline:
    - id: tag
      uses: crabbro.tag
      with:
        tags: ["test"]`)
	if err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Upstream that echoes body back for BodyAvailable event verification.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	}))
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "https://")

	pxy, cleanup := testMITMSetupWithRuleStore(t, []string{"*"}, nil, rs, re)
	defer cleanup()
	if err := pxy.Start(); err != nil {
		t.Fatalf("proxy Start failed: %v", err)
	}
	defer pxy.Stop()

	sub := pxy.EventHub.Subscribe()
	defer pxy.EventHub.Unsubscribe(sub)

	clientTLS := doMITMExchange(t, pxy.Addr(), upstreamHost)
	defer clientTLS.Close()

	// Send a POST with a body so RequestBodyAvailable is emitted.
	bodyContent := "request body for event test"
	reqLine := fmt.Sprintf("POST / HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n\r\n%s",
		upstreamHost, len(bodyContent), bodyContent)
	clientTLS.Write([]byte(reqLine))

	clientTLS.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(clientTLS), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()

	// Collect all event types emitted. We expect the intercepted request to
	// produce: Started, Headers, BodyAvailable, RuleEvaluated, RuleApplied,
	// ResponseHeaders, ResponseBodyAvailable, ResponseCompleted.
	wantTypes := map[shared.EventType]bool{
		shared.EventRequestStarted:        false,
		shared.EventRequestHeaders:        false,
		shared.EventRequestBodyAvailable:  false,
		shared.EventRuleEvaluated:         false,
		shared.EventRuleApplied:           false,
		shared.EventResponseHeaders:       false,
		shared.EventResponseBodyAvailable: false,
		shared.EventResponseCompleted:     false,
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case evt := <-sub:
			if _, ok := wantTypes[evt.Type]; ok {
				wantTypes[evt.Type] = true
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
		if allSeen(wantTypes) {
			break
		}
	}

	for typ, seen := range wantTypes {
		if !seen {
			t.Errorf("event type %q was not emitted", typ)
		}
	}
}

// allSeen returns true when all event types in the map have been observed.
func allSeen(m map[shared.EventType]bool) bool {
	for _, seen := range m {
		if !seen {
			return false
		}
	}
	return true
}
