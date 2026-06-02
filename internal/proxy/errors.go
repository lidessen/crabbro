package proxy

import (
	"encoding/json"
	"log"
	"net/http"
)

// writeError sends a plain-text HTTP error response. It is used during proxy
// connection setup before request context is available, when the proxy needs
// to communicate a failure to the HTTP client.
func writeError(w http.ResponseWriter, status int, msg string) {
	http.Error(w, msg, status)
}

// writeProxyError sends a JSON error envelope and logs the error server-side.
// It is used for proxy-internal errors where structured output and observability
// matter more than raw HTTP client compatibility.
func writeProxyError(w http.ResponseWriter, status int, msg string) {
	log.Printf("proxy error: status=%d msg=%q", status, msg)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": msg,
		},
	})
}
