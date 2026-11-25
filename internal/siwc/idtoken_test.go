package siwc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func signedToken(t *testing.T, alg, kid string, private any, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": alg, "kid": kid})
	payload, _ := json.Marshal(claims)
	message := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(message))
	var sig []byte
	var err error
	switch k := private.(type) {
	case *rsa.PrivateKey:
		sig, err = rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
	case *ecdsa.PrivateKey:
		var r, s interface{ FillBytes([]byte) []byte }
		rr, ss, e := ecdsa.Sign(rand.Reader, k, digest[:])
		err = e
		if err == nil {
			r, s = rr, ss
			sig = make([]byte, 64)
			r.FillBytes(sig[:32])
			s.FillBytes(sig[32:])
		}
	case ed25519.PrivateKey:
		sig = ed25519.Sign(k, []byte(message))
	}
	if err != nil {
		t.Fatal(err)
	}
	return message + "." + b64(sig)
}
func makeKey(t *testing.T, alg, kid string) (jwk, any) {
	t.Helper()
	switch alg {
	case "RS256":
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		return jwk{Kty: "RSA", Kid: kid, Alg: alg, N: b64(k.N.Bytes()), E: b64([]byte{1, 0, 1})}, k
	case "ES256":
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return jwk{Kty: "EC", Kid: kid, Alg: alg, Crv: "P-256", X: b64(k.X.FillBytes(make([]byte, 32))), Y: b64(k.Y.FillBytes(make([]byte, 32)))}, k
	case "EdDSA":
		pub, k, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return jwk{Kty: "OKP", Kid: kid, Alg: alg, Crv: "Ed25519", X: b64(pub)}, k
	}
	t.Fatal("unknown algorithm")
	return jwk{}, nil
}
func metadata(base string) Discovery {
	return Discovery{Issuer: base, AuthorizationEndpoint: base + "/authorize", TokenEndpoint: base + "/token", JWKSURI: base + "/jwks"}
}
func validClaims(base string) map[string]any {
	return map[string]any{"iss": base, "aud": "client", "sub": "subject", "exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce", "account_id": "account", "email": "user@example.test"}
}

func TestVerifyIDToken(t *testing.T) {
	for _, alg := range []string{"RS256", "ES256", "EdDSA"} {
		t.Run(alg, func(t *testing.T) {
			key, private := makeKey(t, alg, "key")
			var calls atomic.Int32
			base, hc := testServer(t, func(string) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					json.NewEncoder(w).Encode(jwks{Keys: []jwk{key}})
				})
			})
			c, d := Client{Issuer: base, HTTPClient: hc}, metadata(base)
			claims := validClaims(base)
			token := signedToken(t, alg, "key", private, claims)
			id, err := c.VerifyIDToken(context.Background(), d, token, "client", "nonce")
			if err != nil || id.Subject != "subject" || id.AccountID != "account" || id.Email != "user@example.test" {
				t.Fatalf("valid token rejected: %v, %+v", err, id)
			}
			if _, err := c.VerifyIDToken(context.Background(), d, token, "client", "nonce"); err != nil || calls.Load() != 1 {
				t.Fatal("JWKS cache not reused", err, calls.Load())
			}
			cases := map[string]func(map[string]any){
				"expired":                        func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
				"issuer":                         func(c map[string]any) { c["iss"] = "https://wrong.test" },
				"audience":                       func(c map[string]any) { c["aud"] = "other" },
				"subject":                        func(c map[string]any) { c["sub"] = "" },
				"nonce":                          func(c map[string]any) { c["nonce"] = "other" },
				"missing nonce":                  func(c map[string]any) { delete(c, "nonce") },
				"missing expiry":                 func(c map[string]any) { delete(c, "exp") },
				"future nbf":                     func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() },
				"multiple audiences without azp": func(c map[string]any) { c["aud"] = []string{"client", "other"} },
			}
			for name, change := range cases {
				t.Run(name, func(t *testing.T) {
					v := validClaims(base)
					change(v)
					raw := signedToken(t, alg, "key", private, v)
					if _, err := c.VerifyIDToken(context.Background(), d, raw, "client", "nonce"); err == nil || strings.Contains(err.Error(), raw) {
						t.Fatal("accepted invalid claims or leaked token")
					}
				})
			}
			parts := strings.Split(token, ".")
			sig, _ := decodePart(parts[2])
			sig[0] ^= 1
			parts[2] = b64(sig)
			if _, err := c.VerifyIDToken(context.Background(), d, strings.Join(parts, "."), "client", "nonce"); err == nil {
				t.Fatal("accepted tampered signature")
			}
			if _, err := c.VerifyIDToken(context.Background(), d, signedToken(t, alg, "wrong", private, claims), "client", "nonce"); err == nil || calls.Load() != 2 {
				t.Fatal("bad key ID accepted or rotation not attempted")
			}
		})
	}
}

func TestVerifiedSessionAndExchangeRefresh(t *testing.T) {
	key, private := makeKey(t, "ES256", "key")
	var nonce string
	var grant, returnedID string
	base, hc := testServer(t, func(base string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/jwks":
				json.NewEncoder(w).Encode(jwks{Keys: []jwk{key}})
			case "/token":
				r.ParseForm()
				grant = r.Form.Get("grant_type")
				id := returnedID
				if id == "valid" {
					claims := validClaims(base)
					claims["nonce"] = nonce
					id = signedToken(t, "ES256", "key", private, claims)
				}
				json.NewEncoder(w).Encode(tokenResponse{AccessToken: "bearer-secret", RefreshToken: "refresh", IDToken: id, TokenType: "Bearer", Scope: DirectScope, ExpiresIn: 3600})
			}
		})
	})
	c, d := Client{Issuer: base, HTTPClient: hc}, metadata(base)
	a := Attempt{ClientID: "client", Nonce: "nonce", Verifier: "verifier", RedirectURI: "http://127.0.0.1:1234/callback"}
	cb := Callback{ClientID: "client", Code: "code"}
	nonce = a.Nonce
	returnedID = "valid"
	tokens, err := c.Exchange(context.Background(), d, a, cb)
	if err != nil || grant != "authorization_code" {
		t.Fatal("exchange failed", err)
	}
	session, err := c.VerifiedSession(context.Background(), d, tokens)
	if err != nil || session.AccessToken != "bearer-secret" || session.Identity.Subject != "subject" {
		t.Fatal("verified session failed", err)
	}
	if strings.Contains(fmt.Sprintf("%#v %v", session, session), "secret") {
		t.Fatal("session leaked bearer")
	}
	returnedID = "bad-secret"
	if got, err := c.Exchange(context.Background(), d, a, cb); err == nil || got != (Tokens{}) || strings.Contains(err.Error(), returnedID) {
		t.Fatal("invalid exchange ID token accepted or leaked")
	}
	if got, err := c.Refresh(context.Background(), d, tokens); err == nil || got != (Tokens{}) {
		t.Fatal("invalid refreshed ID token accepted")
	}
	returnedID = "valid"
	if _, err := c.Refresh(context.Background(), d, tokens); err != nil || grant != "refresh_token" {
		t.Fatal("valid refresh failed", err)
	}
	for _, mutate := range []func(*Tokens){func(v *Tokens) { v.AccessToken = "" }, func(v *Tokens) { v.ExpiresAt = time.Now().Add(-time.Second) }, func(v *Tokens) { v.Scope = "openid" }, func(v *Tokens) { v.IDToken = "" }} {
		v := tokens
		mutate(&v)
		if _, err := c.VerifiedSession(context.Background(), d, v); err == nil {
			t.Fatal("accepted unusable session")
		}
	}
}

func TestDiscoveryRevalidatedAndResponseBounded(t *testing.T) {
	var calls atomic.Int32
	base, hc := testServer(t, func(string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, `{"keys":[]}`) })
	})
	c, d := Client{Issuer: base, HTTPClient: hc}, metadata(base)
	for _, change := range []func(*Discovery){func(d *Discovery) { d.Issuer = "https://attacker.test" }, func(d *Discovery) { d.TokenEndpoint = "https://attacker.test/token" }, func(d *Discovery) { d.JWKSURI = "https://attacker.test/jwks" }} {
		bad := d
		change(&bad)
		if _, err := c.token(context.Background(), bad, "client", url.Values{}); err == nil {
			t.Fatal("accepted unpinned token endpoint")
		}
		if _, err := c.VerifyIDToken(context.Background(), bad, "bad", "client", ""); err == nil {
			t.Fatal("accepted unpinned JWKS endpoint")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("sent request to unpinned metadata")
	}
	large := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}` + strings.Repeat(" ", 1<<20))), Header: make(http.Header), Request: r}, nil
	})}
	if err := (Client{HTTPClient: large}).request(context.Background(), http.MethodGet, base, nil, "", &map[string]any{}); err == nil {
		t.Fatal("accepted oversized response")
	}
	redirect := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{"Location": []string{base + "/other"}}, Request: r}, nil
	})}
	if err := (Client{HTTPClient: redirect}).request(context.Background(), http.MethodGet, base, nil, "", &map[string]any{}); err == nil {
		t.Fatal("accepted redirect")
	}
}

func TestJWKSRotationAndConcurrentVerification(t *testing.T) {
	first, firstPrivate := makeKey(t, "ES256", "first")
	second, secondPrivate := makeKey(t, "ES256", "second")
	var current atomic.Pointer[jwk]
	current.Store(&first)
	var calls atomic.Int32
	base, hc := testServer(t, func(string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			json.NewEncoder(w).Encode(jwks{Keys: []jwk{*current.Load()}})
		})
	})
	c, d := Client{Issuer: base, HTTPClient: hc}, metadata(base)
	firstToken := signedToken(t, "ES256", "first", firstPrivate, validClaims(base))
	if _, err := c.VerifyIDToken(context.Background(), d, firstToken, "client", "nonce"); err != nil {
		t.Fatal(err)
	}
	current.Store(&second)
	secondToken := signedToken(t, "ES256", "second", secondPrivate, validClaims(base))
	if _, err := c.VerifyIDToken(context.Background(), d, secondToken, "client", "nonce"); err != nil || calls.Load() != 2 {
		t.Fatal("key rotation failed", err)
	}
	const count = 16
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			_, err := c.VerifyIDToken(context.Background(), d, secondToken, "client", "nonce")
			errs <- err
		}()
	}
	for i := 0; i < count; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}
