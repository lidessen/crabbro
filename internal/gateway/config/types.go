// Package config loads, validates, stages, and atomically publishes
// gateway plans and routes as immutable snapshots.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/shared"
)

const (
	KindPlan  = "gateway-plan"
	KindRoute = "gateway-route"

	ProtocolOpenAIChat      = "openai-chat"
	ProtocolOpenAIResponses = "openai-responses"
	ProtocolAnthropic       = "anthropic"
	ProtocolGemini          = "gemini"
	ProtocolTypeSafeJev     = "typesafe-jev"
	ProtocolOpenAIDecisions = "openai-decisions"
	ProtocolGeminiDecisions = "gemini-decisions"

	FamilyGenerative = "generative"
	FamilyDecision   = "decision"

	TierFast     = "fast"
	TierStrong   = "strong"
	TierFrontier = "frontier"

	PresetBalanced       = "balanced"
	PresetQualityFirst   = "quality-first"
	PresetBurnExpiring   = "burn-expiring"
	PresetStrictFailover = "strict-failover"

	MaxFileBytes = 256 * 1024
	MaxPlans     = 64
	MaxRoutes    = 64

	DefaultTTFTTimeout = 5 * time.Minute
	DefaultIdleTimeout = 2 * time.Minute
	DefaultCacheTTL    = 5 * time.Minute
	DefaultTierTimeout = 800 * time.Millisecond
)

// AllProtocols lists the six supported wire protocols.
var AllProtocols = []string{
	ProtocolOpenAIChat,
	ProtocolOpenAIResponses,
	ProtocolAnthropic,
	ProtocolGemini,
	ProtocolTypeSafeJev,
	ProtocolOpenAIDecisions,
	ProtocolGeminiDecisions,
}

// ProtocolFamily returns the semantic family ("generative" or "decision") for proto.
func ProtocolFamily(proto string) string {
	if proto == ProtocolTypeSafeJev || proto == ProtocolOpenAIDecisions || proto == ProtocolGeminiDecisions {
		return FamilyDecision
	}
	for _, p := range AllProtocols {
		if p == proto {
			return FamilyGenerative
		}
	}
	return ""
}

// TierRank maps model tiers onto a comparable ordinal [1..3].
func TierRank(tier string) int {
	switch tier {
	case TierFast:
		return 1
	case TierStrong:
		return 2
	case TierFrontier:
		return 3
	default:
		return 2
	}
}

// ParseDuration parses Crabbro duration strings supporting ms, s, m, h, and d (24h).
func ParseDuration(raw string) (time.Duration, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if strings.HasSuffix(s, "d") {
		nStr := strings.TrimSuffix(s, "d")
		n, err := strconv.ParseInt(nStr, 10, 64)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("invalid day duration %q", raw)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// Plan configures one API-key coding plan or decision backend.
type Plan struct {
	Version     int                      `yaml:"version" json:"version"`
	ID          string                   `yaml:"id" json:"id"`
	Enabled     *bool                    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Direct      *DirectSpec              `yaml:"direct,omitempty" json:"direct,omitempty"`
	Description string                   `yaml:"description,omitempty" json:"description,omitempty"`
	Credential  string                   `yaml:"credential" json:"credential"`
	ProxyURL    string                   `yaml:"proxyUrl,omitempty" json:"proxyUrl,omitempty"`
	Timeout     TimeoutSpec              `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	Cache       CacheSpec                `yaml:"cache,omitempty" json:"cache,omitempty"`
	Limits      LimitsSpec               `yaml:"limits,omitempty" json:"limits,omitempty"`
	AutoReset   AutoResetSpec            `yaml:"autoReset,omitempty" json:"autoReset,omitempty"`
	Endpoints   map[string]*EndpointSpec `yaml:"endpoints" json:"endpoints"`
	Models      map[string]*ModelSpec    `yaml:"models" json:"models"`
}

// IsEnabled reports whether the plan is active (defaults to true when omitted).
func (p *Plan) IsEnabled() bool {
	return p != nil && (p.Enabled == nil || *p.Enabled)
}

// DirectSpec controls direct plan addressing.
type DirectSpec struct {
	Enabled  *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Profiles []string `yaml:"profiles,omitempty" json:"profiles,omitempty"`
}

// DirectEnabled reports whether direct addressing is available.
func (p *Plan) DirectEnabled() bool {
	return p.IsEnabled() && (p.Direct == nil || p.Direct.Enabled == nil || *p.Direct.Enabled)
}

// AllowsDirectProfile reports whether profile may directly address this plan.
func (p *Plan) AllowsDirectProfile(profile string) bool {
	if p == nil {
		return false
	}
	if p.Direct == nil || len(p.Direct.Profiles) == 0 {
		return true
	}
	for _, v := range p.Direct.Profiles {
		if v == profile {
			return true
		}
	}
	return false
}

// TimeoutSpec configures upstream time budgets for a plan.
type TimeoutSpec struct {
	TTFT string `yaml:"ttft,omitempty" json:"ttft,omitempty"`
	Idle string `yaml:"idle,omitempty" json:"idle,omitempty"`
}

// TTFTDuration returns the configured time-to-first-token timeout or the default.
func (t TimeoutSpec) TTFTDuration() time.Duration {
	if t.TTFT == "" {
		return DefaultTTFTTimeout
	}
	if d, err := ParseDuration(t.TTFT); err == nil && d > 0 {
		return d
	}
	return DefaultTTFTTimeout
}

// IdleDuration returns the configured idle stream timeout or the default.
func (t TimeoutSpec) IdleDuration() time.Duration {
	if t.Idle == "" {
		return DefaultIdleTimeout
	}
	if d, err := ParseDuration(t.Idle); err == nil && d > 0 {
		return d
	}
	return DefaultIdleTimeout
}

// CacheSpec declares prompt-cache TTL defaults for a plan.
type CacheSpec struct {
	TTL string `yaml:"ttl,omitempty" json:"ttl,omitempty"`
}

// TTLDuration returns the configured prompt-cache TTL or DefaultCacheTTL.
func (c CacheSpec) TTLDuration() time.Duration {
	if c.TTL == "" {
		return DefaultCacheTTL
	}
	if d, err := ParseDuration(c.TTL); err == nil && d > 0 {
		return d
	}
	return DefaultCacheTTL
}

// AutoResetSpec opts a plan into automatic provider reset-coupon redemption.
// It is strictly opt-in: without enabled=true the gateway never redeems
// coupons on its own. Reset coupons are provider-private; no coupon
// identifier ever leaves the quota package.
type AutoResetSpec struct {
	Enabled        bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Scopes         []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
	ExpiringWithin string   `yaml:"expiringWithin,omitempty" json:"expiringWithin,omitempty"`
	MinInterval    string   `yaml:"minInterval,omitempty" json:"minInterval,omitempty"`
}

// AutoResetScopeFiveHour and AutoResetScopeWeekly name the redeemable windows.
const (
	AutoResetScopeFiveHour = "fiveHour"
	AutoResetScopeWeekly   = "weekly"
)

// EffectiveAutoResetScopes returns the configured scopes (deduplicated),
// defaulting to both supported windows when omitted.
func (a AutoResetSpec) EffectiveAutoResetScopes() []string {
	var out []string
	for _, s := range a.Scopes {
		if s != AutoResetScopeFiveHour && s != AutoResetScopeWeekly {
			continue
		}
		dup := false
		for _, existing := range out {
			if existing == s {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{AutoResetScopeFiveHour, AutoResetScopeWeekly}
	}
	return out
}

// AllowsAutoResetScope reports whether scope may be auto-redeemed.
func (a AutoResetSpec) AllowsAutoResetScope(scope string) bool {
	for _, s := range a.EffectiveAutoResetScopes() {
		if s == scope {
			return true
		}
	}
	return false
}

// ExpiringWithinDuration returns the coupon expiry horizon. Zero means any
// available coupon qualifies regardless of when it expires.
func (a AutoResetSpec) ExpiringWithinDuration() time.Duration {
	if a.ExpiringWithin == "" {
		return 0
	}
	if d, err := ParseDuration(a.ExpiringWithin); err == nil && d > 0 {
		return d
	}
	return 0
}

// MinIntervalDuration returns the anti-double-spend cooldown between automatic
// redemptions, defaulting to 5m.
func (a AutoResetSpec) MinIntervalDuration() time.Duration {
	if a.MinInterval == "" {
		return 5 * time.Minute
	}
	if d, err := ParseDuration(a.MinInterval); err == nil && d > 0 {
		return d
	}
	return 5 * time.Minute
}

// LimitsSpec configures rate and rolling-window budgets for a plan.
type LimitsSpec struct {
	RPM         int          `yaml:"rpm,omitempty" json:"rpm,omitempty"`
	TPM         int          `yaml:"tpm,omitempty" json:"tpm,omitempty"`
	Concurrency int          `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
	Windows     []WindowSpec `yaml:"windows,omitempty" json:"windows,omitempty"`
}

// WindowSpec configures one rolling or fixed quota window.
type WindowSpec struct {
	Name          string `yaml:"name,omitempty" json:"name,omitempty"`
	Duration      string `yaml:"duration" json:"duration"`
	MaxTokens     int64  `yaml:"maxTokens,omitempty" json:"maxTokens,omitempty"`
	MaxRequests   int64  `yaml:"maxRequests,omitempty" json:"maxRequests,omitempty"`
	ResetSchedule string `yaml:"resetSchedule,omitempty" json:"resetSchedule,omitempty"`
	Mode          string `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// DurationVal parses Duration or returns 0 on error.
func (w WindowSpec) DurationVal() time.Duration {
	d, _ := ParseDuration(w.Duration)
	return d
}

// EffectiveMode returns "soft" or "hard".
func (w WindowSpec) EffectiveMode() string {
	if w.Mode == "hard" {
		return "hard"
	}
	return "soft"
}

// EndpointSpec configures one wire-protocol endpoint for a plan.
type EndpointSpec struct {
	BaseURL    string            `yaml:"baseUrl" json:"baseUrl"`
	AuthHeader string            `yaml:"authHeader,omitempty" json:"authHeader,omitempty"`
	AuthPrefix string            `yaml:"authPrefix,omitempty" json:"authPrefix,omitempty"`
	Headers    map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	APIVersion string            `yaml:"apiVersion,omitempty" json:"apiVersion,omitempty"`
}

// ModelSpec describes a model offered on a plan.
type ModelSpec struct {
	Endpoints       []string       `yaml:"endpoints,omitempty" json:"endpoints,omitempty"`
	Tier            string         `yaml:"tier,omitempty" json:"tier,omitempty"`
	CodingScore     *float64       `yaml:"codingScore,omitempty" json:"codingScore,omitempty"`
	ContextWindow   int            `yaml:"contextWindow,omitempty" json:"contextWindow,omitempty"`
	MaxOutputTokens int            `yaml:"maxOutputTokens,omitempty" json:"maxOutputTokens,omitempty"`
	CacheTTL        string         `yaml:"cacheTtl,omitempty" json:"cacheTtl,omitempty"`
	Capabilities    Capabilities   `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	ExtraBody       map[string]any `yaml:"extraBody,omitempty" json:"extraBody,omitempty"`
}

// EffectiveTier returns the model tier, defaulting to strong.
func (m *ModelSpec) EffectiveTier() string {
	if m == nil || m.Tier == "" {
		return TierStrong
	}
	return m.Tier
}

// EffectiveCodingScore returns the coding score [0,1], defaulting to 0.7.
func (m *ModelSpec) EffectiveCodingScore() float64 {
	if m == nil || m.CodingScore == nil {
		return 0.7
	}
	return *m.CodingScore
}

// EffectiveCacheTTL resolves model TTL -> plan TTL -> default TTL.
func (m *ModelSpec) EffectiveCacheTTL(plan *Plan) time.Duration {
	if m != nil && m.CacheTTL != "" {
		if d, err := ParseDuration(m.CacheTTL); err == nil && d > 0 {
			return d
		}
	}
	if plan != nil {
		return plan.Cache.TTLDuration()
	}
	return DefaultCacheTTL
}

// Capabilities declares feature support for a model.
type Capabilities struct {
	Tools            bool `yaml:"tools,omitempty" json:"tools,omitempty"`
	Vision           bool `yaml:"vision,omitempty" json:"vision,omitempty"`
	Thinking         bool `yaml:"thinking,omitempty" json:"thinking,omitempty"`
	StructuredOutput bool `yaml:"structuredOutput,omitempty" json:"structuredOutput,omitempty"`
	PromptCache      bool `yaml:"promptCache,omitempty" json:"promptCache,omitempty"`
}

// Route maps incoming client requests onto an ordered pool of plan/model targets.
type Route struct {
	Version     int         `yaml:"version" json:"version"`
	ID          string      `yaml:"id" json:"id"`
	Enabled     *bool       `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Description string      `yaml:"description,omitempty" json:"description,omitempty"`
	Priority    int         `yaml:"priority,omitempty" json:"priority,omitempty"`
	Family      string      `yaml:"family,omitempty" json:"family,omitempty"`
	Match       RouteMatch  `yaml:"match" json:"match"`
	Policy      RoutePolicy `yaml:"policy,omitempty" json:"policy,omitempty"`
	Targets     []Target    `yaml:"targets" json:"targets"`
}

// IsEnabled reports whether the route is active (defaults to true when omitted).
func (r *Route) IsEnabled() bool {
	return r != nil && (r.Enabled == nil || *r.Enabled)
}

// EffectiveFamily returns "generative" or "decision".
func (r *Route) EffectiveFamily() string {
	if r != nil && r.Family == FamilyDecision {
		return FamilyDecision
	}
	return FamilyGenerative
}

// RouteMatch selects requests by profile, protocol, and requested model glob.
type RouteMatch struct {
	Profiles  []string `yaml:"profiles,omitempty" json:"profiles,omitempty"`
	Protocols []string `yaml:"protocols,omitempty" json:"protocols,omitempty"`
	Models    []string `yaml:"models,omitempty" json:"models,omitempty"`
}

// RoutePolicy configures scheduling weights, translation, failover, and tiering.
type RoutePolicy struct {
	Preset               string              `yaml:"preset,omitempty" json:"preset,omitempty"`
	Weights              WeightsSpec         `yaml:"weights,omitempty" json:"weights,omitempty"`
	PreferNativeProtocol *bool               `yaml:"preferNativeProtocol,omitempty" json:"preferNativeProtocol,omitempty"`
	AllowCrossProtocol   *bool               `yaml:"allowCrossProtocol,omitempty" json:"allowCrossProtocol,omitempty"`
	AllowEmulation       bool                `yaml:"allowEmulation,omitempty" json:"allowEmulation,omitempty"`
	MaxAttempts          int                 `yaml:"maxAttempts,omitempty" json:"maxAttempts,omitempty"`
	CacheTTLOverride     string              `yaml:"cacheTtlOverride,omitempty" json:"cacheTtlOverride,omitempty"`
	SemanticTiering      SemanticTieringSpec `yaml:"semanticTiering,omitempty" json:"semanticTiering,omitempty"`
}

// EffectivePreset returns the route's preset name, defaulting to balanced.
func (p RoutePolicy) EffectivePreset() string {
	if p.Preset == "" {
		return PresetBalanced
	}
	return p.Preset
}

// PreferNative reports whether same-protocol endpoints are preferred.
func (p RoutePolicy) PreferNative() bool {
	return p.PreferNativeProtocol == nil || *p.PreferNativeProtocol
}

// CrossProtocolAllowed reports whether cross-protocol GenIR translation is allowed.
func (p RoutePolicy) CrossProtocolAllowed() bool {
	return p.AllowCrossProtocol == nil || *p.AllowCrossProtocol
}

// EffectiveMaxAttempts returns MaxAttempts or default 3.
func (p RoutePolicy) EffectiveMaxAttempts() int {
	if p.MaxAttempts <= 0 {
		return 3
	}
	return p.MaxAttempts
}

// WeightsSpec overrides individual preset objective weights.
type WeightsSpec struct {
	Quality  *float64 `yaml:"quality,omitempty" json:"quality,omitempty"`
	Cache    *float64 `yaml:"cache,omitempty" json:"cache,omitempty"`
	Quota    *float64 `yaml:"quota,omitempty" json:"quota,omitempty"`
	Protocol *float64 `yaml:"protocol,omitempty" json:"protocol,omitempty"`
}

// SemanticTieringSpec configures optional cold-boundary demand classification.
type SemanticTieringSpec struct {
	Enabled         bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	DecisionRoute   string   `yaml:"decisionRoute,omitempty" json:"decisionRoute,omitempty"`
	Timeout         string   `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MinConfidence   *float64 `yaml:"minConfidence,omitempty" json:"minConfidence,omitempty"`
	MaxCacheGravity *float64 `yaml:"maxCacheGravity,omitempty" json:"maxCacheGravity,omitempty"`
}

// EffectiveDecisionRoute returns the route ID for tiering, defaulting to "decision".
func (s SemanticTieringSpec) EffectiveDecisionRoute() string {
	if s.DecisionRoute == "" {
		return "decision"
	}
	return s.DecisionRoute
}

// TimeoutDuration returns the configured tiering deadline or DefaultTierTimeout.
func (s SemanticTieringSpec) TimeoutDuration() time.Duration {
	if s.Timeout == "" {
		return DefaultTierTimeout
	}
	if d, err := ParseDuration(s.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultTierTimeout
}

// EffectiveMinConfidence returns MinConfidence or 0.7.
func (s SemanticTieringSpec) EffectiveMinConfidence() float64 {
	if s.MinConfidence == nil {
		return 0.7
	}
	return *s.MinConfidence
}

// EffectiveMaxCacheGravity returns MaxCacheGravity or 0.35.
func (s SemanticTieringSpec) EffectiveMaxCacheGravity() float64 {
	if s.MaxCacheGravity == nil {
		return 0.35
	}
	return *s.MaxCacheGravity
}

// Target names one (plan, model) destination in a Route.
type Target struct {
	Plan             string   `yaml:"plan" json:"plan"`
	Model            string   `yaml:"model" json:"model"`
	EndpointProtocol string   `yaml:"endpointProtocol,omitempty" json:"endpointProtocol,omitempty"`
	Priority         int      `yaml:"priority,omitempty" json:"priority,omitempty"`
	Weight           float64  `yaml:"weight,omitempty" json:"weight,omitempty"`
	Enabled          *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	MinContextTokens int      `yaml:"minContextTokens,omitempty" json:"minContextTokens,omitempty"`
	MaxContextTokens int      `yaml:"maxContextTokens,omitempty" json:"maxContextTokens,omitempty"`
	WhenTier         []string `yaml:"whenTier,omitempty" json:"whenTier,omitempty"`
}

// IsEnabled reports whether this target is active (defaults to true).
func (t Target) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

// Snapshot is an immutable, validated view of all loaded gateway plans and routes.
type Snapshot struct {
	Generation  uint64
	Plans       map[string]*Plan // enabled plans
	AllPlans    map[string]*Plan // all parsed plans (enabled and disabled)
	Routes      []*Route         // enabled routes sorted ascending by Priority, ID
	AllRoutes   []*Route         // all parsed routes
	Diagnostics []shared.Diagnostic
}

// FileStatus reports live/draft status and diagnostics for one gateway config file.
type FileStatus struct {
	Kind        string              `json:"kind"`
	ID          string              `json:"id"`
	Live        bool                `json:"live"`
	Draft       bool                `json:"draft"`
	Enabled     bool                `json:"enabled"`
	Diagnostics []shared.Diagnostic `json:"diagnostics,omitempty"`
}
