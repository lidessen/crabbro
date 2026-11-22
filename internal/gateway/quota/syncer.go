package quota

import (
	"context"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/pkg/api"
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
	// OnExpiryReminder is invoked once per coupon (or underutilized window)
	// when its expiry enters the plan's configured reminder lead time.
	OnExpiryReminder func(reminder QuotaReminder)

	leads map[string]time.Duration // per-plan reminder lead time

	reminderMu sync.Mutex
	reminded   map[string]struct{} // emitted reminder keys, collision-free per coupon/window

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
		values:   map[string]RemoteQuota{},
		resets:   gate,
		leads:    map[string]time.Duration{},
		reminded: map[string]struct{}{},
		clock:    time.Now,
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
	q.Reminders = quotaReminders(id, &q, now, s.reminderLead(id))
	return &q, true
}

// reminderLead returns the plan's configured reminder horizon. Plans synced
// before this field existed (and direct test fixtures) use the 24h default.
func (s *Syncer) reminderLead(planID string) time.Duration {
	s.mu.RLock()
	lead := s.leads[planID]
	s.mu.RUnlock()
	if lead <= 0 {
		return config.DefaultQuotaReminderLeadTime
	}
	return lead
}

// quotaReminders derives the current expiry reminders from one snapshot.
// It is a pure view used both by Get and by event emission; event dedupe
// happens in checkExpiryReminders.
func quotaReminders(planID string, q *RemoteQuota, now time.Time, lead time.Duration) []api.QuotaReminder {
	if q == nil || q.Stale || lead <= 0 {
		return nil
	}
	cutoff := now.Add(lead)
	var out []api.QuotaReminder
	for _, cp := range q.Resets {
		if cp.ExpiresAt == nil || !cp.ExpiresAt.After(now) || cp.ExpiresAt.After(cutoff) {
			continue
		}
		out = append(out, api.QuotaReminder{
			PlanID:    planID,
			Kind:      "expiring_coupon",
			Scope:     cp.Scope,
			Message:   "Reset coupon expires soon",
			ExpiresAt: *cp.ExpiresAt,
		})
	}
	// Preserve the aggregate-expiry reminder for snapshots that do not carry
	// per-coupon scope (the scope is unknown in that case).
	if len(q.Resets) == 0 && q.ResetsRemaining > 0 && q.SoonestResetExpiry != nil &&
		q.SoonestResetExpiry.After(now) && !q.SoonestResetExpiry.After(cutoff) {
		out = append(out, api.QuotaReminder{
			PlanID:    planID,
			Kind:      "expiring_coupon",
			Message:   "Reset coupon expires soon",
			ExpiresAt: *q.SoonestResetExpiry,
		})
	}
	for _, w := range q.Windows {
		if (w.Name != "Weekly" && w.Name != "Monthly") || w.Trend == nil || w.Trend.Status != "underutilized" {
			continue
		}
		if w.ResetAt == nil || !w.ResetAt.After(now) || w.ResetAt.After(cutoff) {
			continue
		}
		out = append(out, api.QuotaReminder{
			PlanID:       planID,
			Kind:         "underutilized_window",
			Scope:        w.Name,
			Message:      w.Name + " quota resets soon",
			ExpiresAt:    *w.ResetAt,
			RemainingPct: w.Trend.RemainingPct,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].ExpiresAt.Equal(out[j].ExpiresAt) {
			return out[i].ExpiresAt.Before(out[j].ExpiresAt)
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Scope < out[j].Scope
	})
	return out
}

// reminderKey identifies one sanitized reminder without provider coupon
// identifiers. Two coupons with the same scope and expiry are
// indistinguishable through the privacy boundary and intentionally collapse.
func reminderKey(r api.QuotaReminder) string {
	return r.PlanID + "\x00" + r.Kind + "\x00" + r.Scope + "\x00" + strconv.FormatInt(r.ExpiresAt.UTC().UnixNano(), 10)
}

// checkExpiryReminders emits at most one event per coupon/window and prunes
// entries that no longer appear in fresh snapshots, so a long-running daemon
// does not leak state. Callers must pass a fresh (non-stale) snapshot.
func (s *Syncer) checkExpiryReminders(planID string, q *RemoteQuota, now time.Time, lead time.Duration) []api.QuotaReminder {
	current := quotaReminders(planID, q, now, lead)
	s.reminderMu.Lock()
	if s.reminded == nil {
		s.reminded = map[string]struct{}{}
	}
	prefix := planID + "\x00"
	currentKeys := make(map[string]struct{}, len(current))
	for _, r := range current {
		currentKeys[reminderKey(r)] = struct{}{}
	}
	for key := range s.reminded {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if _, ok := currentKeys[key]; !ok {
			delete(s.reminded, key)
		}
	}
	fresh := make([]api.QuotaReminder, 0, len(current))
	for _, r := range current {
		key := reminderKey(r)
		if _, seen := s.reminded[key]; seen {
			continue
		}
		s.reminded[key] = struct{}{}
		fresh = append(fresh, r)
	}
	s.reminderMu.Unlock()
	if s.OnExpiryReminder != nil {
		for _, r := range fresh {
			s.OnExpiryReminder(r)
		}
	}
	return fresh
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

// sanitizedSyncError maps one fetch failure onto the snapshot's LastError
// status. ok=false means the failure is not a sync fault at all (the plan's
// provider mapping resolved to no quota endpoint): the caller must leave any
// stored snapshot untouched instead of marking it stale, so a plan without a
// quota endpoint never degrades routing state. Only the fixed,
// provider-attributed summaries from providers.go pass the boundary
// verbatim; everything else collapses to the generic marker.
func sanitizedSyncError(err error) (string, bool) {
	if err == nil {
		return "", true
	}
	if err.Error() == "unsupported provider" {
		return "", false
	}
	for _, prefix := range []string{
		"endpoint requires session auth or rejected credentials (HTTP ",
		"zenmux key lacks management scope (HTTP ",
	} {
		if strings.HasPrefix(err.Error(), prefix) {
			return err.Error(), true
		}
	}
	return "provider sync failed", true
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
		status, syncFault := sanitizedSyncError(err)
		if !syncFault {
			// No quota endpoint exists for this provider mapping (e.g. the
			// zenmux-jev relay): it is a configuration fact, not a failed
			// sync, so keep the previous snapshot as-is.
			s.mu.Unlock()
			return
		}
		q = s.values[id]
		q.Stale = true
		q.LastError = status
		if q.SyncedAt != nil {
			q.SyncSource = "cached"
		}
	} else {
		now := time.Now().UTC()
		q.SyncedAt = &now
		q.SyncSource = "live-api"
		// fetch never populates Trend, yet quotaReminders — and the
		// gateway_quota_expiring event path behind syncPlan — requires it to
		// recognize underutilized windows. Derive it exactly like Get does so
		// the stored snapshot and the emitted events agree.
		trendNow := s.now()
		for i := range q.Windows {
			q.Windows[i].Trend = windowTrend(q.Windows[i], trendNow)
		}
	}
	s.values[id] = q
	s.leads[id] = p.QuotaReminder.LeadTimeDuration()
	s.mu.Unlock()
	if err == nil {
		s.checkExpiryReminders(id, &q, s.now(), p.QuotaReminder.LeadTimeDuration())
		if hook {
			s.maybeAutoReset(p, q)
		}
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
