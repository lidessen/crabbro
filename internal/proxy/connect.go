package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

// handleConnect handles an HTTP CONNECT request for tunneling.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Parse the target host:port from the request.
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}

	// Create request record for the CONNECT tunnel.
	req := p.RingBuffer.Add("CONNECT", "https", host, "", "", "https://"+host, r.RemoteAddr)
	req.StartedAt = startTime

	// Capture request headers.
	req.RequestHeaders = make(shared.Headers)
	for k, v := range r.Header {
		req.RequestHeaders[k] = strings.Join(v, ", ")
	}

	// Emit events.
	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventRequestStarted,
		RequestID: req.ID,
		Timestamp: startTime,
	})
	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventRequestHeaders,
		RequestID: req.ID,
		Timestamp: startTime,
	})

	// -----------------------------------------------------------------------
	// MITM interception path (Layer 3 — HTTPS decryption)
	// -----------------------------------------------------------------------
	if p.MITMEnabled && p.MITMHandler != nil && p.MITMHandler.Matcher.IsAllowed(host) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			completeRequest(req, startTime, http.StatusInternalServerError)
			p.EventHub.Emit(&shared.Event{
				Type:      shared.EventRequestFailed,
				RequestID: req.ID,
				Timestamp: time.Now(),
			})
			writeError(w, http.StatusInternalServerError, "Hijacking not supported")
			return
		}
		clientConn, _, err := hijacker.Hijack()
		if err != nil {
			completeRequest(req, startTime, http.StatusInternalServerError)
			p.EventHub.Emit(&shared.Event{
				Type:      shared.EventRequestFailed,
				RequestID: req.ID,
				Timestamp: time.Now(),
			})
			writeError(w, http.StatusInternalServerError, "Hijacking failed")
			return
		}

		// Send 200 Connection Established.
		clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		req.Intercepted = true
		req.Status = http.StatusOK

		go p.handleMITMExchange(clientConn, req, host, startTime)
		return
	}

	// -----------------------------------------------------------------------
	// Normal CONNECT tunnel (passthrough)
	// -----------------------------------------------------------------------

	// Connect to the target.
	targetConn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		now := time.Now()
		req.CompletedAt = &now
		req.Status = http.StatusBadGateway
		req.DurationMs = time.Since(startTime).Milliseconds()
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: now,
			Payload:   map[string]string{"error": "failed to connect to " + host},
		})
		writeError(w, http.StatusBadGateway, "Failed to connect to "+host)
		return
	}
	defer targetConn.Close()

	// Tell the client the tunnel is established.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		now := time.Now()
		req.CompletedAt = &now
		req.Status = http.StatusInternalServerError
		req.DurationMs = time.Since(startTime).Milliseconds()
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: now,
		})
		writeError(w, http.StatusInternalServerError, "Hijacking not supported")
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		now := time.Now()
		req.CompletedAt = &now
		req.Status = http.StatusInternalServerError
		req.DurationMs = time.Since(startTime).Milliseconds()
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: now,
		})
		writeError(w, http.StatusInternalServerError, "Hijacking failed")
		return
	}
	defer clientConn.Close()

	// Send 200 Connection Established.
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Tunnel established.
	req.Status = http.StatusOK

	// Bidirectional copy with deadline-based tunnel tracking.
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(targetConn, clientConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, targetConn)
		done <- struct{}{}
	}()

	// Wait for one side to close, then close both.
	<-done
	now := time.Now()
	req.CompletedAt = &now
	req.DurationMs = time.Since(startTime).Milliseconds()

	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventResponseCompleted,
		RequestID: req.ID,
		Timestamp: now,
	})
}

// handleMITMExchange performs the full MITM interception exchange on a hijacked
// CONNECT connection. It is designed to run in a goroutine.
//
// The caller must have already sent "200 Connection Established" to the client.
// This method takes ownership of clientConn and closes it when done.
//
// The decrypted TLS connection is read in a loop to support HTTP/1.1 keep-alive:
// each decrypted HTTP request creates its own request record in the ring buffer,
// while the original CONNECT tunnel record (connectReq) tracks the tunnel lifetime.
// The loop exits when the client closes, sends "Connection: close", or on an
// unrecoverable error.
func (p *Proxy) handleMITMExchange(clientConn net.Conn, connectReq *shared.Request, host string, connectStartTime time.Time) {
	defer clientConn.Close()

	// Step 1: TLS server-side handshake with the client (proxy acts as the server).
	tlsConn, err := p.MITMHandler.Intercept(clientConn, host)
	if err != nil {
		completeRequest(connectReq, connectStartTime, 0)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: connectReq.ID,
			Timestamp: time.Now(),
			Payload:   map[string]string{"error": fmt.Sprintf("MITM TLS handshake: %v", err)},
		})
		return
	}
	defer tlsConn.Close()

	// Step 2: Request loop for HTTP/1.1 keep-alive.
	reader := bufio.NewReader(tlsConn)
	reqNum := 0

	for {
		plainReq, readErr := http.ReadRequest(reader)
		if readErr != nil {
			// Failure on the first request is fatal to the tunnel; subsequent
			// failures just mean keep-alive expired or the client closed.
			if reqNum == 0 {
				completeRequest(connectReq, connectStartTime, 0)
				p.EventHub.Emit(&shared.Event{
					Type:      shared.EventRequestFailed,
					RequestID: connectReq.ID,
					Timestamp: time.Now(),
					Payload:   map[string]string{"error": fmt.Sprintf("read plaintext request: %v", readErr)},
				})
			}
			break
		}
		reqNum++

		// Check for Connection: close before processing.
		closeAfter := strings.EqualFold(plainReq.Header.Get("Connection"), "close")

		// Create a new request record for this decrypted HTTP exchange.
		p.processDecryptedRequest(tlsConn, plainReq, host)

		if closeAfter {
			break
		}
	}

	// Step final: complete the CONNECT tunnel record (it survived the full
	// exchange).
	completeRequest(connectReq, connectStartTime, http.StatusOK)
	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventResponseCompleted,
		RequestID: connectReq.ID,
		Timestamp: time.Now(),
	})
}

// processDecryptedRequest handles a single decrypted HTTP request that arrived
// over an already-established MITM TLS connection. It:
//
//  1. Creates a request record in the ring buffer with Intercepted=true
//  2. Captures request headers and body
//  3. Evaluates rules
//  4. Handles short-circuit actions (abort, block, mock)
//  5. Forwards the request to the upstream TLS server
//  6. Reads the upstream response and writes it back to the client
//  7. Completes the request record
//
// Errors are recorded on the individual request record and do not affect the
// enclosing CONNECT tunnel.
func (p *Proxy) processDecryptedRequest(tlsConn *tls.Conn, plainReq *http.Request, host string) {
	startTime := time.Now()
	reqURL := fmt.Sprintf("https://%s%s", host, plainReq.URL.RequestURI())

	req := p.RingBuffer.Add(plainReq.Method, "https", host,
		plainReq.URL.Path, plainReq.URL.RawQuery, reqURL,
		tlsConn.RemoteAddr().String())
	req.StartedAt = startTime
	req.Intercepted = true

	// Capture request headers.
	req.RequestHeaders = make(shared.Headers)
	for k, v := range plainReq.Header {
		req.RequestHeaders[k] = strings.Join(v, ", ")
	}

	// Emit request lifecycle events.
	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventRequestStarted,
		RequestID: req.ID,
		Timestamp: startTime,
	})
	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventRequestHeaders,
		RequestID: req.ID,
		Timestamp: startTime,
	})

	// Read request body.
	var bodyBytes []byte
	if plainReq.Body != nil {
		bodyBytes, _ = io.ReadAll(plainReq.Body)
		plainReq.Body.Close()
		if len(bodyBytes) > 0 {
			ref := p.BodyCache.Store(bodyBytes)
			req.RequestBodyRef = ref
			p.EventHub.Emit(&shared.Event{
				Type:      shared.EventRequestBodyAvailable,
				RequestID: req.ID,
				Timestamp: time.Now(),
			})
		}
	}

	// Evaluate rules.
	reqBodyStr := ""
	if req.RequestBodyRef != "" {
		if bc := p.BodyCache.Get(req.RequestBodyRef); bc != nil {
			reqBodyStr = string(bc)
		}
	}
	ra := p.evaluateRules(req, reqBodyStr, shared.DefaultRuleTimeout)
	if len(ra.Traces) > 0 {
		req.RuleTrace = ra.Traces
	}
	if len(ra.Tags) > 0 {
		req.Tags = append(req.Tags, ra.Tags...)
	}

	// Emit rule events.
	for _, trace := range ra.Traces {
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRuleEvaluated,
			RequestID: req.ID,
			Timestamp: time.Now(),
			Payload: map[string]any{
				"ruleId":  trace.RuleID,
				"matched": trace.Matched,
			},
		})
		if trace.Matched && len(trace.Actions) > 0 {
			p.EventHub.Emit(&shared.Event{
				Type:      shared.EventRuleApplied,
				RequestID: req.ID,
				Timestamp: time.Now(),
				Payload: map[string]any{
					"ruleId":  trace.RuleID,
					"actions": trace.Actions,
				},
			})
		}
	}

	// Handle short-circuit actions (abort, block, mock).
	if ra.AbortAction {
		completeRequest(req, startTime, 0)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: time.Now(),
			Payload:   map[string]string{"error": "request aborted by rule"},
		})
		return
	}

	if ra.BlockAction != nil {
		status := 403
		body := ""
		if s, ok := ra.BlockAction.Params["status"].(int); ok {
			status = s
		}
		if b, ok := ra.BlockAction.Params["body"].(string); ok {
			body = b
		}
		writeHTTPResponse(tlsConn, status, "text/plain", body)
		completeRequest(req, startTime, status)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventResponseCompleted,
			RequestID: req.ID,
			Timestamp: time.Now(),
		})
		return
	}

	if ra.MockAction != nil {
		status := 200
		headers := map[string]string{}
		body := ""
		if s, ok := ra.MockAction.Params["status"].(int); ok {
			status = s
		}
		if h, ok := ra.MockAction.Params["headers"].(map[string]string); ok {
			headers = h
		}
		if b, ok := ra.MockAction.Params["body"].(string); ok {
			body = b
		}
		writeCustomResponse(tlsConn, status, headers, body)
		completeRequest(req, startTime, status)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventResponseCompleted,
			RequestID: req.ID,
			Timestamp: time.Now(),
		})
		return
	}

	// Forward the decrypted request to the upstream server.
	upstreamURL := &url.URL{
		Scheme:   "https",
		Host:     host,
		Path:     plainReq.URL.Path,
		RawQuery: plainReq.URL.RawQuery,
	}

	// Apply URL rewrite if rule triggered it.
	if ra.RewriteURL != nil {
		if parsed, err := url.Parse(*ra.RewriteURL); err == nil {
			upstreamURL = parsed
		}
		req.URL = *ra.RewriteURL
	}

	// Build upstream HTTP request.
	upstreamReq, err := http.NewRequest(plainReq.Method, upstreamURL.String(), nil)
	if err != nil {
		writeHTTPResponse(tlsConn, http.StatusBadGateway, "text/plain", "Bad Gateway")
		completeRequest(req, startTime, http.StatusBadGateway)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: time.Now(),
			Payload:   map[string]string{"error": err.Error()},
		})
		return
	}

	// Copy headers from the original request, removing hop-by-hop headers.
	for k, v := range plainReq.Header {
		if isHopByHop(k) {
			continue
		}
		for _, hv := range v {
			upstreamReq.Header.Add(k, hv)
		}
	}

	// Apply rule header modifications.
	for _, name := range ra.RemoveHeader {
		upstreamReq.Header.Del(name)
	}
	for name, value := range ra.SetHeaders {
		upstreamReq.Header.Set(name, value)
	}

	// Restore request body if it was read.
	if len(bodyBytes) > 0 {
		upstreamReq.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))
		upstreamReq.ContentLength = int64(len(bodyBytes))
	}

	// Dial the upstream server with TLS client-side.
	serverName := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		serverName = h
	}

	dialTLS := p.DialTLS
	if dialTLS == nil {
		dialTLS = tls.DialWithDialer
	}
	upstreamConn, err := dialTLS(&net.Dialer{Timeout: 10 * time.Second},
		"tcp", host,
		&tls.Config{ServerName: serverName})
	if err != nil {
		writeHTTPResponse(tlsConn, http.StatusBadGateway, "text/plain", fmt.Sprintf("Failed to connect to %s", host))
		completeRequest(req, startTime, http.StatusBadGateway)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: time.Now(),
			Payload:   map[string]string{"error": fmt.Sprintf("upstream TLS dial: %v", err)},
		})
		return
	}
	defer upstreamConn.Close()

	// Send the request to upstream.
	if err := upstreamReq.Write(upstreamConn); err != nil {
		writeHTTPResponse(tlsConn, http.StatusBadGateway, "text/plain", "Bad Gateway")
		completeRequest(req, startTime, http.StatusBadGateway)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: time.Now(),
			Payload:   map[string]string{"error": fmt.Sprintf("write upstream request: %v", err)},
		})
		return
	}

	// Emit response headers event.
	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventResponseHeaders,
		RequestID: req.ID,
		Timestamp: time.Now(),
	})

	// Read the upstream response.
	upstreamResp, err := http.ReadResponse(bufio.NewReader(upstreamConn), upstreamReq)
	if err != nil {
		writeHTTPResponse(tlsConn, http.StatusBadGateway, "text/plain", "Bad Gateway")
		completeRequest(req, startTime, http.StatusBadGateway)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: time.Now(),
			Payload:   map[string]string{"error": fmt.Sprintf("read upstream response: %v", err)},
		})
		return
	}
	defer upstreamResp.Body.Close()

	// Read and cache response body.
	respBody, err := io.ReadAll(upstreamResp.Body)
	if err == nil && len(respBody) > 0 {
		ref := p.BodyCache.Store(respBody)
		req.ResponseBodyRef = ref
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventResponseBodyAvailable,
			RequestID: req.ID,
			Timestamp: time.Now(),
		})
	}

	// Restore the body for writing back to the client (io.ReadAll drained it).
	upstreamResp.Body = io.NopCloser(strings.NewReader(string(respBody)))

	// Capture response metadata.
	req.Status = upstreamResp.StatusCode
	req.ResponseHeaders = make(shared.Headers)
	for k, v := range upstreamResp.Header {
		req.ResponseHeaders[k] = strings.Join(v, ", ")
	}

	// Write the response back to the client through the TLS connection.
	if err := upstreamResp.Write(tlsConn); err != nil {
		completeRequest(req, startTime, req.Status)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: time.Now(),
			Payload:   map[string]string{"error": fmt.Sprintf("write client response: %v", err)},
		})
		return
	}

	// Complete the request record.
	completeRequest(req, startTime, upstreamResp.StatusCode)
	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventResponseCompleted,
		RequestID: req.ID,
		Timestamp: time.Now(),
	})
}

// completeRequest fills in the completed-at timestamp, status, and duration
// for a request record.
func completeRequest(req *shared.Request, startTime time.Time, status int) {
	now := time.Now()
	req.CompletedAt = &now
	req.Status = status
	req.DurationMs = time.Since(startTime).Milliseconds()
}

// writeHTTPResponse writes a simple HTTP/1.1 response to conn with the given
// status, content type, and body.
func writeHTTPResponse(conn net.Conn, status int, contentType, body string) {
	statusText := http.StatusText(status)
	if statusText == "" {
		statusText = "Unknown"
	}
	resp := fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s",
		status, statusText, contentType, len(body), body)
	conn.Write([]byte(resp))
}

// writeCustomResponse writes an HTTP/1.1 response to conn with the given
// status, custom headers map, and body.
func writeCustomResponse(conn net.Conn, status int, headers map[string]string, body string) {
	statusText := http.StatusText(status)
	if statusText == "" {
		statusText = "Unknown"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP/1.1 %d %s\r\n", status, statusText)
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(body), body)
	conn.Write([]byte(b.String()))
}
