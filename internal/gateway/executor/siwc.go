package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/provider"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/siwc"
)

// Refresh tokens rotate, so all executors in this process serialize their use.
var siwcRefreshMu sync.Mutex

func (e *Executor) siwcSession(ctx context.Context, ref string, rejected *siwc.Tokens) (siwc.Tokens, string, bool, error) {
	store := siwc.Store{Secrets: e.Secrets}
	tokens, err := store.Load(ref)
	if err != nil {
		return tokens, "", false, errors.New("SIWC session unavailable")
	}
	d, err := e.SIWCClient.Discover(ctx)
	if err != nil {
		return tokens, "", false, errors.New("SIWC verification unavailable")
	}
	if siwcNeedsRefresh(tokens, rejected) {
		var refreshed bool
		tokens, refreshed, err = e.refreshSIWCSession(ctx, d, store, ref, rejected)
		if err != nil {
			return tokens, "", false, err
		}
		if refreshed {
			return tokens, tokens.AccessToken, true, nil
		}
	}
	session, err := e.SIWCClient.VerifiedSession(ctx, d, tokens)
	if err != nil {
		return tokens, "", false, errors.New("SIWC verification failed")
	}
	return tokens, session.AccessToken, false, nil
}

// Only expiry or a 401 permits a refresh; invalid identity or scope fails closed.
func siwcNeedsRefresh(tokens siwc.Tokens, rejected *siwc.Tokens) bool {
	return !tokens.ExpiresAt.After(time.Now()) ||
		(rejected != nil && tokens.AccessToken == rejected.AccessToken && tokens.RefreshToken == rejected.RefreshToken)
}

func (e *Executor) refreshSIWCSession(ctx context.Context, d siwc.Discovery, store siwc.Store, ref string, rejected *siwc.Tokens) (siwc.Tokens, bool, error) {
	siwcRefreshMu.Lock()
	defer siwcRefreshMu.Unlock()

	tokens, err := store.Load(ref)
	if err != nil {
		return tokens, false, errors.New("SIWC session unavailable")
	}
	if !siwcNeedsRefresh(tokens, rejected) {
		return tokens, false, nil
	}

	// Bind the refreshed credentials to the account in the stored session.
	identity, err := e.SIWCClient.VerifyIDToken(ctx, d, tokens.IDToken, tokens.ClientID, "")
	if err != nil || !tokens.GrantedDirect() || !strings.EqualFold(tokens.TokenType, "Bearer") {
		return tokens, false, errors.New("SIWC verification failed")
	}
	next, err := e.SIWCClient.Refresh(ctx, d, tokens)
	if err != nil {
		return tokens, false, errors.New("SIWC refresh failed")
	}
	session, err := e.SIWCClient.VerifiedSession(ctx, d, next)
	if err != nil || session.Identity.Subject != identity.Subject || session.Identity.AccountID != identity.AccountID {
		return tokens, false, errors.New("SIWC verification failed")
	}
	if err := store.Save(ref, next); err != nil {
		return tokens, false, errors.New("SIWC session persistence failed")
	}
	return next, true, nil
}

func siwcBody(body []byte) ([]byte, error) {
	var v map[string]json.RawMessage
	if json.Unmarshal(body, &v) != nil || v == nil {
		return nil, errors.New("invalid SIWC request body")
	}
	v["stream"] = json.RawMessage("true")
	v["store"] = json.RawMessage("false")
	return json.Marshal(v)
}

// completedResponse collects the terminal response for non-streaming clients.
func completedResponse(src io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(io.LimitReader(src, 16<<20))
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) == nil && (event.Type == "response.completed" || event.Type == "response.incomplete") && len(event.Response) > 0 && string(event.Response) != "null" {
			return event.Response, nil
		}
	}
	return nil, errors.New("SIWC stream ended without a completed response")
}

func siwcValues(snap *config.Snapshot, secrets *secret.Store) []string {
	if snap == nil || secrets == nil {
		return nil
	}
	var values []string
	for _, p := range snap.AllPlans {
		if auth := p.EffectiveAuth(); auth != nil && auth.Kind == provider.OAuthSIWC {
			if t, err := (siwc.Store{Secrets: secrets}).Load(auth.SessionRef); err == nil {
				values = append(values, siwc.ScrubValues(&t)...)
			}
		}
	}
	return values
}

// scrubReader retains a suffix so credentials split across reads are redacted.
type scrubReader struct {
	src     io.Reader
	values  []string
	pending []byte
	ready   []byte
	done    bool
	err     error
}

func newScrubReader(src io.Reader, values []string) *scrubReader {
	return &scrubReader{src: src, values: values}
}
func (s *scrubReader) Read(p []byte) (int, error) {
	for len(s.ready) == 0 && !s.done {
		b := make([]byte, 4096)
		n, err := s.src.Read(b)
		s.pending = append(s.pending, b[:n]...)
		if err != nil {
			s.done = true
			s.err = err
		}

		// Keep only a suffix that could be the beginning of a credential.
		limit := len(s.pending)
		if !s.done {
			for _, v := range s.values {
				max := len(v) - 1
				if max > len(s.pending) {
					max = len(s.pending)
				}
				for n := max; n > 0; n-- {
					if bytes.Equal(s.pending[len(s.pending)-n:], []byte(v[:n])) {
						if len(s.pending)-n < limit {
							limit = len(s.pending) - n
						}
						break
					}
				}
			}
			// Retain an entire matching credential if it overlaps the boundary.
			for _, v := range s.values {
				if v == "" {
					continue
				}
				for i := 0; i < limit; {
					j := bytes.Index(s.pending[i:], []byte(v))
					if j < 0 {
						break
					}
					j += i
					if j+len(v) > limit {
						limit = j
						break
					}
					i = j + len(v)
				}
			}
		}

		if limit > 0 {
			s.ready = []byte(secret.ScrubText(string(s.pending[:limit]), s.values...))
			s.pending = s.pending[limit:]
		}
	}
	if len(s.ready) > 0 {
		n := copy(p, s.ready)
		s.ready = s.ready[n:]
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	return 0, io.EOF
}

func siwcTarget(req *http.Request) bool {
	return req.URL.Scheme == "https" && req.URL.Host == "api.openai.com" && req.URL.Path == "/v1/responses" && req.Method == http.MethodPost && req.URL.User == nil && req.URL.RawQuery == "" && req.URL.Fragment == ""
}
