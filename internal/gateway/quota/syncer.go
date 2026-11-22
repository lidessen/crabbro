package quota

import (
	"context"
	"github.com/lidessen/crabbro/pkg/api"
	"math"
	"net/http"
	"path/filepath"
	"strings"
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
	resets  *resetGate

	// OnAutoReset is invoked after every verified automatic redemption. The
	// gateway uses it to clear plan cooling and emit a sanitized event.
	OnAutoReset func(planID, scope string, out ResetOutcome)

	clockMu sync.Mutex
	clock   func() time.Time
}

func New(secrets *secret.Store, rootDir string) *Syncer {
	gate := newResetGate()
	gate.stateDir = filepath.Join(rootDir, "reset-guards")
	return &Syncer{
		secrets: secrets,
		client: &http.Client{
			Timeout: 8 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		values: map[string]RemoteQuota{},
		resets: gate,
		clock:  time.Now,
	}
}

// now returns the syncer clock.
func (s *Syncer) now() time.Time {
	s.clockMu.Lock()
	f := s.clock
	s.clockMu.Unlock()
	if f == nil {
		return time.Now()
	}
	return f()
}

// SetClock replaces the clock used for redemption cooldown accounting. It
// exists so tests can advance the anti-double-spend interval deterministically.
func (s *Syncer) SetClock(f func() time.Time) {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	s.clock = f
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
	now := s.now()
	for i := range q.Windows {
		q.Windows[i].Trend = windowTrend(q.Windows[i], now)
	}
	q.Reminders = nil
	if !q.Stale {
		if q.ResetsRemaining > 0 && q.SoonestResetExpiry != nil && q.SoonestResetExpiry.After(now) && q.SoonestResetExpiry.Sub(now) <= 24*time.Hour {
			q.Reminders = append(q.Reminders, api.QuotaReminder{PlanID: id, Kind: "expiring_coupon", Message: "Reset coupon expires soon", ExpiresAt: *q.SoonestResetExpiry})
		}
		for _, w := range q.Windows {
			if (w.Name == "Weekly" || w.Name == "Monthly") && w.Trend != nil && w.Trend.Status == "underutilized" && w.ResetAt != nil && w.ResetAt.After(now) && w.ResetAt.Sub(now) <= 24*time.Hour {
				q.Reminders = append(q.Reminders, api.QuotaReminder{PlanID: id, Kind: "underutilized_window", Message: w.Name + " quota resets soon", ExpiresAt: *w.ResetAt, RemainingPct: w.Trend.RemainingPct})
			}
		}
	}
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
	for _, p := range plans {
		if p == nil || !p.IsEnabled() || provider(p) == "" {
			continue
		}
		wg.Add(1)
		go func(p *config.Plan) {
			defer wg.Done()
			s.syncOne(ctx, p)
		}(p)
	}
	wg.Wait()
}

// syncOne fetches and stores the remote quota snapshot for a single plan and
// raises the opt-in auto-reset evaluation when a configured window is exhausted.
func (s *Syncer) syncOne(ctx context.Context, p *config.Plan) {
	s.syncPlan(ctx, p, true)
}

// syncPlan fetches and stores one plan snapshot; hook controls whether the
// opt-in auto-reset evaluation is raised afterwards.
func (s *Syncer) syncPlan(ctx context.Context, p *config.Plan, hook bool) {
	if s == nil || p == nil || !p.IsEnabled() || provider(p) == "" {
		return
	}
	id := p.ID
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
	if err != nil {
		q = s.values[id]
		q.Stale = true
		q.LastError = "provider sync failed"
		if err != nil && strings.HasPrefix(err.Error(), "endpoint requires session auth or rejected credentials (HTTP ") {
			q.LastError = err.Error()
		}
		if q.SyncedAt != nil {
			q.SyncSource = "cached"
		}
	} else {
		now := time.Now().UTC()
		q.SyncedAt = &now
		q.SyncSource = "live-api"
	}
	s.values[id] = q
	s.mu.Unlock()
	if err == nil && hook {
		s.maybeAutoReset(p, q)
	}
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

func windowTrend(w QuotaWindow, now time.Time) *api.WindowTrend {
	hours := map[string]float64{"5 hours": 5, "Weekly": 168, "Monthly": 720}[w.Name]
	if hours == 0 || w.ResetAt == nil {
		return nil
	}
	elapsed := math.Max(0, math.Min(100, (1-w.ResetAt.Sub(now).Hours()/hours)*100))
	t := &api.WindowTrend{WindowHours: hours, ElapsedPct: elapsed, Status: "early"}
	if w.UsagePct != nil {
		used := math.Max(0, math.Min(100, *w.UsagePct))
		delta, remaining := used-elapsed, 100-used
		t.PaceDeltaPct, t.RemainingPct = &delta, &remaining
		if elapsed >= 2 {
			projected := math.Min(100, used/elapsed*100)
			t.ProjectedEndPct = &projected
			t.Status = "on_track"
			if projected < 60 {
				t.Status = "underutilized"
			} else if projected >= 95 {
				t.Status = "hot"
			}
		}
		if used >= 99.9 {
			t.Status = "exhausted"
		}
	} else if elapsed >= 2 {
		t.Status = "on_track"
	}
	return t
}
