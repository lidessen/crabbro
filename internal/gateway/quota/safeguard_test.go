package quota

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
)

func TestPreExpirySafeguardIsScopeLocal(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	s := New(nil, t.TempDir())
	s.SetClock(func() time.Time { return now })
	pct := func(v float64) *float64 { return &v }
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	spec := config.SafeguardSpec{Enabled: true, LeadTime: "30m", MinUsedPct: 80}

	seed := func(q RemoteQuota) {
		s.mu.Lock()
		s.values["p"] = q
		s.mu.Unlock()
	}

	// Five-hour usage is high and its coupon expires inside the lead time.
	seed(RemoteQuota{
		Windows: []QuotaWindow{{Name: "5 hours", UsagePct: pct(85)}, {Name: "Weekly", UsagePct: pct(20)}},
		Resets: []ResetCoupon{
			{Scope: ScopeFiveHour, ExpiresAt: at(20 * time.Minute)},
			{Scope: ScopeWeekly, ExpiresAt: at(20 * time.Minute)},
		},
	})
	if !s.preExpirySafeguardEligible("p", ScopeFiveHour, spec, now) {
		t.Fatal("high five-hour usage should trigger the safeguard")
	}
	// Same expiring coupon set, but the weekly window is nowhere near
	// exhausted: spending a weekly coupon to reset five-hour is wasteful.
	if s.preExpirySafeguardEligible("p", ScopeWeekly, spec, now) {
		t.Fatal("weekly coupon would be spent while weekly quota remained")
	}

	// Weekly quota itself is nearly consumed: the safeguard is worthwhile.
	seed(RemoteQuota{
		Windows: []QuotaWindow{{Name: "5 hours", UsagePct: pct(85)}, {Name: "Weekly", UsagePct: pct(85)}},
		Resets:  []ResetCoupon{{Scope: ScopeWeekly, ExpiresAt: at(20 * time.Minute)}},
	})
	if !s.preExpirySafeguardEligible("p", ScopeWeekly, spec, now) {
		t.Fatal("high weekly usage should allow the safeguard")
	}

	// Outside the lead time, below the usage threshold, and stale data all
	// fail closed.
	seed(RemoteQuota{
		Windows: []QuotaWindow{{Name: "5 hours", UsagePct: pct(85)}},
		Resets:  []ResetCoupon{{Scope: ScopeFiveHour, ExpiresAt: at(45 * time.Minute)}},
	})
	if s.preExpirySafeguardEligible("p", ScopeFiveHour, spec, now) {
		t.Fatal("coupon outside the lead time triggered the safeguard")
	}
	seed(RemoteQuota{
		Windows: []QuotaWindow{{Name: "5 hours", UsagePct: pct(50)}},
		Resets:  []ResetCoupon{{Scope: ScopeFiveHour, ExpiresAt: at(20 * time.Minute)}},
	})
	if s.preExpirySafeguardEligible("p", ScopeFiveHour, spec, now) {
		t.Fatal("usage below the threshold triggered the safeguard")
	}
	seed(RemoteQuota{
		Windows: []QuotaWindow{{Name: "5 hours", UsagePct: pct(95)}},
		Resets:  []ResetCoupon{{Scope: ScopeFiveHour, ExpiresAt: at(20 * time.Minute)}},
		Stale:   true,
	})
	if s.preExpirySafeguardEligible("p", ScopeFiveHour, spec, now) {
		t.Fatal("stale quota triggered the safeguard")
	}
	if s.preExpirySafeguardEligible("p", ScopeFiveHour, config.SafeguardSpec{}, now) {
		t.Fatal("disabled safeguard triggered")
	}
}

func TestPreExpirySafeguardEndToEnd(t *testing.T) {
	now := time.Now()
	policy := config.AutoResetSpec{
		Enabled:   true,
		Scopes:    []string{ScopeWeekly},
		Safeguard: config.SafeguardSpec{Enabled: true, LeadTime: "30m", MinUsedPct: 80},
	}
	pct := func(v float64) *float64 { return &v }
	expiry := now.Add(21 * time.Minute) // inside the 30m lead
	at := func() *time.Time { return &expiry }

	t.Run("weekly coupon stays put while weekly quota remains", func(t *testing.T) {
		mock := newBigModelMock(t)
		mock.coupons = fmt.Sprintf(`{"code":200,"success":true,"data":{"weekResets":[{"recordId":"99001","grantType":"DIRECT","expireTime":%q,"available":true}]}}`, expiry.In(shanghaiZone).Format("2006-01-02 15:04:05"))
		server := httptest.NewServer(mock)
		defer server.Close()

		s := newTestSyncer(t)
		s.SetClock(func() time.Time { return now })
		p := zcodePlan(server.URL, policy)
		s.mu.Lock()
		s.values["zcode"] = RemoteQuota{
			Windows: []QuotaWindow{{Name: "5 hours", UsagePct: pct(100)}, {Name: "Weekly", UsagePct: pct(20)}},
			Resets:  []ResetCoupon{{Scope: ScopeWeekly, ExpiresAt: at()}},
		}
		s.mu.Unlock()
		s.AutoReset(context.Background(), map[string]*config.Plan{"zcode": p}, "zcode", false)
		if _, useCalls := mock.stats(); useCalls != 0 {
			t.Fatalf("weekly coupon redeemed with weekly quota remaining (%d /use calls)", useCalls)
		}
	})

	t.Run("weekly coupon redeems when weekly quota is nearly spent", func(t *testing.T) {
		mock := newBigModelMock(t)
		mock.coupons = fmt.Sprintf(`{"code":200,"success":true,"data":{"weekResets":[{"recordId":"99001","grantType":"DIRECT","expireTime":%q,"available":true}]}}`, expiry.In(shanghaiZone).Format("2006-01-02 15:04:05"))
		server := httptest.NewServer(mock)
		defer server.Close()

		s := newTestSyncer(t)
		s.SetClock(func() time.Time { return now })
		p := zcodePlan(server.URL, policy)
		s.mu.Lock()
		s.values["zcode"] = RemoteQuota{
			Windows: []QuotaWindow{{Name: "5 hours", UsagePct: pct(100)}, {Name: "Weekly", UsagePct: pct(85)}},
			Resets:  []ResetCoupon{{Scope: ScopeWeekly, ExpiresAt: at()}},
		}
		s.mu.Unlock()
		s.AutoReset(context.Background(), map[string]*config.Plan{"zcode": p}, "zcode", false)
		if _, useCalls := mock.stats(); useCalls != 1 {
			t.Fatalf("weekly coupon was not redeemed at high weekly usage (%d /use calls)", useCalls)
		}
	})

	t.Run("expiring five-hour coupon triggers before exhaustion", func(t *testing.T) {
		mock := newBigModelMock(t)
		mock.coupons = fmt.Sprintf(`{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":4242,"grantType":"DIRECT","expireTime":%q,"available":true}]}}`, expiry.In(shanghaiZone).Format("2006-01-02 15:04:05"))
		server := httptest.NewServer(mock)
		defer server.Close()

		s := newTestSyncer(t)
		s.SetClock(func() time.Time { return now })
		p := zcodePlan(server.URL, config.AutoResetSpec{
			Enabled:   true,
			Scopes:    []string{ScopeFiveHour},
			Safeguard: config.SafeguardSpec{Enabled: true, LeadTime: "30m", MinUsedPct: 80},
		})
		s.mu.Lock()
		s.values["zcode"] = RemoteQuota{
			Windows: []QuotaWindow{{Name: "5 hours", UsagePct: pct(85)}},
			Resets:  []ResetCoupon{{Scope: ScopeFiveHour, ExpiresAt: at()}},
		}
		s.mu.Unlock()
		s.AutoReset(context.Background(), map[string]*config.Plan{"zcode": p}, "zcode", false)
		if _, useCalls := mock.stats(); useCalls != 1 {
			t.Fatalf("expiring five-hour coupon was not redeemed (%d /use calls)", useCalls)
		}
	})
}
