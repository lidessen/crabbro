package mitm

import (
	"crypto/x509"
	"fmt"
	"sort"
	"sync"
	"testing"
)

func TestNewCertManager(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	// Zero value — should use default.
	cm := NewCertManager(ca, 0)
	if cm.maxCerts != DefaultMaxCerts {
		t.Errorf("expected maxCerts=%d, got %d", DefaultMaxCerts, cm.maxCerts)
	}

	// Negative value — should use default.
	cm = NewCertManager(ca, -1)
	if cm.maxCerts != DefaultMaxCerts {
		t.Errorf("expected maxCerts=%d, got %d", DefaultMaxCerts, cm.maxCerts)
	}

	// Explicit value — should preserve it.
	cm = NewCertManager(ca, 500)
	if cm.maxCerts != 500 {
		t.Errorf("expected maxCerts=%d, got %d", 500, cm.maxCerts)
	}
}

func TestGetCertificate(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	cert, err := cm.GetCertificate("example.com")
	if err != nil {
		t.Fatalf("GetCertificate() failed: %v", err)
	}
	if cert == nil {
		t.Fatal("GetCertificate() returned nil")
	}
	if cert.PrivateKey == nil {
		t.Fatal("cert.PrivateKey is nil")
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("cert.Certificate is empty")
	}
}

func TestGetCertificate_Cached(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	cert1, err := cm.GetCertificate("example.com")
	if err != nil {
		t.Fatalf("first GetCertificate() failed: %v", err)
	}
	cert2, err := cm.GetCertificate("example.com")
	if err != nil {
		t.Fatalf("second GetCertificate() failed: %v", err)
	}

	// Parse leaf certificates to compare serial numbers.
	leaf1, err := x509.ParseCertificate(cert1.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf1: %v", err)
	}
	leaf2, err := x509.ParseCertificate(cert2.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf2: %v", err)
	}

	if leaf1.SerialNumber.Cmp(leaf2.SerialNumber) != 0 {
		t.Error("expected same serial number (cached), but got different serial numbers")
	}
}

func TestGetCertificate_MultipleHosts(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	cert1, err := cm.GetCertificate("host-a.example.com")
	if err != nil {
		t.Fatalf("GetCertificate(host-a) failed: %v", err)
	}
	cert2, err := cm.GetCertificate("host-b.example.com")
	if err != nil {
		t.Fatalf("GetCertificate(host-b) failed: %v", err)
	}

	leaf1, err := x509.ParseCertificate(cert1.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf1: %v", err)
	}
	leaf2, err := x509.ParseCertificate(cert2.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf2: %v", err)
	}

	if leaf1.SerialNumber.Cmp(leaf2.SerialNumber) == 0 {
		t.Error("expected different serial numbers for different hosts, but they are equal")
	}
}

func TestGetCertificate_PortStripped(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	cert1, err := cm.GetCertificate("example.com")
	if err != nil {
		t.Fatalf("GetCertificate(example.com) failed: %v", err)
	}
	cert2, err := cm.GetCertificate("example.com:443")
	if err != nil {
		t.Fatalf("GetCertificate(example.com:443) failed: %v", err)
	}

	leaf1, err := x509.ParseCertificate(cert1.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf1: %v", err)
	}
	leaf2, err := x509.ParseCertificate(cert2.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf2: %v", err)
	}

	if leaf1.SerialNumber.Cmp(leaf2.SerialNumber) != 0 {
		t.Error("expected same certificate for example.com and example.com:443, but got different serial numbers")
	}
}

func TestGetCertificate_DNSNames(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	cert, err := cm.GetCertificate("secure.example.com")
	if err != nil {
		t.Fatalf("GetCertificate() failed: %v", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "secure.example.com" {
		t.Errorf("expected DNSNames=[secure.example.com], got %v", leaf.DNSNames)
	}
	if leaf.Subject.CommonName != "secure.example.com" {
		t.Errorf("expected CN=secure.example.com, got %q", leaf.Subject.CommonName)
	}
}

func TestGetCertificate_SignedByCA(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	cert, err := cm.GetCertificate("signed-test.example.com")
	if err != nil {
		t.Fatalf("GetCertificate() failed: %v", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	// Verify the leaf cert was signed by the CA.
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)

	opts := x509.VerifyOptions{
		Roots: roots,
	}
	if _, err := leaf.Verify(opts); err != nil {
		t.Errorf("leaf cert verification against CA failed: %v", err)
	}
}

func TestGetCertificate_CacheEviction(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	maxCerts := 10
	cm := NewCertManager(ca, maxCerts)

	// Generate certificates for more hosts than the cache allows.
	numHosts := maxCerts + 5
	hosts := make([]string, numHosts)
	for i := 0; i < numHosts; i++ {
		hosts[i] = fmt.Sprintf("host-%d.test", i)
	}

	for _, host := range hosts {
		_, err := cm.GetCertificate(host)
		if err != nil {
			t.Fatalf("GetCertificate(%s) failed: %v", host, err)
		}
	}

	// Cache should be bounded by maxCerts.
	if cm.Len() > maxCerts {
		t.Errorf("cache size %d exceeds maxCerts %d", cm.Len(), maxCerts)
	}

	// The first 5 hosts should have been evicted (FIFO eviction).
	// Only the last maxCerts hosts should remain.
	for i := 0; i < 5; i++ {
		host := fmt.Sprintf("host-%d.test", i)
		// These should need regeneration, so should succeed.
		cert, err := cm.GetCertificate(host)
		if err != nil {
			t.Fatalf("regeneration of evicted host %s failed: %v", host, err)
		}
		if cert == nil {
			t.Fatalf("regeneration of evicted host %s returned nil", host)
		}
	}
}

func TestGetCertificate_FullConcurrent(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	hosts := []string{"a.com", "b.com", "c.com", "d.com", "e.com"}
	concurrency := 50

	var wg sync.WaitGroup
	// Collect errors via a channel to avoid data races on t.Errorf from
	// multiple goroutines.
	errCh := make(chan string, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			host := hosts[n%len(hosts)]
			cert, getErr := cm.GetCertificate(host)
			if getErr != nil {
				errCh <- fmt.Sprintf("GetCertificate(%s): %v", host, getErr)
				return
			}
			if cert == nil {
				errCh <- fmt.Sprintf("GetCertificate(%s) returned nil", host)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for msg := range errCh {
		t.Error(msg)
	}
}

func TestLen(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	if cm.Len() != 0 {
		t.Errorf("expected Len()=0 for empty cache, got %d", cm.Len())
	}

	_, _ = cm.GetCertificate("a.example.com")
	if cm.Len() != 1 {
		t.Errorf("expected Len()=1 after adding one host, got %d", cm.Len())
	}

	_, _ = cm.GetCertificate("b.example.com")
	if cm.Len() != 2 {
		t.Errorf("expected Len()=2 after adding two hosts, got %d", cm.Len())
	}
}

func TestClear(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	_, _ = cm.GetCertificate("a.example.com")
	_, _ = cm.GetCertificate("b.example.com")
	_, _ = cm.GetCertificate("c.example.com")

	if cm.Len() != 3 {
		t.Fatalf("expected Len()=3 before clear, got %d", cm.Len())
	}

	cm.Clear()
	if cm.Len() != 0 {
		t.Errorf("expected Len()=0 after clear, got %d", cm.Len())
	}

	// Cache should still be usable after clear.
	_, err = cm.GetCertificate("d.example.com")
	if err != nil {
		t.Errorf("GetCertificate after clear failed: %v", err)
	}
	if cm.Len() != 1 {
		t.Errorf("expected Len()=1 after adding after clear, got %d", cm.Len())
	}
}

func TestHosts(t *testing.T) {
	ca, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}
	cm := NewCertManager(ca, 100)

	// Empty cache.
	hosts := cm.Hosts()
	if len(hosts) != 0 {
		t.Errorf("expected empty hosts list, got %v", hosts)
	}

	// Add hosts out of order.
	_, _ = cm.GetCertificate("z.example.com")
	_, _ = cm.GetCertificate("a.example.com")
	_, _ = cm.GetCertificate("m.example.com")

	hosts = cm.Hosts()
	expected := []string{"a.example.com", "m.example.com", "z.example.com"}
	if len(hosts) != len(expected) {
		t.Errorf("expected %v, got %v", expected, hosts)
	}
	for i, h := range hosts {
		if h != expected[i] {
			t.Errorf("hosts[%d]=%s, expected %s", i, h, expected[i])
		}
	}

	if !sort.StringsAreSorted(hosts) {
		t.Error("hosts list is not sorted")
	}
}
