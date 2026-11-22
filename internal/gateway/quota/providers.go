package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
)

var safeLabel = regexp.MustCompile(`^[A-Za-z0-9 $.,_+%-]{1,80}$`)
var tokenLabel = regexp.MustCompile(`(?i)[a-f0-9]{16,}|[a-z0-9_-]{24,}`)

func safe(s string) string {
	if !safeLabel.MatchString(s) || tokenLabel.MatchString(s) || strings.Contains(s, "@") || strings.Contains(s, "/Users/") {
		return ""
	}
	return s
}
func number(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case string:
		n, e := strconv.ParseFloat(x, 64)
		return n, e == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
	}
	return 0, false
}
func timestamp(v any) *time.Time {
	var t time.Time
	switch x := v.(type) {
	case float64:
		if x > 0 {
			t = time.UnixMilli(int64(x)).UTC()
		}
	case string:
		if n, e := strconv.ParseInt(x, 10, 64); e == nil && n > 0 {
			t = time.UnixMilli(n).UTC()
		} else {
			t, _ = time.Parse(time.RFC3339, x)
		}
	}
	if t.IsZero() {
		return nil
	}
	return &t
}

// shanghaiZone is the fixed +08:00 offset BigModel reports wall-clock times
// in ("2006-01-02 15:04:05" strings without a zone designator).
var shanghaiZone = time.FixedZone("Asia/Shanghai", 8*60*60)

// shanghaiStamp parses a naive "2006-01-02 15:04:05" wall-clock string
// reported in Asia/Shanghai local time and normalizes it to UTC.
func shanghaiStamp(v string) *time.Time {
	raw := strings.TrimSpace(v)
	if raw == "" {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", raw, shanghaiZone)
	if err != nil || t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func percent(used, cap any) *float64 {
	u, ok := number(used)
	c, ok2 := number(cap)
	if !ok || !ok2 || c <= 0 || u < 0 {
		return nil
	}
	n := math.Min(100, u/c*100)
	return &n
}
func provider(p *config.Plan) string {
	s := strings.ToLower(p.ID + " " + p.Credential)
	for _, n := range []string{"zcode", "commandcode", "vercel", "deepseek"} {
		if strings.Contains(s, n) {
			return n
		}
	}
	return ""
}

// zcodeBase returns the API origin used for BigModel quota and reset endpoints,
// honoring the first usable plan endpoint base URL override.
func zcodeBase(p *config.Plan) string {
	endpoint := "https://open.bigmodel.cn"
	for _, ep := range p.Endpoints {
		if ep != nil {
			if u, e := url.Parse(ep.BaseURL); e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
				endpoint = u.Scheme + "://" + u.Host
				break
			}
		}
	}
	return endpoint
}
func fetchJSON(ctx context.Context, client *http.Client, endpoint, key string, bearer bool) (map[string]any, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if e != nil {
		return nil, errors.New("invalid endpoint")
	}
	if bearer {
		key = "Bearer " + key
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Accept", "application/json")
	resp, e := client.Do(req)
	if e != nil {
		return nil, errors.New("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New("provider request failed")
	}
	var m map[string]any
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if dec.Decode(&m) != nil || m == nil {
		return nil, errors.New("invalid provider response")
	}
	return m, nil
}
func envelope(m map[string]any) (map[string]any, error) {
	n, ok := number(m["code"])
	if !ok || (n != 0 && n != 200) {
		return nil, errors.New("provider request failed")
	}
	d := object(m["data"])
	if d == nil {
		return nil, errors.New("invalid provider response")
	}
	return d, nil
}
func fetch(ctx context.Context, c *http.Client, p *config.Plan, key string) (RemoteQuota, error) {
	var q RemoteQuota
	kind := provider(p)
	var endpoint string
	switch kind {
	case "zcode":
		endpoint = zcodeBase(p)
	case "commandcode":
		endpoint = "https://api.commandcode.ai/alpha/billing/credits"
	case "vercel":
		endpoint = "https://ai-gateway.vercel.sh/v1/credits"
	case "deepseek":
		endpoint = "https://api.deepseek.com/user/balance"
	default:
		return q, errors.New("unsupported provider")
	}
	if kind == "zcode" {
		endpoint += "/api/monitor/usage/quota/limit"
	}
	m, e := fetchJSON(ctx, c, endpoint, key, kind != "zcode")
	if e != nil {
		return q, e
	}
	switch kind {
	case "zcode":
		d, err := envelope(m)
		if err != nil {
			return q, err
		}
		if s, ok := d["level"].(string); ok {
			q.PlanTier = safe(s)
			if key != "" && strings.Contains(q.PlanTier, key) {
				q.PlanTier = ""
			}
		}
		list, _ := d["limits"].([]any)
		for _, item := range list {
			w := object(item)
			unit, _ := number(w["unit"])
			name := ""
			if unit == 3 {
				name = "5 hours"
			}
			if unit == 6 {
				name = "Weekly"
			}
			if name == "" {
				continue
			}
			var pct *float64
			if u, ok := number(w["usage"]); ok && u > 0 {
				if rem, ok := number(w["remaining"]); ok {
					pct = percent(math.Max(0, u-rem), u)
				}
			}
			if pct == nil {
				if v, ok := number(w["percentage"]); ok && v >= 0 && v <= 100 {
					pct = &v
				}
			}
			q.Windows = append(q.Windows, QuotaWindow{Name: name, UsagePct: pct, ResetAt: timestamp(w["nextResetTime"])})
		}
		coupons, err := listResetCoupons(ctx, c, zcodeBase(p), key)
		if err != nil {
			return q, err
		}
		now := time.Now()
		for _, cp := range coupons {
			if cp.expiresAt != nil && !cp.expiresAt.After(now) {
				continue
			}
			q.Resets = append(q.Resets, ResetCoupon{Scope: cp.scope, ExpiresAt: cp.expiresAt})
			q.ResetsRemaining++
			if cp.expiresAt != nil && (q.SoonestResetExpiry == nil || cp.expiresAt.Before(*q.SoonestResetExpiry)) {
				q.SoonestResetExpiry = cp.expiresAt
			}
		}
	case "commandcode":
		c := object(m["data"])
		if c == nil {
			c = m
		}
		limits := object(c["windowLimits"])
		for _, entry := range []struct{ k, n string }{{"fiveHour", "5 hours"}, {"weekly", "Weekly"}} {
			if x := object(limits[entry.k]); x != nil {
				q.Windows = append(q.Windows, QuotaWindow{Name: entry.n, UsagePct: percent(x["used"], x["cap"]), ResetAt: timestamp(x["resetAt"])})
			}
		}
		credits := object(c["credits"])
		if credits == nil {
			credits = c
		}
		total, found := number(credits["totalBalance"])
		found = found && total >= 0 && total < 1e12
		if !found {
			total = 0
			for _, k := range []string{"monthlyCredits", "extraUsageBalance", "purchasedCredits", "freeCredits"} {
				if n, ok := number(credits[k]); ok && n >= 0 && n < 1e12 {
					total += n
					found = true
				}
			}
		}
		if found {
			q.Balance = safe(fmt.Sprintf("%.2f credits", total))
		}
	case "vercel":
		v, ok := number(m["balance"])
		tp := object(m["token_pass"])
		if !ok {
			v, ok = number(tp["remaining"])
		}
		if ok && v >= 0 && v < 1e12 {
			q.Balance = safe(fmt.Sprintf("$%.2f", v))
		}
		q.ResetAt = timestamp(tp["resets_at"])
	case "deepseek":
		items, _ := m["balance_infos"].([]any)
		if len(items) > 0 {
			x := object(items[0])
			currency, _ := x["currency"].(string)
			if !regexp.MustCompile(`^[A-Z]{3}$`).MatchString(currency) {
				currency = ""
			}
			if n, ok := number(x["total_balance"]); ok && currency != "" && n >= 0 && n < 1e12 {
				q.Balance = safe(fmt.Sprintf("%s %.2f", currency, n))
			}
		}
	}
	if len(q.Windows) > 0 {
		q.UsagePct = q.Windows[0].UsagePct
		if q.ResetAt == nil {
			q.ResetAt = q.Windows[0].ResetAt
		}
	}
	return q, nil
}
