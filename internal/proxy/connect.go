package proxy

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/lidessen/crabbro/internal/shared"
)

// handleConnect handles an HTTP CONNECT request for tunneling.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Parse the target host:port from the request.
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}

	if p.Inspect != nil {
		if p.Sessions != nil {
			p.Inspect.Scopes = p.Sessions
		}
		if p.Inspect.BypassHost(remoteAddr(r.RemoteAddr), host) {
			p.tunnelPassthrough(w, r, host, startTime, nil)
			return
		}
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

	// MITM interception uses one policy snapshot for this new tunnel.
	policy := p.policyForTunnel()
	if p.MITMHandler != nil && policy != nil && policy.ShouldIntercept(host) {
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

		// Leaf cert is generated before the 200 so a cert failure can still be
		// an HTTP response. The handshake itself starts after 200.
		if _, certErr := p.MITMHandler.CertManager.GetCertificate(host); certErr != nil {
			clientConn.Close()
			completeRequest(req, startTime, http.StatusBadGateway)
			p.EventHub.Emit(&shared.Event{
				Type:      shared.EventRequestFailed,
				RequestID: req.ID,
				Timestamp: time.Now(),
				Payload:   map[string]string{"stage": "tls"},
			})
			return
		}

		// Send 200 Connection Established.
		clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		req.Intercepted = true
		req.Status = http.StatusOK
		req.PolicyGeneration = policy.Generation

		go p.handleMITMExchange(clientConn, req, host, startTime)
		return
	}

	// -----------------------------------------------------------------------
	// Normal CONNECT tunnel (passthrough)
	// -----------------------------------------------------------------------
	p.tunnelPassthrough(w, r, host, startTime, req)
}

// tunnelPassthrough copies bytes for a CONNECT that is not intercepted.
// req may be nil when an explicit scope bypass asked not to record the tunnel.
func (p *Proxy) tunnelPassthrough(w http.ResponseWriter, r *http.Request, host string, startTime time.Time, req *shared.Request) {
	targetConn, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		if req != nil {
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
		}
		writeError(w, http.StatusBadGateway, "Failed to connect to "+host)
		return
	}
	defer targetConn.Close()

	// Tell the client the tunnel is established.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		if req != nil {
			now := time.Now()
			req.CompletedAt = &now
			req.Status = http.StatusInternalServerError
			req.DurationMs = time.Since(startTime).Milliseconds()
			p.EventHub.Emit(&shared.Event{
				Type:      shared.EventRequestFailed,
				RequestID: req.ID,
				Timestamp: now,
			})
		}
		writeError(w, http.StatusInternalServerError, "Hijacking not supported")
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		if req != nil {
			now := time.Now()
			req.CompletedAt = &now
			req.Status = http.StatusInternalServerError
			req.DurationMs = time.Since(startTime).Milliseconds()
			p.EventHub.Emit(&shared.Event{
				Type:      shared.EventRequestFailed,
				RequestID: req.ID,
				Timestamp: now,
			})
		}
		writeError(w, http.StatusInternalServerError, "Hijacking failed")
		return
	}
	defer clientConn.Close()

	// Send 200 Connection Established.
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Tunnel established.
	if req != nil {
		req.Status = http.StatusOK
	}

	p.trackTunnel(clientConn)
	defer p.untrackTunnel(clientConn)

	// Half-close: EOF in one direction closes the matching writer only.
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(targetConn, clientConn)
		if cw, ok := targetConn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, targetConn)
		if cw, ok := clientConn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()

	// Both directions run until EOF. CloseWrite on one side lets the other finish.
	<-done
	<-done
	if req != nil {
		now := time.Now()
		req.CompletedAt = &now
		req.DurationMs = time.Since(startTime).Milliseconds()
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventResponseCompleted,
			RequestID: req.ID,
			Timestamp: now,
		})
	}
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
	p.trackTunnel(clientConn)
	defer p.untrackTunnel(clientConn)

	tlsConn, err := p.MITMHandler.Intercept(clientConn, host)
	if err != nil {
		completeRequest(connectReq, connectStartTime, 0)
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: connectReq.ID,
			Timestamp: time.Now(),
			Payload:   map[string]string{"stage": "tls"},
		})
		return
	}
	defer tlsConn.Close()

	sni := tlsConn.ConnectionState().ServerName
	if tlsConn.ConnectionState().NegotiatedProtocol == "h2" {
		p.serveMITMH2(tlsConn, connectReq, host, sni, connectStartTime)
		return
	}
	lis := &singleConnListener{conn: tlsConn}
	var finishOnce sync.Once
	finished := make(chan struct{})
	finish := func() { finishOnce.Do(func() { close(finished) }) }

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqHost := r.Host
			if reqHost == "" && r.URL != nil {
				reqHost = r.URL.Host
			}
			if !authorityMatches(host, reqHost, sni) {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			p.serveClientExchange(w, r, "mitm", connectReq.ID, host)
		}),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       30 * time.Second,
		ConnState: func(c net.Conn, state http.ConnState) {
			if state == http.StateClosed || state == http.StateHijacked {
				finish()
			}
		},
	}
	// Serve returns when Accept reports the single connection has been
	// delivered. The request goroutine keeps running; ConnState reports when
	// that connection actually closes.
	go func() { _ = srv.Serve(lis) }()
	<-finished
	_ = srv.Close()

	completeRequest(connectReq, connectStartTime, http.StatusOK)
	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventResponseCompleted,
		RequestID: connectReq.ID,
		Timestamp: time.Now(),
	})
}

func (p *Proxy) serveMITMH2(tlsConn *tls.Conn, connectReq *shared.Request, host, sni string, connectStartTime time.Time) {
	h2 := &http2.Server{}
	h2.ServeConn(tlsConn, &http2.ServeConnOpts{
		BaseConfig: &http.Server{IdleTimeout: 60 * time.Second},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqHost := r.Host
			if reqHost == "" && r.URL != nil {
				reqHost = r.URL.Host
			}
			if !authorityMatches(host, reqHost, sni) {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			r.Proto = "HTTP/2.0"
			p.serveClientExchange(w, r, "mitm", connectReq.ID, host)
		}),
	})
	completeRequest(connectReq, connectStartTime, http.StatusOK)
	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventResponseCompleted,
		RequestID: connectReq.ID,
		Timestamp: time.Now(),
	})
}

// singleConnListener feeds one already-accepted connection to http.Server.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error { return nil }

func (l *singleConnListener) Addr() net.Addr {
	if l.conn == nil || l.conn.LocalAddr() == nil {
		return &net.TCPAddr{}
	}
	return l.conn.LocalAddr()
}

// completeRequest fills in the completed-at timestamp, status, and duration
// for a request record.
func completeRequest(req *shared.Request, startTime time.Time, status int) {
	now := time.Now()
	req.CompletedAt = &now
	req.Status = status
	req.DurationMs = time.Since(startTime).Milliseconds()
}
