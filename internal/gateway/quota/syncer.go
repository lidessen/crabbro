package quota

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
)

// Syncer owns in-memory provider snapshots.
type Syncer struct {
	secrets *secret.Store
	client  *http.Client
	mu      sync.RWMutex
	values  map[string]RemoteQuota
	refresh sync.Mutex
}

func New(secrets *secret.Store) *Syncer {
	return &Syncer{secrets: secrets, client: &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, values: map[string]RemoteQuota{}}
}

// Get returns a detached snapshot.
func (s *Syncer) Get(id string) (*RemoteQuota, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	q, ok := s.values[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	q.Windows = append([]QuotaWindow(nil), q.Windows...)
	q.Resets = append([]ResetCoupon(nil), q.Resets...)
	return &q, true
}

// Refresh fetches configured providers concurrently and waits for this refresh to finish.
func (s *Syncer) Refresh(ctx context.Context, plans map[string]*config.Plan) {
	if s == nil {
		return
	}
	s.refresh.Lock()
	defer s.refresh.Unlock()
	var wg sync.WaitGroup
	for id, p := range plans {
		if p == nil || !p.IsEnabled() || provider(p) == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			key, err := s.secrets.Get(p.Credential)
			var q RemoteQuota
			if err == nil && key != "" {
				q, err = fetch(ctx, s.client, p, key)
			} else {
				err = http.ErrNoCookie
			}
			if ctx.Err() != nil {
				return
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if err != nil {
				q = s.values[id]
				q.Stale = true
				q.LastError = "provider sync failed"
				if q.SyncedAt != nil {
					q.SyncSource = "cached"
				}
			} else {
				now := time.Now().UTC()
				q.SyncedAt = &now
				q.SyncSource = "live-api"
			}
			s.values[id] = q
		}()
	}
	wg.Wait()
}

// Run starts an immediate refresh and repeats until cancellation.
func (s *Syncer) Run(ctx context.Context, plans func() map[string]*config.Plan) {
	s.Refresh(ctx, plans())
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Refresh(ctx, plans())
		}
	}
}
