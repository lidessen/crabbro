package mitm

import (
	"crypto/tls"
	"net"
	"sync"
	"testing"
)

func TestNewMITMHandler(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	h := NewMITMHandler(ca, 100)
	if h.CA != ca {
		t.Error("NewMITMHandler did not store CA")
	}
	if h.CertManager == nil {
		t.Error("NewMITMHandler did not create CertManager")
	}
	if h.Matcher == nil {
		t.Error("NewMITMHandler did not create DomainMatcher")
	}
	if h.Enabled() {
		t.Error("NewMITMHandler should start disabled")
	}
	if h.CertInstalled() {
		t.Error("NewMITMHandler should start with cert not installed")
	}
}

func TestEnabled(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	if h.Enabled() {
		t.Error("should start disabled")
	}

	h.SetEnabled(true)
	if !h.Enabled() {
		t.Error("should be enabled after SetEnabled(true)")
	}

	h.SetEnabled(false)
	if h.Enabled() {
		t.Error("should be disabled after SetEnabled(false)")
	}

	// Repeated calls should be fine.
	h.SetEnabled(true)
	h.SetEnabled(true)
	if !h.Enabled() {
		t.Error("should remain enabled after double SetEnabled(true)")
	}
}

func TestCertInstalled(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	if h.CertInstalled() {
		t.Error("should start with cert not installed")
	}

	h.SetCertInstalled(true)
	if !h.CertInstalled() {
		t.Error("should report installed after SetCertInstalled(true)")
	}

	h.SetCertInstalled(false)
	if h.CertInstalled() {
		t.Error("should report not installed after SetCertInstalled(false)")
	}
}

func TestGetCertPEM(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	pem := h.GetCertPEM()
	if len(pem) == 0 {
		t.Fatal("GetCertPEM() returned empty bytes")
	}

	// Verify it's valid PEM by parsing it.
	reloaded, err := ParsePEM(pem, ca.KeyPEM())
	if err != nil {
		t.Fatalf("GetCertPEM() output is not valid PEM: %v", err)
	}
	if !reloaded.Cert.Equal(ca.Cert) {
		t.Error("GetCertPEM() returned a different certificate")
	}
}

func TestIntercept_NotAllowed(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	// Empty allowlist means all allowed, so we need to set one and NOT include
	// the host to get a rejection.
	h.Matcher.SetAllow([]string{"allowed.example.com"})
	_, err = h.Intercept(nil, "blocked.example.com")
	if err == nil {
		t.Fatal("Intercept with non-allowed host should return error")
	}
}

func TestIntercept_Denied(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	h.Matcher.AddDeny("evil.com")
	_, err = h.Intercept(nil, "evil.com")
	if err == nil {
		t.Fatal("Intercept with denied host should return error")
	}
}

func TestIntercept_SuccessfulTLS(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("test.example.com")

	// Create a pipe for in-memory connection.
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	// Perform TLS client handshake in background.
	errCh := make(chan error, 1)
	go func() {
		// Client connects with TLS — since we don't have the CA cert in the
		// client's root pool, InsecureSkipVerify is needed. This mirrors how a
		// browser would connect before the root CA is installed.
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "test.example.com",
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		if handshakeErr := clientTLS.Handshake(); handshakeErr != nil {
			errCh <- handshakeErr
			return
		}
		defer clientTLS.Close()

		// Write a simple HTTP request over the TLS connection.
		_, writeErr := clientTLS.Write([]byte("GET / HTTP/1.1\r\nHost: test.example.com\r\n\r\n"))
		if writeErr != nil {
			errCh <- writeErr
			return
		}

		// Read the response.
		buf := make([]byte, 1024)
		n, readErr := clientTLS.Read(buf)
		if readErr != nil {
			errCh <- readErr
			return
		}
		if n == 0 {
			errCh <- net.ErrClosed
			return
		}
		errCh <- nil
	}()

	// MITM intercept on the server end.
	tlsConn, err := h.Intercept(serverEnd, "test.example.com")
	if err != nil {
		t.Fatalf("Intercept() failed: %v", err)
	}
	defer tlsConn.Close()

	// Verify we can read the decrypted HTTP request.
	buf := make([]byte, 1024)
	n, err := tlsConn.Read(buf)
	if err != nil {
		t.Fatalf("read from TLS conn: %v", err)
	}
	if n == 0 {
		t.Fatal("read 0 bytes from TLS conn")
	}

	// Write a response back.
	_, err = tlsConn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nOK"))
	if err != nil {
		t.Fatalf("write to TLS conn: %v", err)
	}

	// Check that the client side completed without error.
	if err := <-errCh; err != nil {
		t.Fatalf("client TLS handshake/exchange failed: %v", err)
	}
}

func TestIntercept_PortStripped(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	// Allow without port, connect with port.
	h.Matcher.AddAllow("test.example.com")

	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	go func() {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "test.example.com",
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	_, err = h.Intercept(serverEnd, "test.example.com:443")
	if err != nil {
		t.Fatalf("Intercept with port should succeed: %v", err)
	}
}

func TestIntercept_ConcurrentSafe(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("*")

	var wg sync.WaitGroup
	errCh := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			serverEnd, clientEnd := net.Pipe()

			go func() {
				tlsConfig := &tls.Config{
					InsecureSkipVerify: true,
					ServerName:         "test.example.com",
				}
				clientTLS := tls.Client(clientEnd, tlsConfig)
				_ = clientTLS.Handshake()
				clientTLS.Close()
			}()

			_, interceptErr := h.Intercept(serverEnd, "test.example.com")
			serverEnd.Close()
			if interceptErr != nil {
				errCh <- interceptErr
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent Intercept failed: %v", err)
	}
}

func TestIntercept_NoAllowlistAllAllowed(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	// Empty allowlist = all domains allowed.
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	go func() {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "anything.example.com",
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	_, err = h.Intercept(serverEnd, "anything.example.com")
	if err != nil {
		t.Fatalf("Intercept with empty allowlist (all allowed) should succeed: %v", err)
	}
}

func TestEnabled_ConcurrentSafe(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				h.SetEnabled(true)
			} else {
				_ = h.Enabled()
			}
		}(i)
	}
	wg.Wait()

	// Should not race or panic.
	h.SetEnabled(false)
	_ = h.Enabled()
}

func TestStats_InitiallyZero(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	stats := h.Stats()
	if stats.TotalHandshakes != 0 {
		t.Errorf("expected TotalHandshakes=0, got %d", stats.TotalHandshakes)
	}
	if stats.SuccessfulHandshakes != 0 {
		t.Errorf("expected SuccessfulHandshakes=0, got %d", stats.SuccessfulHandshakes)
	}
	if stats.FailedHandshakes != 0 {
		t.Errorf("expected FailedHandshakes=0, got %d", stats.FailedHandshakes)
	}
	if stats.ActiveConnections != 0 {
		t.Errorf("expected ActiveConnections=0, got %d", stats.ActiveConnections)
	}
}

func TestStats_AfterSuccessfulIntercept(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("example.com")

	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	go func() {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "example.com",
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	tlsConn, err := h.Intercept(serverEnd, "example.com")
	if err != nil {
		t.Fatalf("Intercept() failed: %v", err)
	}
	tlsConn.Close()

	stats := h.Stats()
	if stats.TotalHandshakes != 1 {
		t.Errorf("expected TotalHandshakes=1, got %d", stats.TotalHandshakes)
	}
	if stats.SuccessfulHandshakes != 1 {
		t.Errorf("expected SuccessfulHandshakes=1, got %d", stats.SuccessfulHandshakes)
	}
	if stats.FailedHandshakes != 0 {
		t.Errorf("expected FailedHandshakes=0, got %d", stats.FailedHandshakes)
	}
	// activeConnections is a live gauge tracking connections Intercept has
	// established; individual Close() does not decrement it — only
	// CloseActiveConnections() does.
	if stats.ActiveConnections != 1 {
		t.Errorf("expected ActiveConnections=1 (still tracked), got %d", stats.ActiveConnections)
	}
}

func TestStats_AfterFailedIntercept(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("example.com")

	// Pass a closed connection — TLS handshake will fail.
	serverEnd, clientEnd := net.Pipe()
	clientEnd.Close()

	_, err = h.Intercept(serverEnd, "example.com")
	if err == nil {
		t.Fatal("expected Intercept to fail on closed connection")
	}

	stats := h.Stats()
	if stats.TotalHandshakes != 1 {
		t.Errorf("expected TotalHandshakes=1, got %d", stats.TotalHandshakes)
	}
	if stats.SuccessfulHandshakes != 0 {
		t.Errorf("expected SuccessfulHandshakes=0, got %d", stats.SuccessfulHandshakes)
	}
	if stats.FailedHandshakes != 1 {
		t.Errorf("expected FailedHandshakes=1, got %d", stats.FailedHandshakes)
	}
	if stats.ActiveConnections != 0 {
		t.Errorf("expected ActiveConnections=0, got %d", stats.ActiveConnections)
	}
}

func TestResetStats(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	// Trigger a failed handshake which bumps total+failed (deny returns before
	// stats are incremented, so we need a connection that fails during the
	// TLS handshake).
	h.Matcher.AddAllow("reset.example.com")
	serverEnd, clientEnd := net.Pipe()
	clientEnd.Close()
	_, _ = h.Intercept(serverEnd, "reset.example.com")

	stats := h.Stats()
	if stats.TotalHandshakes == 0 {
		t.Fatal("expected stats to be non-zero before reset")
	}

	h.ResetStats()
	stats = h.Stats()
	if stats.TotalHandshakes != 0 {
		t.Errorf("expected TotalHandshakes=0 after reset, got %d", stats.TotalHandshakes)
	}
	if stats.SuccessfulHandshakes != 0 {
		t.Errorf("expected SuccessfulHandshakes=0 after reset, got %d", stats.SuccessfulHandshakes)
	}
	if stats.FailedHandshakes != 0 {
		t.Errorf("expected FailedHandshakes=0 after reset, got %d", stats.FailedHandshakes)
	}
	// ResetStats does not reset activeConnections since it is a live gauge.
	if stats.ActiveConnections != 0 {
		t.Errorf("expected ActiveConnections=0, got %d", stats.ActiveConnections)
	}
}

func TestActiveConnections(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("*")

	if n := h.ActiveConnections(); n != 0 {
		t.Fatalf("expected 0 active connections initially, got %d", n)
	}

	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	go func() {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "act.example.com",
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	tlsConn, err := h.Intercept(serverEnd, "act.example.com")
	if err != nil {
		t.Fatalf("Intercept() failed: %v", err)
	}

	if n := h.ActiveConnections(); n != 1 {
		t.Errorf("expected 1 active connection, got %d", n)
	}

	// Individual Close() does not decrement the gauge — only CloseActiveConnections() does.
	tlsConn.Close()
	if n := h.ActiveConnections(); n != 1 {
		t.Errorf("expected ActiveConnections=1 (Close doesn't decrement), got %d", n)
	}

	// Use CloseActiveConnections to properly clean up.
	h.CloseActiveConnections()
	if n := h.ActiveConnections(); n != 0 {
		t.Errorf("expected 0 active connections after CloseActiveConnections, got %d", n)
	}
}

func TestCloseActiveConnections(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("*")

	// Establish two connections.
	var conns []*tls.Conn
	for i := 0; i < 2; i++ {
		serverEnd, clientEnd := net.Pipe()
		go func(ce net.Conn) {
			tlsConfig := &tls.Config{
				InsecureSkipVerify: true,
				ServerName:         "close.example.com",
			}
			clientTLS := tls.Client(ce, tlsConfig)
			_ = clientTLS.Handshake()
		}(clientEnd)

		tlsConn, ictxErr := h.Intercept(serverEnd, "close.example.com")
		if ictxErr != nil {
			t.Fatalf("Intercept #%d failed: %v", i, ictxErr)
		}
		conns = append(conns, tlsConn)
	}

	if n := h.ActiveConnections(); n != 2 {
		t.Fatalf("expected 2 active connections, got %d", n)
	}

	// Close all tracked connections.
	h.CloseActiveConnections()

	// After close, all tlsConns should be closed — reads return error.
	for i, tc := range conns {
		buf := make([]byte, 1)
		if _, readErr := tc.Read(buf); readErr == nil {
			t.Errorf("conn #%d: expected error reading from closed connection", i)
		}
	}

	// Active connections count should be 0.
	if n := h.ActiveConnections(); n != 0 {
		t.Errorf("expected 0 active connections after CloseActiveConnections, got %d", n)
	}
}

func TestCloseActiveConnections_Empty(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	// Should not panic when no connections are tracked.
	h.CloseActiveConnections()
}

func TestSetMinTLSVersion(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	h.SetMinTLSVersion(tls.VersionTLS12)

	// Verify by making a successful handshake.
	h.Matcher.AddAllow("tls.example.com")
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	go func() {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "tls.example.com",
			MaxVersion:         tls.VersionTLS12,
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	_, err = h.Intercept(serverEnd, "tls.example.com")
	if err != nil {
		t.Fatalf("Intercept with MinVersion=TLS12 should succeed: %v", err)
	}
}

func TestSetMaxTLSVersion(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)

	h.SetMaxTLSVersion(tls.VersionTLS12)

	// Verify the setting works by making a TLS 1.2 handshake.
	h.Matcher.AddAllow("tlsmax.example.com")
	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	go func() {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "tlsmax.example.com",
			MinVersion:         tls.VersionTLS12,
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	_, err = h.Intercept(serverEnd, "tlsmax.example.com")
	if err != nil {
		t.Fatalf("Intercept with MaxVersion=TLS12 should succeed: %v", err)
	}
}

func TestIntercept_AfterCloseActiveConnections(t *testing.T) {
	// Regression test: new connections should still work after CloseActiveConnections.
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("*")

	// First connection.
	serverEnd, clientEnd := net.Pipe()
	go func() {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "regression.example.com",
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	tlsConn, err := h.Intercept(serverEnd, "regression.example.com")
	if err != nil {
		t.Fatalf("first Intercept failed: %v", err)
	}

	// Close all.
	h.CloseActiveConnections()

	if n := h.ActiveConnections(); n != 0 {
		t.Errorf("expected 0 after close, got %d", n)
	}

	// The old conn should be dead.
	buf := make([]byte, 1)
	if _, readErr := tlsConn.Read(buf); readErr == nil {
		t.Error("expected old connection to be closed")
	}

	// Second connection should work fine.
	serverEnd2, clientEnd2 := net.Pipe()
	defer serverEnd2.Close()
	defer clientEnd2.Close()

	go func() {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "new.example.com",
		}
		clientTLS := tls.Client(clientEnd2, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	tlsConn2, err := h.Intercept(serverEnd2, "new.example.com")
	if err != nil {
		t.Fatalf("second Intercept after CloseActiveConnections failed: %v", err)
	}
	defer tlsConn2.Close()

	if n := h.ActiveConnections(); n != 1 {
		t.Errorf("expected 1 active connection after new Intercept, got %d", n)
	}
}

func TestIntercept_HandshakeClientTimeout(t *testing.T) {
	// Client connects but never handshakes — should clean up properly.
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("timeout.example.com")

	serverEnd, clientEnd := net.Pipe()

	// Close the client end immediately so the handshake fails.
	clientEnd.Close()

	_, err = h.Intercept(serverEnd, "timeout.example.com")
	if err == nil {
		t.Fatal("expected Intercept to fail when client disconnects before handshake")
	}

	stats := h.Stats()
	if stats.TotalHandshakes != 1 {
		t.Errorf("expected TotalHandshakes=1, got %d", stats.TotalHandshakes)
	}
	if stats.FailedHandshakes != 1 {
		t.Errorf("expected FailedHandshakes=1, got %d", stats.FailedHandshakes)
	}
	if stats.SuccessfulHandshakes != 0 {
		t.Errorf("expected SuccessfulHandshakes=0, got %d", stats.SuccessfulHandshakes)
	}
	if stats.ActiveConnections != 0 {
		t.Errorf("expected ActiveConnections=0 after fail, got %d", stats.ActiveConnections)
	}
}

func TestSetMinTLSVersion_BlocksOlderClient(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("test.example.com")

	// Server requires TLS 1.3+.
	h.SetMinTLSVersion(tls.VersionTLS13)

	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	go func() {
		// Client only supports up to TLS 1.2.
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "test.example.com",
			MaxVersion:         tls.VersionTLS12,
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	_, err = h.Intercept(serverEnd, "test.example.com")
	if err == nil {
		t.Fatal("expected Intercept to fail when client TLS 1.2 connects with server MinVersion=TLS 1.3")
	}
}

func TestSetMaxTLSVersion_BlocksClientRequiringNewer(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	h := NewMITMHandler(ca, 100)
	h.Matcher.AddAllow("test.example.com")

	// Server only accepts up to TLS 1.2.
	h.SetMaxTLSVersion(tls.VersionTLS12)

	serverEnd, clientEnd := net.Pipe()
	defer serverEnd.Close()
	defer clientEnd.Close()

	go func() {
		// Client requires at least TLS 1.3.
		tlsConfig := &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         "test.example.com",
			MinVersion:         tls.VersionTLS13,
		}
		clientTLS := tls.Client(clientEnd, tlsConfig)
		_ = clientTLS.Handshake()
		clientTLS.Close()
	}()

	_, err = h.Intercept(serverEnd, "test.example.com")
	if err == nil {
		t.Fatal("expected Intercept to fail when server MaxVersion=TLS 1.2 and client MinVersion=TLS 1.3")
	}
}
