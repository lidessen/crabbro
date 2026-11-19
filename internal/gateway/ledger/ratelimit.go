package ledger

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// HeaderSnapshot holds Tier-1 upstream rate-limit header observations for one plan.
type HeaderSnapshot struct {
	UpdatedAt         time.Time
	RequestsLimit     int64
	RequestsRemaining int64
	RequestsResetAt   time.Time
	HasRequests       bool
	TokensLimit       int64
	TokensRemaining   int64
	TokensResetAt     time.Time
	HasTokens         bool
}

type aimdState struct {
	factor float64
	last429 time.Time
}

// Calibrator maintains Tier-1 upstream rate-limit headers and Tier-3 reactive AIMD throttling.
type Calibrator struct {
	mu      sync.Mutex
	headers map[string]HeaderSnapshot
	aimd    map[string]aimdState
}

// NewCalibrator initializes an empty Calibrator.
func NewCalibrator() *Calibrator {
	return &Calibrator{
		headers: map[string]HeaderSnapshot{},
		aimd:    map[string]aimdState{},
	}
}

// UpdateHeaders parses Anthropic and OpenAI-style ratelimit headers from an upstream response.
func (c *Calibrator) UpdateHeaders(planID string, h http.Header, now time.Time) {
	if c == nil || planID == "" || len(h) == 0 {
		return
	}
	var snap HeaderSnapshot
	snap.UpdatedAt = now

	// Requests limit/remaining/reset
	if lim, rem, ok := parseLimitPair(h, "anthropic-ratelimit-requests-limit", "anthropic-ratelimit-requests-remaining"); ok {
		snap.HasRequests = true
		snap.RequestsLimit = lim
		snap.RequestsRemaining = rem
		snap.RequestsResetAt = parseResetTime(h.Get("anthropic-ratelimit-requests-reset"), now)
	} else if lim, rem, ok := parseLimitPair(h, "x-ratelimit-limit-requests", "x-ratelimit-remaining-requests"); ok {
		snap.HasRequests = true
		snap.RequestsLimit = lim
		snap.RequestsRemaining = rem
		snap.RequestsResetAt = parseResetTime(h.Get("x-ratelimit-reset-requests"), now)
	}

	// Tokens limit/remaining/reset
	if lim, rem, ok := parseLimitPair(h, "anthropic-ratelimit-tokens-limit", "anthropic-ratelimit-tokens-remaining"); ok {
		snap.HasTokens = true
		snap.TokensLimit = lim
		snap.TokensRemaining = rem
		snap.TokensResetAt = parseResetTime(h.Get("anthropic-ratelimit-tokens-reset"), now)
	} else if lim, rem, ok := parseLimitPair(h, "x-ratelimit-limit-tokens", "x-ratelimit-remaining-tokens"); ok {
		snap.HasTokens = true
		snap.TokensLimit = lim
		snap.TokensRemaining = rem
		snap.TokensResetAt = parseResetTime(h.Get("x-ratelimit-reset-tokens"), now)
	}

	if !snap.HasRequests && !snap.HasTokens {
		return
	}
	c.mu.Lock()
	c.headers[planID] = snap
	c.mu.Unlock()
}

// HeaderQuota returns the latest non-expired upstream rate-limit header snapshot for planID.
func (c *Calibrator) HeaderQuota(planID string, now time.Time) (HeaderSnapshot, bool) {
	if c == nil {
		return HeaderSnapshot{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snap, ok := c.headers[planID]
	if !ok {
		return HeaderSnapshot{}, false
	}
	// Expire header observations if their reset window passed or they are older than 10 minutes.
	if now.Sub(snap.UpdatedAt) > 10*time.Minute {
		return HeaderSnapshot{}, false
	}
	if snap.HasTokens && !snap.TokensResetAt.IsZero() && now.After(snap.TokensResetAt) {
		snap.HasTokens = false
	}
	if snap.HasRequests && !snap.RequestsResetAt.IsZero() && now.After(snap.RequestsResetAt) {
		snap.HasRequests = false
	}
	if !snap.HasTokens && !snap.HasRequests {
		return HeaderSnapshot{}, false
	}
	return snap, true
}

// NoteRateLimit backs off the Tier-3 reactive capacity factor for planID on 429/quota errors.
func (c *Calibrator) NoteRateLimit(planID string, now time.Time) {
	if c == nil || planID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.aimd[planID]
	if !ok {
		st.factor = 1.0
	}
	st.factor *= 0.5
	if st.factor < 0.1 {
		st.factor = 0.1
	}
	st.last429 = now
	c.aimd[planID] = st
}

// NoteSuccess linearly recovers the Tier-3 capacity factor toward 1.0.
func (c *Calibrator) NoteSuccess(planID string) {
	if c == nil || planID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.aimd[planID]
	if !ok {
		return
	}
	st.factor += 0.1
	if st.factor >= 1.0 {
		delete(c.aimd, planID)
		return
	}
	c.aimd[planID] = st
}

// EffectiveCapFactor returns a multiplier in [0.1, 1.0] reflecting recent 429 backpressure.
func (c *Calibrator) EffectiveCapFactor(planID string, now time.Time) float64 {
	if c == nil {
		return 1.0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.aimd[planID]
	if !ok {
		return 1.0
	}
	if !st.last429.IsZero() && now.Sub(st.last429) > 15*time.Minute {
		delete(c.aimd, planID)
		return 1.0
	}
	return st.factor
}

func parseLimitPair(h http.Header, limitHeader, remHeader string) (int64, int64, bool) {
	ls := strings.TrimSpace(h.Get(limitHeader))
	rs := strings.TrimSpace(h.Get(remHeader))
	if ls == "" || rs == "" {
		return 0, 0, false
	}
	lim, err1 := strconv.ParseInt(ls, 10, 64)
	rem, err2 := strconv.ParseInt(rs, 10, 64)
	if err1 != nil || err2 != nil || lim <= 0 {
		return 0, 0, false
	}
	if rem < 0 {
		rem = 0
	}
	return lim, rem, true
}

func parseResetTime(raw string, now time.Time) time.Time {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(d)
	}
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
		if sec > 1_000_000_000 {
			return time.Unix(sec, 0)
		}
		return now.Add(time.Duration(sec) * time.Second)
	}
	return time.Time{}
}
