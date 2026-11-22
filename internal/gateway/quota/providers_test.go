package quota

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
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
