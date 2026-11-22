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

	"crypto/subtle"

	"github.com/lidessen/crabbro/internal/capture"
	"github.com/lidessen/crabbro/internal/decision"
	"github.com/lidessen/crabbro/internal/fixture"
	"github.com/lidessen/crabbro/internal/gateway"
	"github.com/lidessen/crabbro/internal/history"
	"github.com/lidessen/crabbro/internal/inspect"
	"github.com/lidessen/crabbro/internal/mitm"
	"github.com/lidessen/crabbro/internal/plugin"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/session"
	"github.com/lidessen/crabbro/internal/shared"
)

// Server is the control API server, accessible over UDS and/or TCP.
type Server struct {
	UDS          string
	TCPAddr      string
	RingBuffer   *ring.RequestBuffer
	BodyCache    *capture.BodyCache
	EventHub     *capture.EventHub
	RuleStore    *rule.RuleStore
	RuleEngine   *rule.RuleEngine
	FixtureStore *fixture.FixtureStore
	ProxyAddr    string
	Version      string

	// RequestCountFn returns the current number of proxied requests.
	RequestCountFn func() int64
	// RuleCountFn returns the current number of registered rules.
	RuleCountFn func() int

	// MITMHandler is optional — if set, MITM control endpoints are registered.
	MITMHandler *mitm.MITMHandler

	// Sessions is the trajectory log. Nil disables session routes' data.
	Sessions *session.Store
	// History is the durable exchange log. Nil keeps the in-memory ring only.
	History *history.Store
	// Decision is the optional Jev / OpenAI client. Nil means unconfigured.
	Decision *decision.Client
	// Plugins is the Node host supervisor. Nil disables plugin routes.
	Plugins *plugin.Manager
	// Secrets is the shared mode-0600 credential store.
	Secrets *secret.Store
	// Gateway is the multi-protocol LLM & Decision gateway. Nil disables it.
	Gateway *gateway.Gateway
	// Inspect attributes traffic and holds session scope plus durable lists.
	Inspect *inspect.Runtime
	// RulesDir is the on-disk rule directory. Nil disables draft/commit.
	RulesDir *rule.Dir
	// Replay runs a captured request back through the proxy.
	Replay func(shared.VerifyRequest) (shared.VerifyResult, error)
	// PublishMITM republishes the network policy after a control-plane change.
	PublishMITM func()
	// Token is required on TCP control connections. Unix sockets are owner-only
	// and do not need it. Empty disables the check.
	Token string

	httpServer  *http.Server
	mux         *http.ServeMux
	handler     http.Handler // middleware-wrapped handler used by http.Server and ServeHTTP
	startOnce   sync.Once
	startedAt   time.Time
	udsListener net.Listener
	tcpListener net.Listener
}

// New creates a new API server backed by the given proxy and store components.
// If mitmHandler is non-nil, MITM control routes are registered during construction.
func New(uds string, tcpAddr string, rb *ring.RequestBuffer, bodyCache *capture.BodyCache, eventHub *capture.EventHub, fixtureStore *fixture.FixtureStore, ruleStore *rule.RuleStore, ruleEngine *rule.RuleEngine, proxyAddr string, mitmHandler *mitm.MITMHandler) *Server {
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
		MITMHandler:  mitmHandler,
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
	s.mux.HandleFunc("/v1/rules/preview", s.handleRulesPreview)
	s.mux.HandleFunc("/v1/rules/diagnose", s.handleRulesDiagnose)
	s.mux.HandleFunc("/v1/rules/reload", s.handleRulesReload)
	s.mux.HandleFunc("/v1/rules", s.handleRules)
	s.mux.HandleFunc("/v1/sources", s.handleSources)
	s.mux.HandleFunc("/v1/sources/", s.handleSources)
	s.mux.HandleFunc("/v1/scope", s.handleScope)
	s.mux.HandleFunc("/v1/config/status", s.handleConfigStatus)
	s.mux.HandleFunc("/v1/config/draft", s.handleConfigDraft)
	s.mux.HandleFunc("/v1/config/validate", s.handleConfigValidate)
	s.mux.HandleFunc("/v1/config/commit", s.handleConfigCommit)
	s.mux.HandleFunc("/v1/config/discard", s.handleConfigDiscard)
	s.mux.HandleFunc("/v1/plugins/host", s.handlePluginHost)
	s.mux.HandleFunc("/v1/plugins", s.handlePlugins)
	s.mux.HandleFunc("/v1/plugins/", s.handlePluginByID)
	s.mux.HandleFunc("/v1/secrets", s.handleSecrets)
	s.mux.HandleFunc("/v1/secrets/", s.handleSecretByName)
	s.mux.HandleFunc("/v1/gateway/status", s.handleGatewayStatus)
	s.mux.HandleFunc("/v1/gateway/plans", s.handleGatewayPlans)
	s.mux.HandleFunc("/v1/gateway/quotas/refresh", s.handleGatewayQuotaRefresh)
	s.mux.HandleFunc("/v1/gateway/quotas/reset", s.handleGatewayQuotaReset)
	s.mux.HandleFunc("/v1/gateway/plans/", s.handleGatewayPlanAction)
	s.mux.HandleFunc("/v1/gateway/routes", s.handleGatewayRoutes)
	s.mux.HandleFunc("/v1/gateway/usage", s.handleGatewayUsage)
	s.mux.HandleFunc("/v1/gateway/preview", s.handleGatewayPreview)
	s.mux.HandleFunc("/v1/gateway/reload", s.handleGatewayReload)
	s.mux.HandleFunc("/v1/rules/", s.handleRulesByID)

	s.mux.HandleFunc("/v1/fixtures", s.handleFixturesListOrSave)
	s.mux.HandleFunc("/v1/fixtures/", s.handleFixtureByName)

	s.mux.HandleFunc("/v1/sessions/current", s.handleSessionCurrent)
	s.mux.HandleFunc("/v1/sessions", s.handleSessions)
	s.mux.HandleFunc("/v1/sessions/", s.handleSessionByID)
	s.mux.HandleFunc("/v1/activity", s.handleActivity)
	s.mux.HandleFunc("/v1/trajectory", s.handleTrajectory)
	s.mux.HandleFunc("/v1/verify/replay", s.handleVerifyReplay)
	s.mux.HandleFunc("/v1/find", s.handleFind)
	s.mux.HandleFunc("/v1/decision", s.handleDecisionStatus)
	s.mux.HandleFunc("/v1/history", s.handleHistory)
	s.mux.HandleFunc("/v1/history/prune", s.handleHistoryPrune)
	s.mux.HandleFunc("/v1/rules/draft", s.handleRulesDraft)

	// Milestone 3: MITM routes (registered only when a MITMHandler is set).
	if s.MITMHandler != nil {
		s.mux.HandleFunc("/v1/mitm/enable", s.handleMITMEnable)
		s.mux.HandleFunc("/v1/mitm/disable", s.handleMITMDisable)
		s.mux.HandleFunc("/v1/mitm/status", s.handleMITMStatus)
		s.mux.HandleFunc("/v1/mitm/cert", s.handleMITMCert)
		s.mux.HandleFunc("/v1/mitm/cert-installed", s.handleMITMCertInstalled)
		s.mux.HandleFunc("/v1/mitm/allow", s.handleMITMAllow)
		s.mux.HandleFunc("/v1/mitm/deny", s.handleMITMDeny)
	}

	// Wrap with middleware: recovery (outermost) → auth → logging → routes.
	s.handler = wrapMiddleware(s.authMiddleware(s.mux))
}

type connClassKeyType struct{}

var connClassKey connClassKeyType

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class, _ := r.Context().Value(connClassKey).(string)
		if class == "tcp" && s.Token != "" {
			got := r.Header.Get("Authorization")
			want := "Bearer " + s.Token
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				writeError(w, http.StatusUnauthorized, shared.ErrCodeUnauthorized, "control API token required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Start begins listening on UDS and/or TCP. It returns once all listeners are
// ready, or an error if none can start.
func (s *Server) Start() error {
	var startErr error
	s.startOnce.Do(func() {
		s.startedAt = time.Now()

		s.httpServer = &http.Server{
			Handler: s.handler,
			ConnContext: func(ctx context.Context, c net.Conn) context.Context {
				if _, ok := c.(*net.UnixConn); ok {
					return context.WithValue(ctx, connClassKey, "unix")
				}
				return context.WithValue(ctx, connClassKey, "tcp")
			},
		}

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
					os.Chmod(udsPath, 0600)
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

func (s *Server) publishMITM() {
	if s.PublishMITM != nil {
		s.PublishMITM()
	}
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
