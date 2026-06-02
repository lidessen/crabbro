package proxy

import (
	"io"
	"net"
	"net/http"
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
