package quota

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Reset window scopes accepted by the control API and autoReset policy.
const (
	ScopeFiveHour = "fiveHour"
	ScopeWeekly   = "weekly"

	// exhaustedWindowPct is the provider-reported usage percentage at or above
	// which a window counts as exhausted for opt-in auto redemption.
	exhaustedWindowPct = 99.0

	// grantTypeDirect is the only grant type BigModel reports today.
	grantTypeDirect = "DIRECT"

	// autoResetBudget bounds one automatic redemption cycle.
	autoResetBudget = 45 * time.Second
)

// Sentinel redemption errors. They are safe to surface: they carry no
// provider identifiers, secrets, or raw response bodies.
var (
	ErrResetInFlight    = errors.New("a reset is already in progress for this plan")
	ErrResetCooldown    = errors.New("reset minimum interval has not elapsed")
	ErrResetNoCoupons   = errors.New("no available reset coupons for the requested scope")
	ErrResetUnsupported = errors.New("plan does not support reset coupons")
	// ErrResetWeeklyUsageLow marks an automatic weekly redemption withheld by
	// the usage guard: a matching coupon exists, but the weekly window is not
	// consumed enough (or its usage is unknown) to justify spending it. It is
	// distinct from ErrResetNoCoupons so refusals are not mistaken for an
	// empty inventory.
	ErrResetWeeklyUsageLow = errors.New("weekly coupon withheld: weekly window usage is below the required minimum")
)

// resetEntry mirrors one row of the provider /list payload. recordId and
// grantType are provider-private: they exist only to build the /use request
// body and never leave this package (no snapshot, log, event, or API field).
type resetEntry struct {
	RecordID   json.Number `json:"recordId"`
	GrantType  string      `json:"grantType"`
	ExpiresAt  string      `json:"expiresAt"`
	ExpireTime string      `json:"expireTime"`
	Available  bool        `json:"available"`
}

type resetListEnvelope struct {
	Code    int   `json:"code"`
	Success *bool `json:"success"`
	Data    *struct {
		FiveHourResets []resetEntry `json:"fiveHourResets"`
		WeekResets     []resetEntry `json:"weekResets"`
	} `json:"data"`
}

type resetUseResponse struct {
	Code    int    `json:"code"`
	Success bool   `json:"success"`
	Data    any    `json:"data"`
	Message string `json:"msg"`
}

// resetCoupon is the internal view of one available coupon.
type resetCoupon struct {
	scope     string // "fiveHour" | "weekly"
	recordID  json.Number
	grantType string
	expiresAt *time.Time
}

// resetTypeForScope maps an internal scope onto the provider reset type.
func resetTypeForScope(scope string) string {
	if scope == ScopeWeekly {
		return "WEEK"
	}
	return "FIVE_HOUR"
}

// windowNameForScope maps an internal scope onto the sanitized window name
// reported by provider quota snapshots.
func windowNameForScope(scope string) string {
	if scope == ScopeWeekly {
		return "Weekly"
	}
	return "5 hours"
}

// couponExpiry normalizes one entry's expiry, preferring an explicit
// expiresAt and falling back to the naive Asia/Shanghai expireTime string.
func couponExpiry(e resetEntry) *time.Time {
	if exp := timestamp(e.ExpiresAt); exp != nil {
		return exp
	}
	return shanghaiStamp(e.ExpireTime)
}

// listResetCoupons performs a fresh read of the provider reset-coupon list.
func listResetCoupons(ctx context.Context, c *http.Client, base, key string) ([]resetCoupon, error) {
	u, err := url.Parse(base + "/api/biz/customer-package-reset/list")
	if err != nil || u.Host == "" {
		return nil, errors.New("invalid endpoint")
	}
	q := u.Query()
	q.Set("targetType", "PERSONAL")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("invalid endpoint")
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, errors.New("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("provider request failed")
	}
	var env resetListEnvelope
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil || env.Data == nil {
		return nil, errors.New("invalid provider response")
	}
	if env.Code != 0 && env.Code != 200 {
		return nil, errors.New("provider request failed")
	}
	var out []resetCoupon
	groups := []struct {
		scope   string
		entries []resetEntry
	}{
		{ScopeFiveHour, env.Data.FiveHourResets},
		{ScopeWeekly, env.Data.WeekResets},
	}
	for _, group := range groups {
		for _, e := range group.entries {
			if !e.Available {
				continue
			}
			grant := e.GrantType
			if grant == "" {
				grant = grantTypeDirect
			}
			out = append(out, resetCoupon{scope: group.scope, recordID: e.RecordID, grantType: grant, expiresAt: couponExpiry(e)})
		}
	}
	return out, nil
}

// useResetCoupon redeems exactly one coupon with a unique request id.
// Any transport or envelope ambiguity is returned as an error: callers fail
// closed (no blind retry) so a coupon can never be spent twice.
func useResetCoupon(ctx context.Context, c *http.Client, base, key string, cp resetCoupon) error {
	u, err := url.Parse(base + "/api/biz/customer-package-reset/use")
	if err != nil || u.Host == "" {
		return errors.New("invalid endpoint")
	}
	requestID, err := newRequestID()
	if err != nil {
		return errors.New("request id unavailable")
	}
	if cp.recordID == "" {
		return errors.New("coupon is missing its provider identifier")
	}
	body, err := json.Marshal(map[string]any{
		"targetType": "PERSONAL",
		"resetType":  resetTypeForScope(cp.scope),
		"recordId":   cp.recordID,
		"grantType":  cp.grantType,
		"requestId":  requestID,
	})
	if err != nil {
		return errors.New("invalid reset request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid endpoint")
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return errors.New("reset request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("reset rejected (status %d)", resp.StatusCode)
	}
	var out resetUseResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return errors.New("invalid reset response")
	}
	// Verified success requires the documented envelope. Anything else is
	// treated as not-redeemed so the normal cooling/failover path stays in
	// charge instead of guessing.
	if out.Code != 200 || !out.Success {
		return errors.New("reset rejected by provider")
	}
	return nil
}

// newRequestID returns a random RFC 4122-shaped identifier for one /use call.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}
