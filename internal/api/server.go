package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lidessen/chiling/internal/fixture"
	"github.com/lidessen/chiling/internal/proxy"
	"github.com/lidessen/chiling/internal/ring"
	"github.com/lidessen/chiling/internal/rule"
	"github.com/lidessen/chiling/internal/shared"
)

// Server is the control API server, accessible over UDS and/or TCP.
type Server struct {
	UDS        string
	TCPAddr    string
	RingBuffer   *ring.RequestBuffer
	BodyCache    *proxy.BodyCache
	EventHub     *proxy.EventHub
	RuleStore    *rule.RuleStore
	RuleEngine   *rule.RuleEngine
	FixtureStore *fixture.FixtureStore
	ProxyAddr    string
	Version      string

	// RequestCountFn returns the current number of proxied requests.
	RequestCountFn func() int64
	// RuleCountFn returns the current number of registered rules.
	RuleCountFn func() int

	httpServer  *http.Server
	mux         *http.ServeMux
	handler     http.Handler // middleware-wrapped handler used by http.Server and ServeHTTP
	startOnce   sync.Once
	startedAt   time.Time
	udsListener net.Listener
	tcpListener net.Listener
}

// New creates a new API server backed by the given proxy and store components.
func New(uds string, tcpAddr string, rb *ring.RequestBuffer, bodyCache *proxy.BodyCache, eventHub *proxy.EventHub, fixtureStore *fixture.FixtureStore, ruleStore *rule.RuleStore, ruleEngine *rule.RuleEngine, proxyAddr string) *Server {
	s := &Server{
		UDS:          uds,
		TCPAddr:      tcpAddr,
		RingBuffer:   rb,
		BodyCache:    bodyCache,
		EventHub:     eventHub,
		RuleStore:    ruleStore,
		RuleEngine:   ruleEngine,
		FixtureStore: fixtureStore,
		ProxyAddr:    proxyAddr,
		Version:      "0.1.0",
		mux:          http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

// StartedAt returns when the server started.
func (s *Server) StartedAt() time.Time {
	return s.startedAt
}

// registerRoutes sets up all API routes with middleware (recovery + logging).
func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/v1/status", s.handleStatus)
	s.mux.HandleFunc("/v1/events", s.handleEvents)
	s.mux.HandleFunc("/v1/requests", s.handleRequests)
	s.mux.HandleFunc("/v1/requests/", s.handleRequestDetail)
	// Milestone 2: Rule routes — note: /v1/rules/test must be registered
	// before /v1/rules/ to avoid the trailing-slash catch-all.
	s.mux.HandleFunc("/v1/rules/test", s.handleRulesTest)
	s.mux.HandleFunc("/v1/rules", s.handleRules)
	s.mux.HandleFunc("/v1/rules/", s.handleRulesByID)

	s.mux.HandleFunc("/v1/fixtures", s.handleFixturesListOrSave)
	s.mux.HandleFunc("/v1/fixtures/", s.handleFixtureByName)

	// Wrap with middleware: recovery (outermost) → logging → routes.
	s.handler = wrapMiddleware(s.mux)
}

// Start begins listening on UDS and/or TCP. It returns once all listeners are
// ready, or an error if none can start.
func (s *Server) Start() error {
	var startErr error
	s.startOnce.Do(func() {
		s.startedAt = time.Now()

		s.httpServer = &http.Server{Handler: s.handler}

		// Start UDS listener.
		if s.UDS != "" {
			udsPath := expandPath(s.UDS)
			if err := os.MkdirAll(filepath.Dir(udsPath), 0755); err != nil {
				log.Printf("api: failed to create UDS directory: %v", err)
			} else {
				os.Remove(udsPath)
				lis, err := net.Listen("unix", udsPath)
				if err != nil {
					log.Printf("api: failed to listen on UDS %s: %v", udsPath, err)
				} else {
					s.udsListener = lis
					os.Chmod(udsPath, 0666)
					go s.httpServer.Serve(lis)
					log.Printf("api: listening on UDS %s", udsPath)
				}
			}
		}

		// Start TCP listener.
		if s.TCPAddr != "" {
			lis, err := net.Listen("tcp", s.TCPAddr)
			if err != nil {
				log.Printf("api: failed to listen on TCP %s: %v", s.TCPAddr, err)
			} else {
				s.tcpListener = lis
				go s.httpServer.Serve(lis)
				log.Printf("api: listening on TCP %s", s.TCPAddr)
			}
		}

		if s.udsListener == nil && s.tcpListener == nil {
			startErr = fmt.Errorf("api: no listeners could be started")
		}
	})
	return startErr
}

// Stop gracefully shuts down the API server.
func (s *Server) Stop() error {
	if s.httpServer == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

// ServeHTTP implements http.Handler for testing and embedding.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// writeJSON encodes v as JSON and writes it to w.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a standard JSON error response.
func writeError(w http.ResponseWriter, status int, code, message string, details ...shared.ErrorDetail) {
	body := shared.ErrorResponse{
		Error: shared.ErrorBody{
			Code:    code,
			Message: message,
		},
	}
	if len(details) > 0 {
		body.Error.Details = details
	}
	writeJSON(w, status, body)
}

// expandPath expands ~ to the user's home directory.
func expandPath(path string) string {
	if len(path) > 1 && path[0] == '~' {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}
