package router

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/gateway/ledger"
	"github.com/lidessen/crabbro/internal/gateway/quota"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
)

type hopDepthCtxKey struct{}

// WithHopDepth stores the internal routing recursion depth on ctx.
func WithHopDepth(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, hopDepthCtxKey{}, depth)
}

// HopDepth returns the current routing recursion depth (0 for external client requests).
func HopDepth(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if d, ok := ctx.Value(hopDepthCtxKey{}).(int); ok {
		return d
	}
	return 0
}

// DecisionCaller invokes an internal decision route without importing internal/gateway.
type DecisionCaller func(ctx context.Context, routeID string, req *ir.DecisionRequest) (*ir.DecisionResponse, error)

// Candidate represents one ranked target produced by Router.Select.
type Candidate struct {
	Target         config.Target
	Plan           *config.Plan
	Endpoint       *config.EndpointSpec
	EndpointProto  string
	ModelSpec      *config.ModelSpec
	NativeProtocol bool
	DecisionMode   string // native | emulated (when family == decision)
	Calibrated     bool
	Scores         shared.CandidateScores
	Eligible       bool
	RejectReason   string
}

// Selection is the explainable routing plan returned by Router.Select.
type Selection struct {
	Route            *config.Route
	Signals          Signals
	DemandTier       string
	SemanticTierUsed bool
	CacheWarmAgeSec  int64
	CacheGravityMax  float64
	Candidates       []Candidate // eligible candidates sorted descending by Score
	AllEvaluated     []Candidate // includes rejected candidates for trace/preview
	Decision         *shared.RouteDecision
}

// Router implements the 4-stage Cache × Quota × Quality route selector.
type Router struct {
	Secrets  *secret.Store
	Sessions *SessionCacheStore
	Health   *HealthStore
	Ledger   *ledger.Store
	Quotas   interface {
		Get(string) (*quota.RemoteQuota, bool)
	}
	DecisionCaller DecisionCaller
	Now            func() time.Time
}

// New creates a Router wired to shared state stores.
func New(secrets *secret.Store, sessions *SessionCacheStore, health *HealthStore, ldg *ledger.Store) *Router {
	return &Router{
		Secrets:  secrets,
		Sessions: sessions,
		Health:   health,
		Ledger:   ldg,
		Now:      time.Now,
	}
}

func (r *Router) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

const (
	ResolutionOverride = "override"
	ResolutionExplicit = "explicit"
	ResolutionDirect   = "direct"
	ResolutionFallback = "fallback"
)

// ResolveRoute resolves overrides, explicit matches, direct addresses, and fallback routes.
func (r *Router) ResolveRoute(snap *config.Snapshot, sig Signals) (*config.Route, string, error) {
	if sig.RouteOverride != "" {
		if snap != nil {
			for _, rt := range snap.Routes {
				if rt.IsEnabled() && rt.ID == sig.RouteOverride {
					return rt, ResolutionOverride, nil
				}
			}
		}
		return nil, "", fmt.Errorf("route override %q not found or disabled", sig.RouteOverride)
	}
	if snap != nil {
		routes := append([]*config.Route(nil), snap.Routes...)
		sort.SliceStable(routes, func(i, j int) bool {
			if routes[i].Priority != routes[j].Priority {
				return routes[i].Priority < routes[j].Priority
			}
			return routes[i].ID < routes[j].ID
		})
		matches := func(rt *config.Route) bool {
			return rt.IsEnabled() && matchListOrEmpty(rt.Match.Profiles, sig.Profile) && (len(rt.Match.Protocols) == 0 || containsStr(rt.Match.Protocols, sig.Protocol))
		}
		for _, rt := range routes {
			if matches(rt) {
				for _, pat := range rt.Match.Models {
					if pat != "*" && matchModelAny([]string{pat}, sig.RequestedModel) {
						return rt, ResolutionExplicit, nil
					}
				}
			}
		}
		if id, raw, ok := strings.Cut(sig.RequestedModel, "/"); ok {
			if p := snap.AllPlans[id]; p != nil {
				if !p.IsEnabled() {
					return nil, "", fmt.Errorf("direct plan %q is disabled", id)
				}
				if !p.DirectEnabled() {
					return nil, "", fmt.Errorf("direct addressing is disabled on plan %q", id)
				}
				if !p.AllowsDirectProfile(sig.Profile) {
					return nil, "", fmt.Errorf("direct access to plan %q is not allowed on profile %q", id, sig.Profile)
				}
				key := raw
				ms := p.Models[key]
				if ms == nil {
					key = NormalizeModelName(raw)
					ms = p.Models[key]
				}
				if ms == nil {
					norm := NormalizeModelName(raw)
					for k, v := range p.Models {
						if v != nil && strings.EqualFold(k, norm) {
							if ms != nil {
								return nil, "", fmt.Errorf("ambiguous case-insensitive model %q on plan %q", raw, id)
							}
							key, ms = k, v
						}
					}
				}
				if raw == "" || ms == nil {
					return nil, "", fmt.Errorf("model %q is not declared on plan %q", raw, id)
				}
				yes := true
				return &config.Route{ID: "direct:" + p.ID, Family: sig.Family, Policy: config.RoutePolicy{Preset: config.PresetStrictFailover, PreferNativeProtocol: &yes, AllowCrossProtocol: &yes, MaxAttempts: 1}, Targets: []config.Target{{Plan: p.ID, Model: key, Priority: 10}}}, ResolutionDirect, nil
			}
		}
		for _, rt := range routes {
			if matches(rt) && (len(rt.Match.Models) == 0 || containsStr(rt.Match.Models, "*")) {
				return rt, ResolutionFallback, nil
			}
		}
	}
	return nil, "", fmt.Errorf("no enabled gateway route matches profile=%q protocol=%q model=%q", sig.Profile, sig.Protocol, sig.RequestedModel)
}

// Select runs the 4-stage routing pipeline and returns an ordered failover candidate list.
func (r *Router) Select(ctx context.Context, snap *config.Snapshot, sig Signals) (*Selection, error) {
	now := r.now()
	rt, resolution, err := r.ResolveRoute(snap, sig)
	if err != nil {
		return nil, err
	}

	// Stage 1 refinement: distinguish ColdStart vs NewUserTurn with SessionCacheStore.
	warm, hasWarm := SessionCacheEntry{}, false
	if r.Sessions != nil && sig.SessionKey != "" {
		warm, hasWarm = r.Sessions.Get(sig.SessionKey)
	}
	if sig.TurnPhase == PhaseColdStart && hasWarm && sig.MessageCount > 1 {
		sig.TurnPhase = PhaseNewUserTurn
	}
	if sig.TurnPhase == PhaseNewUserTurn && !hasWarm {
		sig.TurnPhase = PhaseColdStart
	}

	// Compute max warm cache gravity across targets so Stage 2 knows whether cache is hot.
	var warmAgeSec int64
	maxGravity := 0.0
	if hasWarm {
		warmAgeSec = int64(now.Sub(warm.LastSuccessAt).Seconds())
		if warmAgeSec < 0 {
			warmAgeSec = 0
		}
		maxGravity = r.estimateWarmGravity(rt, snap, warm, sig, now)
	}

	// Stage 2: Optional cold-boundary SemanticTiering.
	demandTier := sig.RequestedTier
	semanticUsed := false
	if r.shouldRunSemanticTiering(ctx, rt, sig, maxGravity) {
		if tier, ok := r.runSemanticTiering(ctx, rt, sig); ok {
			demandTier = tier
			semanticUsed = true
		}
	}

	// Stage 3: Hard-constraint filtering (with soft-window last-resort fallback).
	evaluated := r.evaluateCandidates(snap, rt, sig, demandTier, warm, hasWarm, now)
	eligible := make([]Candidate, 0, len(evaluated))
	for _, c := range evaluated {
		if c.Eligible {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		// Last-resort graceful fallback: allow targets blocked only by "soft" windows.
		evaluated = r.evaluateCandidatesWithSoftFallback(snap, rt, sig, demandTier, warm, hasWarm, now)
		for _, c := range evaluated {
			if c.Eligible {
				eligible = append(eligible, c)
			}
		}
	}
	if len(eligible) == 0 {
		selectionErr := fmt.Errorf("no eligible targets for route %q", rt.ID)
		if resolution == ResolutionDirect && len(evaluated) > 0 {
			selectionErr = fmt.Errorf("no eligible targets for route %q: %s", rt.ID, evaluated[0].RejectReason)
		}
		return &Selection{
			Route:            rt,
			Signals:          sig,
			DemandTier:       demandTier,
			SemanticTierUsed: semanticUsed,
			CacheWarmAgeSec:  warmAgeSec,
			CacheGravityMax:  maxGravity,
			AllEvaluated:     evaluated,
			Decision:         buildRouteDecision(rt, resolution, sig, demandTier, semanticUsed, warmAgeSec, maxGravity, evaluated),
		}, selectionErr
	}

	// Stage 4: Sort descending by unified Total score, then target priority, then plan/model.
	sort.SliceStable(eligible, func(i, j int) bool {
		if rt.Policy.EffectivePreset() == config.PresetStrictFailover && !rt.Policy.PreferNative() && eligible[i].Target.Priority != eligible[j].Target.Priority {
			return eligible[i].Target.Priority < eligible[j].Target.Priority
		}
		if math.Abs(eligible[i].Scores.Total-eligible[j].Scores.Total) > 1e-9 {
			return eligible[i].Scores.Total > eligible[j].Scores.Total
		}
		if eligible[i].Target.Priority != eligible[j].Target.Priority {
			return eligible[i].Target.Priority < eligible[j].Target.Priority
		}
		if eligible[i].Plan.ID != eligible[j].Plan.ID {
			return eligible[i].Plan.ID < eligible[j].Plan.ID
		}
		return eligible[i].Target.Model < eligible[j].Target.Model
	})

	// Also sort AllEvaluated with eligible ranked candidates first followed by rejected ones.
	orderedAll := make([]Candidate, 0, len(evaluated))
	orderedAll = append(orderedAll, eligible...)
	for _, c := range evaluated {
		if !c.Eligible {
			orderedAll = append(orderedAll, c)
		}
	}

	return &Selection{
		Route:            rt,
		Signals:          sig,
		DemandTier:       demandTier,
		SemanticTierUsed: semanticUsed,
		CacheWarmAgeSec:  warmAgeSec,
		CacheGravityMax:  maxGravity,
		Candidates:       eligible,
		AllEvaluated:     orderedAll,
		Decision:         buildRouteDecision(rt, resolution, sig, demandTier, semanticUsed, warmAgeSec, maxGravity, orderedAll),
	}, nil
}

// Preview runs routing selection without touching upstream endpoints.
func (r *Router) Preview(ctx context.Context, snap *config.Snapshot, req shared.GatewayPreviewRequest) shared.GatewayPreviewResponse {
	proto := req.Protocol
	if proto == "" {
		proto = config.ProtocolAnthropic
	}
	fam := config.ProtocolFamily(proto)
	if fam == "" {
		return shared.GatewayPreviewResponse{Matched: false, Error: fmt.Sprintf("unknown protocol %q", proto)}
	}
	phase := req.TurnPhase
	if phase == "" {
		if fam == config.FamilyDecision {
			phase = PhaseDecisionCall
		} else if req.SessionKey != "" && r.Sessions != nil {
			if _, ok := r.Sessions.Get(req.SessionKey); ok {
				phase = PhaseNewUserTurn
			} else {
				phase = PhaseColdStart
			}
		} else {
			phase = PhaseColdStart
		}
	}
	est := req.EstimatedInputTokens
	if est <= 0 {
		est = 1000
	}
	sig := Signals{
		Profile:              req.Profile,
		Protocol:             proto,
		Family:               fam,
		RequestedModel:       req.Model,
		SessionKey:           req.SessionKey,
		TurnPhase:            phase,
		MessageCount:         2,
		RequestedTier:        inferTierFromModel(req.Model),
		EstimatedInputTokens: est,
		NeedTools:            req.Tools,
		NeedVision:           req.Vision,
		NeedThinking:         req.Thinking,
	}
	if phase == PhaseColdStart {
		sig.MessageCount = 1
	}

	sel, err := r.Select(ctx, snap, sig)
	if err != nil {
		resp := shared.GatewayPreviewResponse{Matched: sel != nil && sel.Route != nil, Error: err.Error()}
		if sel != nil {
			if sel.Route != nil {
				resp.RouteID = sel.Route.ID
				resp.Family = sel.Route.EffectiveFamily()
			}
			resp.Decision = sel.Decision
		}
		return resp
	}
	chosen := sel.Candidates[0].Plan.ID + "/" + sel.Candidates[0].Target.Model + " (" + sel.Candidates[0].EndpointProto + ")"
	return shared.GatewayPreviewResponse{
		Matched:  true,
		RouteID:  sel.Route.ID,
		Family:   sel.Route.EffectiveFamily(),
		Chosen:   chosen,
		Decision: sel.Decision,
	}
}

func (r *Router) shouldRunSemanticTiering(ctx context.Context, rt *config.Route, sig Signals, maxGravity float64) bool {
	st := rt.Policy.SemanticTiering
	if !st.Enabled || r.DecisionCaller == nil || HopDepth(ctx) > 0 {
		return false
	}
	if rt.EffectiveFamily() != config.FamilyGenerative {
		return false
	}
	if sig.TurnPhase == PhaseToolLoop || sig.TurnPhase == PhaseAuxSubcall || sig.TurnPhase == PhaseDecisionCall {
		return false
	}
	if sig.TurnPhase == PhaseNewUserTurn && maxGravity >= st.EffectiveMaxCacheGravity() {
		return false
	}
	return strings.TrimSpace(sig.SummaryText) != ""
}

func (r *Router) runSemanticTiering(ctx context.Context, rt *config.Route, sig Signals) (string, bool) {
	st := rt.Policy.SemanticTiering
	tctx, cancel := context.WithTimeout(WithHopDepth(ctx, HopDepth(ctx)+1), st.TimeoutDuration())
	defer cancel()

	dreq := &ir.DecisionRequest{
		Model: st.EffectiveDecisionRoute(),
		State: map[string]any{
			"requestedModel": sig.RequestedModel,
			"summary":        sig.SummaryText,
		},
		Questions: map[string]ir.DecisionQuestion{
			"tier": {
				Type:         ir.DecisionKindChoice,
				Instructions: "Classify the minimum coding model tier needed for this user prompt.",
				Criteria: map[string]string{
					config.TierFast:     "trivial edit, short summary, simple Q&A",
					config.TierStrong:   "ordinary coding, debugging, single-feature implementation",
					config.TierFrontier: "complex architecture, multi-file refactor, deep root-cause reasoning",
				},
			},
		},
	}
	resp, err := r.DecisionCaller(tctx, st.EffectiveDecisionRoute(), dreq)
	if err != nil || resp == nil {
		return "", false
	}
	ans, ok := resp.Answers["tier"]
	if !ok || ans.Choice == "" {
		return "", false
	}
	conf := 0.5
	if ans.Confidence != nil {
		conf = *ans.Confidence
	}
	if conf < st.EffectiveMinConfidence() {
		return "", false
	}
	// Never silently downgrade an explicit frontier+tools+thinking request unless highly confident.
	if sig.RequestedTier == config.TierFrontier && sig.NeedTools && sig.NeedThinking && ans.Choice != config.TierFrontier && conf < 0.85 {
		return "", false
	}
	return ans.Choice, true
}

func (r *Router) estimateWarmGravity(rt *config.Route, snap *config.Snapshot, warm SessionCacheEntry, sig Signals, now time.Time) float64 {
	plan := snap.Plans[warm.LastPlan]
	if plan == nil {
		return 0
	}
	ms := plan.Models[warm.LastModel]
	ttl := effectiveCacheTTL(rt, plan, ms)
	return computeCacheGravity(warm.LastPlan, warm.LastModel, plan.ID, warm.LastModel, warm, sig, ttl, now)
}

func (r *Router) evaluateCandidates(snap *config.Snapshot, rt *config.Route, sig Signals, demandTier string, warm SessionCacheEntry, hasWarm bool, now time.Time) []Candidate {
	return r.evalTargets(snap, rt, sig, demandTier, warm, hasWarm, now, false)
}

func (r *Router) evaluateCandidatesWithSoftFallback(snap *config.Snapshot, rt *config.Route, sig Signals, demandTier string, warm SessionCacheEntry, hasWarm bool, now time.Time) []Candidate {
	return r.evalTargets(snap, rt, sig, demandTier, warm, hasWarm, now, true)
}

func (r *Router) evalTargets(snap *config.Snapshot, rt *config.Route, sig Signals, demandTier string, warm SessionCacheEntry, hasWarm bool, now time.Time, allowSoftExhausted bool) []Candidate {
	weights := effectiveWeights(rt.Policy)
	out := make([]Candidate, 0, len(rt.Targets))

	for _, tgt := range rt.Targets {
		cand := Candidate{Target: tgt}
		if !tgt.IsEnabled() {
			cand.RejectReason = "target disabled"
			out = append(out, cand)
			continue
		}
		plan := snap.Plans[tgt.Plan]
		if plan == nil {
			cand.RejectReason = "plan missing or disabled"
			out = append(out, cand)
			continue
		}
		cand.Plan = plan
		if r.Secrets != nil && !r.Secrets.Has(plan.Credential) {
			cand.RejectReason = "missing credential " + plan.Credential
			out = append(out, cand)
			continue
		}
		ms := plan.Models[tgt.Model]
		if ms == nil {
			cand.RejectReason = "model not declared on plan"
			out = append(out, cand)
			continue
		}
		cand.ModelSpec = ms

		epProto, ep, rej := chooseEndpoint(rt, plan, tgt, ms, sig.Protocol)
		if rej != "" {
			cand.RejectReason = rej
			out = append(out, cand)
			continue
		}
		cand.EndpointProto = epProto
		cand.Endpoint = ep
		cand.NativeProtocol = epProto == sig.Protocol
		if rt.EffectiveFamily() == config.FamilyDecision {
			if config.ProtocolFamily(epProto) == config.FamilyDecision {
				cand.DecisionMode = "native"
				cand.Calibrated = true
			} else {
				cand.DecisionMode = "emulated"
				cand.Calibrated = false
			}
		}

		if r.Health != nil {
			if cool, until, scope := r.Health.IsPlanCooling(plan.ID, now); cool {
				cand.RejectReason = fmt.Sprintf("plan cooling (%s until %s)", scope, until.Format(time.RFC3339))
				out = append(out, cand)
				continue
			}
			if r.Health.IsModelCooling(plan.ID, tgt.Model, now) {
				cand.RejectReason = "plan+model cooling"
				out = append(out, cand)
				continue
			}
			if plan.Limits.Concurrency > 0 && r.Health.InFlight(plan.ID) >= plan.Limits.Concurrency {
				cand.RejectReason = "concurrency limit reached"
				out = append(out, cand)
				continue
			}
		}

		if len(tgt.WhenTier) > 0 && !containsStr(tgt.WhenTier, demandTier) {
			cand.RejectReason = "whenTier mismatch"
			out = append(out, cand)
			continue
		}
		if tgt.MinContextTokens > 0 && sig.EstimatedInputTokens < tgt.MinContextTokens {
			cand.RejectReason = "below target minContextTokens"
			out = append(out, cand)
			continue
		}
		if tgt.MaxContextTokens > 0 && sig.EstimatedInputTokens > tgt.MaxContextTokens {
			cand.RejectReason = "exceeds target maxContextTokens"
			out = append(out, cand)
			continue
		}
		if ms.ContextWindow > 0 && sig.EstimatedInputTokens > ms.ContextWindow {
			cand.RejectReason = "exceeds model contextWindow"
			out = append(out, cand)
			continue
		}
		if rej := checkCapabilities(sig, ms); rej != "" {
			cand.RejectReason = rej
			out = append(out, cand)
			continue
		}

		quotaScore, blockedHard, blockedSoft := r.computeQuotaScore(plan, now)
		if blockedHard {
			cand.RejectReason = "hard quota window exhausted"
			out = append(out, cand)
			continue
		}
		if blockedSoft && !allowSoftExhausted {
			cand.RejectReason = "soft quota window exhausted"
			out = append(out, cand)
			continue
		}

		if r.Quotas != nil {
			if q, ok := r.Quotas.Get(plan.ID); ok && !q.Stale {
				healthy := r.Health == nil || r.Health.Snapshot(plan.ID, now).Healthy
				if healthy {
					quotaScore += expiringQuotaBoost(q, now)
				}
			}
		}

		var cacheRaw float64
		if hasWarm {
			ttl := effectiveCacheTTL(rt, plan, ms)
			cacheRaw = computeCacheGravity(warm.LastPlan, warm.LastModel, plan.ID, tgt.Model, warm, sig, ttl, now)
		}
		qualRaw := computeQualityMatch(ms, demandTier)
		protoRaw := 0.0
		if cand.NativeProtocol && rt.Policy.PreferNative() {
			protoRaw = 1.0
		}
		priBias := -0.001 * float64(tgt.Priority)
		weightBias := targetWeightBias(tgt.Weight) * math.Min(1, math.Max(0, weights.quality))
		healthPen := 0.0
		if r.Health != nil && rt.Policy.EffectivePreset() != config.PresetStrictFailover {
			hs := r.Health.Snapshot(plan.ID, now)
			healthPen = hs.ErrorEWMA*2.0 + math.Min(0.5, hs.LatencyEWMAMs/20000.0)
		}

		scores := shared.CandidateScores{
			Quality:  weights.quality * qualRaw,
			Cache:    weights.cache * cacheRaw,
			Quota:    weights.quota * quotaScore,
			Protocol: weights.protocol * protoRaw,
			Priority: priBias,
			Weight:   weightBias,
			Health:   healthPen,
		}
		scores.Total = scores.Quality + scores.Cache + scores.Quota + scores.Protocol + scores.Priority + scores.Weight - scores.Health

		cand.Scores = scores
		cand.Eligible = true
		out = append(out, cand)
	}
	return out
}

func chooseEndpoint(rt *config.Route, plan *config.Plan, tgt config.Target, ms *config.ModelSpec, ingressProto string) (string, *config.EndpointSpec, string) {
	fam := rt.EffectiveFamily()
	allowed := func(p string) bool { return ms == nil || len(ms.Endpoints) == 0 || containsStr(ms.Endpoints, p) }
	if tgt.EndpointProtocol != "" {
		if !allowed(tgt.EndpointProtocol) {
			return "", nil, "endpoint protocol not allowed for model"
		}
		ep := plan.Endpoints[tgt.EndpointProtocol]
		if ep == nil {
			return "", nil, "target endpointProtocol not found"
		}
		if fam == config.FamilyGenerative && config.ProtocolFamily(tgt.EndpointProtocol) == config.FamilyDecision {
			return "", nil, "generative route cannot target decision-family endpoint"
		}
		if fam == config.FamilyDecision && config.ProtocolFamily(tgt.EndpointProtocol) != config.FamilyDecision && !rt.Policy.AllowEmulation {
			return "", nil, "decision emulation disabled"
		}
		if tgt.EndpointProtocol != ingressProto && !rt.Policy.CrossProtocolAllowed() {
			return "", nil, "cross-protocol bridging disabled"
		}
		return tgt.EndpointProtocol, ep, ""
	}

	// Prefer native matching protocol if present on the plan.
	if ep := plan.Endpoints[ingressProto]; ep != nil && allowed(ingressProto) && config.ProtocolFamily(ingressProto) == fam {
		return ingressProto, ep, ""
	}
	if fam == config.FamilyDecision {
		if !rt.Policy.CrossProtocolAllowed() {
			return "", nil, "cross-protocol bridging disabled"
		}
		for _, p := range []string{config.ProtocolTypeSafeJev, config.ProtocolOpenAIDecisions, config.ProtocolGeminiDecisions} {
			if ep := plan.Endpoints[p]; ep != nil && allowed(p) {
				return p, ep, ""
			}
		}
		if !rt.Policy.AllowEmulation {
			return "", nil, "no compatible endpoint configured for model"
		}
		for _, p := range []string{config.ProtocolGemini, config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolAnthropic} {
			if ep := plan.Endpoints[p]; ep != nil && allowed(p) {
				return p, ep, ""
			}
		}
		return "", nil, "no compatible endpoint"
	}

	if !rt.Policy.CrossProtocolAllowed() {
		return "", nil, "cross-protocol bridging disabled"
	}
	// Deterministic fallback priority across generative protocols.
	for _, p := range []string{config.ProtocolAnthropic, config.ProtocolOpenAIChat, config.ProtocolOpenAIResponses, config.ProtocolGemini} {
		if ep := plan.Endpoints[p]; ep != nil && allowed(p) {
			return p, ep, ""
		}
	}
	return "", nil, "no compatible endpoint configured for model"
}

func checkCapabilities(sig Signals, ms *config.ModelSpec) string {
	// Only enforce capabilities explicitly declared when false? Wait: if a model
	// has Capabilities explicitly configured, check them when true/false makes sense:
	// Wait: if someone configures a basic model without setting `capabilities.tools: true`,
	// wait: in YAML examples `typesafe-jev` only sets `structuredOutput: true` (tools=false),
	// whereas `glm-4.7` sets `tools: true, vision: false, ...`.
	// If Vision is requested and model explicitly didn't enable vision, reject if other candidates can satisfy,
	// wait: checking `Capabilities` directly requires plan authors to declare `tools: true` / `vision: true`!
	// Wait: let's check schemas/gateway-plan.v1.json and gateway.md:
	// "modelSpec.contextWindow ... or capabilities cannot satisfy the request (tools, vision, thinking, structuredOutput)"
	// Wait: what if `models` in a minimal test omits `capabilities` completely (zero-struct)?
	// Look at TestCrossFileValidationRejectsUnknownTargetAndMissingEmulation in config_test.go!
	// If Capabilities is completely zero-value (`!Tools && !Vision && !Thinking && !StructuredOutput && !PromptCache`),
	// wait: for `vision` it's safer to require `Capabilities.Vision`, or if `Capabilities` has any field set vs omitted?
	// Actually, let's check: in `Capabilities` struct, if ALL booleans are false (none declared),
	// wait: if someone has `vision: false` and `tools: true`, at least one capability is declared; if `Capabilities == (Capabilities{})` (none declared at all), should we assume tools/structuredOutput default or enforce strictly?
	// Let's check: in `gateway.md` §6 Stage 3: "`modelSpec.contextWindow` (or target `maxContextTokens` / `minContextTokens`) or `capabilities` cannot satisfy the request (`tools`, `vision`, `thinking`, `structuredOutput`)."
	// Even cleaner: use pointer bools or check `if sig.NeedVision && !ms.Capabilities.Vision` when Capabilities are declared, OR make Capabilities check strict! Let's check `gateway.md` YAML examples:
	// `glm-4.7` sets `tools: true, vision: false, thinking: true, structuredOutput: true, promptCache: true`.
	// `typesafe-jev` sets `structuredOutput: true`.
	// Let's keep it clean: if `ms.Capabilities` has zero value (`ms.Capabilities == (config.Capabilities{})`), only `NeedVision` rejects (since vision is special); otherwise check each required flag strictly! That way minimal test plans without a `capabilities` block still work for text/tools, while any plan declaring capabilities enforces them strictly!
	if ms == nil {
		return ""
	}
	if ms.Capabilities == (config.Capabilities{}) {
		if sig.NeedVision {
			return "model does not declare vision capability"
		}
		return ""
	}
	if sig.NeedTools && !ms.Capabilities.Tools {
		return "model lacks tools capability"
	}
	if sig.NeedVision && !ms.Capabilities.Vision {
		return "model lacks vision capability"
	}
	if sig.NeedThinking && !ms.Capabilities.Thinking {
		return "model lacks thinking capability"
	}
	if sig.NeedStructuredOutput && !ms.Capabilities.StructuredOutput {
		return "model lacks structuredOutput capability"
	}
	return ""
}

func effectiveCacheTTL(rt *config.Route, plan *config.Plan, ms *config.ModelSpec) time.Duration {
	if rt != nil && rt.Policy.CacheTTLOverride != "" {
		if d, err := config.ParseDuration(rt.Policy.CacheTTLOverride); err == nil && d > 0 {
			return d
		}
	}
	return ms.EffectiveCacheTTL(plan)
}

func computeCacheGravity(warmPlan, warmModel, candPlan, candModel string, warm SessionCacheEntry, sig Signals, ttl time.Duration, now time.Time) float64 {
	if warmPlan == "" || warmPlan != candPlan || warmModel != candModel {
		return 0.0
	}
	age := now.Sub(warm.LastSuccessAt)
	if age < 0 {
		age = 0
	}
	if age >= ttl {
		return 0.0
	}
	if sig.TurnPhase == PhaseToolLoop {
		return 10.0
	}
	if sig.TurnPhase != PhaseNewUserTurn {
		return 0.0
	}
	overlap := 1.0
	if warm.WarmPrefixTokens > 0 && sig.EstimatedInputTokens > 0 {
		overlap = clamp01(float64(warm.WarmPrefixTokens) / float64(sig.EstimatedInputTokens))
	}
	freshness := math.Max(0.15, 1.0-float64(age)/float64(ttl))
	return overlap * freshness
}

func computeQualityMatch(ms *config.ModelSpec, demandTier string) float64 {
	score := ms.EffectiveCodingScore()
	diff := config.TierRank(ms.EffectiveTier()) - config.TierRank(demandTier)
	switch {
	case diff == 0:
		return 1.0 + 0.2*score
	case diff > 0:
		return 0.7 + 0.2*score
	default:
		return -1.5
	}
}

func (r *Router) computeQuotaScore(plan *config.Plan, now time.Time) (urgency float64, blockedHard bool, blockedSoft bool) {
	windows := r.ComputeWindowUsage(plan, now)
	if len(windows) == 0 {
		return 0.0, false, false
	}

	minSurplus := 0.0
	hasDeficit := false
	maxWeightedSurplus := -1e9

	for _, w := range windows {
		if w.RemainingRatio <= 0 {
			if w.Mode == "hard" {
				blockedHard = true
			} else {
				blockedSoft = true
			}
		}
		if w.Surplus < -0.15 {
			if !hasDeficit || w.Surplus < minSurplus {
				minSurplus = w.Surplus
				hasDeficit = true
			}
		}
		weighted := w.Surplus * (1.0 - 0.5*w.TimeLeftRatio)
		if weighted > maxWeightedSurplus {
			maxWeightedSurplus = weighted
		}
	}
	if hasDeficit {
		return minSurplus, blockedHard, blockedSoft
	}
	if maxWeightedSurplus > -1e8 {
		return maxWeightedSurplus, blockedHard, blockedSoft
	}
	return 0.0, blockedHard, blockedSoft
}

const (
	expiryBoostMax         = 0.2
	expiryBoostHorizon     = 24 * time.Hour
	expiryBoostWeekHorizon = 48 * time.Hour
	quotaExhaustedUsagePct = 99.0
)

// expiringQuotaBoost scores burnable quota that is about to disappear. It is
// additive on top of QuotaUrgency and scales both by time-to-expiry (closer
// expiry means more urgency) and by remaining usable quota (quota already
// spent cannot be rescued). An exhausted five-hour window always scores zero:
// no amount of priority can burn quota that is not there, and boosting it
// would only starve healthy plans.
func expiringQuotaBoost(q *quota.RemoteQuota, now time.Time) float64 {
	if q == nil || q.Stale || fiveHourExhausted(q) {
		return 0
	}
	boost := 0.0
	if q.ResetsRemaining > 0 && q.SoonestResetExpiry != nil {
		if factor := expiryTimeFactor(*q.SoonestResetExpiry, now, expiryBoostHorizon); factor > 0 {
			if b := expiryBoostMax * factor * remainingUsableRatio(q); b > boost {
				boost = b
			}
		}
	}
	for _, w := range q.Windows {
		if w.ResetAt == nil || w.Trend == nil {
			continue
		}
		horizon := expiryBoostHorizon
		if w.Name == "Weekly" {
			horizon = expiryBoostWeekHorizon
		}
		if expiryTimeFactor(*w.ResetAt, now, horizon) <= 0 {
			continue
		}
		underutilized := w.Trend.Status == "underutilized" || (w.Trend.ProjectedEndPct != nil && *w.Trend.ProjectedEndPct < 60)
		if !underutilized || (w.UsagePct != nil && *w.UsagePct >= quotaExhaustedUsagePct) {
			continue
		}
		factor := expiryTimeFactor(*w.ResetAt, now, horizon)
		if b := expiryBoostMax * factor * windowRemainingRatio(w); b > boost {
			boost = b
		}
	}
	return boost
}

// expiryTimeFactor maps a future expiry onto [0,1]; 0 means outside the
// horizon or already expired.
func expiryTimeFactor(expiry, now time.Time, horizon time.Duration) float64 {
	if horizon <= 0 || !expiry.After(now) {
		return 0
	}
	left := expiry.Sub(now)
	if left > horizon {
		return 0
	}
	return clamp01(1 - float64(left)/float64(horizon))
}

func isFiveHourWindow(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "5 hours", "5 hour", "5h", "5-hour", "five hour", "five hours", "fivehour":
		return true
	}
	return false
}

// fiveHourUsagePct returns the provider-reported five-hour usage. When the
// provider reports no named windows, the aggregate usage is the only signal.
func fiveHourUsagePct(q *quota.RemoteQuota) (float64, bool) {
	if q == nil {
		return 0, false
	}
	for _, w := range q.Windows {
		if !isFiveHourWindow(w.Name) {
			continue
		}
		if w.UsagePct != nil {
			return clampRange(*w.UsagePct, 0, 100), true
		}
		if w.Trend != nil && w.Trend.RemainingPct != nil {
			return clampRange(100-*w.Trend.RemainingPct, 0, 100), true
		}
	}
	if len(q.Windows) == 0 && q.UsagePct != nil {
		return clampRange(*q.UsagePct, 0, 100), true
	}
	return 0, false
}

// fiveHourExhausted reports whether the plan's five-hour window has no usable
// quota left (the provider's quota_exhausted state).
func fiveHourExhausted(q *quota.RemoteQuota) bool {
	if q == nil {
		return false
	}
	for _, w := range q.Windows {
		if !isFiveHourWindow(w.Name) {
			continue
		}
		if w.Trend != nil && w.Trend.Status == "exhausted" {
			return true
		}
		if w.UsagePct != nil && *w.UsagePct >= quotaExhaustedUsagePct {
			return true
		}
	}
	return len(q.Windows) == 0 && q.UsagePct != nil && *q.UsagePct >= quotaExhaustedUsagePct
}

// remainingUsableRatio returns the fraction of the five-hour window still
// available. Unknown usage is treated as fully available so time-to-expiry
// alone can still break ties.
func remainingUsableRatio(q *quota.RemoteQuota) float64 {
	used, ok := fiveHourUsagePct(q)
	if !ok {
		return 1
	}
	return clamp01(1 - used/100)
}

func windowRemainingRatio(w quota.QuotaWindow) float64 {
	switch {
	case w.UsagePct != nil:
		return clamp01(1 - *w.UsagePct/100)
	case w.Trend != nil && w.Trend.RemainingPct != nil:
		return clamp01(*w.Trend.RemainingPct / 100)
	case w.Trend != nil && w.Trend.ProjectedEndPct != nil:
		return clamp01(1 - *w.Trend.ProjectedEndPct/100)
	default:
		return 1
	}
}

// ComputeWindowUsage calculates current remainingRatio, timeLeftRatio, and surplus
// across all declared quota windows (and Tier-1 header observations) for plan.
func (r *Router) ComputeWindowUsage(plan *config.Plan, now time.Time) []shared.GatewayWindowUsage {
	if plan == nil {
		return nil
	}
	var hdr ledger.HeaderSnapshot
	hasHdr := false
	if r.Ledger != nil && r.Ledger.Calibrator != nil {
		hdr, hasHdr = r.Ledger.Calibrator.HeaderQuota(plan.ID, now)
	}

	out := make([]shared.GatewayWindowUsage, 0, len(plan.Limits.Windows)+1)
	for _, w := range plan.Limits.Windows {
		dur := w.DurationVal()
		if dur <= 0 {
			continue
		}
		since, resetAt, timeLeftRatio := windowBounds(w, dur, now)
		var usedReqs, usedToks int64
		if r.Ledger != nil {
			usedReqs, usedToks, _ = r.Ledger.WindowTotals(plan.ID, since)
		}
		source := "ledger"
		remRatio := 1.0
		if w.MaxTokens > 0 {
			rem := clamp01(1.0 - float64(usedToks)/float64(w.MaxTokens))
			if rem < remRatio {
				remRatio = rem
			}
		}
		if w.MaxRequests > 0 {
			rem := clamp01(1.0 - float64(usedReqs)/float64(w.MaxRequests))
			if rem < remRatio {
				remRatio = rem
			}
		}
		// Apply Tier-3 reactive AIMD cap if plan recently hit 429s.
		if r.Ledger != nil && r.Ledger.Calibrator != nil {
			capFactor := r.Ledger.Calibrator.EffectiveCapFactor(plan.ID, now)
			if capFactor < 1.0 {
				remRatio *= capFactor
			}
		}
		// Prefer Tier-1 exact headers for short/live rate-limit windows when stricter.
		if hasHdr && dur <= time.Hour {
			if hdr.HasTokens && hdr.TokensLimit > 0 {
				hRem := clamp01(float64(hdr.TokensRemaining) / float64(hdr.TokensLimit))
				if hRem < remRatio {
					remRatio = hRem
					source = "header"
				}
				if !hdr.TokensResetAt.IsZero() && hdr.TokensResetAt.After(now) {
					resetAt = hdr.TokensResetAt
					timeLeftRatio = clampRange(float64(resetAt.Sub(now))/float64(dur), 0.01, 1.0)
				}
			}
		}

		resStr := ""
		if !resetAt.IsZero() {
			resStr = resetAt.UTC().Format(time.RFC3339)
		}
		out = append(out, shared.GatewayWindowUsage{
			Name:           w.Name,
			Duration:       w.Duration,
			Mode:           w.EffectiveMode(),
			UsedTokens:     usedToks,
			MaxTokens:      w.MaxTokens,
			UsedRequests:   usedReqs,
			MaxRequests:    w.MaxRequests,
			RemainingRatio: remRatio,
			TimeLeftRatio:  timeLeftRatio,
			Surplus:        remRatio - timeLeftRatio,
			ResetAt:        resStr,
			Source:         source,
		})
	}

	if len(out) == 0 && hasHdr {
		remRatio := 1.0
		var resetAt time.Time
		if hdr.HasTokens && hdr.TokensLimit > 0 {
			remRatio = clamp01(float64(hdr.TokensRemaining) / float64(hdr.TokensLimit))
			resetAt = hdr.TokensResetAt
		} else if hdr.HasRequests && hdr.RequestsLimit > 0 {
			remRatio = clamp01(float64(hdr.RequestsRemaining) / float64(hdr.RequestsLimit))
			resetAt = hdr.RequestsResetAt
		}
		timeLeftRatio := 0.5
		if !resetAt.IsZero() && resetAt.After(now) {
			timeLeftRatio = clampRange(float64(resetAt.Sub(now))/float64(time.Minute), 0.01, 1.0)
		}
		resStr := ""
		if !resetAt.IsZero() {
			resStr = resetAt.UTC().Format(time.RFC3339)
		}
		out = append(out, shared.GatewayWindowUsage{
			Name:           "upstream-ratelimit",
			Duration:       "1m",
			Mode:           "soft",
			RemainingRatio: remRatio,
			TimeLeftRatio:  timeLeftRatio,
			Surplus:        remRatio - timeLeftRatio,
			ResetAt:        resStr,
			Source:         "header",
		})
	}
	return out
}

func windowBounds(w config.WindowSpec, dur time.Duration, now time.Time) (since, resetAt time.Time, timeLeftRatio float64) {
	sched := strings.ToLower(strings.TrimSpace(w.ResetSchedule))
	switch sched {
	case "hourly":
		since = now.UTC().Truncate(time.Hour)
		resetAt = since.Add(time.Hour)
		return since, resetAt, clampRange(float64(resetAt.Sub(now))/float64(time.Hour), 0.01, 1.0)
	case "daily":
		y, m, d := now.UTC().Date()
		since = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		resetAt = since.Add(24 * time.Hour)
		return since, resetAt, clampRange(float64(resetAt.Sub(now))/float64(24*time.Hour), 0.01, 1.0)
	case "weekly":
		y, m, d := now.UTC().Date()
		today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		offset := (int(now.UTC().Weekday()) + 6) % 7 // Monday start
		since = today.AddDate(0, 0, -offset)
		resetAt = since.Add(7 * 24 * time.Hour)
		return since, resetAt, clampRange(float64(resetAt.Sub(now))/float64(7*24*time.Hour), 0.01, 1.0)
	case "fixed":
		step := dur
		unix := now.UTC().UnixNano()
		rem := unix % int64(step)
		since = now.UTC().Add(-time.Duration(rem))
		resetAt = since.Add(step)
		return since, resetAt, clampRange(float64(resetAt.Sub(now))/float64(step), 0.01, 1.0)
	default: // "rolling" or empty
		return now.UTC().Add(-dur), time.Time{}, 0.5
	}
}

type objectiveWeights struct {
	quality  float64
	cache    float64
	quota    float64
	protocol float64
}

// Target weight is a bounded additive bias. Its maximum spread is 2.4,
// below the minimum matched-versus-under-tier quality gap of 2.5 in the
// burn-expiring preset. Scale it down with quality overrides below 1; a zero
// quality weight disables the bias (including strict-failover).
const (
	weightBiasScale = 1.2
	weightBiasMin   = 0.01
	weightBiasMax   = 100.0
)

// targetWeightBias applies the documented target-weight formula. Weights
// outside the schema range are clamped; NaN and the zero value are neutral.
func targetWeightBias(w float64) float64 {
	if math.IsNaN(w) || w <= 0 {
		return 0
	}
	return weightBiasScale * math.Log(clampRange(w, weightBiasMin, weightBiasMax)) / math.Log(weightBiasMax)
}

func effectiveWeights(p config.RoutePolicy) objectiveWeights {
	var w objectiveWeights
	switch p.EffectivePreset() {
	case config.PresetQualityFirst:
		w = objectiveWeights{quality: 2.5, cache: 1.8, quota: 0.6, protocol: 0.2}
	case config.PresetBurnExpiring:
		w = objectiveWeights{quality: 1.0, cache: 1.5, quota: 2.5, protocol: 0.2}
	case config.PresetStrictFailover:
		w = objectiveWeights{quality: 0.0, cache: 1.0, quota: 0.0, protocol: 0.1}
	default: // balanced
		w = objectiveWeights{quality: 1.5, cache: 2.0, quota: 1.2, protocol: 0.3}
	}
	if p.Weights.Quality != nil {
		w.quality = *p.Weights.Quality
	}
	if p.Weights.Cache != nil {
		w.cache = *p.Weights.Cache
	}
	if p.Weights.Quota != nil {
		w.quota = *p.Weights.Quota
	}
	if p.Weights.Protocol != nil {
		w.protocol = *p.Weights.Protocol
	}
	return w
}

func buildRouteDecision(rt *config.Route, resolution string, sig Signals, demandTier string, semanticUsed bool, warmAgeSec int64, maxGravity float64, cands []Candidate) *shared.RouteDecision {
	items := make([]shared.CandidateDecision, 0, len(cands))
	for _, c := range cands {
		planID := c.Target.Plan
		if c.Plan != nil {
			planID = c.Plan.ID
		}
		items = append(items, shared.CandidateDecision{
			Plan:           planID,
			Model:          c.Target.Model,
			Protocol:       c.EndpointProto,
			NativeProtocol: c.NativeProtocol,
			Eligible:       c.Eligible,
			RejectReason:   c.RejectReason,
			Scores:         c.Scores,
		})
	}
	return &shared.RouteDecision{
		RouteID:          rt.ID,
		Resolution:       resolution,
		TurnPhase:        sig.TurnPhase,
		SessionKey:       sig.SessionKey,
		DemandTier:       demandTier,
		SemanticTierUsed: semanticUsed,
		CacheWarmAgeSec:  warmAgeSec,
		CacheGravityMax:  maxGravity,
		Candidates:       items,
	}
}

func matchListOrEmpty(patterns []string, val string) bool {
	if len(patterns) == 0 {
		return val == "" || len(patterns) == 0
	}
	return containsStr(patterns, val)
}

func matchModelAny(patterns []string, model string) bool {
	for _, pat := range patterns {
		if pat == "*" || pat == model || wildcardMatch(pat, model) || wildcardMatch(pat, NormalizeModelName(model)) {
			return true
		}
	}
	return false
}

// wildcardMatch matches '*' across all characters (including '/') and '?' as a single character.
func wildcardMatch(pattern, s string) bool {
	pi, si := 0, 0
	starIdx, matchIdx := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			starIdx = pi
			matchIdx = si
			pi++
		case starIdx != -1:
			pi = starIdx + 1
			matchIdx++
			si = matchIdx
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func clamp01(v float64) float64 {
	return clampRange(v, 0.0, 1.0)
}

func clampRange(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
