// Package router implements Crabbro's 4-stage multi-objective gateway router
// balancing prompt cache gravity, rolling quota urgency, and model quality.
package router

import (
	"container/list"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	defaultSessionCacheCap = 4096
	maxReasonRunes         = 120
	redactedReasonToken    = "[REDACTED]"
)

var (
	reasonBearerPattern   = regexp.MustCompile(`(?i)\bbearer\s+[^\s,;]+`)
	reasonAPIKeyPattern   = regexp.MustCompile(`(?i)\bsk-[a-z0-9_-]+`)
	reasonEmailPattern    = regexp.MustCompile(`(?i)\b[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}\b`)
	reasonKeyValuePattern = regexp.MustCompile(
		`(?i)\b(api[_-]?key|key|access[_-]?token|auth[_-]?token|token|secret|password|passwd|authorization)["']?\s*[=:]\s*["']?[^\s"'&,;]+["']?`)
)

// SanitizeReason turns an arbitrary failure reason into a single-line,
// bounded summary that is safe to store in health state and render in the UI.
// It strips control characters and newlines, redacts secret-like tokens and
// email addresses, collapses whitespace, and caps the result at 120 runes.
// Classification already emits deterministic summaries; this helper is the
// defense for direct callers that pass free-form text.
func SanitizeReason(reason string) string {
	// Truncation can cut inside a redaction marker or leave a bare key= tail;
	// repeating the pass until stable keeps stored reasons stable across
	// repeated Snapshot calls.
	for i := 0; i < 4; i++ {
		next := sanitizeReasonOnce(reason)
		if next == reason {
			return reason
		}
		reason = next
	}
	return reason
}

func sanitizeReasonOnce(reason string) string {
	reason = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, reason)), " ")
	reason = reasonBearerPattern.ReplaceAllString(reason, "Bearer "+redactedReasonToken)
	reason = reasonAPIKeyPattern.ReplaceAllString(reason, redactedReasonToken)
	reason = reasonEmailPattern.ReplaceAllString(reason, redactedReasonToken)
	reason = reasonKeyValuePattern.ReplaceAllString(reason, "$1="+redactedReasonToken)
	reason = strings.Join(strings.Fields(reason), " ")
	return truncateReason(reason, maxReasonRunes)
}

// truncateReason caps reason at limit runes, cutting on a word boundary when
// one is close enough to the cap and marking the cut with an ellipsis.
func truncateReason(reason string, limit int) string {
	runes := []rune(reason)
	if len(runes) <= limit {
		return reason
	}
	head := string(runes[:limit-3])
	if i := strings.LastIndex(head, " "); i > len(head)/2 {
		head = head[:i]
	}
	// Do not end on a half-written redaction marker.
	if i := strings.LastIndex(head, "["); i >= 0 && !strings.Contains(head[i:], "]") {
		head = strings.TrimRight(head[:i], " ")
	}
	return strings.TrimRight(head, " ") + "..."
}

// SessionCacheEntry tracks the last warm upstream target for a session.
type SessionCacheEntry struct {
	SessionKey       string
	LastPlan         string
	LastModel        string
	WarmPrefixTokens int64
	LastSuccessAt    time.Time
}

// SessionCacheStore is a concurrency-safe LRU of recent session placements.
type SessionCacheStore struct {
	cap     int
	mu      sync.Mutex
	ll      *list.List
	entries map[string]*list.Element
}

// NewSessionCacheStore creates a SessionCacheStore bounded to cap entries.
func NewSessionCacheStore(cap int) *SessionCacheStore {
	if cap <= 0 {
		cap = defaultSessionCacheCap
	}
	return &SessionCacheStore{
		cap:     cap,
		ll:      list.New(),
		entries: make(map[string]*list.Element, 128),
	}
}

// Get returns the warm entry for sessionKey if present.
func (s *SessionCacheStore) Get(sessionKey string) (SessionCacheEntry, bool) {
	if s == nil || sessionKey == "" {
		return SessionCacheEntry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	elem, ok := s.entries[sessionKey]
	if !ok {
		return SessionCacheEntry{}, false
	}
	s.ll.MoveToFront(elem)
	return *elem.Value.(*SessionCacheEntry), true
}

// Update records a successful exchange for sessionKey.
func (s *SessionCacheStore) Update(entry SessionCacheEntry) {
	if s == nil || entry.SessionKey == "" {
		return
	}
	if entry.LastSuccessAt.IsZero() {
		entry.LastSuccessAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if elem, ok := s.entries[entry.SessionKey]; ok {
		cur := elem.Value.(*SessionCacheEntry)
		*cur = entry
		s.ll.MoveToFront(elem)
		return
	}
	if s.ll.Len() >= s.cap {
		if back := s.ll.Back(); back != nil {
			old := back.Value.(*SessionCacheEntry)
			delete(s.entries, old.SessionKey)
			s.ll.Remove(back)
		}
	}
	cp := entry
	elem := s.ll.PushFront(&cp)
	s.entries[entry.SessionKey] = elem
}

// PlanHealthSnapshot summarizes live cooldown and EWMA health for one plan.
type PlanHealthSnapshot struct {
	Healthy       bool
	CooldownUntil *time.Time
	CooldownScope string
	LastError     string
	InFlight      int
	ErrorEWMA     float64
	LatencyEWMAMs float64
}

type planState struct {
	cooldownUntil time.Time
	cooldownScope string
	lastError     string
	cooldownStep  int
	inFlight      int
	errorEWMA     float64
	latencyEWMA   float64
	modelCool     map[string]time.Time
}

// HealthStore tracks active plan/model cooldowns, concurrency, and EWMA health.
type HealthStore struct {
	OnChange func(planID, state, reason string)

	mu    sync.Mutex
	plans map[string]*planState
}

// NewHealthStore creates an empty HealthStore.
func NewHealthStore() *HealthStore {
	return &HealthStore{plans: map[string]*planState{}}
}

func (h *HealthStore) ensureLocked(planID string) *planState {
	st, ok := h.plans[planID]
	if !ok {
		st = &planState{modelCool: map[string]time.Time{}}
		h.plans[planID] = st
	}
	return st
}

// NextRateLimitBackoff returns the exponential backoff duration (15s, 30s, 60s, max 5m)
// for a plan's consecutive rate-limit failures.
func (h *HealthStore) NextRateLimitBackoff(planID string) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.ensureLocked(planID)
	d := 15 * time.Second << st.cooldownStep
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	if st.cooldownStep < 5 {
		st.cooldownStep++
	}
	return d
}

// EnterPlanCooldown marks an entire plan unavailable until now+dur.
func (h *HealthStore) EnterPlanCooldown(planID, scope, reason string, dur time.Duration, now time.Time) {
	if h == nil || planID == "" || dur <= 0 {
		return
	}
	cleanReason := SanitizeReason(reason)
	h.mu.Lock()
	st := h.ensureLocked(planID)
	until := now.Add(dur)
	if until.After(st.cooldownUntil) {
		st.cooldownUntil = until
	}
	st.cooldownScope = scope
	st.lastError = cleanReason
	cb := h.OnChange
	h.mu.Unlock()

	if cb != nil {
		cb(planID, "cooling", cleanReason)
	}
}

// EnterModelCooldown marks only a specific (plan, model) pair unavailable.
func (h *HealthStore) EnterModelCooldown(planID, model, reason string, dur time.Duration, now time.Time) {
	if h == nil || planID == "" || model == "" || dur <= 0 {
		return
	}
	cleanReason := SanitizeReason(reason)
	h.mu.Lock()
	st := h.ensureLocked(planID)
	until := now.Add(dur)
	if until.After(st.modelCool[model]) {
		st.modelCool[model] = until
	}
	st.lastError = cleanReason
	h.mu.Unlock()
}

// IsPlanCooling reports whether planID is in active cooldown at now.
func (h *HealthStore) IsPlanCooling(planID string, now time.Time) (bool, time.Time, string) {
	if h == nil {
		return false, time.Time{}, ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.plans[planID]
	if !ok || st.cooldownUntil.IsZero() {
		return false, time.Time{}, ""
	}
	if now.Before(st.cooldownUntil) {
		return true, st.cooldownUntil, st.cooldownScope
	}
	return false, time.Time{}, ""
}

// IsModelCooling reports whether (planID, model) is in active model-level cooldown.
func (h *HealthStore) IsModelCooling(planID, model string, now time.Time) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.plans[planID]
	if !ok {
		return false
	}
	until, ok := st.modelCool[model]
	return ok && now.Before(until)
}

// ResetCooldown clears plan-level and model-level cooldowns for planID immediately.
func (h *HealthStore) ResetCooldown(planID string) {
	if h == nil || planID == "" {
		return
	}
	h.mu.Lock()
	st, ok := h.plans[planID]
	if ok {
		st.cooldownUntil = time.Time{}
		st.cooldownScope = ""
		st.cooldownStep = 0
		st.errorEWMA = 0
		st.latencyEWMA = 0
		st.modelCool = map[string]time.Time{}
	}
	cb := h.OnChange
	h.mu.Unlock()

	if cb != nil {
		cb(planID, "healthy", "manual reset")
	}
}

// AcquireInFlight increments and returns the active concurrency count for planID.
func (h *HealthStore) AcquireInFlight(planID string) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.ensureLocked(planID)
	st.inFlight++
	return st.inFlight
}

// ReleaseInFlight decrements the active concurrency count for planID.
func (h *HealthStore) ReleaseInFlight(planID string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.plans[planID]; ok && st.inFlight > 0 {
		st.inFlight--
	}
}

// InFlight returns the current concurrency count for planID.
func (h *HealthStore) InFlight(planID string) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if st, ok := h.plans[planID]; ok {
		return st.inFlight
	}
	return 0
}

// RecordOutcome updates EWMA error rate and latency for planID.
func (h *HealthStore) RecordOutcome(planID string, failed bool, latencyMs int64) {
	if h == nil || planID == "" {
		return
	}
	const alpha = 0.2
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.ensureLocked(planID)
	errSample := 0.0
	if failed {
		errSample = 1.0
	} else {
		st.cooldownStep = 0
	}
	st.errorEWMA = alpha*errSample + (1-alpha)*st.errorEWMA
	if latencyMs > 0 {
		if st.latencyEWMA == 0 {
			st.latencyEWMA = float64(latencyMs)
		} else {
			st.latencyEWMA = alpha*float64(latencyMs) + (1-alpha)*st.latencyEWMA
		}
	}
}

// Snapshot returns a redacted health view for planID at now.
func (h *HealthStore) Snapshot(planID string, now time.Time) PlanHealthSnapshot {
	snap := PlanHealthSnapshot{Healthy: true}
	if h == nil {
		return snap
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.plans[planID]
	if !ok {
		return snap
	}
	snap.InFlight = st.inFlight
	snap.ErrorEWMA = st.errorEWMA
	snap.LatencyEWMAMs = st.latencyEWMA
	snap.LastError = SanitizeReason(st.lastError)
	if !st.cooldownUntil.IsZero() && now.Before(st.cooldownUntil) {
		t := st.cooldownUntil
		snap.Healthy = false
		snap.CooldownUntil = &t
		snap.CooldownScope = st.cooldownScope
	}
	return snap
}
