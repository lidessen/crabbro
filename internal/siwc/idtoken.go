package siwc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const tokenClockSkew = 30 * time.Second

// Identity contains claims authenticated by the issuer's signing key.
type Identity struct {
	Subject   string
	AccountID string
	Email     string
}

// Session contains a verified identity and a currently usable bearer token.
// Callers must still bind Identity to the account they intend to use.
type Session struct {
	Identity    Identity
	AccessToken string
}

func (Session) String() string   { return "[SIWC session redacted]" }
func (Session) GoString() string { return "[SIWC session redacted]" }

// VerifiedSession validates stored credentials before returning a bearer token.
// It fails closed if the stored ID token has expired, even after a refresh that
// did not return a replacement ID token.
func (c Client) VerifiedSession(ctx context.Context, d Discovery, tokens Tokens) (Session, error) {
	if tokens.AccessToken == "" || !strings.EqualFold(tokens.TokenType, "Bearer") || !tokens.ExpiresAt.After(time.Now()) || !tokens.GrantedDirect() {
		return Session{}, errors.New("SIWC session is not usable")
	}
	identity, err := c.VerifyIDToken(ctx, d, tokens.IDToken, tokens.ClientID, "")
	if err != nil {
		return Session{}, err
	}
	return Session{Identity: identity, AccessToken: tokens.AccessToken}, nil
}

type jwk struct {
	Kty    string   `json:"kty"`
	Kid    string   `json:"kid"`
	Alg    string   `json:"alg"`
	Use    string   `json:"use"`
	KeyOps []string `json:"key_ops"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	Crv    string   `json:"crv"`
	X      string   `json:"x"`
	Y      string   `json:"y"`
}
type jwks struct {
	Keys []jwk `json:"keys"`
}
type cacheKey struct {
	uri    string
	client *http.Client
}
type cacheEntry struct {
	keys  jwks
	until time.Time
}

var keyCache = struct {
	sync.Mutex
	entries map[cacheKey]cacheEntry
}{entries: make(map[cacheKey]cacheEntry)}

func (c Client) signingKeys(ctx context.Context, uri string, refresh bool) (jwks, bool, error) {
	key := cacheKey{uri: uri, client: c.HTTPClient}
	keyCache.Lock()
	entry, ok := keyCache.entries[key]
	if !refresh && ok && time.Now().Before(entry.until) {
		keyCache.Unlock()
		return entry.keys, true, nil
	}
	keyCache.Unlock()
	var keys jwks
	if err := c.request(ctx, http.MethodGet, uri, nil, "", &keys); err != nil {
		return jwks{}, false, err
	}
	if len(keys.Keys) == 0 || len(keys.Keys) > 128 {
		return jwks{}, false, errors.New("invalid SIWC signing keys")
	}
	keyCache.Lock()
	if len(keyCache.entries) >= 64 {
		clear(keyCache.entries)
	}
	keyCache.entries[key] = cacheEntry{keys: keys, until: time.Now().Add(5 * time.Minute)}
	keyCache.Unlock()
	return keys, false, nil
}

func decodePart(s string) ([]byte, error) { return base64.RawURLEncoding.Strict().DecodeString(s) }

func (k jwk) publicKey(alg string) (any, error) {
	bad := errors.New("invalid SIWC signing key")
	if k.Alg != "" && k.Alg != alg || k.Use != "" && k.Use != "sig" {
		return nil, bad
	}
	if len(k.KeyOps) > 0 {
		found := false
		for _, op := range k.KeyOps {
			if op == "verify" {
				found = true
			}
		}
		if !found {
			return nil, bad
		}
	}
	switch alg {
	case "RS256":
		if k.Kty != "RSA" {
			return nil, bad
		}
		n, err := decodePart(k.N)
		if err != nil {
			return nil, bad
		}
		e, err := decodePart(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, bad
		}
		modulus := new(big.Int).SetBytes(n)
		exponent := new(big.Int).SetBytes(e).Int64()
		if modulus.BitLen() < 2048 || exponent < 3 || exponent > 1<<31-1 || exponent%2 == 0 {
			return nil, bad
		}
		return &rsa.PublicKey{N: modulus, E: int(exponent)}, nil
	case "ES256":
		if k.Kty != "EC" || k.Crv != "P-256" {
			return nil, bad
		}
		x, err := decodePart(k.X)
		if err != nil || len(x) != 32 {
			return nil, bad
		}
		y, err := decodePart(k.Y)
		if err != nil || len(y) != 32 {
			return nil, bad
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			return nil, bad
		}
		return pub, nil
	case "EdDSA":
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			return nil, bad
		}
		x, err := decodePart(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, bad
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, bad
}

func findKey(keys jwks, kid, alg string) (any, error) {
	var match *jwk
	for i := range keys.Keys {
		if keys.Keys[i].Kid == kid {
			if match != nil {
				return nil, errors.New("ambiguous SIWC signing key")
			}
			match = &keys.Keys[i]
		}
	}
	if match == nil {
		return nil, errors.New("SIWC signing key not found")
	}
	return match.publicKey(alg)
}

// VerifyIDToken checks the signature and OIDC claims. A nonempty expectedNonce
// requires an exact nonce match; pass an empty nonce for refresh or stored tokens.
func (c Client) VerifyIDToken(ctx context.Context, d Discovery, raw, clientID, expectedNonce string) (Identity, error) {
	bad := errors.New("invalid SIWC ID token")
	if err := c.validateDiscovery(d); err != nil {
		return Identity{}, err
	}
	if clientID == "" || clientID == DynamicClientID || len(raw) > 1<<20 {
		return Identity{}, bad
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Identity{}, bad
	}
	headerData, err := decodePart(parts[0])
	if err != nil {
		return Identity{}, bad
	}
	var header struct {
		Alg  string          `json:"alg"`
		Kid  string          `json:"kid"`
		Crit json.RawMessage `json:"crit"`
		B64  *bool           `json:"b64"`
	}
	if json.Unmarshal(headerData, &header) != nil || header.Kid == "" || (header.Alg != "RS256" && header.Alg != "ES256" && header.Alg != "EdDSA") || len(header.Crit) != 0 || header.B64 != nil {
		return Identity{}, bad
	}
	payload, err := decodePart(parts[1])
	if err != nil {
		return Identity{}, bad
	}
	sig, err := decodePart(parts[2])
	if err != nil {
		return Identity{}, bad
	}
	keys, cached, err := c.signingKeys(ctx, d.JWKSURI, false)
	if err != nil {
		return Identity{}, err
	}
	key, err := findKey(keys, header.Kid, header.Alg)
	if err != nil && cached {
		keys, _, err = c.signingKeys(ctx, d.JWKSURI, true)
		if err != nil {
			return Identity{}, err
		}
		key, err = findKey(keys, header.Kid, header.Alg)
	}
	if err != nil {
		return Identity{}, bad
	}
	message := []byte(parts[0] + "." + parts[1])
	digest := sha256.Sum256(message)
	valid := false
	switch k := key.(type) {
	case *rsa.PublicKey:
		valid = rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig) == nil
	case *ecdsa.PublicKey:
		if len(sig) == 64 {
			valid = ecdsa.Verify(k, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
		}
	case ed25519.PublicKey:
		valid = ed25519.Verify(k, message, sig)
	}
	if !valid {
		return Identity{}, bad
	}
	var claims struct {
		Issuer          string          `json:"iss"`
		Subject         string          `json:"sub"`
		Audience        json.RawMessage `json:"aud"`
		AuthorizedParty string          `json:"azp"`
		Expiry          *int64          `json:"exp"`
		NotBefore       *int64          `json:"nbf"`
		Nonce           string          `json:"nonce"`
		AccountID       string          `json:"account_id"`
		Email           string          `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Issuer != d.Issuer || strings.TrimSpace(claims.Subject) == "" || claims.Expiry == nil {
		return Identity{}, bad
	}
	var audience []string
	var single string
	if json.Unmarshal(claims.Audience, &single) == nil {
		audience = []string{single}
	} else if json.Unmarshal(claims.Audience, &audience) != nil {
		return Identity{}, bad
	}
	found := false
	for _, a := range audience {
		if a == clientID {
			found = true
		}
	}
	if !found || (len(audience) > 1 && claims.AuthorizedParty != clientID) || (claims.AuthorizedParty != "" && claims.AuthorizedParty != clientID) {
		return Identity{}, bad
	}
	now := time.Now()
	if !now.Before(time.Unix(*claims.Expiry, 0).Add(tokenClockSkew)) || (claims.NotBefore != nil && now.Add(tokenClockSkew).Before(time.Unix(*claims.NotBefore, 0))) {
		return Identity{}, bad
	}
	if expectedNonce != "" && subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(expectedNonce)) != 1 {
		return Identity{}, bad
	}
	return Identity{Subject: claims.Subject, AccountID: claims.AccountID, Email: claims.Email}, nil
}
