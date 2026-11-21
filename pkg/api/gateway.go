package api

import "time"

// TokenUsage records token counters reported by or tapped from an LLM backend.
type TokenUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64 `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens  int64 `json:"reasoningTokens,omitempty"`
}

// Total returns InputTokens + OutputTokens + CacheReadTokens + CacheWriteTokens + ReasoningTokens.
func (u TokenUsage) Total() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.ReasoningTokens
}

// CandidateScores records the multi-objective utility breakdown for one target.
type CandidateScores struct {
	Quality  float64 `json:"quality"`
	Cache    float64 `json:"cache"`
	Quota    float64 `json:"quota"`
	Protocol float64 `json:"protocol"`
	Priority float64 `json:"priority,omitempty"`
	Health   float64 `json:"health"`
	Total    float64 `json:"total"`
}

// CandidateDecision describes one candidate evaluated by the gateway router.
type CandidateDecision struct {
	Plan           string          `json:"plan"`
	Model          string          `json:"model"`
	Protocol       string          `json:"protocol"`
	NativeProtocol bool            `json:"nativeProtocol"`
	Eligible       bool            `json:"eligible"`
	RejectReason   string          `json:"rejectReason,omitempty"`
	Scores         CandidateScores `json:"scores"`
}

// RouteDecision captures the explainable routing outcome for a gateway exchange.
type RouteDecision struct {
	Resolution       string              `json:"resolution,omitempty"`
	RouteID          string              `json:"routeId"`
	TurnPhase        string              `json:"turnPhase"`
	SessionKey       string              `json:"sessionKey,omitempty"`
	DemandTier       string              `json:"demandTier"`
	SemanticTierUsed bool                `json:"semanticTierUsed"`
	CacheWarmAgeSec  int64               `json:"cacheWarmAgeSec,omitempty"`
	CacheGravityMax  float64             `json:"cacheGravityMax,omitempty"`
	Candidates       []CandidateDecision `json:"candidates,omitempty"`
}

// GatewayAttempt records a single upstream dispatch attempt during failover.
type GatewayAttempt struct {
	Plan            string `json:"plan"`
	Model           string `json:"model"`
	Protocol        string `json:"protocol"`
	Status          int    `json:"status,omitempty"`
	ErrorKind       string `json:"errorKind,omitempty"`
	ErrorMessage    string `json:"errorMessage,omitempty"`
	LatencyMs       int64  `json:"latencyMs,omitempty"`
	TTFTMs          int64  `json:"ttftMs,omitempty"`
	CooldownApplied string `json:"cooldownApplied,omitempty"`
}

// GatewayTrace is attached to captured gateway exchanges.
type GatewayTrace struct {
	Profile          string           `json:"profile,omitempty"`
	RouteID          string           `json:"routeId,omitempty"`
	Family           string           `json:"family,omitempty"`
	IngressProtocol  string           `json:"ingressProtocol,omitempty"`
	RequestedModel   string           `json:"requestedModel,omitempty"`
	ChosenPlan       string           `json:"chosenPlan,omitempty"`
	ChosenModel      string           `json:"chosenModel,omitempty"`
	UpstreamProtocol string           `json:"upstreamProtocol,omitempty"`
	Translated       bool             `json:"translated,omitempty"`
	DecisionMode     string           `json:"decisionMode,omitempty"`
	Calibrated       *bool            `json:"calibrated,omitempty"`
	TurnPhase        string           `json:"turnPhase,omitempty"`
	SessionKey       string           `json:"sessionKey,omitempty"`
	CacheGravity     float64          `json:"cacheGravity,omitempty"`
	RouteDecision    *RouteDecision   `json:"routeDecision,omitempty"`
	Attempts         []GatewayAttempt `json:"attempts,omitempty"`
	Usage            *TokenUsage      `json:"usage,omitempty"`
}

// SecretSummary describes a stored secret without revealing its value.
type SecretSummary struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updatedAt"`
	SizeBytes int64     `json:"sizeBytes"`
}

// SecretListResponse is returned by GET /v1/secrets.
type SecretListResponse struct {
	Secrets []SecretSummary `json:"secrets"`
}

// GatewayStatusResponse is returned by GET /v1/gateway/status.
type GatewayStatusResponse struct {
	Enabled            bool         `json:"enabled"`
	ListenAddr         string       `json:"listenAddr,omitempty"`
	SnapshotGeneration uint64       `json:"snapshotGeneration"`
	PlanCount          int          `json:"planCount"`
	RouteCount         int          `json:"routeCount"`
	HealthyPlans       int          `json:"healthyPlans"`
	CoolingPlans       int          `json:"coolingPlans"`
	Diagnostics        []Diagnostic `json:"diagnostics,omitempty"`
}

// GatewayWindowUsage reports rolling consumption against a declared plan window.
type GatewayWindowUsage struct {
	Name           string  `json:"name,omitempty"`
	Duration       string  `json:"duration"`
	Mode           string  `json:"mode,omitempty"`
	UsedTokens     int64   `json:"usedTokens"`
	MaxTokens      int64   `json:"maxTokens,omitempty"`
	UsedRequests   int64   `json:"usedRequests"`
	MaxRequests    int64   `json:"maxRequests,omitempty"`
	RemainingRatio float64 `json:"remainingRatio"`
	TimeLeftRatio  float64 `json:"timeLeftRatio"`
	Surplus        float64 `json:"surplus"`
	ResetAt        string  `json:"resetAt,omitempty"`
	Source         string  `json:"source"`
}

// RemoteQuotaWindow describes a provider-reported usage window.
type RemoteQuotaWindow struct {
	Name     string     `json:"name"`
	UsagePct *float64   `json:"usagePct,omitempty"`
	ResetAt  *time.Time `json:"resetAt,omitempty"`
}

// RemoteResetCoupon describes an available reset coupon.
type RemoteResetCoupon struct {
	Scope     string     `json:"scope"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// RemoteQuotaJSON is a sanitized provider-reported snapshot.
type RemoteQuotaJSON struct {
	PlanTier           string              `json:"planTier,omitempty"`
	Balance            string              `json:"balance,omitempty"`
	UsagePct           *float64            `json:"usagePct,omitempty"`
	ResetAt            *time.Time          `json:"resetAt,omitempty"`
	ExpiresAt          *time.Time          `json:"expiresAt,omitempty"`
	Windows            []RemoteQuotaWindow `json:"windows,omitempty"`
	Resets             []RemoteResetCoupon `json:"resets,omitempty"`
	ResetsRemaining    int                 `json:"resetsRemaining"`
	SoonestResetExpiry *time.Time          `json:"soonestResetExpiry,omitempty"`
	SyncSource         string              `json:"syncSource"`
	SyncedAt           *time.Time          `json:"syncedAt,omitempty"`
	Stale              bool                `json:"stale"`
	LastError          string              `json:"lastError,omitempty"`
}

// GatewayPlanStatus is a redacted view of one configured plan and its live state.
type GatewayPlanStatus struct {
	RemoteQuota    *RemoteQuotaJSON     `json:"remoteQuota,omitempty"`
	DirectEnabled  bool                 `json:"directEnabled"`
	DirectProfiles []string             `json:"directProfiles,omitempty"`
	ID             string               `json:"id"`
	Enabled        bool                 `json:"enabled"`
	Description    string               `json:"description,omitempty"`
	Credential     string               `json:"credential"`
	HasSecret      bool                 `json:"hasSecret"`
	Endpoints      []string             `json:"endpoints"`
	Models         []string             `json:"models"`
	Healthy        bool                 `json:"healthy"`
	CooldownUntil  *time.Time           `json:"cooldownUntil,omitempty"`
	CooldownScope  string               `json:"cooldownScope,omitempty"`
	LastError      string               `json:"lastError,omitempty"`
	InFlight       int                  `json:"inFlight"`
	ErrorEWMA      float64              `json:"errorEwma,omitempty"`
	LatencyEWMAMs  float64              `json:"latencyEwmaMs,omitempty"`
	Windows        []GatewayWindowUsage `json:"windows,omitempty"`
}

// GatewayPlanListResponse is returned by GET /v1/gateway/plans.
type GatewayPlanListResponse struct {
	Plans []GatewayPlanStatus `json:"plans"`
}

// GatewayRouteTargetStatus summarizes one target inside a route.
type GatewayRouteTargetStatus struct {
	Plan             string   `json:"plan"`
	Model            string   `json:"model"`
	EndpointProtocol string   `json:"endpointProtocol,omitempty"`
	Priority         int      `json:"priority"`
	Enabled          bool     `json:"enabled"`
	WhenTier         []string `json:"whenTier,omitempty"`
}

// GatewayRouteStatus describes a configured route and its match/policy summary.
type GatewayRouteStatus struct {
	ID          string                     `json:"id"`
	Enabled     bool                       `json:"enabled"`
	Description string                     `json:"description,omitempty"`
	Priority    int                        `json:"priority"`
	Family      string                     `json:"family"`
	Profiles    []string                   `json:"profiles,omitempty"`
	Protocols   []string                   `json:"protocols,omitempty"`
	Models      []string                   `json:"models,omitempty"`
	Preset      string                     `json:"preset,omitempty"`
	Targets     []GatewayRouteTargetStatus `json:"targets"`
}

// GatewayRouteListResponse is returned by GET /v1/gateway/routes.
type GatewayRouteListResponse struct {
	Routes []GatewayRouteStatus `json:"routes"`
}

// GatewayUsageBucket aggregates gateway accounting for one (plan, model) pair.
type GatewayUsageBucket struct {
	Plan             string  `json:"plan"`
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	ReasoningTokens  int64   `json:"reasoningTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	CacheHitRatio    float64 `json:"cacheHitRatio,omitempty"`
	AvgTTFTMs        float64 `json:"avgTtftMs,omitempty"`
	AvgDurationMs    float64 `json:"avgDurationMs,omitempty"`
}

// GatewayUsageResponse is returned by GET /v1/gateway/usage.
type GatewayUsageResponse struct {
	Window  string               `json:"window"`
	Since   time.Time            `json:"since"`
	Until   time.Time            `json:"until"`
	Totals  GatewayUsageBucket   `json:"totals"`
	Buckets []GatewayUsageBucket `json:"buckets"`
}

// GatewayPreviewRequest is the body for POST /v1/gateway/preview.
type GatewayPreviewRequest struct {
	Profile        string `json:"profile,omitempty"`
	Protocol       string `json:"protocol"`
	Model          string `json:"model"`
	EstInputTokens int    `json:"estInputTokens,omitempty"`
	TurnPhase      string `json:"turnPhase,omitempty"`
	SessionKey     string `json:"sessionKey,omitempty"`
	Tools          bool   `json:"tools,omitempty"`
	Vision         bool   `json:"vision,omitempty"`
	Thinking       bool   `json:"thinking,omitempty"`
}

// GatewayPreviewResponse is returned by POST /v1/gateway/preview.
type GatewayPreviewResponse struct {
	Matched  bool           `json:"matched"`
	RouteID  string         `json:"routeId,omitempty"`
	Family   string         `json:"family,omitempty"`
	Chosen   string         `json:"chosen,omitempty"`
	Decision *RouteDecision `json:"decision,omitempty"`
	Error    string         `json:"error,omitempty"`
}
