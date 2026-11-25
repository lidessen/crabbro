package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/codec"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/provider"
	"github.com/lidessen/crabbro/internal/gateway/router"
	"github.com/lidessen/crabbro/internal/scrub"
	"github.com/lidessen/crabbro/internal/siwc"
)

// Authenticator owns the credentials and request lifecycle for one attempt.
type Authenticator interface {
	Load(context.Context) error
	Prepare(*http.Request, []byte) ([]byte, error)
	HandleUnauthorized(context.Context, *http.Request, []byte) (bool, error)
	ScrubValues() []string
	ProcessResponse(*http.Response, bool) error
	DecisionAllowed() bool
}

type keyAuthenticator struct {
	executor  *Executor
	candidate router.Candidate
	value     string
	invalid   bool
}

func (a *keyAuthenticator) Load(_ context.Context) error {
	if a.invalid {
		return errors.New("unsupported authentication kind")
	}
	var err error
	a.value, err = a.executor.Secrets.Get(a.candidate.CredentialRef)
	return err
}
func (a *keyAuthenticator) Prepare(req *http.Request, body []byte) ([]byte, error) {
	codec.InjectCredential(req.Header, a.candidate.Endpoint, a.candidate.EndpointProto, a.value)
	return body, nil
}
func (a *keyAuthenticator) HandleUnauthorized(context.Context, *http.Request, []byte) (bool, error) {
	return false, nil
}
func (a *keyAuthenticator) ScrubValues() []string                      { return []string{a.value} }
func (a *keyAuthenticator) ProcessResponse(*http.Response, bool) error { return nil }
func (a *keyAuthenticator) DecisionAllowed() bool                      { return true }

type siwcAuthenticator struct {
	executor  *Executor
	candidate router.Candidate
	ref       string
	tokens    siwc.Tokens
	value     string
	refreshed bool
	values    []string
}

func (a *siwcAuthenticator) Load(ctx context.Context) error {
	c := a.candidate
	if c.Endpoint == nil || c.Endpoint.BaseURL != "https://api.openai.com/v1" || c.EndpointProto != config.ProtocolOpenAIResponses || c.Plan.ProxyURL != "" {
		return errors.New("invalid SIWC endpoint")
	}
	original, _ := (siwc.Store{Secrets: a.executor.Secrets}).Load(a.ref)
	a.values = siwc.ScrubValues(&original)
	var err error
	a.tokens, a.value, a.refreshed, err = a.executor.siwcSession(ctx, a.ref, nil)
	a.values = append(a.values, siwc.ScrubValues(&a.tokens)...)
	return err
}
func (a *siwcAuthenticator) Prepare(req *http.Request, body []byte) ([]byte, error) {
	if !siwcTarget(req) {
		return nil, errors.New("invalid SIWC endpoint")
	}
	body, err := siwcBody(body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header = scrub.StripAuthHeaders(req.Header)
	req.Header.Set("Authorization", "Bearer "+a.value)
	return body, nil
}
func (a *siwcAuthenticator) HandleUnauthorized(ctx context.Context, req *http.Request, body []byte) (bool, error) {
	if a.refreshed {
		return false, nil
	}
	previous := a.tokens
	var err error
	a.tokens, a.value, _, err = a.executor.siwcSession(ctx, a.ref, &previous)
	a.values = append(a.values, siwc.ScrubValues(&a.tokens)...)
	if err != nil {
		return false, errors.New("SIWC refresh failed")
	}
	a.refreshed = true
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+a.value)
	return true, nil
}
func (a *siwcAuthenticator) ScrubValues() []string { return a.values }
func (a *siwcAuthenticator) DecisionAllowed() bool { return false }
func (a *siwcAuthenticator) ProcessResponse(resp *http.Response, stream bool) error {
	resp.Header = scrub.ScrubHeaders(resp.Header, a.values, a.candidate.Endpoint.AuthHeader)
	resp.Body = struct {
		io.Reader
		io.Closer
	}{newScrubReader(resp.Body, a.values), resp.Body}
	if !stream && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		body, err := completedResponse(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.Header.Set("Content-Type", "application/json")
	}
	return nil
}
func (e *Executor) authenticator(c router.Candidate) Authenticator {
	if a := c.Plan.EffectiveAuth(); a != nil && a.Kind == provider.OAuthSIWC {
		return &siwcAuthenticator{executor: e, candidate: c, ref: a.SessionRef}
	}
	a := c.Plan.EffectiveAuth()
	return &keyAuthenticator{executor: e, candidate: c, invalid: a != nil && a.Kind != provider.APIKey && a.Kind != provider.KeyPool}
}
