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
	gatewayprovider "github.com/lidessen/crabbro/internal/gateway/provider"
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

// fetchFailure is the only provider error shape that may cross the quota
// boundary. It records a request stage class and HTTP status, never a URL,
// header, credential, or response body.
type fetchFailure struct {
	class  string // request | transport | http | parse
	status int
}

func (e *fetchFailure) Error() string {
	switch e.class {
	case "http":
		if e.status == http.StatusUnauthorized || e.status == http.StatusForbidden {
			return fmt.Sprintf("endpoint requires session auth or rejected credentials (HTTP %d)", e.status)
		}
		if e.status > 0 {
			return fmt.Sprintf("provider request failed (HTTP %d)", e.status)
		}
		return "provider request failed"
	case "transport":
		return "request failed"
	case "parse":
		return "invalid provider response"
	default:
		return "invalid endpoint"
	}
}

// fetchError is the sanitized, stage-attributed diagnostic stored in
// RemoteQuota.LastError when a sync attempt fails.
type fetchError struct {
	Stage  string // primary | fallback
	Kind   string // request | transport | http | parse
	Status int
}

func (e *fetchError) Error() string {
	stage := e.Stage
	if stage == "" {
		stage = "primary"
	}
	switch e.Kind {
	case "http":
		if e.Status > 0 {
			return fmt.Sprintf("%s: HTTP %d", stage, e.Status)
		}
		return stage + ": HTTP error"
	case "transport":
		return stage + ": transport error"
	case "parse":
		return stage + ": parse error"
	default:
		return stage + ": request error"
	}
}

// syncFetchError combines the primary and legacy fallback diagnostics so a
// stale minimax-cn snapshot says which stage failed and why.
type syncFetchError struct {
	Primary  *fetchError
	Fallback *fetchError
}

func (e *syncFetchError) Error() string {
	parts := make([]string, 0, 2)
	if e.Primary != nil {
		parts = append(parts, e.Primary.Error())
	}
	if e.Fallback != nil {
		parts = append(parts, e.Fallback.Error())
	}
	return "quota sync failed (" + strings.Join(parts, "; ") + ")"
}

func stageFetchError(stage string, err error) *fetchError {
	if err == nil {
		return nil
	}
	var ff *fetchFailure
	if errors.As(err, &ff) {
		return &fetchError{Stage: stage, Kind: ff.class, Status: ff.status}
	}
	return &fetchError{Stage: stage, Kind: "parse"}
}

func parseFetchError(stage string) *fetchError {
	return &fetchError{Stage: stage, Kind: "parse"}
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

// providerNoQuota lists plan-name fragments that must never resolve to a
// quota provider. "zenmux-jev" is a Jev-protocol relay whose key
// (zenmux_jev_api_key) has no Zenmux management scope: substring-matching it
// as "zenmux" would drive a guaranteed-403 request against the management
// subscription endpoint on every sync and mark the plan stale. Plans without
// a provider are skipped by the syncer instead.
var providerNoQuota = []string{"zenmux-jev"}

func provider(p *config.Plan) string {
	if auth := p.EffectiveAuth(); auth != nil && auth.Kind == gatewayprovider.OAuthSIWC {
		return ""
	}
	s := strings.ToLower(p.ID + " " + p.Credential)
	for _, n := range providerNoQuota {
		if strings.Contains(s, n) {
			return ""
		}
	}
	for _, n := range []string{"zcode", "commandcode", "vercel", "deepseek", "kimi-code-cn", "minimax-cn", "opencode-go", "zenmux", "xiaomi"} {
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

// xiaomiQuotaURL returns the Xiaomi MiMo Token Plan quota endpoint. The
// default is the CN token-plan origin; a plan that pins an openai-chat or
// openai-responses endpoint on another token-plan-*.xiaomimimo.com host
// (regional or private deployment) keeps that scheme and host.
func xiaomiQuotaURL(p *config.Plan) string {
	for _, proto := range []string{config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses} {
		ep := p.Endpoints[proto]
		if ep == nil {
			continue
		}
		u, err := url.Parse(ep.BaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())
		if !strings.HasPrefix(host, "token-plan-") || !strings.HasSuffix(host, ".xiaomimimo.com") {
			continue
		}
		return u.Scheme + "://" + u.Host + "/v1/platform/token_plan"
	}
	return "https://token-plan-cn.xiaomimimo.com/v1/platform/token_plan"
}
func fetchJSON(ctx context.Context, client *http.Client, endpoint, key string, bearer bool) (map[string]any, error) {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if e != nil {
		return nil, &fetchFailure{class: "request"}
	}
	if bearer {
		key = "Bearer " + key
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Accept", "application/json")
	resp, e := client.Do(req)
	if e != nil {
		return nil, &fetchFailure{class: "transport"}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &fetchFailure{class: "http", status: resp.StatusCode}
	}
	var m map[string]any
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if dec.Decode(&m) != nil || m == nil {
		return nil, &fetchFailure{class: "parse"}
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

// MiniMax token-plan endpoints (mirroring the opencode-minimax-auth
// reference): remains_percent is primary; older deployments only serve the
// legacy coding_plan document with count fields.
const (
	minimaxRemainsPercent = "https://www.minimaxi.com/backend/account/token_plan/remains_percent"
	minimaxLegacyRemains  = "https://www.minimaxi.com/v1/api/openplatform/coding_plan/remains"
)

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
	case "kimi-code-cn":
		endpoint = "https://api.kimi.com/coding/v1/usages"
	case "minimax-cn":
		return fetchMiniMax(ctx, c, key)
	case "opencode-go":
		endpoint = "https://opencode.ai/zen/go/v1/usage"
	case "zenmux":
		endpoint = "https://zenmux.ai/api/v1/management/subscription/detail"
	case "xiaomi":
		endpoint = xiaomiQuotaURL(p)
	default:
		return q, errors.New("unsupported provider")
	}
	if kind == "zcode" {
		endpoint += "/api/monitor/usage/quota/limit"
	}
	m, e := fetchJSON(ctx, c, endpoint, key, kind != "zcode")
	if e != nil {
		var ff *fetchFailure
		if kind == "zenmux" && errors.As(e, &ff) && ff.class == "http" && ff.status == http.StatusForbidden {
			// The management subscription endpoint answers a normal
			// (non-management) key with 403; name the scope gap so the
			// surfaced status says what actually failed.
			return q, errors.New("zenmux key lacks management scope (HTTP 403)")
		}
		return q, &syncFetchError{Primary: stageFetchError("primary", e)}
	}
	switch kind {
	case "zcode":
		d, err := envelope(m)
		if err != nil {
			return q, &syncFetchError{Primary: parseFetchError("primary")}
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
			return q, &syncFetchError{Primary: &fetchError{Stage: "primary", Kind: "request"}}
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
		// m["balance"] is the cash balance; the token pass is a separate
		// prepaid allowance. Both can coexist, and the token pass must not
		// be dropped just because a balance exists.
		if v, ok := number(m["balance"]); ok && v >= 0 && v < 1e12 {
			q.Balance = safe(fmt.Sprintf("$%.2f", v))
		}
		if tp := object(m["token_pass"]); tp != nil {
			w := QuotaWindow{Name: "Token Pass", ResetAt: timestamp(tp["resets_at"])}
			// Usage percentage, most exact first: explicit used/limit,
			// then limit-remaining, and when the payload reports only the
			// remaining credit, surface it as a sanitized note instead of
			// guessing a percentage or overwriting the cash Balance.
			if used, ok := number(tp["used"]); ok {
				w.UsagePct = percent(used, tp["limit"])
			}
			if w.UsagePct == nil {
				if limit, ok := number(tp["limit"]); ok && limit > 0 {
					if rem, ok2 := number(tp["remaining"]); ok2 && rem >= 0 && rem <= limit {
						w.UsagePct = percent(limit-rem, limit)
					}
				}
			}
			if w.UsagePct == nil {
				if rem, ok := number(tp["remaining"]); ok && rem >= 0 && rem < 1e12 {
					w.Note = safe(fmt.Sprintf("$%.2f remaining", rem))
				}
			}
			q.Windows = append(q.Windows, w)
			if q.ResetAt == nil {
				q.ResetAt = w.ResetAt
			}
		}
	case "kimi-code-cn":
		if x := object(m["usage"]); x != nil {
			q.Windows = append(q.Windows, QuotaWindow{Name: "Weekly", UsagePct: percent(x["used"], x["limit"]), ResetAt: timestamp(x["resetTime"])})
		}
		list, _ := m["limits"].([]any)
		for _, item := range list {
			x := object(item)
			window := object(x["window"])
			duration, _ := number(window["duration"])
			if duration == 300 && window["timeUnit"] == "TIME_UNIT_MINUTE" {
				d := object(x["detail"])
				used := d["used"]
				if used == nil {
					used = float64(0)
					if limit, ok := number(d["limit"]); ok {
						if remaining, ok := number(d["remaining"]); ok && remaining >= 0 && remaining <= limit {
							used = limit - remaining
						}
					}
				}
				q.Windows = append([]QuotaWindow{{Name: "5 hours", UsagePct: percent(used, d["limit"]), ResetAt: timestamp(d["resetTime"])}}, q.Windows...)
			}
		}
	case "opencode-go":
		u := object(m["usage"])
		for _, entry := range []struct{ key, name string }{{"rolling", "5 hours"}, {"weekly", "Weekly"}, {"monthly", "Monthly"}} {
			if x := object(u[entry.key]); x != nil {
				if n, ok := number(x["percent"]); ok && n >= 0 && n <= 100 {
					q.Windows = append(q.Windows, QuotaWindow{Name: entry.name, UsagePct: &n, ResetAt: timestamp(x["resetsAt"])})
				}
			}
		}
	case "zenmux":
		if m["success"] != true {
			return q, &syncFetchError{Primary: parseFetchError("primary")}
		}
		d := object(m["data"])
		if tier, ok := object(d["plan"])["tier"].(string); ok && !strings.Contains(tier, key) {
			q.PlanTier = safe(tier)
		}
		for _, entry := range []struct{ key, name string }{{"quota_5_hour", "5 hours"}, {"quota_7_day", "Weekly"}} {
			if x := object(d[entry.key]); x != nil {
				if pct := percent(x["used_flows"], x["max_flows"]); pct != nil {
					q.Windows = append(q.Windows, QuotaWindow{Name: entry.name, UsagePct: pct})
				}
			}
		}

	case "xiaomi":
		// Verified live shape: {"object":"token_plan","plan_code":"standard",
		// "period":{"start_at":...,"end_at":...},
		// "quota":{"total_credits":N,"used_credits":M,"remaining_credits":R},...}
		credits := object(m["quota"])
		total, okTotal := number(credits["total_credits"])
		used, okUsed := number(credits["used_credits"])
		if credits == nil || !okTotal || !okUsed || total <= 0 || used < 0 {
			return q, &syncFetchError{Primary: parseFetchError("primary")}
		}
		if code, ok := m["plan_code"].(string); ok && code != "" && (key == "" || !strings.Contains(code, key)) {
			q.PlanTier = safe(code)
		}
		pct := math.Min(100, math.Max(0, used/total*100))
		var resetAt *time.Time
		if period := object(m["period"]); period != nil {
			if s, ok := period["end_at"].(string); ok {
				resetAt = shanghaiStamp(s)
			}
		}
		q.Windows = append(q.Windows, QuotaWindow{Name: "Monthly", UsagePct: &pct, ResetAt: resetAt})

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
	if (kind == "kimi-code-cn" || kind == "opencode-go" || kind == "zenmux" || kind == "xiaomi") && len(q.Windows) == 0 {
		return q, &syncFetchError{Primary: parseFetchError("primary")}
	}
	if len(q.Windows) > 0 {
		q.UsagePct = q.Windows[0].UsagePct
		if q.ResetAt == nil {
			q.ResetAt = q.Windows[0].ResetAt
		}
	}
	return q, nil
}

// flexStamp parses provider timestamps that may be epoch milliseconds,
// RFC3339 strings, or naive "2006-01-02 15:04:05" wall-clock strings
// reported in Asia/Shanghai (the zone MiniMax reports without a designator).
func flexStamp(v any) *time.Time {
	if s, ok := v.(string); ok {
		if t := shanghaiStamp(s); t != nil {
			return t
		}
	}
	return timestamp(v)
}

// fetchMiniMax tries the remains_percent document first and the legacy
// coding_plan counts document second. Both stages retain their own sanitized
// diagnostic (stage, kind, HTTP status) so a stale minimax-cn snapshot says
// exactly which endpoint rejected the sync and whether it was transport,
// HTTP, or parse-level.
func fetchMiniMax(ctx context.Context, c *http.Client, key string) (RemoteQuota, error) {
	var q RemoteQuota
	primary, primaryErr := fetchJSON(ctx, c, minimaxRemainsPercent, key, true)
	if primaryErr == nil && parseMiniMaxQuota(&q, primary) > 0 {
		finalizeMiniMaxQuota(&q)
		return q, nil
	}
	legacy, legacyErr := fetchJSON(ctx, c, minimaxLegacyRemains, key, true)
	if legacyErr == nil && parseMiniMaxQuota(&q, legacy) > 0 {
		finalizeMiniMaxQuota(&q)
		return q, nil
	}
	primaryFailure := parseFetchError("primary")
	if primaryErr != nil {
		primaryFailure = stageFetchError("primary", primaryErr)
	}
	fallbackFailure := parseFetchError("fallback")
	if legacyErr != nil {
		fallbackFailure = stageFetchError("fallback", legacyErr)
	}
	return q, &syncFetchError{Primary: primaryFailure, Fallback: fallbackFailure}
}

func finalizeMiniMaxQuota(q *RemoteQuota) {
	if q == nil || len(q.Windows) == 0 {
		return
	}
	q.UsagePct = q.Windows[0].UsagePct
	if q.ResetAt == nil {
		q.ResetAt = q.Windows[0].ResetAt
	}
}

func miniMaxPercent(v any) (float64, bool) {
	if s, ok := v.(string); ok {
		v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	}
	return number(v)
}

// parseMiniMaxQuota reads one MiniMax token-plan document into quota windows
// and reports how many windows were parsed. Both endpoint shapes carry a
// model_remains[] row for the "general" plan; the remains_percent document
// reports current_interval_/current_weekly_ {used,total,remaining}_percent
// plus end_time/weekly_end_time reset stamps, while the legacy coding_plan
// document reports the same windows as total_count/usage_count pairs
// (usage_count being the still-remaining count). Each window prefers the
// percentage triple and falls back to the legacy counts.
func parseMiniMaxQuota(q *RemoteQuota, m map[string]any) int {
	rows, _ := m["model_remains"].([]any)
	parsed := 0
	for _, row := range rows {
		x := object(row)
		if x["model_name"] != "general" {
			continue
		}
		for _, entry := range []struct{ prefix, resetKey, name string }{
			{"current_interval", "end_time", "5 hours"},
			{"current_weekly", "weekly_end_time", "Weekly"},
		} {
			var pct *float64
			if v, ok := miniMaxPercent(x[entry.prefix+"_used_percent"]); ok && v >= 0 && v <= 100 {
				pct = &v
			}
			if pct == nil {
				total, okT := miniMaxPercent(x[entry.prefix+"_total_percent"])
				rem, okR := miniMaxPercent(x[entry.prefix+"_remaining_percent"])
				if okT && okR && total > 0 && rem >= 0 {
					if p := percent(total-rem, total); p != nil {
						pct = p
					}
				}
			}
			if pct == nil {
				if rem, ok := miniMaxPercent(x[entry.prefix+"_remaining_percent"]); ok && rem >= 0 && rem <= 100 {
					used := 100 - rem
					pct = &used
				}
			}
			if pct == nil {
				total, okT := number(x[entry.prefix+"_total_count"])
				remaining, okR := number(x[entry.prefix+"_usage_count"])
				if okT && okR && total > 0 && remaining >= 0 && remaining <= total {
					pct = percent(total-remaining, total)
				}
			}
			if pct == nil {
				continue
			}
			q.Windows = append(q.Windows, QuotaWindow{Name: entry.name, UsagePct: pct, ResetAt: flexStamp(x[entry.resetKey])})
			parsed++
		}
		break
	}
	return parsed
}
