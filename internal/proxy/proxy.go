// Package proxy implements an HTTP forward proxy with CONNECT support and
// request/response capture into the in-memory ring buffer.
package proxy

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lidessen/crabbro/internal/history"
	"github.com/lidessen/crabbro/internal/inspect"
	"github.com/lidessen/crabbro/internal/mitm"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/session"
	"github.com/lidessen/crabbro/internal/shared"
)

// Proxy is an HTTP forward proxy that captures request/response metadata and
// emits lifecycle events.
type Proxy struct {
	ListenAddr  string
	RingBuffer  *ring.RequestBuffer
	BodyCache   *BodyCache
	EventHub    *EventHub
	RuleStore   *rule.RuleStore
	RuleEngine  *rule.RuleEngine
	ReadTimeout time.Duration

	// MITMHandler performs the client-facing TLS handshake. Interception
	// decisions come from Policy, not from a second enabled flag.
	MITMHandler *mitm.MITMHandler
	// MITMEnabled mirrors the last published policy for callers that still
	// read the field. CONNECT routing uses Policy.
	MITMEnabled bool

	Policy   PolicyStore
	Sessions *session.Store
	History  *history.Store
	// Inspect attributes exchanges and applies view scope. Nil keeps the
	// proxy behavior from before inspection existed.
	Inspect *inspect.Runtime

	// DialTLS allows injecting a custom TLS dialer for upstream connections
	// in tests. If nil, the shared transport verifies server certificates.
	DialTLS func(dialer *net.Dialer, network, addr string, config *tls.Config) (*tls.Conn, error)

	server        *http.Server
	listener      net.Listener
	startOnce     sync.Once
	startedAt     time.Time
	reqCount      atomic.Int64
	transportOnce sync.Once
	httpTransport *http.Transport

	tunnelMu sync.Mutex
	tunnels  map[net.Conn]struct{}
}

// New creates a new Proxy with sensible defaults.
func remoteAddr(s string) net.Addr {
	a, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		return nil
	}
	return a
}

func New(addr string, rb *ring.RequestBuffer, ruleStore *rule.RuleStore, ruleEngine *rule.RuleEngine) *Proxy {
	return &Proxy{
		ListenAddr:  addr,
		RingBuffer:  rb,
		BodyCache:   NewBodyCache(shared.MaxBodyCacheBytes, shared.MaxBodySize),
		EventHub:    NewEventHub(),
		RuleStore:   ruleStore,
		RuleEngine:  ruleEngine,
		ReadTimeout: 30 * time.Second,
		Sessions:    session.New(1024),
		tunnels:     map[net.Conn]struct{}{},
	}
}

// PublishPolicy stores one immutable network policy and mirrors Enabled onto
// MITMEnabled so status readers and tests that inspect the field stay aligned.
func (p *Proxy) PublishPolicy(pol NetworkPolicy) uint64 {
	gen := p.Policy.Publish(pol)
	p.MITMEnabled = pol.Enabled
	return gen
}

// PublishFromMITM snapshots the handler's current enabled flag and patterns.
// An empty allowlist denies interception.
func (p *Proxy) PublishFromMITM() uint64 {
	pol := NetworkPolicy{EmptyAllowDenies: true}
	if p.MITMHandler != nil {
		pol.Enabled = p.MITMHandler.Enabled()
		pol.Allow = p.MITMHandler.Matcher.AllowPatterns()
		pol.Deny = p.MITMHandler.Matcher.DenyPatterns()
	}
	return p.PublishPolicy(pol)
}

// policyForTunnel returns the published snapshot, or a legacy snapshot derived
// from MITMEnabled and the handler matcher when nothing has been published.
// The legacy path keeps empty-allowlist = allow so existing unit tests that
// set the fields directly keep their contract. Published policies deny an
// empty allowlist.
func (p *Proxy) policyForTunnel() *NetworkPolicy {
	if pol := p.Policy.Current(); pol != nil {
		return pol
	}
	pol := &NetworkPolicy{Enabled: p.MITMEnabled && p.MITMHandler != nil}
	if p.MITMHandler != nil {
		pol.Allow = p.MITMHandler.Matcher.AllowPatterns()
		pol.Deny = p.MITMHandler.Matcher.DenyPatterns()
	}
	return pol
}

// Start begins listening for proxy connections. It returns once the listener
// is ready.
func (p *Proxy) Start() error {
	var err error
	p.startOnce.Do(func() {
		p.startedAt = time.Now()
		p.listener, err = net.Listen("tcp", p.ListenAddr)
		if err != nil {
			return
		}
		p.server = &http.Server{
			Handler:           http.HandlerFunc(p.handleProxy),
			ReadHeaderTimeout: p.ReadTimeout,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			if serveErr := p.server.Serve(p.listener); serveErr != nil && serveErr != http.ErrServerClosed {
				log.Printf("proxy server error: %v", serveErr)
			}
		}()
	})
	return err
}

// Stop gracefully shuts down the proxy server.
func (p *Proxy) Stop() error {
	p.closeTunnels()
	if p.MITMHandler != nil {
		p.MITMHandler.CloseActiveConnections()
	}
	if p.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.server.Shutdown(ctx)
}

func (p *Proxy) trackTunnel(c net.Conn) {
	p.tunnelMu.Lock()
	p.tunnels[c] = struct{}{}
	p.tunnelMu.Unlock()
}

func (p *Proxy) untrackTunnel(c net.Conn) {
	p.tunnelMu.Lock()
	delete(p.tunnels, c)
	p.tunnelMu.Unlock()
}

func (p *Proxy) closeTunnels() {
	p.tunnelMu.Lock()
	conns := make([]net.Conn, 0, len(p.tunnels))
	for c := range p.tunnels {
		conns = append(conns, c)
	}
	p.tunnelMu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// Addr returns the address the proxy is listening on.
func (p *Proxy) Addr() string {
	if p.listener != nil {
		return p.listener.Addr().String()
	}
	return p.ListenAddr
}

// RequestCount returns the number of requests handled.
func (p *Proxy) RequestCount() int64 {
	return p.reqCount.Load()
}

// StartedAt returns when the proxy started.
func (p *Proxy) StartedAt() time.Time {
	return p.startedAt
}

// handleProxy dispatches to the appropriate handler based on the request method
// and target.
func (p *Proxy) handleProxy(w http.ResponseWriter, r *http.Request) {
	p.reqCount.Add(1)

	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}
