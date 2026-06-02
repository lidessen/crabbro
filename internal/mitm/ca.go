package mitm

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Default file and directory names for the root CA.
const (
	// DefaultCertDir is the default directory for CA certificate and key files.
	DefaultCertDir = "~/.chiling/certs"

	// DefaultCertFile is the default filename for the PEM-encoded CA certificate.
	DefaultCertFile = "rootCA.pem"

	// DefaultKeyFile is the default filename for the PEM-encoded CA private key.
	DefaultKeyFile = "rootCA-key.pem"

	// CAValidity is the validity period for the root CA certificate (10 years).
	CAValidity = 10 * 365 * 24 * time.Hour

	// rsaKeyBits is the RSA key size used for CA and leaf certificates.
	rsaKeyBits = 2048

	// clockSkew is the NotBefore adjustment to tolerate clock skew.
	clockSkew = 24 * time.Hour
)

// CAStore holds a loaded or generated root CA certificate and its private key.
//
// CertDER and KeyDER are the DER-encoded forms, preserved so the PEM output
// round-trips exactly (instead of re-marshaling from the parsed structures).
type CAStore struct {
	// Cert is the parsed X.509 certificate.
	Cert *x509.Certificate

	// Key is the RSA private key.
	Key *rsa.PrivateKey

	// CertDER is the raw DER-encoded certificate bytes returned by
	// x509.CreateCertificate.
	CertDER []byte

	// KeyDER is the PKCS1-encoded private key bytes.
	KeyDER []byte
}

// GenerateCA creates a new RSA 2048-bit root CA certificate and private key.
//
// The returned CAStore contains both the parsed certificate and the raw DER
// bytes needed for PEM encoding. The certificate is self-signed, valid for
// 10 years, with CA constraints set as required for an issuing root.
func GenerateCA() (*CAStore, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generate CA serial number: %w", err)
	}

	// SubjectKeyIdentifier is derived from the public key per RFC 5280.
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("marshal CA public key: %w", err)
	}
	ski := sha1.Sum(pubDER)

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "chiling root CA",
		},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(CAValidity),
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		SubjectKeyId:          ski[:],
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, fmt.Errorf("parse generated CA certificate: %w", err)
	}

	return &CAStore{
		Cert:    cert,
		CertDER: certDER,
		Key:     key,
		KeyDER:  x509.MarshalPKCS1PrivateKey(key),
	}, nil
}

// SavePEM writes the CA certificate and private key to PEM files.
//
// The certificate file is written with 0644 permissions; the key file with
// 0600. Parent directories are created with 0700 permissions if needed.
//
// certPath and keyPath are filesystem paths. A leading "~/" is expanded to
// the user's home directory.
func SavePEM(store *CAStore, certPath, keyPath string) error {
	certPath = expandHome(certPath)
	keyPath = expandHome(keyPath)

	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return fmt.Errorf("create cert directory %s: %w", filepath.Dir(certPath), err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
		return fmt.Errorf("create key directory %s: %w", filepath.Dir(keyPath), err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: store.CertDER,
	})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return fmt.Errorf("write CA cert to %s: %w", certPath, err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: store.KeyDER,
	})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		// Attempt to remove the cert file we already wrote to avoid a
		// half-written state.
		os.Remove(certPath)
		return fmt.Errorf("write CA key to %s: %w", keyPath, err)
	}

	return nil
}

// LoadPEM reads a CA certificate and private key from PEM-encoded files.
//
// Returns a CAStore with the parsed certificate and key loaded.
// certPath and keyPath are filesystem paths. A leading "~/" is expanded to
// the user's home directory.
func LoadPEM(certPath, keyPath string) (*CAStore, error) {
	certPath = expandHome(certPath)
	keyPath = expandHome(keyPath)

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("read CA cert from %s: %w", certPath, err)
	}

	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("read CA key from %s: %w", keyPath, err)
	}

	return ParsePEM(certPEM, keyPEM)
}

// ParsePEM parses CA certificate and private key from PEM-encoded byte slices.
//
// certPEM must contain a single "CERTIFICATE" PEM block. keyPEM must contain a
// single "RSA PRIVATE KEY" PEM block. Both blocks are consumed exactly; any
// trailing data after the first block is treated as an error.
func ParsePEM(certPEM, keyPEM []byte) (*CAStore, error) {
	certBlock, rest := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("cert PEM: no valid PEM block found")
	}
	if len(rest) > 0 {
		return nil, fmt.Errorf("cert PEM: unexpected trailing data (%d bytes)", len(rest))
	}
	if certBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("cert PEM: unexpected block type %q (expected CERTIFICATE)", certBlock.Type)
	}

	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}

	if !cert.IsCA {
		return nil, fmt.Errorf("loaded certificate is not a CA (IsCA is false)")
	}

	keyBlock, rest := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("key PEM: no valid PEM block found")
	}
	if len(rest) > 0 {
		return nil, fmt.Errorf("key PEM: unexpected trailing data (%d bytes)", len(rest))
	}
	if keyBlock.Type != "RSA PRIVATE KEY" {
		return nil, fmt.Errorf("key PEM: unexpected block type %q (expected RSA PRIVATE KEY)", keyBlock.Type)
	}

	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA private key: %w", err)
	}

	return &CAStore{
		Cert:    cert,
		CertDER: certBlock.Bytes,
		Key:     key,
		KeyDER:  keyBlock.Bytes,
	}, nil
}

// CertPEM returns the PEM-encoded certificate bytes for a CAStore.
func (s *CAStore) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: s.CertDER,
	})
}

// KeyPEM returns the PEM-encoded private key bytes for a CAStore.
func (s *CAStore) KeyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: s.KeyDER,
	})
}

// expandHome replaces a leading "~/" with the user's home directory.
// Returns the original path unchanged if there is no leading "~/".
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
