// Package mitm provides HTTPS man-in-the-middle certificate management and TLS
// interception for the chiling proxy daemon.
//
// The package supports:
//
//   - Root CA generation, PEM serialization, and file load/save (ca.go)
//   - Per-host certificate generation with in-memory LRU caching (certgen.go, planned)
//   - TLS interception handling (intercept.go, planned)
//   - Domain allowlist/denylist matching (config.go, planned)
//
// # Root CA
//
// The GenerateCA function creates a self-signed RSA 2048-bit root certificate
// authority valid for 10 years. The certificate can be saved and loaded as PEM
// files using SavePEM and LoadPEM.
//
// The root CA is generated once on first MITM-enabled launch and reused on
// subsequent starts. The cert directory (~/.chiling/certs/) is created with
// 0700 permissions on first use.
//
// # Certificate Attributes
//
//   - Key type: RSA 2048-bit
//   - Validity: 10 years (with 24h clock skew tolerance)
//   - BasicConstraints: CA: true
//   - KeyUsage: CertSign, DigitalSignature
//   - Subject CN: "chiling root CA"
//   - IsCA: true
package mitm
