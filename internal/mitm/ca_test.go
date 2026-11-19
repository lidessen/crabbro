package mitm

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateCA(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	if store.Cert == nil {
		t.Fatal("GenerateCA() returned nil Cert")
	}
	if store.Key == nil {
		t.Fatal("GenerateCA() returned nil Key")
	}
	if len(store.CertDER) == 0 {
		t.Fatal("GenerateCA() returned empty CertDER")
	}
	if len(store.KeyDER) == 0 {
		t.Fatal("GenerateCA() returned empty KeyDER")
	}
}

func TestGeneratedCAIsSelfSigned(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	// Verify the cert is self-signed: check signature against its own public key.
	if err := store.Cert.CheckSignatureFrom(store.Cert); err != nil {
		t.Fatalf("CA cert is not self-signed: %v", err)
	}
}

func TestGeneratedCAIsCA(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	if !store.Cert.IsCA {
		t.Fatal("GenerateCA() produced a cert where IsCA is false")
	}
}

func TestGeneratedCAKeyUsage(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	expected := x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature
	if store.Cert.KeyUsage != expected {
		t.Fatalf("KeyUsage = %d, want %d (CertSign|DigitalSignature)", store.Cert.KeyUsage, expected)
	}
}

func TestGeneratedCABasicConstraints(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	if !store.Cert.BasicConstraintsValid {
		t.Fatal("BasicConstraintsValid is false")
	}
}

func TestGeneratedCASubjectCN(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	if store.Cert.Subject.CommonName != "crabbro root CA" {
		t.Fatalf("Subject CN = %q, want %q", store.Cert.Subject.CommonName, "crabbro root CA")
	}
}

func TestGeneratedCAValidity(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	expectedValidity := 10 * 365 * 24 // hours (10 years)
	validity := store.Cert.NotAfter.Sub(store.Cert.NotBefore)
	validityHours := int(validity.Hours())

	// Allow some tolerance for the clock skew adjustment.
	if validityHours < expectedValidity-48 || validityHours > expectedValidity+48 {
		t.Fatalf("CA validity = %d hours, want ~%d hours", validityHours, expectedValidity)
	}
}

func TestCARSAKeyBits(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	bits := store.Key.N.BitLen()
	if bits != rsaKeyBits {
		t.Fatalf("CA key size = %d bits, want %d", bits, rsaKeyBits)
	}
}

func TestCertPEMFormat(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	pemBytes := store.CertPEM()
	if len(pemBytes) == 0 {
		t.Fatal("CertPEM() returned empty bytes")
	}

	// ParsePEM should be able to re-read the PEM output.
	reloaded, err := ParsePEM(pemBytes, store.KeyPEM())
	if err != nil {
		t.Fatalf("ParsePEM(CertPEM) failed: %v", err)
	}

	if !reloaded.Cert.Equal(store.Cert) {
		t.Fatal("ParsePEM(CertPEM()) produced a different certificate")
	}
}

func TestKeyPEMFormat(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	pemBytes := store.KeyPEM()
	if len(pemBytes) == 0 {
		t.Fatal("KeyPEM() returned empty bytes")
	}

	reloaded, err := ParsePEM(store.CertPEM(), pemBytes)
	if err != nil {
		t.Fatalf("ParsePEM(KeyPEM) failed: %v", err)
	}

	if reloaded.Key.D.Cmp(store.Key.D) != 0 {
		t.Fatal("ParsePEM(KeyPEM()) produced a different private key")
	}
}

func TestPEMRoundTrip(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	// Round-trip through PEM bytes.
	reloaded, err := ParsePEM(store.CertPEM(), store.KeyPEM())
	if err != nil {
		t.Fatalf("ParsePEM round-trip failed: %v", err)
	}

	// Verify the reloaded cert chain works.
	if err := reloaded.Cert.CheckSignatureFrom(reloaded.Cert); err != nil {
		t.Fatal("Reloaded CA cert is not self-signed")
	}
}

func TestSaveAndLoadPEM(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "rootCA.pem")
	keyPath := filepath.Join(dir, "rootCA-key.pem")

	if err := SavePEM(store, certPath, keyPath); err != nil {
		t.Fatalf("SavePEM() failed: %v", err)
	}

	// Verify the files exist.
	if _, err := os.Stat(certPath); os.IsNotExist(err) {
		t.Fatal("SavePEM() did not create cert file")
	}
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		t.Fatal("SavePEM() did not create key file")
	}

	// LoadPEM should recover the same CA.
	loaded, err := LoadPEM(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadPEM() failed: %v", err)
	}

	if !loaded.Cert.Equal(store.Cert) {
		t.Fatal("LoadPEM() loaded a different certificate")
	}

	// Signature should still verify.
	if err := loaded.Cert.CheckSignatureFrom(loaded.Cert); err != nil {
		t.Fatal("Loaded CA cert is not self-signed")
	}
}

func TestSavePEMKeyPermissions(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "rootCA.pem")
	keyPath := filepath.Join(dir, "rootCA-key.pem")

	if err := SavePEM(store, certPath, keyPath); err != nil {
		t.Fatalf("SavePEM() failed: %v", err)
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}

	// Key file should be owner-readable only (0600).
	if info.Mode().Perm() != 0600 {
		t.Fatalf("key file permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestSavePEMCertPermissions(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "rootCA.pem")
	keyPath := filepath.Join(dir, "rootCA-key.pem")

	if err := SavePEM(store, certPath, keyPath); err != nil {
		t.Fatalf("SavePEM() failed: %v", err)
	}

	info, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat cert file: %v", err)
	}

	// Cert file should be world-readable (0644).
	if info.Mode().Perm() != 0644 {
		t.Fatalf("cert file permissions = %o, want 0644", info.Mode().Perm())
	}
}

func TestParsePEMRejectsInvalidCertPEM(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	// Corrupt cert PEM.
	_, err = ParsePEM([]byte("not-valid-pem"), store.KeyPEM())
	if err == nil {
		t.Fatal("ParsePEM with invalid cert PEM should have failed")
	}
}

func TestParsePEMRejectsInvalidKeyPEM(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	// Corrupt key PEM.
	_, err = ParsePEM(store.CertPEM(), []byte("not-valid-pem"))
	if err == nil {
		t.Fatal("ParsePEM with invalid key PEM should have failed")
	}
}

func TestParsePEMRejectsCertWithTrailingData(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	// Append garbage to the cert PEM.
	certPEM := append(store.CertPEM(), []byte("\nextra garbage")...)
	_, err = ParsePEM(certPEM, store.KeyPEM())
	if err == nil {
		t.Fatal("ParsePEM with trailing cert data should have failed")
	}
}

func TestParsePEMRejectsKeyWithTrailingData(t *testing.T) {
	store, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() failed: %v", err)
	}

	keyPEM := append(store.KeyPEM(), []byte("\nextra garbage")...)
	_, err = ParsePEM(store.CertPEM(), keyPEM)
	if err == nil {
		t.Fatal("ParsePEM with trailing key data should have failed")
	}
}

func TestLoadPEMFileNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadPEM(filepath.Join(dir, "nonexistent.pem"), filepath.Join(dir, "nonexistent-key.pem"))
	if err == nil {
		t.Fatal("LoadPEM with nonexistent files should have failed")
	}
}

func TestExpandHome(t *testing.T) {
	expanded := expandHome("~/test/path")
	if expanded == "~/test/path" {
		t.Fatal("expandHome() did not expand ~/")
	}
	if len(expanded) == 0 {
		t.Fatal("expandHome() returned empty string")
	}
}

func TestExpandHomeNoTilde(t *testing.T) {
	result := expandHome("/absolute/path")
	if result != "/absolute/path" {
		t.Fatalf("expandHome(%q) = %q, want unchanged", "/absolute/path", result)
	}
}

func TestExpandHomeRelative(t *testing.T) {
	result := expandHome("relative/path")
	if result != "relative/path" {
		t.Fatalf("expandHome(%q) = %q, want unchanged", "relative/path", result)
	}
}

func TestLoadPEMNonexistentDir(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadPEM(filepath.Join(dir, "nonexistent", "rootCA.pem"), filepath.Join(dir, "nonexistent", "rootCA-key.pem"))
	if err == nil {
		t.Fatal("LoadPEM with nonexistent directory should have failed")
	}
}

func TestMultipleGenerateCAProduceDifferentCerts(t *testing.T) {
	store1, err := GenerateCA()
	if err != nil {
		t.Fatalf("first GenerateCA() failed: %v", err)
	}

	store2, err := GenerateCA()
	if err != nil {
		t.Fatalf("second GenerateCA() failed: %v", err)
	}

	if store1.Cert.SerialNumber.Cmp(store2.Cert.SerialNumber) == 0 {
		t.Fatal("Two GenerateCA() calls produced the same serial number")
	}
}
