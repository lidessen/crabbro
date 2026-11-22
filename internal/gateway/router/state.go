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
	// unclassifiedReason is the only text stored or shown when a reason is
	// not one of the classifier's fixed summaries. No part of the input is
	// echoed: it may hold upstream bodies, credentials, or prompts.
	unclassifiedReason = "unclassified failure (details omitted)"
)

var (
	// classifiedReasons is the closed set of fixed summaries emitted by
	// internal/gateway/executor/classify.go. Anything not in this set and
	// not matching classifiedReasonPatterns is unclassified.
	classifiedReasons = map[string]struct{}{
		"rate limited (429)":                       {},
		"quota exhausted":                          {},
		"quota exhausted (429)":                    {},
		"payment or quota required (402)":          {},
		"upstream auth error (401)":                {},
		"upstream permission denied (403)":         {},
		"model or endpoint not found":              {},
		"model or endpoint not found (404)":        {},
		"model capability or endpoint unavailable": {},
		"context length exceeded":                  {},
		"upstream timeout":                         {},
		"transport error":                          {},
		"mid-stream drop":                          {},
		"upstream SSE error":                       {},
		"upstream SSE error (rate_limit)":          {},
		"upstream SSE error (quota_exhausted)":     {},
		"upstream SSE error (auth_error)":          {},
		"429 rate limited":                         {},
		"insufficient balance":                     {},
	}
	// classifiedReasonPatterns allows only the classifier's parameterized
	// status-code summaries; the digits are the sole variable part.
	classifiedReasonPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^upstream invalid request \(\d{3}\)$`),
		regexp.MustCompile(`^upstream rejected request \(\d{3}\)$`),
		regexp.MustCompile(`^upstream server error \(\d{3}\)$`),
	}
)

// SanitizeReason restricts an arbitrary failure reason to the fixed summaries
// the gateway classifier emits, so it is safe to store in health state and
// render in the UI. Whitespace and control characters are normalized first;
// an empty result stays empty. Any non-empty input outside the allowlist
// becomes unclassifiedReason, and no substring of the input is copied into
// the result, so upstream bodies, credentials, URLs, prompts, and emails
// cannot pass through.
func SanitizeReason(reason string) string {
	reason = normalizeReason(reason)
	if reason == "" {
		return ""
	}
	if _, ok := classifiedReasons[reason]; ok {
		return reason
	}
	for _, pattern := range classifiedReasonPatterns {
		if pattern.MatchString(reason) {
			return reason
		}
	}
	return unclassifiedReason
}

// normalizeReason collapses whitespace and control characters into single
// spaces and trims both ends.
func normalizeReason(reason string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, reason)), " ")
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
	keyCool       map[string]time.Time
	keyStep       map[string]int
	cursor        uint64
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
		st = &planState{modelCool: map[string]time.Time{}, keyCool: map[string]time.Time{}, keyStep: map[string]int{}}
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

// NextKeyBackoff advances the independent rate-limit backoff for a key.
func (h *HealthStore) NextKeyBackoff(planID, ref string) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.ensureLocked(planID)
	d := 15 * time.Second << st.keyStep[ref]
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	if st.keyStep[ref] < 5 {
		st.keyStep[ref]++
	}
	return d
}
func (h *HealthStore) ResetKeyBackoff(planID, ref string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.ensureLocked(planID)
	delete(st.keyStep, ref)
}
func (h *HealthStore) EnterKeyCooldown(planID, ref string, dur time.Duration, now time.Time) {
	if h == nil || ref == "" || dur <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.ensureLocked(planID)
	until := now.Add(dur)
	if until.After(st.keyCool[ref]) {
		st.keyCool[ref] = until
	}
}
func (h *HealthStore) IsKeyCooling(planID, ref string, now time.Time) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.plans[planID]
	return st != nil && now.Before(st.keyCool[ref])
}
func (h *HealthStore) NextKeyCursor(planID string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.ensureLocked(planID)
	n := st.cursor
	st.cursor++
	return n
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
		st.keyCool = map[string]time.Time{}
		st.keyStep = map[string]int{}
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
