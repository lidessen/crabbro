package siwc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/lidessen/crabbro/internal/secret"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestScrubValues(t *testing.T) {
	if got := ScrubValues(nil); len(got) != 0 {
		t.Fatalf("nil tokens returned %d values", len(got))
	}
	for _, tc := range []struct {
		tokens Tokens
		want   []string
	}{
		{Tokens{}, nil},
		{Tokens{AccessToken: "access", RefreshToken: "refresh", IDToken: "id"}, []string{"access", "refresh", "id"}},
		{Tokens{RefreshToken: "refresh"}, []string{"refresh"}},
	} {
		if got := ScrubValues(&tc.tokens); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("ScrubValues = %v, want %v", got, tc.want)
		}
	}
}

func TestDiscoveryPinsEndpoints(t *testing.T) {
	for _, field := range []string{"authorization", "token", "jwks"} {
		for _, endpoint := range []string{"https://attacker.example/path", "http://127.0.0.1:0/path"} {
			t.Run(field+"/"+endpoint, func(t *testing.T) {
				base, hc := testServer(t, func(base string) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						d := Discovery{Issuer: base, AuthorizationEndpoint: base + "/authorize", TokenEndpoint: base + "/token", JWKSURI: base + "/jwks"}
						switch field {
						case "authorization":
							d.AuthorizationEndpoint = endpoint
						case "token":
							d.TokenEndpoint = endpoint
						case "jwks":
							d.JWKSURI = endpoint
						}
						json.NewEncoder(w).Encode(d)
					})
				})
				if _, err := (Client{Issuer: base, HTTPClient: hc}).Discover(context.Background()); err == nil {
					t.Fatal("accepted endpoint outside issuer host")
				}
			})
		}
	}
}

func TestRefreshRejectsScopeDowngrade(t *testing.T) {
	base, hc := testServer(t, func(base string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"access_token":"new-access","token_type":"Bearer","scope":"openid","expires_in":3600}`)
		})
	})
	old := Tokens{ClientID: "client", RefreshToken: "refresh", Scope: DirectScope}
	next, err := (Client{Issuer: base, HTTPClient: hc}).Refresh(context.Background(), Discovery{Issuer: base, AuthorizationEndpoint: base + "/authorize", TokenEndpoint: base + "/token", JWKSURI: base + "/jwks"}, old)
	if err == nil || !strings.Contains(err.Error(), "scope") || next != (Tokens{}) {
		t.Fatal("refresh accepted downgraded scope")
	}
}

// Prefer a real server, but fall back to an in-process transport in sandboxes
// that disallow binding loopback sockets.
func testServer(t *testing.T, handler func(string) http.Handler) (string, *http.Client) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		base := "http://" + ln.Addr().String()
		server := httptest.NewUnstartedServer(handler(base))
		server.Listener = ln
		server.Start()
		t.Cleanup(server.Close)
		return base, server.Client()
	}
	base := "http://127.0.0.1:12345"
	h := handler(base)
	return base, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Result(), nil
	})}
}

func TestAuthorizationExchangeRefresh(t *testing.T) {
	var mu sync.Mutex
	var forms []url.Values
	base, hc := testServer(t, func(base string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/.well-known/openid-configuration":
				json.NewEncoder(w).Encode(Discovery{Issuer: base, AuthorizationEndpoint: base + "/authorize", TokenEndpoint: base + "/oauth/token", JWKSURI: base + "/jwks"})
			case "/oauth/token":
				if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
					t.Error("invalid token request")
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				mu.Lock()
				forms = append(forms, r.PostForm)
				mu.Unlock()
				if r.Form.Get("grant_type") == "refresh_token" {
					io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`)
					return
				}
				io.WriteString(w, `{"access_token":"access-secret","refresh_token":"refresh-secret","token_type":"Bearer","scope":"openid chatgpt.tokens.use.direct offline_access","expires_in":3600}`)
			default:
				http.NotFound(w, r)
			}
		})
	})
	c := Client{Issuer: base, HTTPClient: hc}
	d, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auth, attempt, err := d.AuthorizationURL("http://127.0.0.1:34567/auth/callback", "urn:uuid:host", "Crabbro", "")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(auth)
	q := u.Query()
	for key, want := range map[string]string{"client_id": DynamicClientID, "agent_name_hint": "Crabbro", "ext_agent_host_id": "urn:uuid:host", "resource": Resource, "scope": requestedScopes, "code_challenge_method": "S256", "code_challenge": challenge(attempt.Verifier), "state": attempt.State, "nonce": attempt.Nonce} {
		if q.Get(key) != want {
			t.Errorf("%s: got %q, want %q", key, q.Get(key), want)
		}
	}
	req := httptest.NewRequest("GET", attempt.RedirectURI+"?"+url.Values{"code": {"code-secret"}, "state": {attempt.State}, "client_id": {"issued-client"}}.Encode(), nil)
	cb, err := attempt.ValidateCallback(req)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Exchange(context.Background(), d, attempt, cb)
	if err != nil {
		t.Fatal(err)
	}
	if !tok.GrantedDirect() || tok.ClientID != "issued-client" || tok.AccessToken != "access-secret" {
		t.Fatal("invalid credentials")
	}
	next, err := c.Refresh(context.Background(), d, tok)
	if err != nil {
		t.Fatal(err)
	}
	if next.RefreshToken != "new-refresh" || next.AccessToken != "new-access" || next.Scope != tok.Scope || next.IDToken != tok.IDToken {
		t.Fatal("refresh did not preserve and rotate expected fields")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(forms) != 2 {
		t.Fatalf("got %d token requests", len(forms))
	}
	for key, want := range map[string]string{"grant_type": "authorization_code", "code": "code-secret", "code_verifier": attempt.Verifier, "client_id": "issued-client", "redirect_uri": attempt.RedirectURI, "resource": Resource} {
		if forms[0].Get(key) != want {
			t.Errorf("exchange %s mismatch", key)
		}
	}
	for key, want := range map[string]string{"grant_type": "refresh_token", "refresh_token": "refresh-secret", "client_id": "issued-client", "resource": Resource, "scope": ""} {
		if forms[1].Get(key) != want {
			t.Errorf("refresh %s mismatch", key)
		}
	}
}

func TestCallbackValidation(t *testing.T) {
	d := Discovery{Issuer: DefaultIssuer, AuthorizationEndpoint: DefaultIssuer + "/authorize", TokenEndpoint: DefaultIssuer + "/token", JWKSURI: DefaultIssuer + "/jwks"}
	_, a, err := d.AuthorizationURL("http://127.0.0.1:34567/auth/callback", "host", "Crabbro", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]string{
		"wrong state": "state=wrong&code=abc&client_id=issued", "denied": "state=" + a.State + "&error=access_denied", "missing client": "state=" + a.State + "&code=abc", "dynamic client": "state=" + a.State + "&code=abc&client_id=" + DynamicClientID, "duplicate": "state=" + a.State + "&state=" + a.State + "&code=abc&client_id=issued",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := a.ValidateCallback(httptest.NewRequest("GET", a.RedirectURI+"?"+query, nil))
			if err == nil {
				t.Fatal("accepted invalid callback")
			}
		})
	}
	_, returning, err := d.AuthorizationURL(a.RedirectURI, "host", "Crabbro", "issued")
	if err != nil {
		t.Fatal(err)
	}
	cb, err := returning.ValidateCallback(httptest.NewRequest("GET", a.RedirectURI+"?state="+returning.State+"&code=abc", nil))
	if err != nil || cb.ClientID != "issued" {
		t.Fatal("returning client rejected")
	}
	_, err = returning.ValidateCallback(httptest.NewRequest("GET", a.RedirectURI+"?state="+returning.State+"&code=abc&client_id=other", nil))
	if err == nil {
		t.Fatal("accepted changed client")
	}
	_, _, err = d.AuthorizationURL("http://localhost:1234/callback", "host", "Crabbro", "")
	if err == nil {
		t.Fatal("accepted non-IP callback")
	}
}

func TestStoreAndHostID(t *testing.T) {
	dir := t.TempDir()
	s := Store{Secrets: secret.New(dir)}
	const count = 12
	ids := make([]string, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) { defer wg.Done(); ids[i], errs[i] = s.HostID("host-id") }(i)
	}
	wg.Wait()
	for i := range ids {
		if errs[i] != nil || ids[i] != ids[0] || !strings.HasPrefix(ids[i], "urn:uuid:") {
			t.Fatal("host ID not stable", errs[i])
		}
	}
	tok := Tokens{ClientID: "client", AccessToken: "access-secret", RefreshToken: "refresh-secret", IDToken: "id-secret"}
	if err := s.Save("session", tok); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("session")
	if err != nil || got != tok {
		t.Fatal("session round trip failed", err)
	}
	for _, ref := range []string{"host-id", "session"} {
		fi, err := os.Stat(filepath.Join(dir, ref))
		if err != nil || fi.Mode().Perm() != 0600 {
			t.Fatalf("invalid file mode for %s", ref)
		}
	}
	for _, v := range []any{tok, &tok, fmt.Sprintf("%#v", tok)} {
		str := fmt.Sprintf("%v", v)
		if strings.Contains(str, "secret") {
			t.Fatal("formatted credentials leaked")
		}
	}
	if err := s.Delete("session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("session"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expected missing session")
	}
}

func TestErrorsDoNotLeak(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New("access-secret refresh-secret") })}
	c := Client{HTTPClient: hc}
	d := Discovery{Issuer: DefaultIssuer, AuthorizationEndpoint: DefaultIssuer + "/authorize", TokenEndpoint: DefaultIssuer + "/token", JWKSURI: DefaultIssuer + "/jwks"}
	_, err := c.Refresh(context.Background(), d, Tokens{ClientID: "client", RefreshToken: "refresh-secret"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("request error leaked credentials")
	}
	hc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("refresh-secret access-secret")), Request: r}, nil
	})
	_, err = c.Refresh(context.Background(), d, Tokens{ClientID: "client", RefreshToken: "refresh-secret"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("response error leaked credentials")
	}
}

func TestDiscoveryRejectsIssuerMismatchAndUserinfo(t *testing.T) {
	for _, field := range []string{"issuer", "authorization", "token", "jwks"} {
		t.Run(field, func(t *testing.T) {
			base, hc := testServer(t, func(base string) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					d := Discovery{Issuer: base, AuthorizationEndpoint: base + "/authorize", TokenEndpoint: base + "/token", JWKSURI: base + "/jwks"}
					switch field {
					case "issuer":
						d.Issuer = "https://other.example"
					case "authorization":
						d.AuthorizationEndpoint = "https://issuer@attacker.example/authorize"
					case "token":
						d.TokenEndpoint = "https://issuer@attacker.example/token"
					case "jwks":
						d.JWKSURI = "https://issuer@attacker.example/jwks"
					}
					_ = json.NewEncoder(w).Encode(d)
				})
			})
			if _, err := (Client{Issuer: base, HTTPClient: hc}).Discover(context.Background()); err == nil {
				t.Fatal("invalid discovery accepted")
			}
		})
	}
}

func TestAuthorizationAndTokenRejectUntrustedDiscovery(t *testing.T) {
	base := "https://issuer.example"
	good := Discovery{Issuer: base, AuthorizationEndpoint: base + "/authorize", TokenEndpoint: base + "/token", JWKSURI: base + "/jwks"}
	for name, mutate := range map[string]func(*Discovery){
		"issuer mismatch":        func(d *Discovery) { d.Issuer = "https://other.example" },
		"authorization userinfo": func(d *Discovery) { d.AuthorizationEndpoint = "https://issuer@attacker.example/authorize" },
		"token userinfo":         func(d *Discovery) { d.TokenEndpoint = "https://issuer@attacker.example/token" },
	} {
		t.Run(name, func(t *testing.T) {
			d := good
			mutate(&d)
			if _, _, err := d.AuthorizationURL("http://127.0.0.1:1234/callback", "host", "agent", ""); err == nil {
				t.Fatal("authorization accepted invalid discovery")
			}
			c := Client{Issuer: base, HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected token request"); return nil, nil })}}
			if _, err := c.Refresh(context.Background(), d, Tokens{ClientID: "issued", RefreshToken: "refresh"}); err == nil {
				t.Fatal("refresh accepted invalid discovery")
			}
		})
	}
}
