package ledger

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestLedgerRecordWindowAndAggregate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "usage.db")
	st, err := Open(dbPath, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().UTC()
	entries := []UsageEntry{
		{
			Timestamp:       now.Add(-2 * time.Hour),
			PlanID:          "glm-coding",
			Model:           "glm-4.7",
			Protocol:        "anthropic",
			RouteID:         "coding-main",
			SessionKey:      "ses_1",
			Status:          200,
			InputTokens:     1000,
			OutputTokens:    200,
			CacheReadTokens: 3000,
			TTFTMs:          320,
			DurationMs:      1100,
		},
		{
			Timestamp:    now.Add(-30 * time.Minute),
			PlanID:       "glm-coding",
			Model:        "glm-4.7",
			Protocol:     "openai-chat",
			RouteID:      "coding-main",
			SessionKey:   "ses_2",
			Status:       429,
			ErrorKind:    "rate_limit",
			InputTokens:  50,
			OutputTokens: 0,
			TTFTMs:       120,
			DurationMs:   120,
		},
		{
			Timestamp:    now.Add(-40 * 24 * time.Hour), // outside 30d retention -> pruned on explicit prune
			PlanID:       "old-plan",
			Model:        "old-model",
			Protocol:     "openai-chat",
			RouteID:      "r",
			SessionKey:   "s",
			Status:       200,
			InputTokens:  500,
			OutputTokens: 100,
		},
	}
	for _, e := range entries {
		if err := st.Record(e); err != nil {
			t.Fatal(err)
		}
	}

	reqs, toks, err := st.WindowTotals("glm-coding", now.Add(-5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if reqs != 2 || toks != 4250 {
		t.Fatalf("WindowTotals = (%d, %d), want (2, 4250)", reqs, toks)
	}

	agg, err := st.Aggregate("5h", now.Add(-5*time.Hour), now, "")
	if err != nil {
		t.Fatal(err)
	}
	if agg.Totals.Requests != 2 || agg.Totals.Errors != 1 || agg.Totals.TotalTokens != 4250 {
		t.Fatalf("agg totals = %+v", agg.Totals)
	}
	if agg.Totals.CacheHitRatio <= 0.7 {
		t.Fatalf("cache hit ratio = %v, want > 0.7", agg.Totals.CacheHitRatio)
	}

	pruned, err := st.Prune()
	if err != nil || pruned != 1 {
		t.Fatalf("Prune = %d, %v (want 1)", pruned, err)
	}
}

func TestCalibratorHeadersAndAIMD(t *testing.T) {
	cal := NewCalibrator()
	now := time.Now().UTC()
	h := http.Header{}
	h.Set("anthropic-ratelimit-tokens-limit", "100000")
	h.Set("anthropic-ratelimit-tokens-remaining", "25000")
	h.Set("anthropic-ratelimit-tokens-reset", now.Add(2*time.Minute).Format(time.RFC3339))

	cal.UpdateHeaders("ant", h, now)
	snap, ok := cal.HeaderQuota("ant", now)
	if !ok || !snap.HasTokens || snap.TokensLimit != 100000 || snap.TokensRemaining != 25000 {
		t.Fatalf("HeaderQuota = %+v, ok=%v", snap, ok)
	}

	cal.NoteRateLimit("ant", now)
	if f := cal.EffectiveCapFactor("ant", now); f != 0.5 {
		t.Fatalf("factor after 429 = %v, want 0.5", f)
	}
	cal.NoteSuccess("ant")
	if f := cal.EffectiveCapFactor("ant", now); f != 0.6 {
		t.Fatalf("factor after success = %v, want 0.6", f)
	}
}
