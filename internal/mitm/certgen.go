package mitm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultMaxCerts is the default maximum number of cached leaf certificates.
	DefaultMaxCerts = 1024

	// leafValidity is how long a generated leaf certificate remains valid (30 days).
	leafValidity = 30 * 24 * time.Hour

	// leafClockSkew is the NotBefore adjustment to tolerate clock skew (1 hour).
	leafClockSkew = 1 * time.Hour
)

// CertManager caches and generates per-host TLS certificates signed by a root CA.
//
// All public methods are safe for concurrent use. The cache is bounded by the
// maxCerts limit; when full, the oldest cached entry is evicted (FIFO).
type CertManager struct {
	mu       sync.RWMutex
	cache    map[string]*tls.Certificate // key = lowercased hostname (port stripped)
	ca       *CAStore
	maxCerts int
	order    []string // tracks insertion order for FIFO eviction
}

// NewCertManager creates a new CertManager with the given root CA and maximum
// cache size. If maxCerts <= 0, DefaultMaxCerts (1024) is used.
func NewCertManager(ca *CAStore, maxCerts int) *CertManager {
	if maxCerts <= 0 {
		maxCerts = DefaultMaxCerts
	}
	return &CertManager{
		cache:    make(map[string]*tls.Certificate, maxCerts),
		ca:       ca,
		maxCerts: maxCerts,
		order:    make([]string, 0, maxCerts),
	}
}

// GetCertificate returns a TLS certificate for the given host, generating and
// caching one if necessary.
//
// The host may include a port suffix (e.g. "example.com:443"); it is stripped
// before lookup and certificate generation. The hostname is lowercased so that
// "Example.Com" and "example.com" share the same cached certificate.
//
// The returned certificate is a non-CA leaf certificate signed by the root CA,
// valid for 30 days (with a 1-hour clock-skew grace period). It contains the
// hostname as both the Subject CN and a DNS SAN entry.
func (m *CertManager) GetCertificate(host string) (*tls.Certificate, error) {
	host = stripPort(host)
	host = strings.ToLower(host)

	// Check cache under read lock.
	m.mu.RLock()
	cert, ok := m.cache[host]
	m.mu.RUnlock()
	if ok {
		return cert, nil
	}

	// Generate a new certificate signed by the root CA.
	cert, err := m.generate(host)
	if err != nil {
		return nil, err
	}

	// Store in cache under write lock.
	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check: another goroutine may have inserted the entry while we
	// were generating the certificate.
	if existing, ok := m.cache[host]; ok {
		return existing, nil
	}

	// Evict the oldest entry if at capacity.
	if len(m.cache) >= m.maxCerts {
		m.evictOne()
	}

	m.cache[host] = cert
	m.order = append(m.order, host)
	return cert, nil
}

// generate creates a new leaf certificate for the given hostname, signed by
// the root CA.
func (m *CertManager) generate(host string) (*tls.Certificate, error) {
	// Generate RSA 2048-bit key pair.
	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key for %q: %w", host, err)
	}

	// Generate random 128-bit serial number.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate leaf serial for %q: %w", host, err)
	}

	// SubjectKeyId is SHA-1 of the PKIX public key DER (RFC 5280).
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal leaf public key for %q: %w", host, err)
	}
	ski := sha1.Sum(pubDER)

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: host,
		},
		DNSNames:              []string{host},
		NotBefore:             now.Add(-leafClockSkew),
		NotAfter:              now.Add(leafValidity),
		BasicConstraintsValid: true,
		IsCA:                  false,
		MaxPathLen:            -1,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		SubjectKeyId:          ski[:],
		AuthorityKeyId:        m.ca.Cert.SubjectKeyId,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, m.ca.Cert, &key.PublicKey, m.ca.Key)
	if err != nil {
		return nil, fmt.Errorf("create leaf certificate for %q: %w", host, err)
	}

	return &tls.Certificate{
		Certificate: [][]byte{certDER, m.ca.CertDER},
		PrivateKey:  key,
	}, nil
}

// evictOne removes the oldest cached entry (FIFO eviction).
// Must be called while holding m.mu write lock.
func (m *CertManager) evictOne() {
	if len(m.order) == 0 {
		return
	}
	oldest := m.order[0]
	delete(m.cache, oldest)
	m.order = m.order[1:]
}

// Len returns the number of cached certificates.
func (m *CertManager) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.cache)
}

// Clear removes all cached certificates.
func (m *CertManager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache = make(map[string]*tls.Certificate, m.maxCerts)
	m.order = make([]string, 0, m.maxCerts)
}

// Hosts returns a sorted list of cached hostnames.
func (m *CertManager) Hosts() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	hosts := make([]string, 0, len(m.cache))
	for host := range m.cache {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}
