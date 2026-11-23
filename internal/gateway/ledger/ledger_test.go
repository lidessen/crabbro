package ledger

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestAggregateWeightsNonzeroSamples(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "usage.db"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	for _, entry := range []UsageEntry{
		{Timestamp: now, PlanID: "a", Model: "m", TTFTMs: 100, DurationMs: 400},
		{Timestamp: now, PlanID: "a", Model: "m"},
		{Timestamp: now, PlanID: "b", Model: "m", TTFTMs: 300, DurationMs: 200},
		{Timestamp: now, PlanID: "c", Model: "m", DurationMs: 600},
	} {
		if err := st.Record(entry); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.Aggregate("test", now.Add(-time.Second), now.Add(time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.Requests != 4 || got.Totals.AvgTTFTMs != 200 || got.Totals.AvgDurationMs != 400 {
		t.Fatalf("totals = %+v, want 4 requests, TTFT 200, duration 400", got.Totals)
	}
	filtered, err := st.Aggregate("test", now.Add(-time.Second), now.Add(time.Second), "a")
	if err != nil || filtered.Totals.AvgTTFTMs != 100 || filtered.Totals.AvgDurationMs != 400 {
		t.Fatalf("filtered totals = %+v, err = %v", filtered.Totals, err)
	}
}

func TestMigrateLegacyUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE gateway_usage (
		id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, plan_id TEXT NOT NULL, model TEXT NOT NULL,
		protocol TEXT NOT NULL, route_id TEXT NOT NULL, session_key TEXT NOT NULL,
		status INTEGER NOT NULL, error_kind TEXT NOT NULL DEFAULT '', input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0, cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_write_tokens INTEGER NOT NULL DEFAULT 0, reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0, ttft_ms INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0)`)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_, err = db.Exec(`INSERT INTO gateway_usage (ts, plan_id, model, protocol, route_id, session_key, status) VALUES (?, 'p', 'm', '', '', '', 200)`, now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		st, err := Open(path, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Aggregate("test", now.Add(-time.Second), now.Add(time.Second), "")
		if err != nil || got.Totals.Requests != 1 || got.Totals.Retries != 0 {
			t.Fatalf("legacy totals = %+v, err = %v", got.Totals, err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

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
