package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/siwc"
)

type authTransport func(*http.Request) (*http.Response, error)

func (f authTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type memoryAuthListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *memoryAuthListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *memoryAuthListener) Close() error { l.once.Do(func() { close(l.closed) }); return nil }
func (l *memoryAuthListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 32123}
}
func (l *memoryAuthListener) dial(context.Context, string, string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	}
}

func TestGatewayAuthLoginRoundTrip(t *testing.T) {
	for _, validNonce := range []bool{true, false} {
		t.Run(fmt.Sprint("validNonce=", validNonce), func(t *testing.T) {
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			const issuer = "http://127.0.0.1:12345"
			const access, refresh, code = "private-access-value", "private-refresh-value", "private-code-value"
			var nonce, challenge, redirect, idToken string
			encode := base64.RawURLEncoding.EncodeToString
			client := siwc.Client{Issuer: issuer, HTTPClient: &http.Client{Transport: authTransport(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					json.NewEncoder(w).Encode(siwc.Discovery{Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token", JWKSURI: issuer + "/jwks"})
				case "/jwks":
					json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "test", "alg": "EdDSA", "x": encode(pub)}}})
				case "/token":
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
					if encode(sum[:]) != challenge || r.Form.Get("code") != code || r.Form.Get("client_id") != "issued-client" || r.Form.Get("redirect_uri") != redirect {
						t.Error("invalid exchange parameters")
					}
					if !validNonce {
						nonce = "incorrect"
					}
					header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "test"})
					claims, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "test-subject", "aud": "issued-client", "nonce": nonce, "exp": time.Now().Add(time.Hour).Unix()})
					message := encode(header) + "." + encode(claims)
					idToken = message + "." + encode(ed25519.Sign(priv, []byte(message)))
					json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": refresh, "id_token": idToken, "token_type": "Bearer", "scope": siwc.DirectScope, "expires_in": 3600})
				default:
					t.Errorf("unexpected request path: %s", r.URL.Path)
				}
				return w.Result(), nil
			})}}
			listener := &memoryAuthListener{conns: make(chan net.Conn), closed: make(chan struct{})}
			store := siwc.Store{Secrets: secret.New(t.TempDir())}
			refs := gatewaySIWCStatusEntry{PlanID: "account", SessionRef: "session", HostIDRef: "host"}
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = gatewayAuthLogin(ctx, client, store, refs, &output, true, func(ctx context.Context, raw string) error {
				u, err := url.Parse(raw)
				if err != nil {
					return err
				}
				q := u.Query()
				nonce, challenge, redirect = q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")
				if q.Get("client_id") != siwc.DynamicClientID || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || nonce == "" || q.Get("ext_agent_host_id") == "" {
					t.Error("invalid authorization parameters")
				}
				callback, _ := url.Parse(redirect)
				callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {code}, "client_id": {"issued-client"}}.Encode()
				hc := &http.Client{Transport: &http.Transport{DialContext: listener.dial}}
				bad := *callback
				bad.RawQuery = url.Values{"state": {"incorrect"}, "code": {code}, "client_id": {"issued-client"}}.Encode()
				badResp, err := hc.Get(bad.String())
				if err != nil {
					return err
				}
				io.Copy(io.Discard, badResp.Body)
				badResp.Body.Close()
				if badResp.StatusCode != http.StatusBadRequest {
					t.Error("accepted invalid callback state")
				}
				resp, err := hc.Get(callback.String())
				if err != nil {
					return err
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Errorf("callback status %d", resp.StatusCode)
				}
				return nil
			}, func(string, string) (net.Listener, error) { return listener, nil })
			if validNonce && err != nil {
				t.Fatalf("login failed: %v", err)
			}
			if !validNonce && err == nil {
				t.Fatal("accepted incorrect nonce")
			}
			if err != nil && strings.Contains(err.Error(), code) {
				t.Fatal("error leaked code")
			}
			for _, sensitive := range []string{access, refresh, code, idToken} {
				if sensitive != "" && strings.Contains(output.String(), sensitive) {
					t.Error("output leaked credential")
				}
			}
			got, loadErr := store.Load("session")
			if validNonce {
				if loadErr != nil || got.AccessToken != access || got.RefreshToken != refresh || got.IDToken != idToken {
					t.Fatal("session not saved")
				}
				fi, err := os.Stat(filepath.Join(store.Secrets.Dir, "session"))
				if err != nil || fi.Mode().Perm() != 0600 {
					t.Fatal("session permissions incorrect")
				}
			} else if !os.IsNotExist(loadErr) {
				t.Fatal("invalid token was stored")
			}
		})
	}
}

func TestGatewayAuthLogoutPreservesHost(t *testing.T) {
	store := siwc.Store{Secrets: secret.New(t.TempDir())}
	refs := gatewaySIWCStatusEntry{SessionRef: "session", HostIDRef: "host"}
	host, err := store.HostID("host")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("session", siwc.Tokens{AccessToken: "private"}); err != nil {
		t.Fatal(err)
	}
	if err := gatewayAuthLogout(store, refs); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("session"); !os.IsNotExist(err) {
		t.Fatal("session still exists")
	}
	if next, err := store.HostID("host"); err != nil || next != host {
		t.Fatal("host ID changed")
	}
	if err := gatewayAuthLogout(store, gatewaySIWCStatusEntry{SessionRef: "host", HostIDRef: "host"}); err == nil {
		t.Fatal("accepted identical refs")
	}
}

func TestGatewayAuthUsage(t *testing.T) {
	for _, args := range [][]string{{"auth"}, {"auth", "status", "extra"}, {"auth", "login"}, {"auth", "login", "one", "two"}, {"auth", "logout"}, {"auth", "logout", "one", "two"}, {"siwc-status"}, {"auth", "unknown"}} {
		if err := cmdGateway(&clientConfig{}, args); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	for _, args := range [][]string{{"auth", "--help"}, {"auth", "login", "--help"}, {"auth", "status", "--help"}, {"auth", "logout", "--help"}} {
		if err := cmdGateway(&clientConfig{}, args); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGatewayAuthCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous := gatewayAuthRequest
	t.Cleanup(func() { gatewayAuthRequest = previous })
	gatewayAuthRequest = func(_ *clientConfig, method, path string, _ io.Reader) (*http.Response, error) {
		if method != http.MethodGet || path != "/v1/gateway/plans" {
			t.Error("unexpected plan request")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"plans":[{"id":"account","healthy":true,"siwc":{"sessionRef":"session","hostIdRef":"host","accessToken":"private-token"}},{"id":"api","siwc":null}]}`))}, nil
	}
	for _, pretty := range [][]string{{"auth", "status"}, {"auth", "status", "--pretty"}} {
		if err := cmdGateway(&clientConfig{}, pretty); err != nil {
			t.Fatal(err)
		}
	}
	store := gatewayAuthStore()
	if _, err := store.HostID("host"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("session", siwc.Tokens{AccessToken: "private"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdGateway(&clientConfig{}, []string{"auth", "logout", "account"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("session"); !os.IsNotExist(err) {
		t.Fatal("session still exists")
	}
	if !store.Secrets.Has("host") {
		t.Fatal("host ID removed")
	}
	for _, id := range []string{"api", "unknown"} {
		if err := cmdGateway(&clientConfig{}, []string{"auth", "logout", id}); err == nil {
			t.Errorf("accepted plan %s", id)
		}
	}
}

func TestGatewayAuthLoginNoBrowser(t *testing.T) {
	const issuer = "http://127.0.0.1:12345"
	client := siwc.Client{Issuer: issuer, HTTPClient: &http.Client{Transport: authTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		json.NewEncoder(w).Encode(siwc.Discovery{Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token", JWKSURI: issuer + "/jwks"})
		return w.Result(), nil
	})}}
	listener := &memoryAuthListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	err := gatewayAuthLogin(ctx, client, siwc.Store{Secrets: secret.New(t.TempDir())}, gatewaySIWCStatusEntry{SessionRef: "session", HostIDRef: "host"}, &out, false, func(context.Context, string) error {
		t.Error("browser launched with --no-browser")
		return nil
	}, func(string, string) (net.Listener, error) { return listener, nil })
	if err == nil || !strings.Contains(out.String(), issuer+"/authorize?") {
		t.Fatal("expected URL and canceled login")
	}
}
