package executor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/lidessen/crabbro/internal/gateway/config"
)

// TransportPool caches *http.Transport instances keyed by optional per-plan proxyUrl.
type TransportPool struct {
	mu         sync.Mutex
	transports map[string]*http.Transport
}

// NewTransportPool creates a pooled upstream HTTP transport manager.
func NewTransportPool() *TransportPool {
	return &TransportPool{
		transports: map[string]*http.Transport{},
	}
}

// ForProxy returns a pooled *http.Transport configured for direct egress (proxyURL == "")
// or the specified HTTP/HTTPS/SOCKS5 proxy URL.
func (p *TransportPool) ForProxy(proxyURL string) (*http.Transport, error) {
	key := strings.TrimSpace(proxyURL)
	p.mu.Lock()
	if tr, ok := p.transports[key]; ok {
		p.mu.Unlock()
		return tr, nil
	}
	p.mu.Unlock()

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: config.DefaultTTFTTimeout,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   32,
	}

	if key != "" {
		u, err := url.Parse(key)
		if err != nil {
			return nil, fmt.Errorf("invalid proxyUrl %q: %w", key, err)
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https":
			tr.Proxy = http.ProxyURL(u)
		case "socks5", "socks5h":
			var auth *xproxy.Auth
			if u.User != nil {
				pwd, _ := u.User.Password()
				auth = &xproxy.Auth{User: u.User.Username(), Password: pwd}
			}
			sd, err := xproxy.SOCKS5("tcp", u.Host, auth, dialer)
			if err != nil {
				return nil, fmt.Errorf("socks5 proxy %q: %w", key, err)
			}
			if cd, ok := sd.(xproxy.ContextDialer); ok {
				tr.DialContext = cd.DialContext
			} else {
				tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					return sd.Dial(network, addr)
				}
			}
		default:
			return nil, fmt.Errorf("unsupported proxyUrl scheme %q", u.Scheme)
		}
	}

	p.mu.Lock()
	if existing, ok := p.transports[key]; ok {
		p.mu.Unlock()
		tr.CloseIdleConnections()
		return existing, nil
	}
	p.transports[key] = tr
	p.mu.Unlock()
	return tr, nil
}

// Close closes idle connections on all pooled transports.
func (p *TransportPool) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	trs := make([]*http.Transport, 0, len(p.transports))
	for _, tr := range p.transports {
		trs = append(trs, tr)
	}
	p.transports = map[string]*http.Transport{}
	p.mu.Unlock()
	for _, tr := range trs {
		tr.CloseIdleConnections()
	}
}
