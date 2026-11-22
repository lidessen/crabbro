package quota

import (
	"testing"
	"time"
)

func TestWeeklyFallbackAndReminders(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := New(nil, t.TempDir())
	s.SetClock(func() time.Time { return now })
	five, weekly := 100.0, 76.0
	expiry, reset := now.Add(4*time.Hour), now.Add(20*time.Hour)
	s.values["p"] = RemoteQuota{Windows: []QuotaWindow{{Name: "5 hours", UsagePct: &five}, {Name: "Weekly", UsagePct: &weekly, ResetAt: &reset}}, Resets: []ResetCoupon{{Scope: "weekly", ExpiresAt: &expiry}}, ResetsRemaining: 1, SoonestResetExpiry: &expiry}
	if s.autoResetEligible("p", "weekly", now) {
		t.Fatal("weekly coupon spent prematurely")
	}
	// An expiring weekly coupon is not a licence to reset a five-hour window
	// while the weekly window still has plenty of quota left.
	expiry = now.Add(2 * time.Hour)
	if s.autoResetEligible("p", "weekly", now) {
		t.Fatal("weekly coupon spent while weekly quota remained")
	}
	// Only heavy weekly consumption makes the fallback worthwhile.
	expiry = now.Add(4 * time.Hour)
	weekly = 90
	if !s.autoResetEligible("p", "weekly", now) {
		t.Fatal("high weekly usage should be eligible")
	}
	weekly = 20
	q, _ := s.Get("p")
	if len(q.Reminders) != 2 || q.Reminders[0].Kind != "expiring_coupon" || q.Reminders[1].Kind != "underutilized_window" {
		t.Fatalf("reminders = %+v", q.Reminders)
	}
}
