package quota

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
)

// fetchFrom serves payload on every path (404 when empty) and fetches one
// quota snapshot for plan through the test server.
func fetchFrom(t *testing.T, plan, payload string) (RemoteQuota, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if payload == "" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, payload)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	return fetch(context.Background(), &http.Client{Transport: rewriteTransport{u}}, &config.Plan{ID: plan}, "test-key")
}

// A Vercel payload with both a cash balance and a token pass must keep the
// cash balance in Balance and additionally surface the token pass as its own
// window: coexistence is the normal shape, not an either/or.
func TestVercelBalancePlusTokenPass(t *testing.T) {
	q, err := fetchFrom(t, "vercel", `{"balance":17.66,"token_pass":{"remaining":70.62,"resets_at":"2026-11-23T00:00:00Z"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if q.Balance != "$17.66" {
		t.Fatalf("Balance = %q, want the cash balance $17.66", q.Balance)
	}
	if len(q.Windows) != 1 {
		t.Fatalf("windows = %d, want 1 (Token Pass): %+v", len(q.Windows), q.Windows)
	}
	w := q.Windows[0]
	if w.Name != "Token Pass" {
		t.Fatalf("window name = %q, want Token Pass", w.Name)
	}
	if w.UsagePct != nil {
		t.Fatalf("remaining-only token pass must not guess a usage percentage, got %v", *w.UsagePct)
	}
	if w.Note != "$70.62 remaining" {
		t.Fatalf("note = %q, want $70.62 remaining", w.Note)
	}
	if w.ResetAt == nil || !w.ResetAt.Equal(time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("window ResetAt = %v, want 2026-11-23T00:00:00Z", w.ResetAt)
	}
	if q.ResetAt == nil || !q.ResetAt.Equal(*w.ResetAt) {
		t.Fatalf("snapshot ResetAt = %v, want the token pass reset", q.ResetAt)
	}
}

// When the token pass reports used/limit (or limit/remaining) the window
// carries a real usage percentage and no note.
func TestVercelTokenPassUsagePercentage(t *testing.T) {
	for _, tt := range []struct {
		name, payload string
		want          float64
	}{
		{"used+limit", `{"balance":17.66,"token_pass":{"limit":100,"used":30,"remaining":70,"resets_at":"2026-11-23T00:00:00Z"}}`, 30},
		{"limit-remaining", `{"balance":17.66,"token_pass":{"limit":100,"remaining":70.62,"resets_at":"2026-11-23T00:00:00Z"}}`, 29.38},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q, err := fetchFrom(t, "vercel", tt.payload)
			if err != nil {
				t.Fatal(err)
			}
			if q.Balance != "$17.66" {
				t.Fatalf("Balance = %q, want $17.66", q.Balance)
			}
			if len(q.Windows) != 1 || q.Windows[0].UsagePct == nil {
				t.Fatalf("windows = %+v, want one window with a usage percentage", q.Windows)
			}
			if math.Abs(*q.Windows[0].UsagePct-tt.want) > 0.01 {
				t.Fatalf("usagePct = %v, want %.2f", *q.Windows[0].UsagePct, tt.want)
			}
			if q.Windows[0].Note != "" {
				t.Fatalf("note = %q, want empty when a percentage is known", q.Windows[0].Note)
			}
		})
	}
}

// A balance-only payload keeps working exactly as before.
func TestVercelBalanceOnly(t *testing.T) {
	q, err := fetchFrom(t, "vercel", `{"balance":17.66}`)
	if err != nil {
		t.Fatal(err)
	}
	if q.Balance != "$17.66" || len(q.Windows) != 0 || q.ResetAt != nil {
		t.Fatalf("quota = %+v, want balance-only snapshot", q)
	}
}

// The MiniMax remains_percent document yields both windows with reset
// timestamps, preferring the percentage triple over legacy counts.
func TestMiniMaxRemainsPercent(t *testing.T) {
	q, err := fetchFrom(t, "minimax-cn", `{"model_remains":[{"model_name":"code","current_interval_used_percent":90},
		{"model_name":"general",
		 "current_interval_used_percent":40,"current_interval_total_percent":100,"current_interval_remaining_percent":60,
		 "current_weekly_total_percent":100,"current_weekly_remaining_percent":75,
		 "start_time":"2026-11-22 08:00:00","end_time":"2026-11-22 13:00:00",
		 "weekly_start_time":"2026-11-17 00:00:00","weekly_end_time":"2026-11-24 00:00:00"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Windows) != 2 {
		t.Fatalf("windows = %+v, want 5 hours + Weekly", q.Windows)
	}
	five, weekly := q.Windows[0], q.Windows[1]
	if five.Name != "5 hours" || five.UsagePct == nil || *five.UsagePct != 40 {
		t.Fatalf("5 hours window = %+v, want 40%% used", five)
	}
	// end_time is a naive Asia/Shanghai wall-clock string: 13:00 +08 == 05:00 UTC.
	if five.ResetAt == nil || !five.ResetAt.Equal(time.Date(2026, 11, 22, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("5 hours ResetAt = %v, want 2026-11-22T05:00:00Z", five.ResetAt)
	}
	if weekly.Name != "Weekly" || weekly.UsagePct == nil || *weekly.UsagePct != 25 {
		t.Fatalf("Weekly window = %+v, want 25%% used (total-remaining percentages)", weekly)
	}
	if weekly.ResetAt == nil || !weekly.ResetAt.Equal(time.Date(2026, 11, 23, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("Weekly ResetAt = %v, want 2026-11-23T16:00:00Z", weekly.ResetAt)
	}
}

func TestMiniMaxLiveResponseShapes(t *testing.T) {
	for _, tt := range []struct {
		name, payload string
		five, weekly  float64
	}{
		{"primary", `{"base_resp":{"status_code":0},"model_remains":[{"model_name":"general","current_interval_used_percent":"40.25%","current_interval_total_percent":"100%","current_weekly_used_percent":"25%","current_interval_total_count":0,"end_time":1795342800000}]}`, 40.25, 25},
		{"fallback", `{"base_resp":{"status_code":0},"model_remains":[{"model_name":"general","current_interval_total_count":0,"current_interval_usage_count":0,"current_interval_remaining_percent":60,"current_weekly_total_count":0,"current_weekly_usage_count":0,"current_weekly_remaining_percent":75,"end_time":1795342800000}]}`, 40, 25},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q, err := fetchFrom(t, "minimax-cn", tt.payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(q.Windows) != 2 || q.Windows[0].UsagePct == nil || q.Windows[1].UsagePct == nil || *q.Windows[0].UsagePct != tt.five || *q.Windows[1].UsagePct != tt.weekly {
				t.Fatalf("unexpected windows: %+v", q.Windows)
			}
			if q.Windows[0].ResetAt == nil || q.Windows[0].ResetAt.UnixMilli() != 1795342800000 {
				t.Fatalf("unexpected reset: %v", q.Windows[0].ResetAt)
			}
		})
	}
}

// Older MiniMax deployments only serve the legacy coding_plan counts
// document: the remains_percent request fails and the fallback endpoint is
// tried before giving up.
func TestMiniMaxLegacyFallbackEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend/account/token_plan/remains_percent":
			http.NotFound(w, r)
		case "/v1/api/openplatform/coding_plan/remains":
			fmt.Fprint(w, `{"model_remains":[{"model_name":"general",
				"current_interval_total_count":100,"current_interval_usage_count":75,
				"current_weekly_total_count":100,"current_weekly_usage_count":50,
				"end_time":"2026-11-22 13:00:00","weekly_end_time":"2026-11-24 00:00:00"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	q, err := fetch(context.Background(), &http.Client{Transport: rewriteTransport{u}}, &config.Plan{ID: "minimax-cn"}, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Windows) != 2 {
		t.Fatalf("windows = %+v, want 5 hours + Weekly from the legacy document", q.Windows)
	}
	// Legacy usage_count reports the remaining count: 100-75 used = 25%.
	if q.Windows[0].Name != "5 hours" || q.Windows[0].UsagePct == nil || *q.Windows[0].UsagePct != 25 {
		t.Fatalf("5 hours window = %+v, want 25%% used", q.Windows[0])
	}
	if q.Windows[0].ResetAt == nil || !q.Windows[0].ResetAt.Equal(time.Date(2026, 11, 22, 5, 0, 0, 0, time.UTC)) {
		t.Fatalf("5 hours ResetAt = %v, want the parsed end_time", q.Windows[0].ResetAt)
	}
	if q.Windows[1].Name != "Weekly" || q.Windows[1].UsagePct == nil || *q.Windows[1].UsagePct != 50 {
		t.Fatalf("Weekly window = %+v, want 50%% used", q.Windows[1])
	}
}

// zenmux-jev is a Jev-protocol relay, not a Zenmux management tenant: it must
// resolve to no provider so the syncer never fires a guaranteed-403 request
// against the management subscription endpoint, while a real zenmux plan
// still resolves.
func TestProviderSkipsZenmuxJev(t *testing.T) {
	if got := provider(&config.Plan{ID: "zenmux-jev", Credential: "zenmux_jev_api_key"}); got != "" {
		t.Fatalf("provider(zenmux-jev) = %q, want empty (no quota endpoint for the relay)", got)
	}
	if got := provider(&config.Plan{ID: "zenmux", Credential: "zenmux_management_key"}); got != "zenmux" {
		t.Fatalf("provider(zenmux) = %q, want zenmux", got)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(403)
	}))
	defer server.Close()
	store := secret.New(t.TempDir())
	if err := store.Set("zenmux_jev_api_key", "test-key"); err != nil {
		t.Fatal(err)
	}
	s := New(store, t.TempDir())
	u, _ := url.Parse(server.URL)
	s.client.Transport = rewriteTransport{u}
	s.Refresh(context.Background(), map[string]*config.Plan{
		"zenmux-jev": {ID: "zenmux-jev", Credential: "zenmux_jev_api_key"},
	})
	if requests != 0 {
		t.Fatalf("zenmux-jev issued %d requests, want 0 (no quota endpoint, plan skipped)", requests)
	}
	if _, ok := s.Get("zenmux-jev"); ok {
		t.Fatal("zenmux-jev must not produce a quota snapshot")
	}
}

// A real zenmux plan whose key lacks management scope gets a clear 403
// status instead of a generic failure, and the snapshot is marked stale.
func TestZenmuxManagementScopeRejectionSurfaces(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer server.Close()
	store := secret.New(t.TempDir())
	if err := store.Set("zenmux_key", "test-key"); err != nil {
		t.Fatal(err)
	}
	s := New(store, t.TempDir())
	u, _ := url.Parse(server.URL)
	s.client.Transport = rewriteTransport{u}
	s.Refresh(context.Background(), map[string]*config.Plan{
		"zenmux": {ID: "zenmux", Credential: "zenmux_key"},
	})
	q, ok := s.Get("zenmux")
	if !ok {
		t.Fatal("zenmux snapshot missing")
	}
	if !q.Stale {
		t.Fatal("rejected credentials must mark the snapshot stale")
	}
	if q.LastError != "zenmux key lacks management scope (HTTP 403)" {
		t.Fatalf("LastError = %q, want the management-scope 403 status", q.LastError)
	}
}

// Both MiniMax endpoints failing must preserve the stage, class, and HTTP
// status of each endpoint in the sanitized stale snapshot. Nothing from the
// URL, credential, or response body may cross the boundary.
func TestMiniMaxSyncPreservesBothEndpointDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend/account/token_plan/remains_percent":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<html>private@example.com secret-token https://host/?api_key=leaked</html>")
		case "/v1/api/openplatform/coding_plan/remains":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "<html>private@example.com secret-token</html>")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := secret.New(t.TempDir())
	if err := store.Set("minimax_key", "test-key"); err != nil {
		t.Fatal(err)
	}
	s := New(store, t.TempDir())
	u, _ := url.Parse(server.URL)
	s.client.Transport = rewriteTransport{u}
	s.Refresh(context.Background(), map[string]*config.Plan{
		"minimax-cn": {ID: "minimax-cn", Credential: "minimax_key"},
	})
	q, ok := s.Get("minimax-cn")
	if !ok || !q.Stale {
		t.Fatalf("stale minimax snapshot missing: %+v", q)
	}
	if q.LastError != "quota sync failed (primary: HTTP 404; fallback: HTTP 500)" {
		t.Fatalf("LastError = %q", q.LastError)
	}
	raw := q.LastError + " " + q.SyncSource
	for _, banned := range []string{"http://", "https://", server.URL, "test-key", "secret-token", "private@example.com", "api_key"} {
		if strings.Contains(raw, banned) {
			t.Fatalf("sanitized diagnostic leaked %q: %s", banned, raw)
		}
	}
}

// A successful HTTP status with an unusable body is a parse failure, and the
// legacy endpoint's own parse failure must remain visible.
func TestMiniMaxSyncPreservesParseFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html>not json secret-token</html>`)
	}))
	defer server.Close()
	store := secret.New(t.TempDir())
	if err := store.Set("minimax_key", "test-key"); err != nil {
		t.Fatal(err)
	}
	s := New(store, t.TempDir())
	u, _ := url.Parse(server.URL)
	s.client.Transport = rewriteTransport{u}
	s.Refresh(context.Background(), map[string]*config.Plan{
		"minimax-cn": {ID: "minimax-cn", Credential: "minimax_key"},
	})
	q, ok := s.Get("minimax-cn")
	if !ok || !q.Stale {
		t.Fatalf("stale minimax snapshot missing: %+v", q)
	}
	if q.LastError != "quota sync failed (primary: parse error; fallback: parse error)" {
		t.Fatalf("LastError = %q", q.LastError)
	}
	if strings.Contains(q.LastError, "secret-token") || strings.Contains(q.LastError, "html") {
		t.Fatalf("parse diagnostic leaked provider body: %s", q.LastError)
	}
}

// Mixed transport/HTTP diagnostics keep both stages distinct.
func TestMiniMaxSyncDistinguishesTransportAndHTTP(t *testing.T) {
	primary := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend/account/token_plan/remains_percent" {
			primary = true
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	store := secret.New(t.TempDir())
	if err := store.Set("minimax_key", "test-key"); err != nil {
		t.Fatal(err)
	}
	s := New(store, t.TempDir())
	u, _ := url.Parse(server.URL)
	s.client.Transport = rewriteTransport{u}
	// The legacy endpoint request fails at transport level.
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/api/openplatform/coding_plan/remains" {
			return nil, fmt.Errorf("dial tcp secret-token failed")
		}
		u2 := *r.URL
		u2.Scheme = u.Scheme
		u2.Host = u.Host
		r2 := r.Clone(r.Context())
		r2.URL = &u2
		return http.DefaultTransport.RoundTrip(r2)
	})
	s.Refresh(context.Background(), map[string]*config.Plan{
		"minimax-cn": {ID: "minimax-cn", Credential: "minimax_key"},
	})
	q, ok := s.Get("minimax-cn")
	if !ok || !q.Stale {
		t.Fatalf("stale minimax snapshot missing: %+v", q)
	}
	if !primary || q.LastError != "quota sync failed (primary: HTTP 503; fallback: transport error)" {
		t.Fatalf("primaryHit=%t LastError=%q", primary, q.LastError)
	}
	if strings.Contains(q.LastError, "secret-token") {
		t.Fatalf("transport diagnostic leaked error text: %s", q.LastError)
	}
}

// The verified Xiaomi MiMo Token Plan document reports the monthly credit
// allowance as integer credits plus an Asia/Shanghai period end stamp. The
// snapshot carries one Monthly window and the plan_code tier.
func TestXiaomiTokenPlan(t *testing.T) {
	method, path, auth, accept := "", "", "", ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		auth, accept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		fmt.Fprint(w, `{"object":"token_plan","plan_code":"standard",
			"period":{"start_at":"2026-11-07 08:23:26","end_at":"2026-12-07 23:59:59"},
			"quota":{"total_credits":11000000000,"used_credits":154062451,"remaining_credits":10845937549},
			"auto_renew":true,"claw_enabled":false}`)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	q, err := fetch(context.Background(), &http.Client{Transport: rewriteTransport{u}},
		&config.Plan{ID: "xiaomi", Credential: "xiaomi_api_key"}, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet {
		t.Fatalf("method = %q, want GET", method)
	}
	if path != "/v1/platform/token_plan" {
		t.Fatalf("path = %q, want /v1/platform/token_plan", path)
	}
	if auth != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want Bearer test-key", auth)
	}
	if accept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", accept)
	}
	if q.PlanTier != "standard" {
		t.Fatalf("PlanTier = %q, want standard", q.PlanTier)
	}
	if len(q.Windows) != 1 {
		t.Fatalf("windows = %+v, want one Monthly window", q.Windows)
	}
	w := q.Windows[0]
	if w.Name != "Monthly" {
		t.Fatalf("window name = %q, want Monthly", w.Name)
	}
	if w.UsagePct == nil || math.Abs(*w.UsagePct-1.4005677) > 0.0001 {
		t.Fatalf("usagePct = %v, want 154062451/11000000000*100 ≈ 1.40057", w.UsagePct)
	}
	// end_at is a naive Asia/Shanghai wall clock: 23:59:59 +08 == 15:59:59 UTC.
	if w.ResetAt == nil || !w.ResetAt.Equal(time.Date(2026, 12, 7, 15, 59, 59, 0, time.UTC)) {
		t.Fatalf("window ResetAt = %v, want 2026-12-07T15:59:59Z", w.ResetAt)
	}
	if q.UsagePct == nil || *q.UsagePct != *w.UsagePct {
		t.Fatalf("snapshot usagePct = %v, want the Monthly window percentage", q.UsagePct)
	}
	if q.ResetAt == nil || !q.ResetAt.Equal(*w.ResetAt) {
		t.Fatalf("snapshot ResetAt = %v, want the Monthly reset", q.ResetAt)
	}
}

// A token_plan 200 response with no plan_code still yields the Monthly window,
// and usage above the allowance clamps to 100%.
func TestXiaomiTokenPlanNoPlanCode(t *testing.T) {
	q, err := fetchFrom(t, "xiaomi", `{"quota":{"total_credits":100,"used_credits":150},
		"period":{"end_at":"2026-12-07 23:59:59"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if q.PlanTier != "" {
		t.Fatalf("PlanTier = %q, want empty when plan_code is absent", q.PlanTier)
	}
	if len(q.Windows) != 1 || q.Windows[0].UsagePct == nil || *q.Windows[0].UsagePct != 100 {
		t.Fatalf("windows = %+v, want one Monthly window clamped to 100%%", q.Windows)
	}
}

// A token_plan document without a usable monthly allowance (missing or
// non-positive total_credits) is a parse failure, not a fresh snapshot, and
// the non-empty-window guard keeps an unexpected 200 body from being stored.
func TestXiaomiTokenPlanRejectsBadTotal(t *testing.T) {
	for _, tt := range []struct{ name, payload string }{
		{"zero-total", `{"object":"token_plan","plan_code":"standard","quota":{"total_credits":0,"used_credits":10}}`},
		{"missing-total", `{"object":"token_plan","plan_code":"standard","quota":{"used_credits":10}}`},
		{"empty", `{}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q, err := fetchFrom(t, "xiaomi", tt.payload)
			if err == nil {
				t.Fatalf("quota = %+v, want an error for %s", q, tt.name)
			}
			if len(q.Windows) != 0 {
				t.Fatalf("windows = %+v, want none for %s", q.Windows, tt.name)
			}
			if !strings.Contains(err.Error(), "parse error") {
				t.Fatalf("error = %v, want a sanitized parse error", err)
			}
		})
	}
}

// A plan pinned to another token-plan-*.xiaomimimo.com host derives the quota
// endpoint from that scheme and host; anything else keeps the CN default.
func TestXiaomiQuotaURL(t *testing.T) {
	derived := &config.Plan{Endpoints: map[string]*config.EndpointSpec{
		config.ProtocolOpenAIChat: {BaseURL: "https://token-plan-sg.xiaomimimo.com/v1"},
	}}
	if got := xiaomiQuotaURL(derived); got != "https://token-plan-sg.xiaomimimo.com/v1/platform/token_plan" {
		t.Fatalf("derived endpoint = %q", got)
	}
	responses := &config.Plan{Endpoints: map[string]*config.EndpointSpec{
		config.ProtocolOpenAIResponses: {BaseURL: "http://token-plan-private.xiaomimimo.com/v1"},
	}}
	if got := xiaomiQuotaURL(responses); got != "http://token-plan-private.xiaomimimo.com/v1/platform/token_plan" {
		t.Fatalf("responses endpoint = %q", got)
	}
	for _, p := range []*config.Plan{
		{},
		{Endpoints: map[string]*config.EndpointSpec{config.ProtocolOpenAIChat: {BaseURL: "https://api.xiaomimimo.com/v1"}}},
		{Endpoints: map[string]*config.EndpointSpec{config.ProtocolOpenAIChat: {BaseURL: "https://token-plan-evil.example.com/v1"}}},
	} {
		if got := xiaomiQuotaURL(p); got != "https://token-plan-cn.xiaomimimo.com/v1/platform/token_plan" {
			t.Fatalf("endpoint = %q, want the CN default", got)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
