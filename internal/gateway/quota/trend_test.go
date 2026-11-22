package quota

import (
	"math"
	"testing"
	"time"
)

func TestWindowTrend(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reset := now.Add(4 * time.Hour)
	used := 10.0
	trend := windowTrend(QuotaWindow{Name: "5 hours", ResetAt: &reset, UsagePct: &used}, now)
	if trend == nil || math.Abs(trend.ElapsedPct-20) > 1e-9 || trend.ProjectedEndPct == nil || math.Abs(*trend.ProjectedEndPct-50) > 1e-9 || trend.Status != "underutilized" {
		t.Fatalf("unexpected trend: %+v", trend)
	}
	used = 100
	if got := windowTrend(QuotaWindow{Name: "5 hours", ResetAt: &reset, UsagePct: &used}, now); got.Status != "exhausted" {
		t.Fatalf("status: %s", got.Status)
	}
	if windowTrend(QuotaWindow{Name: "unknown", ResetAt: &reset}, now) != nil {
		t.Fatal("unknown duration has trend")
	}
}
