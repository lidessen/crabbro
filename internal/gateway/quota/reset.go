package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
)

// ResetOutcome is the sanitized result of one redemption attempt. It is the
// quota-internal alias of the /v1/gateway/quotas/reset response: provider
// coupon identifiers never appear in it.
type ResetOutcome = apiResetOutcome

// attemptGuard throttles automatic retries after an ambiguous or failed
// redemption attempt until real evidence arrives again.
const attemptGuard = 30 * time.Second

// resetGate serializes redemptions and tracks anti-double-spend cooldowns.
type resetGate struct {
	mu        sync.Mutex
	locks     map[string]*sync.Mutex // single-flight per plan
	last      map[string]time.Time   // last verified redemption per plan+scope
	lasting   map[string]time.Time   // last attempt per plan+scope (any outcome)
	uncertain map[string]bool        // unverified /use result per plan+scope
	stateDir  string                 // persistent guards for interrupted requests
}

func newResetGate() *resetGate {
	return &resetGate{locks: map[string]*sync.Mutex{}, last: map[string]time.Time{}, lasting: map[string]time.Time{}, uncertain: map[string]bool{}}
}

func (g *resetGate) lock(planID string) *sync.Mutex {
	g.mu.Lock()
	defer g.mu.Unlock()
	l, ok := g.locks[planID]
	if !ok {
		l = &sync.Mutex{}
		g.locks[planID] = l
	}
	return l
}

func (g *resetGate) noteRedeem(planID, scope string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.uncertain, planID+"\x00"+scope)
	g.last[planID+"\x00"+scope] = at
	g.lasting[planID+"\x00"+scope] = at
}

func (g *resetGate) noteAttempt(planID, scope string, at time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lasting[planID+"\x00"+scope] = at
}

// An unverified /use result requires operator reconciliation before another
// automatic redemption. A timer alone cannot establish whether it succeeded.
func (g *resetGate) guardPath(planID, scope string) string {
	sum := sha256.Sum256([]byte(planID + "\x00" + scope))
	return filepath.Join(g.stateDir, hex.EncodeToString(sum[:]))
}

func (g *resetGate) persistUncertain(planID, scope string) error {
	if err := os.MkdirAll(g.stateDir, 0700); err != nil {
		return errors.New("reset guard unavailable")
	}
	if err := os.WriteFile(g.guardPath(planID, scope), []byte("pending\n"), 0600); err != nil {
		return errors.New("reset guard unavailable")
	}
	return nil
}

func (g *resetGate) noteUncertain(planID, scope string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.uncertain[planID+"\x00"+scope] = true
}

func (g *resetGate) isUncertain(planID, scope string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.uncertain[planID+"\x00"+scope] {
		return true
	}
	_, err := os.Stat(g.guardPath(planID, scope))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func (g *resetGate) lastRedeem(planID, scope string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.last[planID+"\x00"+scope]
	return t, ok
}

func (g *resetGate) lastAttempt(planID, scope string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.lasting[planID+"\x00"+scope]
	return t, ok
}

// NormalizeResetScope validates an operator-supplied scope. An empty scope
// means "pick the soonest-expiring coupon across both windows".
func NormalizeResetScope(scope string) error {
	switch scope {
	case "", ScopeFiveHour, ScopeWeekly:
		return nil
	}
	return fmt.Errorf("invalid scope %q (want fiveHour or weekly)", scope)
}

// Reset explicitly redeems at most one available reset coupon for planID.
// It always re-reads the provider /list endpoint, picks the soonest-expiring
// coupon matching scope, redeems it once with a fresh request id, and then
// refreshes the remote quota snapshot for the plan.
func (s *Syncer) Reset(ctx context.Context, plans map[string]*config.Plan, planID, scope string, dryRun bool) (ResetOutcome, error) {
	out := ResetOutcome{Plan: planID, Scope: scope, DryRun: dryRun}
	if s == nil {
		return out, errors.New("quota syncer unavailable")
	}
	if err := NormalizeResetScope(scope); err != nil {
		return out, err
	}
	p := plans[planID]
	if p == nil {
		return out, fmt.Errorf("plan %q not found", planID)
	}
	if !p.IsEnabled() {
		return out, fmt.Errorf("plan %q is disabled", planID)
	}
	if provider(p) != "zcode" {
		return out, ErrResetUnsupported
	}
	got, err := s.redeem(ctx, p, scope, dryRun, nil)
	if got.Plan != "" {
		out = got
	}
	return out, err
}

// AutoReset evaluates the opt-in auto-redeem policy for planID. It is safe to
// call from any goroutine, performs no network I/O while a cooldown is
// active, and never retries an ambiguous redemption: on any failure the
// normal cooling/failover machinery stays in charge.
//
// upstreamTrigger marks signals raised by the executor (quota exhaustion or
// rate-limit cooling). Those refresh the window evidence first so the
// redemption decision is always based on fresh provider data.
func (s *Syncer) AutoReset(ctx context.Context, plans map[string]*config.Plan, planID string, upstreamTrigger bool) {
	if s == nil || ctx == nil {
		return
	}
	p := plans[planID]
	if p == nil || !p.IsEnabled() || !p.AutoReset.Enabled || provider(p) != "zcode" {
		return
	}
	policy := p.AutoReset
	minInterval := policy.MinIntervalDuration()
	now := s.now()
	var eligible []string
	for _, scope := range policy.EffectiveAutoResetScopes() {
		if s.cooldownActive(planID, scope, minInterval, now) {
			continue
		}
		eligible = append(eligible, scope)
	}
	if len(eligible) == 0 {
		return
	}
	if upstreamTrigger {
		// Refresh window evidence before spending anything. The quiet variant
		// skips the post-sync hook so this evaluation stays the only actor.
		s.syncPlan(ctx, p, false)
	}
	for _, scope := range eligible {
		if !policy.AllowsAutoResetScope(scope) {
			continue
		}
		// Normal exhaustion triggers and the opt-in pre-expiry safeguard are
		// evaluated independently; both still pass through the same guards.
		if !s.autoResetEligible(planID, scope, now) && !s.preExpirySafeguardEligible(planID, scope, policy.Safeguard, now) {
			continue
		}
		out, err := s.redeem(ctx, p, scope, false, &policy)
		if err != nil || !out.Redeemed {
			continue // fail closed: no retry, no cooldown clearing
		}
		if s.OnAutoReset != nil {
			s.OnAutoReset(planID, scope, out)
		}
	}
}

// AutoResetAsync evaluates the opt-in auto-redeem policy on a detached
// context so callers on a request path never block on provider I/O.
func (s *Syncer) AutoResetAsync(plans func() map[string]*config.Plan, planID string, upstreamTrigger bool) {
	if s == nil || plans == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), autoResetBudget)
		defer cancel()
		s.AutoReset(ctx, plans(), planID, upstreamTrigger)
	}()
}

// cooldownActive reports whether the anti-double-spend interval for one
// (plan, scope) pair is still running. A recent ambiguous attempt also guards
// the pair so a failing provider is never hammered.
func (s *Syncer) cooldownActive(planID, scope string, minInterval time.Duration, now time.Time) bool {
	if s.resets.isUncertain(planID, scope) {
		return true
	}
	if minInterval > 0 {
		if last, ok := s.resets.lastRedeem(planID, scope); ok && now.Sub(last) < minInterval {
			return true
		}
	}
	if last, ok := s.resets.lastAttempt(planID, scope); ok && now.Sub(last) < attemptGuard {
		return true
	}
	return false
}

// scopeExhausted reports whether fresh (non-stale) provider data shows the
// window for scope at or above the exhaustion threshold.
func (s *Syncer) scopeExhausted(planID, scope string) bool {
	q, ok := s.Get(planID)
	if !ok || q.Stale {
		return false
	}
	name := windowNameForScope(scope)
	for _, w := range q.Windows {
		if w.Name != name || w.UsagePct == nil {
			continue
		}
		if *w.UsagePct >= exhaustedWindowPct {
			return true
		}
	}
	return false
}

// weeklyRecoveryMinPct is the weekly usage at or above which an exhausted
// five-hour window may fall back to a weekly coupon. Spending a weekly coupon
// while the weekly window still has plenty of quota left wastes it, so the
// fallback is deliberately stricter than the normal matching-window trigger.
const weeklyRecoveryMinPct = 90.0

// autoResetEligible reports whether fresh quota evidence justifies spending a
// coupon for scope. A weekly coupon may recover an exhausted five-hour window
// only when the weekly window itself is nearly consumed; the pre-expiry
// safeguard handles use-it-or-lose-it cases separately with a scope-local
// threshold.
func (s *Syncer) autoResetEligible(planID, scope string, now time.Time) bool {
	if s.scopeExhausted(planID, scope) {
		return true
	}
	if scope != ScopeWeekly || !s.scopeExhausted(planID, ScopeFiveHour) {
		return false
	}
	q, ok := s.Get(planID)
	if !ok || q.Stale {
		return false
	}
	used, ok := windowUsagePct(q, ScopeWeekly)
	return ok && used >= weeklyRecoveryMinPct
}

// windowUsagePct returns the usage percentage of the window behind scope.
// Aggregate usage is only used when the provider reports no named windows.
func windowUsagePct(q *RemoteQuota, scope string) (float64, bool) {
	if q == nil {
		return 0, false
	}
	name := windowNameForScope(scope)
	for _, w := range q.Windows {
		if w.Name == name && w.UsagePct != nil {
			return *w.UsagePct, true
		}
	}
	if len(q.Windows) == 0 && q.UsagePct != nil {
		return *q.UsagePct, true
	}
	return 0, false
}

// soonestCouponExpiry returns the earliest future coupon expiry for scope.
func soonestCouponExpiry(q *RemoteQuota, scope string, now time.Time) (time.Time, bool) {
	if q == nil {
		return time.Time{}, false
	}
	var soonest time.Time
	for _, cp := range q.Resets {
		if cp.Scope != scope || cp.ExpiresAt == nil || !cp.ExpiresAt.After(now) {
			continue
		}
		if soonest.IsZero() || cp.ExpiresAt.Before(soonest) {
			soonest = *cp.ExpiresAt
		}
	}
	// Older/simplified snapshots may only carry the aggregate expiry. The
	// caller still enforces the per-scope usage threshold, so this cannot
	// spend a weekly coupon while weekly quota remains.
	if soonest.IsZero() && len(q.Resets) == 0 && q.SoonestResetExpiry != nil && q.SoonestResetExpiry.After(now) {
		soonest = *q.SoonestResetExpiry
	}
	return soonest, !soonest.IsZero()
}

// preExpirySafeguardEligible implements the opt-in rule
//
//	soonestCouponExpiry(scope) < leadTime AND used(scope) >= minUsedPercent
//
// The usage threshold is evaluated against the same window the coupon resets.
// This is what prevents an expiring weekly coupon from burning itself merely
// to recover a five-hour window while weekly quota remains plentiful.
func (s *Syncer) preExpirySafeguardEligible(planID, scope string, spec config.SafeguardSpec, now time.Time) bool {
	if !spec.Enabled {
		return false
	}
	q, ok := s.Get(planID)
	if !ok || q.Stale {
		return false
	}
	expiry, ok := soonestCouponExpiry(q, scope, now)
	if !ok || expiry.Sub(now) > spec.LeadTimeDuration() {
		return false
	}
	used, ok := windowUsagePct(q, scope)
	if !ok {
		return false
	}
	return used >= spec.EffectiveMinUsedPercent()
}

// maybeAutoReset launches the opt-in evaluation when a freshly synced
// snapshot shows an exhausted configured window or an expiring coupon that
// crosses the pre-expiry safeguard threshold.
func (s *Syncer) maybeAutoReset(p *config.Plan, q RemoteQuota) {
	if !p.AutoReset.Enabled || provider(p) != "zcode" || q.Stale {
		return
	}
	now := s.now()
	for _, scope := range p.AutoReset.EffectiveAutoResetScopes() {
		if !p.AutoReset.AllowsAutoResetScope(scope) {
			continue
		}
		if s.autoResetEligible(p.ID, scope, now) || s.preExpirySafeguardEligible(p.ID, scope, p.AutoReset.Safeguard, now) {
			s.AutoResetAsync(func() map[string]*config.Plan { return map[string]*config.Plan{p.ID: p} }, p.ID, false)
			return
		}
	}
}

// redeem performs one guarded redemption cycle for plan p.
func (s *Syncer) redeem(ctx context.Context, p *config.Plan, scope string, dryRun bool, policy *config.AutoResetSpec) (ResetOutcome, error) {
	out := ResetOutcome{Plan: p.ID, Scope: scope, DryRun: dryRun}
	if p.SIWC != nil {
		return out, errors.New("quota reset is unavailable for SIWC sessions")
	}
	minInterval := time.Duration(0)
	if policy != nil {
		minInterval = policy.MinIntervalDuration()
	}
	// Single-flight per plan: at most one redemption cycle per plan at a time.
	lock := s.resets.lock(p.ID)
	if !lock.TryLock() {
		return out, ErrResetInFlight
	}
	defer lock.Unlock()

	now := s.now()
	if policy != nil && s.cooldownActive(p.ID, scope, minInterval, now) {
		return out, ErrResetCooldown
	}
	var key string
	for _, ref := range p.CredentialRefs() {
		value, err := s.secrets.Get(ref)
		if err == nil && value != "" {
			key = value
			break
		}
	}
	if key == "" {
		return out, errors.New("plan credential is not set")
	}
	// Always re-read the coupon list: never redeem from cached state.
	coupons, err := listResetCoupons(ctx, s.client, zcodeBase(p), key)
	if err != nil {
		return out, err
	}
	horizon := time.Duration(0)
	if policy != nil {
		horizon = policy.ExpiringWithinDuration()
	}
	var matching []resetCoupon
	for _, cp := range coupons {
		if scope != "" && cp.scope != scope {
			continue
		}
		if cp.expiresAt != nil && !cp.expiresAt.After(now) {
			continue
		}
		if horizon > 0 && (cp.expiresAt == nil || cp.expiresAt.After(now.Add(horizon))) {
			continue
		}
		matching = append(matching, cp)
	}
	out.CouponsAvailable = len(matching)
	if len(matching) == 0 {
		s.resets.noteAttempt(p.ID, scope, s.now())
		return out, ErrResetNoCoupons
	}
	pick := matching[0]
	for _, cp := range matching[1:] {
		if cp.expiresAt != nil && (pick.expiresAt == nil || cp.expiresAt.Before(*pick.expiresAt)) {
			pick = cp
		}
	}
	if policy != nil && pick.scope == ScopeWeekly && !s.scopeExhausted(p.ID, ScopeWeekly) {
		// Recovering a different window with a weekly coupon is only safe
		// when the weekly window is genuinely consumed. The safeguard may
		// lower that bar for a coupon that is about to expire, but never
		// below its configured usage threshold.
		minUsed := weeklyRecoveryMinPct
		guard := policy.Safeguard
		if guard.Enabled && pick.expiresAt != nil && pick.expiresAt.After(now) && pick.expiresAt.Sub(now) <= guard.LeadTimeDuration() {
			minUsed = guard.EffectiveMinUsedPercent()
		}
		q, ok := s.Get(p.ID)
		used, hasUsage := 0.0, false
		if ok && !q.Stale {
			used, hasUsage = windowUsagePct(q, ScopeWeekly)
		}
		if !hasUsage || used < minUsed {
			// The coupon exists but the guard refuses to spend it while weekly
			// quota remains. Report that distinctly (auto path only; the
			// manual Reset never runs this branch) instead of pretending the
			// inventory is empty.
			if !hasUsage {
				return out, fmt.Errorf("%w: weekly usage unavailable", ErrResetWeeklyUsageLow)
			}
			return out, fmt.Errorf("%w: %.1f%% used, requires at least %.1f%%", ErrResetWeeklyUsageLow, used, minUsed)
		}
	}
	out.Scope = pick.scope
	out.CouponExpiresAt = pick.expiresAt
	if dryRun {
		return out, nil
	}
	// Record the pending attempt before sending /use, so a restart cannot
	// turn an unknown outcome into a blind automatic retry.
	if err := s.resets.persistUncertain(p.ID, pick.scope); err != nil {
		return out, err
	}
	if err := useResetCoupon(ctx, s.client, zcodeBase(p), key, pick); err != nil {
		s.resets.noteAttempt(p.ID, pick.scope, s.now())
		s.resets.noteUncertain(p.ID, pick.scope)
		return out, err
	}
	out.Redeemed = true
	// Failure to remove the guard is safe: subsequent automatic attempts stop.
	_ = os.Remove(s.resets.guardPath(p.ID, pick.scope))
	s.resets.noteRedeem(p.ID, pick.scope, s.now())
	// Immediately refresh remote quota so the plan reflects the reset window.
	s.syncOne(ctx, p)
	return out, nil
}
