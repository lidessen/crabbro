package proxy

import (
	"net/http"
	"strings"
)

// handleHTTP processes a plain HTTP proxy request (absolute-form URL).
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	p.serveClientExchange(w, r, "http", "", "")
}

// isHopByHop returns true for HTTP/1.1 hop-by-hop headers that must not be
// forwarded by a proxy.
func isHopByHop(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "proxy-connection", "te", "trailer",
		"transfer-encoding", "upgrade":
		return true
	}
	return false
}
