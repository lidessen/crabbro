package router

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/gateway/ledger"
	"github.com/lidessen/crabbro/internal/gateway/quota"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
	"github.com/lidessen/crabbro/pkg/api"
)

func setupRouterFixture(t *testing.T) (*Router, *config.Snapshot, *ledger.Store, *secret.Store) {
	t.Helper()
	sec := secret.New(t.TempDir())
	for _, k := range []string{"k1", "k2", "k_expiring"} {
		if err := sec.Set(k, "secret-value-"+k); err != nil {
			t.Fatal(err)
		}
	}
	ldg, err := ledger.Open(filepath.Join(t.TempDir(), "usage.db"), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ldg.Close() })

	scoreStrong := 0.85
	scoreFrontier := 0.95
	scoreFast := 0.55

	p1 := &config.Plan{
		ID: "kimi-coding", Credential: "k1",
		Cache: config.CacheSpec{TTL: "5m"},
		Limits: config.LimitsSpec{Windows: []config.WindowSpec{{
			Name: "5h", Duration: "5h", MaxTokens: 100000, ResetSchedule: "rolling", Mode: "soft",
		}}},
		Endpoints: map[string]*config.EndpointSpec{
			config.ProtocolAnthropic: {BaseURL: "https://api.kimi.example/anthropic"},
		},
		Models: map[string]*config.ModelSpec{
			"kimi-k2.5": {Tier: config.TierStrong, CodingScore: &scoreStrong, ContextWindow: 200000, Capabilities: config.Capabilities{Tools: true, Thinking: true, PromptCache: true}},
		},
	}
	p2 := &config.Plan{
		ID: "glm-coding", Credential: "k2",
		Cache: config.CacheSpec{TTL: "5m"},
		Limits: config.LimitsSpec{Windows: []config.WindowSpec{{
			Name: "5h", Duration: "5h", MaxTokens: 100000, ResetSchedule: "rolling", Mode: "soft",
		}}},
		Endpoints: map[string]*config.EndpointSpec{
			config.ProtocolAnthropic:  {BaseURL: "https://api.glm.example/anthropic"},
			config.ProtocolOpenAIChat: {BaseURL: "https://api.glm.example/v4"},
		},
		Models: map[string]*config.ModelSpec{
			"glm-4.7": {Tier: config.TierStrong, CodingScore: &scoreStrong, ContextWindow: 200000, Capabilities: config.Capabilities{Tools: true, Thinking: true, PromptCache: true}},
		},
	}
	p3 := &config.Plan{
		ID: "expiring-frontier", Credential: "k_expiring",
		Cache: config.CacheSpec{TTL: "5m"},
		Limits: config.LimitsSpec{Windows: []config.WindowSpec{{
			Name: "hourly", Duration: "1h", MaxTokens: 100000, ResetSchedule: "hourly", Mode: "soft",
		}}},
		Endpoints: map[string]*config.EndpointSpec{
			config.ProtocolAnthropic: {BaseURL: "https://api.exp.example/anthropic"},
		},
		Models: map[string]*config.ModelSpec{
			"opus-max":  {Tier: config.TierFrontier, CodingScore: &scoreFrontier, ContextWindow: 200000, Capabilities: config.Capabilities{Tools: true, Thinking: true}},
			"mini-fast": {Tier: config.TierFast, CodingScore: &scoreFast, ContextWindow: 64000, Capabilities: config.Capabilities{Tools: true}},
		},
	}

	rt := &config.Route{
		ID: "coding-main", Priority: 10, Family: config.FamilyGenerative,
		Match: config.RouteMatch{Protocols: []string{config.ProtocolAnthropic, config.ProtocolOpenAIChat}, Models: []string{"coding-main", "claude-*"}},
		Policy: config.RoutePolicy{
			Preset: config.PresetBalanced,
		},
		Targets: []config.Target{
			{Plan: "kimi-coding", Model: "kimi-k2.5", Priority: 10},
			{Plan: "glm-coding", Model: "glm-4.7", Priority: 20},
		},
	}
	snap := &config.Snapshot{
		Generation: 1,
		Plans:      map[string]*config.Plan{"kimi-coding": p1, "glm-coding": p2, "expiring-frontier": p3},
		AllPlans:   map[string]*config.Plan{"kimi-coding": p1, "glm-coding": p2, "expiring-frontier": p3},
		Routes:     []*config.Route{rt},
		AllRoutes:  []*config.Route{rt},
	}

	r := New(sec, NewSessionCacheStore(128), NewHealthStore(), ldg)
	return r, snap, ldg, sec
}

func TestToolLoopCachePinAndTTLExpiryRebalance(t *testing.T) {
	r, snap, ldg, _ := setupRouterFixture(t)
	now := time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }

	// Burn 85% of kimi-coding quota so glm-coding has much better QuotaUrgency.
	for i := 0; i < 85; i++ {
		_ = ldg.Record(ledger.UsageEntry{Timestamp: now.Add(-10 * time.Minute), PlanID: "kimi-coding", Model: "kimi-k2.5", Status: 200, TotalTokens: 1000})
	}

	// Warm session sits on kimi-coding 30s ago (< 5m TTL).
	r.Sessions.Update(SessionCacheEntry{
		SessionKey:       "ses_warm",
		LastPlan:         "kimi-coding",
		LastModel:        "kimi-k2.5",
		WarmPrefixTokens: 50000,
		LastSuccessAt:    now.Add(-30 * time.Second),
	})

	// 1. ToolLoop MUST hard-pin to kimi-coding despite kimi's heavy quota deficit.
	toolBody := []byte(`{"model":"coding-main","messages":[{"role":"user","content":"edit"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`)
	sig := ExtractSignals("", config.ProtocolAnthropic, config.FamilyGenerative, "coding-main", "ses_warm", toolBody)
	if sig.TurnPhase != PhaseToolLoop {
		t.Fatalf("phase = %s, want ToolLoop", sig.TurnPhase)
	}
	sel, err := r.Select(context.Background(), snap, sig)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Candidates[0].Plan.ID != "kimi-coding" {
		t.Fatalf("ToolLoop failed to pin kimi-coding: chose %s (scores: %+v)", sel.Candidates[0].Plan.ID, sel.Decision.Candidates)
	}

	// 2. Advance time past 5m cache TTL -> cacheGravity drops to 0, router rebalances freely to glm-coding!
	now = now.Add(6 * time.Minute)
	userBody := []byte(`{"model":"coding-main","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"next step"}]}`)
	sig2 := ExtractSignals("", config.ProtocolAnthropic, config.FamilyGenerative, "coding-main", "ses_warm", userBody)
	sel2, err := r.Select(context.Background(), snap, sig2)
	if err != nil {
		t.Fatal(err)
	}
	if sel2.Candidates[0].Plan.ID != "glm-coding" {
		t.Fatalf("expected rebalance to glm-coding after TTL expiry, got %s", sel2.Candidates[0].Plan.ID)
	}
	if sel2.CacheGravityMax != 0 {
		t.Fatalf("CacheGravityMax = %v, want 0 after TTL expiry", sel2.CacheGravityMax)
	}
}

func TestExpiringSurplusBeatsDeficitAndSemanticTieringRespectsBoundary(t *testing.T) {
	r, snap, _, _ := setupRouterFixture(t)
	// 54 minutes into the hour -> only 10% timeLeftRatio in hourly window, 100% quota left (+0.90 surplus).
	now := time.Date(2026, 11, 19, 10, 54, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }

	snap.Routes[0].Targets = []config.Target{
		{Plan: "glm-coding", Model: "glm-4.7", Priority: 10},
		{Plan: "expiring-frontier", Model: "opus-max", Priority: 20},
	}
	snap.Routes[0].Policy.Preset = config.PresetBurnExpiring

	coldBody := []byte(`{"model":"coding-main","messages":[{"role":"user","content":"write a unit test"}]}`)
	sig := ExtractSignals("", config.ProtocolAnthropic, config.FamilyGenerative, "coding-main", "", coldBody)
	sel, err := r.Select(context.Background(), snap, sig)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Candidates[0].Plan.ID != "expiring-frontier" {
		t.Fatalf("expected expiring-frontier to win burn-expiring surplus, got %s (%+v)", sel.Candidates[0].Plan.ID, sel.Decision.Candidates)
	}

	// Verify SemanticTiering never triggers during ToolLoop or hopDepth > 0, and triggers once on ColdStart.
	calls := 0
	r.DecisionCaller = func(ctx context.Context, routeID string, req *ir.DecisionRequest) (*ir.DecisionResponse, error) {
		calls++
		conf := 0.9
		return &ir.DecisionResponse{
			Model:   "jev-latest",
			Answers: map[string]ir.DecisionAnswer{"tier": {Type: ir.DecisionKindChoice, Choice: config.TierFrontier, Confidence: &conf}},
		}, nil
	}
	snap.Routes[0].Policy.SemanticTiering = config.SemanticTieringSpec{Enabled: true, DecisionRoute: "decision"}

	// hopDepth=1 -> skipped
	if _, err := r.Select(WithHopDepth(context.Background(), 1), snap, sig); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("semanticTiering ran at hopDepth=1")
	}

	// ToolLoop -> skipped
	toolSig := sig
	toolSig.TurnPhase = PhaseToolLoop
	if _, err := r.Select(context.Background(), snap, toolSig); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("semanticTiering ran during ToolLoop")
	}

	// ColdStart hopDepth=0 -> runs once and upgrades DemandTier to frontier!
	selTiered, err := r.Select(context.Background(), snap, sig)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !selTiered.SemanticTierUsed || selTiered.DemandTier != config.TierFrontier {
		t.Fatalf("expected semanticTiering upgrade, calls=%d used=%v tier=%s", calls, selTiered.SemanticTierUsed, selTiered.DemandTier)
	}

	// Preview helper works cleanly.
	prev := r.Preview(context.Background(), snap, shared.GatewayPreviewRequest{
		Protocol: config.ProtocolAnthropic, Model: "coding-main", EstimatedInputTokens: 2000,
	})
	if !prev.Matched || prev.RouteID != "coding-main" || prev.Chosen == "" {
		t.Fatalf("Preview = %+v", prev)
	}
}

func TestMatchModelAnySlashWildcard(t *testing.T) {
	if !matchModelAny([]string{"deepseek/*"}, "deepseek/deepseek-v4.1-flash") {
		t.Fatal("expected deepseek/* to match slash-separated model id")
	}
	if !matchModelAny([]string{"*flash*"}, "deepseek/deepseek-v4.1-flash") {
		t.Fatal("expected *flash* to match slash-separated model id")
	}
	if matchModelAny([]string{"openai/*"}, "deepseek/deepseek-v4.1-flash") {
		t.Fatal("unexpected match for openai/*")
	}
}

type quotaFixture map[string]*quota.RemoteQuota

func (q quotaFixture) Get(id string) (*quota.RemoteQuota, bool) { v, ok := q[id]; return v, ok }
func TestExpiringCouponBoost(t *testing.T) {
	r, snap, _, _ := setupRouterFixture(t)
	now := time.Now().UTC()
	r.Now = func() time.Time { return now }
	snap.Routes[0].Targets[1].Priority = snap.Routes[0].Targets[0].Priority
	expiry := now.Add(12 * time.Hour)
	usage := 10.0
	r.Quotas = quotaFixture{"glm-coding": {ResetsRemaining: 1, SoonestResetExpiry: &expiry, UsagePct: &usage}}
	sig := ExtractSignals("", config.ProtocolAnthropic, config.FamilyGenerative, "coding-main", "", []byte(`{"model":"coding-main","messages":[{"role":"user","content":"hello"}]}`))
	sel, err := r.Select(context.Background(), snap, sig)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Candidates[0].Plan.ID != "glm-coding" {
		t.Fatalf("coupon did not win: %+v", sel.Decision.Candidates)
	}
}

func TestExpiringQuotaBoostScalesWithTimeAndRemainingQuota(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	pct := func(v float64) *float64 { return &v }
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }

	quotaAt := func(usage float64, left time.Duration) *quota.RemoteQuota {
		return &quota.RemoteQuota{UsagePct: pct(usage), ResetsRemaining: 1, SoonestResetExpiry: at(left)}
	}
	near := expiringQuotaBoost(quotaAt(10, time.Hour), now)
	far := expiringQuotaBoost(quotaAt(10, 12*time.Hour), now)
	spent := expiringQuotaBoost(quotaAt(85, time.Hour), now)
	if !(near > far && far > 0) {
		t.Fatalf("boost must grow with time-to-expiry: near=%f far=%f", near, far)
	}
	if !(near > spent && spent >= 0) {
		t.Fatalf("boost must shrink with used quota: fresh=%f spent=%f", near, spent)
	}
	if got := expiringQuotaBoost(quotaAt(10, -time.Hour), now); got != 0 {
		t.Fatalf("expired coupon boosted by %f", got)
	}
	if got := expiringQuotaBoost(quotaAt(10, 25*time.Hour), now); got != 0 {
		t.Fatalf("coupon outside horizon boosted by %f", got)
	}

	// An exhausted five-hour window is quota_exhausted: no boost at all,
	// even when a coupon expires immediately.
	exhausted := &quota.RemoteQuota{
		Windows:            []api.RemoteQuotaWindow{{Name: "5 hours", UsagePct: pct(100)}},
		ResetsRemaining:    1,
		SoonestResetExpiry: at(time.Minute),
	}
	if got := expiringQuotaBoost(exhausted, now); got != 0 {
		t.Fatalf("quota_exhausted plan boosted by %f", got)
	}
}

func TestExpiringCouponBoostBeatsPriorityButNotQuotaExhaustion(t *testing.T) {
	r, snap, _, _ := setupRouterFixture(t)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	// glm-coding starts at a worse priority (20 vs 10), so it only wins when
	// the expiry boost is actually applied.
	expiry := now.Add(12 * time.Hour)
	usage := 10.0
	r.Quotas = quotaFixture{"glm-coding": {ResetsRemaining: 1, SoonestResetExpiry: &expiry, UsagePct: &usage}}
	sig := ExtractSignals("", config.ProtocolAnthropic, config.FamilyGenerative, "coding-main", "", []byte(`{"model":"coding-main","messages":[{"role":"user","content":"hello"}]}`))
	sel, err := r.Select(context.Background(), snap, sig)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Candidates[0].Plan.ID != "glm-coding" {
		t.Fatalf("expiring coupon did not overcome priority: %+v", sel.Decision.Candidates)
	}

	// Same coupon, but the five-hour window is exhausted: glm-coding must not
	// receive the boost and keeps losing on priority.
	exhausted := 100.0
	r.Quotas = quotaFixture{"glm-coding": {
		Windows:            []api.RemoteQuotaWindow{{Name: "5 hours", UsagePct: &exhausted}},
		ResetsRemaining:    1,
		SoonestResetExpiry: &expiry,
		UsagePct:           &exhausted,
	}}
	sel, err = r.Select(context.Background(), snap, sig)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Candidates[0].Plan.ID != "kimi-coding" {
		t.Fatalf("quota_exhausted plan was boosted: %+v", sel.Decision.Candidates)
	}
}

func TestUnderutilizedWindowBoost(t *testing.T) {
	r, snap, _, _ := setupRouterFixture(t)
	now := time.Now().UTC()
	r.Now = func() time.Time { return now }
	snap.Routes[0].Targets[1].Priority = snap.Routes[0].Targets[0].Priority
	reset := now.Add(36 * time.Hour)
	projected := 30.0
	r.Quotas = quotaFixture{"glm-coding": {Windows: []api.RemoteQuotaWindow{{Name: "Weekly", ResetAt: &reset, Trend: &api.WindowTrend{ProjectedEndPct: &projected}}}}}
	sig := ExtractSignals("", config.ProtocolAnthropic, config.FamilyGenerative, "coding-main", "", []byte(`{"model":"coding-main","messages":[{"role":"user","content":"hello"}]}`))
	sel, err := r.Select(context.Background(), snap, sig)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Candidates[0].Plan.ID != "glm-coding" {
		t.Fatalf("window did not win: %+v", sel.Decision.Candidates)
	}
}
