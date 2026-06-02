package proxy

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

// handleHTTP processes a plain HTTP proxy request (no CONNECT).
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// Parse the target URL from the absolute-form request line.
	targetURL := r.URL
	if !targetURL.IsAbs() {
		// Reconstruct from Host header if not in absolute form.
		targetURL = &url.URL{
			Scheme: "http",
			Host:   r.Host,
			Path:   r.URL.Path,
		}
	}

	scheme := targetURL.Scheme
	host := targetURL.Host
	path := targetURL.Path
	query := targetURL.RawQuery
	urlStr := targetURL.String()

	// Create request record.
	req := p.RingBuffer.Add(r.Method, scheme, host, path, query, urlStr, r.RemoteAddr)
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

	// Read and cache request body.
	if r.Body != nil {
		bodyBytes, _ := io.ReadAll(r.Body)
		r.Body.Close()
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

	// Build upstream request.
	upstreamReq, err := http.NewRequest(r.Method, urlStr, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "failed to create upstream request")
		return
	}

	// Copy headers, removing hop-by-hop headers.
	for k, v := range r.Header {
		if isHopByHop(k) {
			continue
		}
		for _, hv := range v {
			upstreamReq.Header.Add(k, hv)
		}
	}

	// Send upstream request.
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		// Record the failure.
		now := time.Now()
		req.CompletedAt = &now
		req.DurationMs = time.Since(startTime).Milliseconds()
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventRequestFailed,
			RequestID: req.ID,
			Timestamp: now,
			Payload:   map[string]string{"error": err.Error()},
		})
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()

	// Capture response metadata.
	req.Status = resp.StatusCode
	req.ResponseHeaders = make(shared.Headers)
	for k, v := range resp.Header {
		req.ResponseHeaders[k] = strings.Join(v, ", ")
	}

	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventResponseHeaders,
		RequestID: req.ID,
		Timestamp: time.Now(),
	})

	// Read and cache response body.
	respBody, err := io.ReadAll(resp.Body)
	if err == nil && len(respBody) > 0 {
		ref := p.BodyCache.Store(respBody)
		req.ResponseBodyRef = ref
		p.EventHub.Emit(&shared.Event{
			Type:      shared.EventResponseBodyAvailable,
			RequestID: req.ID,
			Timestamp: time.Now(),
		})
	}

	// Complete the record.
	now := time.Now()
	req.CompletedAt = &now
	req.DurationMs = time.Since(startTime).Milliseconds()

	p.EventHub.Emit(&shared.Event{
		Type:      shared.EventResponseCompleted,
		RequestID: req.ID,
		Timestamp: now,
	})

	// Copy response headers to client.
	for k, v := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// isHopByHop returns true for HTTP/1.1 hop-by-hop headers that must not be
// forwarded by a proxy.
func isHopByHop(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding",
		"upgrade":
		return true
	}
	return false
}
