package executor

import (
	"context"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/provider"
	"github.com/lidessen/crabbro/internal/gateway/router"
	"github.com/lidessen/crabbro/internal/secret"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthenticators(t *testing.T) {
	store := secret.New(t.TempDir())
	if err := store.Set("key", "test-secret-value"); err != nil {
		t.Fatal(err)
	}
	e := New(nil, store, nil, nil)
	for _, kind := range []provider.AuthKind{provider.APIKey, provider.KeyPool} {
		c := router.Candidate{Plan: &config.Plan{Auth: &config.AuthSpec{Kind: kind}}, CredentialRef: "key", Endpoint: &config.EndpointSpec{}, EndpointProto: config.ProtocolOpenAIChat}
		a := e.authenticator(c)
		if err := a.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/chat/completions", nil)
		if _, err := a.Prepare(req, nil); err != nil || req.Header.Get("Authorization") != "Bearer test-secret-value" {
			t.Fatalf("auth injection failed: %v", err)
		}
		if retry, err := a.HandleUnauthorized(context.Background(), req, nil); retry || err != nil {
			t.Fatal("key unexpectedly retried")
		}
	}
	c := router.Candidate{Plan: &config.Plan{Auth: &config.AuthSpec{Kind: provider.OAuthSIWC, SessionRef: "session"}}, Endpoint: &config.EndpointSpec{BaseURL: "https://example.com"}, EndpointProto: config.ProtocolOpenAIResponses}
	a := e.authenticator(c)
	if a.DecisionAllowed() || a.Load(context.Background()) == nil {
		t.Fatal("SIWC must reject untrusted endpoints and decisions")
	}
}
