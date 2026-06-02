// Package proxy implements an HTTP forward proxy with CONNECT support and
// request/response capture into the in-memory ring buffer.
package proxy

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lidessen/chiling/internal/ring"
	"github.com/lidessen/chiling/internal/shared"
)

// Proxy is an HTTP forward proxy that captures request/response metadata and
// emits lifecycle events.
type Proxy struct {
	ListenAddr  string
	RingBuffer  *ring.RequestBuffer
	BodyCache   *BodyCache
	EventHub    *EventHub
	ReadTimeout time.Duration

	server    *http.Server
	listener  net.Listener
	startOnce sync.Once
	startedAt time.Time
	reqCount  atomic.Int64
}

// New creates a new Proxy with sensible defaults.
func New(addr string, rb *ring.RequestBuffer) *Proxy {
	return &Proxy{
		ListenAddr:  addr,
		RingBuffer:  rb,
		BodyCache:   NewBodyCache(shared.MaxBodyCacheBytes, shared.MaxBodySize),
		EventHub:    NewEventHub(),
		ReadTimeout: 30 * time.Second,
	}
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
		mux := http.NewServeMux()
		mux.HandleFunc("/", p.handleProxy)

		p.server = &http.Server{
			Handler:     mux,
			ReadTimeout: p.ReadTimeout,
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
	if p.server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.server.Shutdown(ctx)
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
