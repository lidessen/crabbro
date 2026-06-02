package mitm

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

// MITMStats holds cumulative statistics for the TLS interception handler.
type MITMStats struct {
	// TotalHandshakes is the total number of TLS handshakes attempted.
	TotalHandshakes int64 `json:"totalHandshakes"`
	// SuccessfulHandshakes is the number of successful TLS handshakes.
	SuccessfulHandshakes int64 `json:"successfulHandshakes"`
	// FailedHandshakes is the number of failed TLS handshakes.
	FailedHandshakes int64 `json:"failedHandshakes"`
	// ActiveConnections is the current number of active intercepted TLS connections.
	ActiveConnections int64 `json:"activeConnections"`
}

// MITMHandler orchestrates TLS interception for HTTPS connections.
//
// It manages the MITM enable/disable state, performs TLS server-side handshakes
// with clients using dynamically-generated certificates, tracks active connections
// for graceful shutdown, and provides domain filtering via DomainMatcher.
type MITMHandler struct {
	CA            *CAStore
	CertManager   *CertManager
	Matcher       *DomainMatcher

	mu            sync.RWMutex
	enabled       bool
	certInstalled bool

	// Connection tracking for graceful shutdown.
	conns   map[*tls.Conn]struct{}
	connMu  sync.Mutex

	// Cumulative statistics (accessed atomically).
	totalHandshakes      int64
	successfulHandshakes int64
	failedHandshakes     int64
	activeConnections    int64

	// Configurable TLS settings.
	minTLSVersion uint16
	maxTLSVersion uint16
}

// NewMITMHandler creates a MITMHandler with the given root CA, cert cache, and
// domain matcher. MaxCerts controls the per-host certificate cache size.
func NewMITMHandler(ca *CAStore, maxCerts int) *MITMHandler {
	return &MITMHandler{
		CA:          ca,
		CertManager: NewCertManager(ca, maxCerts),
		Matcher:     NewDomainMatcher(nil, nil),
		conns:       make(map[*tls.Conn]struct{}),
	}
}

// Intercept performs a TLS server-side handshake with the client on the given
// connection, using a dynamically-generated certificate for the target host.
//
// The caller MUST have already sent "HTTP/1.1 200 Connection Established\r\n\r\n"
// to the client. On success, Intercept returns a *tls.Conn through which
// plaintext HTTP can be read and written. The returned connection is tracked
// for lifecycle management; callers should close it normally via (*tls.Conn).Close().
//
// Callers should check Enabled() and Matcher.IsAllowed() before calling this;
// Intercept still checks IsAllowed as a safety net.
func (h *MITMHandler) Intercept(clientConn net.Conn, host string) (*tls.Conn, error) {
	if !h.Matcher.IsAllowed(host) {
		return nil, fmt.Errorf("host %q not allowed for MITM interception", host)
	}

	atomic.AddInt64(&h.totalHandshakes, 1)
	atomic.AddInt64(&h.activeConnections, 1)

	cert, err := h.CertManager.GetCertificate(host)
	if err != nil {
		atomic.AddInt64(&h.failedHandshakes, 1)
		atomic.AddInt64(&h.activeConnections, -1)
		return nil, fmt.Errorf("get certificate for %s: %w", host, err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{*cert},
	}
	if h.minTLSVersion != 0 {
		tlsConfig.MinVersion = h.minTLSVersion
	}
	if h.maxTLSVersion != 0 {
		tlsConfig.MaxVersion = h.maxTLSVersion
	}

	tlsConn := tls.Server(clientConn, tlsConfig)
	if err := tlsConn.Handshake(); err != nil {
		clientConn.Close()
		atomic.AddInt64(&h.failedHandshakes, 1)
		atomic.AddInt64(&h.activeConnections, -1)
		return nil, fmt.Errorf("TLS handshake with client for %s: %w", host, err)
	}

	atomic.AddInt64(&h.successfulHandshakes, 1)

	// Track for lifecycle management.
	h.addConn(tlsConn)

	return tlsConn, nil
}

// addConn registers a *tls.Conn for lifecycle tracking.
func (h *MITMHandler) addConn(c *tls.Conn) {
	h.connMu.Lock()
	h.conns[c] = struct{}{}
	h.connMu.Unlock()
}

// CloseActiveConnections closes all currently tracked TLS connections and
// resets the tracking set. New connections created after this call are tracked
// in a fresh set. This is safe to call concurrently with active Intercept calls.
//
// Typical usage is during graceful shutdown when the daemon wants to drain all
// intercepted HTTPS connections.
func (h *MITMHandler) CloseActiveConnections() {
	h.connMu.Lock()
	n := len(h.conns)
	conns := make([]*tls.Conn, 0, n)
	for tc := range h.conns {
		conns = append(conns, tc)
	}
	h.conns = make(map[*tls.Conn]struct{})
	h.connMu.Unlock()

	if n > 0 {
		atomic.AddInt64(&h.activeConnections, -int64(n))
	}

	for _, tc := range conns {
		tc.Close()
	}
}

// ActiveConnections returns the current number of active intercepted
// connections. This is the number of Intercept calls that succeeded minus
// failures and connections closed via CloseActiveConnections.
func (h *MITMHandler) ActiveConnections() int64 {
	return atomic.LoadInt64(&h.activeConnections)
}

// Stats returns cumulative handshake and connection statistics.
func (h *MITMHandler) Stats() MITMStats {
	return MITMStats{
		TotalHandshakes:      atomic.LoadInt64(&h.totalHandshakes),
		SuccessfulHandshakes: atomic.LoadInt64(&h.successfulHandshakes),
		FailedHandshakes:     atomic.LoadInt64(&h.failedHandshakes),
		ActiveConnections:    atomic.LoadInt64(&h.activeConnections),
	}
}

// ResetStats zeroes all handshake and connection statistics.
// ActiveConnections is not reset by this call since it is a live gauge.
func (h *MITMHandler) ResetStats() {
	atomic.StoreInt64(&h.totalHandshakes, 0)
	atomic.StoreInt64(&h.successfulHandshakes, 0)
	atomic.StoreInt64(&h.failedHandshakes, 0)
}

// SetMinTLSVersion sets the minimum TLS version for the server-side handshake.
// Pass 0 to accept Go's default (TLS 1.2). Valid values are tls.VersionTLS10
// through tls.VersionTLS13.
func (h *MITMHandler) SetMinTLSVersion(v uint16) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.minTLSVersion = v
}

// SetMaxTLSVersion sets the maximum TLS version for the server-side handshake.
// Pass 0 to accept Go's default (TLS 1.3). Valid values are tls.VersionTLS10
// through tls.VersionTLS13.
func (h *MITMHandler) SetMaxTLSVersion(v uint16) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.maxTLSVersion = v
}

// Enabled returns whether MITM interception is currently enabled.
func (h *MITMHandler) Enabled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.enabled
}

// SetEnabled toggles MITM interception on or off.
func (h *MITMHandler) SetEnabled(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled = v
}

// CertInstalled returns whether the root CA certificate has been installed
// in the system trust store.
func (h *MITMHandler) CertInstalled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.certInstalled
}

// SetCertInstalled records whether the root CA certificate has been installed.
func (h *MITMHandler) SetCertInstalled(v bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.certInstalled = v
}

// GetCertPEM returns the PEM-encoded root CA certificate bytes.
func (h *MITMHandler) GetCertPEM() []byte {
	return h.CA.CertPEM()
}
