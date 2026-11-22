package quota

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
)

func TestExpiryReminderDedupesPerCoupon(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	s := New(nil, t.TempDir())
	clock := now
	s.SetClock(func() time.Time { return clock })
	pct := func(v float64) *float64 { return &v }
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	var got []QuotaReminder
	s.OnExpiryReminder = func(r QuotaReminder) { got = append(got, r) }

	fiveExpiry, weekExpiry := at(20*time.Minute), at(3*time.Hour)
	q := &RemoteQuota{
		Windows:         []QuotaWindow{{Name: "5 hours", UsagePct: pct(10)}},
		Resets:          []ResetCoupon{{Scope: ScopeFiveHour, ExpiresAt: fiveExpiry}, {Scope: ScopeWeekly, ExpiresAt: weekExpiry}},
		ResetsRemaining: 2,
	}
	// Only the five-hour coupon is inside the 30m lead time.
	if fresh := s.checkExpiryReminders("p", q, now, 30*time.Minute); len(fresh) != 1 || fresh[0].Scope != ScopeFiveHour || fresh[0].Kind != "expiring_coupon" {
		t.Fatalf("first reminders = %+v", fresh)
	}
	// The same coupon is never emitted twice.
	if fresh := s.checkExpiryReminders("p", q, now, 30*time.Minute); len(fresh) != 0 {
		t.Fatalf("duplicate reminders = %+v", fresh)
	}
	clock = now.Add(10 * time.Minute)
	if fresh := s.checkExpiryReminders("p", q, clock, 30*time.Minute); len(fresh) != 0 {
		t.Fatalf("same coupon re-emitted as it aged = %+v", fresh)
	}
	// A different coupon expiry is a distinct coupon observation.
	otherFive := at(25 * time.Minute)
	q2 := &RemoteQuota{
		Windows:         q.Windows,
		Resets:          []ResetCoupon{{Scope: ScopeFiveHour, ExpiresAt: otherFive}, {Scope: ScopeWeekly, ExpiresAt: weekExpiry}},
		ResetsRemaining: 2,
	}
	if fresh := s.checkExpiryReminders("p", q2, clock, 30*time.Minute); len(fresh) != 1 || !fresh[0].ExpiresAt.Equal(*otherFive) {
		t.Fatalf("replacement coupon reminders = %+v", fresh)
	}
	if len(got) != 2 {
		t.Fatalf("callback count = %d (%+v)", len(got), got)
	}
}

func TestGetRemindersUseConfiguredLeadTime(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	s := New(nil, t.TempDir())
	s.SetClock(func() time.Time { return now })
	expiry := now.Add(30 * time.Minute)
	reset := now.Add(90 * time.Minute)
	usage := 10.0
	s.mu.Lock()
	s.values["p"] = RemoteQuota{
		Windows:            []QuotaWindow{{Name: "Weekly", UsagePct: &usage, ResetAt: &reset}},
		Resets:             []ResetCoupon{{Scope: ScopeFiveHour, ExpiresAt: &expiry}},
		ResetsRemaining:    1,
		SoonestResetExpiry: &expiry,
	}
	s.leads["p"] = 2 * time.Hour
	s.mu.Unlock()

	q, ok := s.Get("p")
	if !ok || len(q.Reminders) != 2 {
		t.Fatalf("reminders with 2h lead = %+v", q.Reminders)
	}
	if q.Reminders[0].Kind != "expiring_coupon" || q.Reminders[0].Scope != ScopeFiveHour {
		t.Fatalf("coupon reminder = %+v", q.Reminders[0])
	}

	s.mu.Lock()
	s.leads["p"] = time.Hour
	s.mu.Unlock()
	q, _ = s.Get("p")
	if len(q.Reminders) != 1 || q.Reminders[0].Kind != "expiring_coupon" {
		t.Fatalf("reminders with 1h lead = %+v", q.Reminders)
	}

	s.mu.Lock()
	s.leads["p"] = 10 * time.Minute
	s.mu.Unlock()
	q, _ = s.Get("p")
	if len(q.Reminders) != 0 {
		t.Fatalf("reminders with 10m lead = %+v", q.Reminders)
	}
}

func TestReminderLeadDefaultsWhenUnset(t *testing.T) {
	s := New(nil, t.TempDir())
	if got := s.reminderLead("missing"); got != config.DefaultQuotaReminderLeadTime {
		t.Fatalf("default lead = %v", got)
	}
}

// TestUnderutilizedWindowReminderEmittedOnce proves the event path reaches
// underutilized_window reminders: syncPlan must derive the same windowTrend
// Get computes (providers never populate Trend), emitting exactly one event
// for an underutilized weekly window and none on a repeat sync.
func TestUnderutilizedWindowReminderEmittedOnce(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	// Weekly window resetting in 30h: ~82% elapsed with only 20% used
	// projects far below 60%, i.e. underutilized, and 30h sits inside the
	// configured 48h reminder lead.
	resetAt := now.Add(30 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/monitor/usage/quota/limit":
			fmt.Fprintf(w, `{"code":0,"data":{"limits":[{"unit":6,"usage":100,"remaining":80,"nextResetTime":%q}]}}`, resetAt.UTC().Format(time.RFC3339))
		case "/api/biz/customer-package-reset/list":
			fmt.Fprint(w, `{"code":200,"data":{}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	s := newTestSyncer(t)
	s.SetClock(func() time.Time { return now })
	var mu sync.Mutex
	var got []QuotaReminder
	s.OnExpiryReminder = func(r QuotaReminder) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r)
	}
	p := &config.Plan{
		ID:            "zcode",
		Credential:    "zcode_key",
		QuotaReminder: config.QuotaReminderSpec{LeadTime: "48h"},
		Endpoints:     map[string]*config.EndpointSpec{"anthropic": {BaseURL: server.URL}},
	}

	s.syncOne(context.Background(), p)
	mu.Lock()
	if len(got) != 1 || got[0].Kind != "underutilized_window" || got[0].Scope != "Weekly" {
		mu.Unlock()
		t.Fatalf("events = %+v, want exactly one underutilized_window for Weekly", got)
	}
	if !got[0].ExpiresAt.Equal(resetAt) {
		mu.Unlock()
		t.Fatalf("event expiry = %v, want %v", got[0].ExpiresAt, resetAt)
	}
	mu.Unlock()

	// A repeat sync of identical evidence is deduped: still exactly one event.
	s.syncOne(context.Background(), p)
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("repeat sync re-emitted the reminder: %+v", got)
	}
}
