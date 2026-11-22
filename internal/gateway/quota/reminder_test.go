package quota

import (
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
