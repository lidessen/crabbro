package executor

import (
	"net/http"
	"net/http/httptest"
	"strings"
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
