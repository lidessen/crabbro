package api

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

// statusWriter wraps http.ResponseWriter to capture the response status code.
// It implements http.Flusher and http.Hijacker by delegating to the underlying
// writer when the concrete type supports those interfaces.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

// WriteHeader captures the status code and delegates to the wrapped writer.
func (sw *statusWriter) WriteHeader(status int) {
	if !sw.written {
		sw.status = status
		sw.written = true
	}
	sw.ResponseWriter.WriteHeader(status)
}

// Write ensures the default 200 status is recorded when Write is called
// without an explicit WriteHeader.
func (sw *statusWriter) Write(b []byte) (int, error) {
	if !sw.written {
		sw.status = http.StatusOK
		sw.written = true
	}
	return sw.ResponseWriter.Write(b)
}

// Flush implements http.Flusher so that SSE streaming works through the
// logging middleware.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker so that CONNECT tunneling works through the
// logging middleware.
func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := sw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijacking not supported")
}

// loggingMiddleware logs each HTTP request with method, path, status code,
// and duration. It uses the statusWriter to capture the response status.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		log.Printf("api: %s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start))
	})
}

// recoveryMiddleware catches panics from downstream handlers and returns a
// 500 Internal Server Error response with a standard JSON error envelope.
// The panic value is logged for debugging.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("api: panic recovered: %v", rec)
				writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// wrapMiddleware chains the middleware in order: recovery (outermost) → logging → handler.
// Panics are caught and logged; every request is logged with status and duration.
func wrapMiddleware(handler http.Handler) http.Handler {
	return recoveryMiddleware(loggingMiddleware(handler))
}
