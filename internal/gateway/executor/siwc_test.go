package executor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/codec"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/router"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
	"github.com/lidessen/crabbro/internal/siwc"
)

func TestUnverifiedSIWCSessionFailsClosed(t *testing.T) {
	secrets := secret.New(t.TempDir())
	const token = "sensitive-access-token"
	if err := (siwc.Store{Secrets: secrets}).Save("session", siwc.Tokens{AccessToken: token}); err != nil {
		t.Fatal(err)
	}
	e := New(nil, secrets, nil, nil)
	cand := router.Candidate{Plan: &config.Plan{ID: "plan", SIWC: &config.SIWCSpec{SessionRef: "session", HostIDRef: "host"}}, CredentialRef: "session"}
	var attempts []shared.GatewayAttempt
	accounted := 0
	out := e.tryCandidate(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), httptest.NewRecorder(), nil, cand, nil, codec.IngressInfo{}, nil, &attempts, &accounted, time.Now())
	if out.status != http.StatusUnauthorized || len(attempts) != 1 || attempts[0].ErrorKind != ErrKindAuthError {
		t.Fatalf("unexpected failure: %+v, %+v", out, attempts)
	}
	if strings.Contains(out.errMsg, token) || strings.Contains(attempts[0].ErrorMessage, token) {
		t.Fatal("SIWC token leaked in failure")
	}
}

type siwcRoundTripper func(*http.Request) (*http.Response, error)

func (f siwcRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSIWCVerifiedExecution(t *testing.T) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	issuer := "https://issuer.example.test"
	h, _ := json.Marshal(map[string]any{"alg": "EdDSA", "kid": "key"})
	claims, _ := json.Marshal(map[string]any{"iss": issuer, "aud": "client", "sub": "subject", "exp": time.Now().Add(time.Hour).Unix()})
	signed := b64(h) + "." + b64(claims)
	idToken := signed + "." + b64(ed25519.Sign(private, []byte(signed)))
	old := siwc.Tokens{AccessToken: "old-access-token", RefreshToken: "old-refresh-token", IDToken: idToken, TokenType: "Bearer", Scope: siwc.DirectScope, ClientID: "client", ExpiresAt: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		name                                      string
		refresh, stream, expired, reject, invalid bool
	}{
		{"verified", false, false, false, false, false}, {"refresh", true, false, false, false, false}, {"expired", false, false, true, false, false}, {"stream", false, true, false, false, false}, {"rejected twice", true, false, false, true, false}, {"expired and rejected", false, false, true, true, false}, {"invalid identity", false, false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secrets := secret.New(t.TempDir())
			tokens := old
			if tc.invalid {
				tokens.IDToken = "invalid-id-token"
			}
			if tc.expired {
				tokens.ExpiresAt = time.Now().Add(-time.Minute)
			}
			if err := (siwc.Store{Secrets: secrets}).Save("session", tokens); err != nil {
				t.Fatal(err)
			}
			refreshes, calls := 0, 0
			client := &http.Client{Transport: siwcRoundTripper(func(r *http.Request) (*http.Response, error) {
				var body any
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					body = map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks"}
				case "/jwks":
					body = map[string]any{"keys": []any{map[string]string{"kty": "OKP", "kid": "key", "alg": "EdDSA", "crv": "Ed25519", "x": b64(pub)}}}
				case "/token":
					refreshes++
					body = map[string]any{"access_token": "new-access-token", "refresh_token": "new-refresh-token", "token_type": "Bearer", "scope": siwc.DirectScope, "expires_in": 3600}
				default:
					t.Fatalf("unexpected issuer path: %s", r.URL.Path)
				}
				raw, _ := json.Marshal(body)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
			})}
			e := New(nil, secrets, nil, nil)
			e.SIWCClient = siwc.Client{Issuer: issuer, HTTPClient: client}
			e.roundTrip = func(r *http.Request) (*http.Response, error) {
				calls++
				raw, _ := io.ReadAll(r.Body)
				var doc map[string]any
				if json.Unmarshal(raw, &doc) != nil || doc["stream"] != true || doc["store"] != false {
					t.Fatalf("unsafe request: %s", raw)
				}
				want := "Bearer old-access-token"
				if tc.expired || (tc.refresh && calls == 2) {
					want = "Bearer new-access-token"
				}
				if r.Header.Get("Authorization") != want {
					t.Fatal("wrong bearer")
				}
				if (tc.refresh && calls == 1) || tc.reject {
					return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("old-access-token old-refresh-token new-access-token new-refresh-token " + idToken))}, nil
				}
				// Exercise all individual credentials, including rotated values, in output.
				body := `data: {"type":"response.completed","response":{"id":"old-access-token old-refresh-token new-access-token new-refresh-token ` + idToken + `","object":"response","output":[]}}` + "\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}
			endpoint := &config.EndpointSpec{BaseURL: "https://api.openai.com/v1"}
			model := &config.ModelSpec{}
			cand := router.Candidate{Plan: &config.Plan{ID: "plan", SIWC: &config.SIWCSpec{SessionRef: "session", HostIDRef: "host"}}, CredentialRef: "session", Endpoint: endpoint, EndpointProto: config.ProtocolOpenAIResponses, ModelSpec: model, Target: config.Target{Model: "model"}}
			selection := &router.Selection{Route: &config.Route{ID: "route"}}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			output := httptest.NewRecorder()
			var attempts []shared.GatewayAttempt
			accounted := 0
			result := e.tryCandidate(request, output, nil, cand, selection, codec.IngressInfo{Protocol: config.ProtocolOpenAIResponses, Action: "responses", Stream: tc.stream}, []byte(`{"model":"model","stream":false,"store":true}`), &attempts, &accounted, time.Now())
			if tc.invalid {
				if result.committed || result.status != 401 || calls != 0 || refreshes != 0 {
					t.Fatalf("unverified identity accepted: %+v", result)
				}
				return
			}
			if tc.reject {
				if result.committed || result.status != 401 {
					t.Fatalf("expected rejected request: %+v", result)
				}
				for _, v := range append(siwc.ScrubValues(&old), "new-access-token", "new-refresh-token") {
					if strings.Contains(string(result.errBody)+result.errMsg+fmt.Sprint(attempts), v) {
						t.Fatal("credential leaked in error")
					}
				}
			} else if !result.committed || output.Code != 200 {
				t.Fatalf("execution failed: %+v, %s", result, output.Body.String())
			}
			wantCalls := 1
			if tc.refresh {
				wantCalls = 2
			}
			wantRefresh := 0
			if tc.refresh || tc.expired {
				wantRefresh = 1
			}
			if calls != wantCalls || refreshes != wantRefresh {
				t.Fatalf("calls=%d refreshes=%d", calls, refreshes)
			}
			for _, sensitive := range siwc.ScrubValues(&old) {
				if strings.Contains(output.Body.String(), sensitive) {
					t.Fatal("credential leaked")
				}
			}
			if (tc.refresh || tc.expired) && (strings.Contains(output.Body.String(), "new-access-token") || strings.Contains(output.Body.String(), "new-refresh-token")) {
				t.Fatal("rotated credential leaked")
			}
			if !tc.reject && !tc.stream && !strings.Contains(output.Body.String(), `"object":"response"`) {
				t.Fatal("missing non-stream response")
			}
		})
	}
	t.Run("serialized refresh", func(t *testing.T) {
		secrets := secret.New(t.TempDir())
		if err := (siwc.Store{Secrets: secrets}).Save("session", old); err != nil {
			t.Fatal(err)
		}
		var refreshes atomic.Int32
		client := &http.Client{Transport: siwcRoundTripper(func(r *http.Request) (*http.Response, error) {
			var body any
			switch r.URL.Path {
			case "/.well-known/openid-configuration":
				body = map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks"}
			case "/jwks":
				body = map[string]any{"keys": []any{map[string]string{"kty": "OKP", "kid": "key", "alg": "EdDSA", "crv": "Ed25519", "x": b64(pub)}}}
			case "/token":
				refreshes.Add(1)
				body = map[string]any{"access_token": "rotated", "refresh_token": "rotated-refresh", "token_type": "Bearer", "scope": siwc.DirectScope, "expires_in": 3600}
			default:
				return nil, fmt.Errorf("unexpected request")
			}
			data, _ := json.Marshal(body)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
		})}
		e := New(nil, secrets, nil, nil)
		e.SIWCClient = siwc.Client{Issuer: issuer, HTTPClient: client}
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, bearer, _, err := e.siwcSession(context.Background(), "session", &old)
				if err != nil {
					errs <- err
				} else if bearer != "rotated" {
					errs <- errors.New("wrong refreshed bearer")
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if refreshes.Load() != 1 {
			t.Fatalf("refresh calls: %d", refreshes.Load())
		}
	})

}

func TestSIWCScrubReaderSplit(t *testing.T) {
	reader := newScrubReader(&oneByteReader{data: []byte("start secret-value end")}, []string{"secret-value"})
	b, err := io.ReadAll(reader)
	if err != nil || strings.Contains(string(b), "secret-value") || !strings.Contains(string(b), "end") {
		t.Fatalf("redaction failed: %s %v", b, err)
	}
}

type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}
