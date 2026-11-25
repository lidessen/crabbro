// Package siwc implements the foundational OpenAI SIWC OAuth flow. Token
// exchange alone does not authenticate an account: callers must verify the
// session and bind its identity before integration with the gateway.
package siwc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultIssuer   = "https://auth.openai.com"
	DirectScope     = "chatgpt.tokens.use.direct"
	Resource        = "https://api.openai.com/v1"
	DynamicClientID = "dynamic_agent_client"
	requestedScopes = "openid profile email offline_access resource.invoke " + DirectScope
)

// Client allows an alternate issuer and HTTP transport for isolated tests.
type Client struct {
	Issuer     string
	HTTPClient *http.Client
}
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

func validEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && ip != nil && ip.IsLoopback()
}

func (c Client) request(ctx context.Context, method, endpoint string, body io.Reader, contentType string, out any) error {
	if !validEndpoint(endpoint) {
		return errors.New("invalid SIWC endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("create SIWC request")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	hc := http.Client{Timeout: 30 * time.Second}
	if c.HTTPClient != nil {
		hc = *c.HTTPClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("SIWC redirects are disabled") }
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("SIWC request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("SIWC request failed (HTTP %d)", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || json.Unmarshal(data, out) != nil {
		return errors.New("invalid SIWC response")
	}
	return nil
}

// Discover retrieves the issuer metadata and rejects an issuer mismatch.
func (c Client) Discover(ctx context.Context) (Discovery, error) {
	issuer, err := c.issuer()
	if err != nil {
		return Discovery{}, err
	}
	var d Discovery
	if err := c.request(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil, "", &d); err != nil {
		return Discovery{}, err
	}
	if err := c.validateDiscovery(d); err != nil {
		return Discovery{}, err
	}
	return d, nil
}

func (c Client) issuer() (string, error) {
	issuer := c.Issuer
	if issuer == "" {
		issuer = DefaultIssuer
	}
	u, err := url.Parse(issuer)
	if err != nil || !validEndpoint(issuer) || u.RawQuery != "" || strings.HasSuffix(issuer, "/") {
		return "", errors.New("invalid SIWC issuer")
	}
	return issuer, nil
}

func (c Client) validateDiscovery(d Discovery) error {
	issuer, err := c.issuer()
	if err != nil || d.Issuer != issuer {
		return errors.New("invalid SIWC discovery metadata")
	}
	u, _ := url.Parse(issuer)
	for _, endpoint := range []string{d.AuthorizationEndpoint, d.TokenEndpoint, d.JWKSURI} {
		if !validEndpoint(endpoint) {
			return errors.New("invalid SIWC discovery metadata")
		}
		e, _ := url.Parse(endpoint)
		if e.Scheme != u.Scheme || !strings.EqualFold(e.Host, u.Host) {
			return errors.New("invalid SIWC discovery metadata")
		}
	}
	return nil
}

// Attempt holds one authorization request. Do not log or serialize it.
type Attempt struct{ State, Nonce, Verifier, RedirectURI, ClientID string }

func (Attempt) String() string   { return "[SIWC authorization attempt redacted]" }
func (Attempt) GoString() string { return "[SIWC authorization attempt redacted]" }

func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path == "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port <= 65535
}

// AuthorizationURL starts dynamic registration when clientID is empty; otherwise
// it starts reauthorization of the specified issued client.
func (d Discovery) AuthorizationURL(redirectURI, hostID, agentName, clientID string) (string, Attempt, error) {
	if !validEndpoint(d.AuthorizationEndpoint) || !validRedirect(redirectURI) || hostID == "" {
		return "", Attempt{}, errors.New("invalid SIWC authorization configuration")
	}
	if clientID == "" {
		clientID = DynamicClientID
	}
	if clientID == DynamicClientID && agentName == "" {
		return "", Attempt{}, errors.New("SIWC agent name is required")
	}
	state, err := randomValue()
	if err != nil {
		return "", Attempt{}, err
	}
	nonce, err := randomValue()
	if err != nil {
		return "", Attempt{}, err
	}
	verifier, err := randomValue()
	if err != nil {
		return "", Attempt{}, err
	}
	a := Attempt{State: state, Nonce: nonce, Verifier: verifier, RedirectURI: redirectURI, ClientID: clientID}
	u, _ := url.Parse(d.AuthorizationEndpoint)
	q := u.Query()
	for k, v := range map[string]string{"client_id": clientID, "ext_agent_host_id": hostID, "response_type": "code", "redirect_uri": redirectURI, "scope": requestedScopes, "resource": Resource, "state": state, "nonce": nonce, "code_challenge_method": "S256", "code_challenge": challenge(verifier)} {
		q.Set(k, v)
	}
	if clientID == DynamicClientID {
		q.Set("agent_name_hint", agentName)
	} else {
		q.Del("agent_name_hint")
	}
	u.RawQuery = q.Encode()
	return u.String(), a, nil
}

// Callback is the validated authorization response; its code is sensitive.
type Callback struct{ Code, ClientID string }

func (Callback) String() string   { return "[SIWC callback redacted]" }
func (Callback) GoString() string { return "[SIWC callback redacted]" }

// ValidateCallback validates a request received by the caller's loopback listener.
func (a Attempt) ValidateCallback(r *http.Request) (Callback, error) {
	if r == nil || r.URL == nil || r.Method != http.MethodGet || !validRedirect(a.RedirectURI) || a.State == "" || a.Verifier == "" {
		return Callback{}, errors.New("invalid SIWC callback")
	}
	redirect, _ := url.Parse(a.RedirectURI)
	if r.Host != redirect.Host || r.URL.Path != redirect.Path {
		return Callback{}, errors.New("invalid SIWC callback target")
	}
	q := r.URL.Query()
	for _, k := range []string{"state", "code", "client_id", "error"} {
		if len(q[k]) > 1 {
			return Callback{}, errors.New("invalid SIWC callback parameters")
		}
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(a.State)) != 1 {
		return Callback{}, errors.New("invalid SIWC callback state")
	}
	if q.Get("error") != "" {
		return Callback{}, errors.New("SIWC authorization denied or failed")
	}
	clientID := q.Get("client_id")
	if a.ClientID == DynamicClientID {
		if clientID == "" || clientID == DynamicClientID {
			return Callback{}, errors.New("SIWC registration did not issue a client ID")
		}
	} else {
		if a.ClientID == "" || (clientID != "" && clientID != a.ClientID) {
			return Callback{}, errors.New("SIWC callback client ID mismatch")
		}
		clientID = a.ClientID
	}
	if q.Get("code") == "" {
		return Callback{}, errors.New("SIWC callback missing code")
	}
	return Callback{Code: q.Get("code"), ClientID: clientID}, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (c Client) token(ctx context.Context, d Discovery, clientID string, form url.Values) (Tokens, error) {
	if clientID == "" || clientID == DynamicClientID {
		return Tokens{}, errors.New("issued SIWC client ID is required")
	}
	if err := c.validateDiscovery(d); err != nil {
		return Tokens{}, err
	}
	form.Set("client_id", clientID)
	form.Set("resource", Resource)
	var result tokenResponse
	if err := c.request(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", &result); err != nil {
		return Tokens{}, err
	}
	if result.AccessToken == "" || !strings.EqualFold(result.TokenType, "Bearer") || result.ExpiresIn <= 0 || result.ExpiresIn > int64((365*24*time.Hour)/time.Second) {
		return Tokens{}, errors.New("invalid SIWC token response")
	}
	return Tokens{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken, TokenType: result.TokenType, Scope: result.Scope, ExpiresAt: time.Now().Add(time.Duration(result.ExpiresIn) * time.Second), ClientID: clientID}, nil
}

// Exchange exchanges a validated callback using the attempt's original verifier.
func (c Client) Exchange(ctx context.Context, d Discovery, a Attempt, cb Callback) (Tokens, error) {
	if cb.Code == "" || cb.ClientID == "" || a.Verifier == "" || !validRedirect(a.RedirectURI) || (a.ClientID != DynamicClientID && cb.ClientID != a.ClientID) {
		return Tokens{}, errors.New("invalid SIWC exchange")
	}
	tokens, err := c.token(ctx, d, cb.ClientID, url.Values{"grant_type": {"authorization_code"}, "code": {cb.Code}, "code_verifier": {a.Verifier}, "redirect_uri": {a.RedirectURI}})
	if err != nil {
		return Tokens{}, err
	}
	if tokens.IDToken != "" {
		if a.Nonce == "" {
			return Tokens{}, errors.New("SIWC exchange nonce is required")
		}
		if _, err := c.VerifyIDToken(ctx, d, tokens.IDToken, tokens.ClientID, a.Nonce); err != nil {
			return Tokens{}, err
		}
	}
	return tokens, nil
}

// Refresh returns replacement credentials. The caller must serialize refreshes
// for a session and atomically persist the result before using it.
func (c Client) Refresh(ctx context.Context, d Discovery, old Tokens) (Tokens, error) {
	if old.RefreshToken == "" {
		return Tokens{}, errors.New("SIWC refresh token is unavailable")
	}
	next, err := c.token(ctx, d, old.ClientID, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {old.RefreshToken}})
	if err != nil {
		return Tokens{}, err
	}
	if next.IDToken != "" {
		if _, err := c.VerifyIDToken(ctx, d, next.IDToken, next.ClientID, ""); err != nil {
			return Tokens{}, err
		}
	}
	if next.RefreshToken == "" {
		next.RefreshToken = old.RefreshToken
	}
	if next.IDToken == "" {
		next.IDToken = old.IDToken
	}
	if next.Scope == "" {
		next.Scope = old.Scope
	}
	if !next.GrantedDirect() {
		return Tokens{}, errors.New("SIWC refresh response lacks required direct scope")
	}
	return next, nil
}
