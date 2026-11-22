// Package gateway hosts Crabbro's multi-protocol LLM & Decision gateway.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lidessen/crabbro/internal/capture"
	"github.com/lidessen/crabbro/internal/gateway/codec"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/executor"
	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/gateway/ledger"
	"github.com/lidessen/crabbro/internal/gateway/quota"
	"github.com/lidessen/crabbro/internal/gateway/router"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
)

// Options configures the LLM & Decision Gateway daemon subsystem.
type Options struct {
	ListenAddr     string
	Token          string
	CaptureMode    string // preview | metadata-only | off
	MaxRequestBody int64
	UsageRetention time.Duration
	RootDir        string // ~/.crabbro/gateway
}

// Gateway owns the HTTP reverse-gateway listener, immutable config snapshots,
// quota ledger, router, and failover executor.
type Gateway struct {
	ListenAddr string
	Token      string
	Config     *config.Store
	Ledger     *ledger.Store
	Secrets    *secret.Store
	Sessions   *router.SessionCacheStore
	Health     *router.HealthStore
	Router     *router.Router
	Quotas     *quota.Syncer
	Executor   *executor.Executor
	Sink       capture.ExchangeSink

	maxBody     int64
	server      *http.Server
	listener    net.Listener
	startOnce   sync.Once
	mu          sync.Mutex
	closed      bool
	quotaCancel context.CancelFunc
}

// New initializes a Gateway rooted at opt.RootDir using shared secrets and sink.
func New(opt Options, secrets *secret.Store, sink capture.ExchangeSink) (*Gateway, error) {
	if opt.RootDir == "" {
		return nil, fmt.Errorf("gateway root directory is required")
	}
	if opt.ListenAddr == "" {
		opt.ListenAddr = shared.DefaultGatewayAddr
	}
	if opt.CaptureMode == "" {
		opt.CaptureMode = executor.CapturePreview
	}
	if opt.MaxRequestBody <= 0 {
		opt.MaxRequestBody = shared.DefaultMaxGatewayRequestBody
	}
	if opt.UsageRetention <= 0 {
		opt.UsageRetention = shared.DefaultGatewayUsageRetention
	}

	cfg := config.Open(opt.RootDir)
	ldg, err := ledger.Open(filepath.Join(opt.RootDir, "usage.db"), opt.UsageRetention)
	if err != nil {
		return nil, err
	}

	sess := router.NewSessionCacheStore(4096)
	health := router.NewHealthStore()
	rtr := router.New(secrets, sess, health, ldg)
	qs := quota.New(secrets, opt.RootDir)
	rtr.Quotas = qs
	exec := executor.New(rtr, secrets, ldg, sink)
	exec.Bridge = codec.NewBridge()
	exec.CaptureMode = opt.CaptureMode
	exec.MaxRequestBody = opt.MaxRequestBody

	gw := &Gateway{
		ListenAddr: opt.ListenAddr,
		Token:      opt.Token,
		Config:     cfg,
		Ledger:     ldg,
		Secrets:    secrets,
		Sessions:   sess,
		Health:     health,
		Router:     rtr,
		Quotas:     qs,
		Executor:   exec,
		Sink:       sink,
		maxBody:    opt.MaxRequestBody,
	}

	rtr.DecisionCaller = gw.InvokeDecision
	// Verified automatic redemptions clear plan cooling immediately so traffic
	// resumes without waiting out the exhaustion cooldown.
	qs.OnAutoReset = func(planID, scope string, out quota.ResetOutcome) {
		if g := gw; g != nil {
			g.Health.ResetCooldown(planID)
			if g.Sink != nil {
				g.Sink.Emit(shared.EventGatewayQuotaReset, "", map[string]any{
					"plan":  planID,
					"scope": scope,
				})
			}
		}
	}
	// Expiry reminders are emitted at most once per coupon/window and reuse
	// the existing event surface; only sanitized fields cross the boundary.
	qs.OnExpiryReminder = func(r quota.QuotaReminder) {
		if gw == nil || gw.Sink == nil {
			return
		}
		gw.Sink.Emit(shared.EventGatewayQuotaExpiring, "", map[string]any{
			"plan":      r.PlanID,
			"kind":      r.Kind,
			"scope":     r.Scope,
			"expiresAt": r.ExpiresAt.UTC().Format(time.RFC3339),
		})
	}
	// Upstream exhaustion/rate-limit cooling raises the opt-in auto-reset
	// evaluation on a detached context so request handling never blocks.
	exec.AutoResetSignal = func(planID string) {
		gw.Quotas.AutoResetAsync(func() map[string]*config.Plan { return gw.Config.Snapshot().AllPlans }, planID, true)
	}
	cfg.OnChange = func(snap *config.Snapshot) {
		if sink != nil {
			sink.Emit(shared.EventGatewayConfigChanged, "", map[string]any{
				"generation": snap.Generation,
				"plans":      len(snap.Plans),
				"routes":     len(snap.Routes),
			})
		}
	}
	health.OnChange = func(planID, state, reason string) {
		if sink != nil {
			sink.Emit(shared.EventGatewayPlanHealthChanged, "", map[string]any{
				"plan":   planID,
				"state":  state,
				"reason": reason,
			})
		}
	}
	return gw, nil
}

// Start starts the gateway HTTP listener. Non-loopback binds require Token.
func (g *Gateway) Start() error {
	if !isLoopbackListen(g.ListenAddr) && strings.TrimSpace(g.Token) == "" {
		return fmt.Errorf("non-loopback gateway listener %q requires --gateway-token", g.ListenAddr)
	}
	var err error
	g.startOnce.Do(func() {
		var ln net.Listener
		ln, err = net.Listen("tcp", g.ListenAddr)
		if err != nil {
			return
		}
		g.mu.Lock()
		g.listener = ln
		g.server = &http.Server{
			Handler:           g,
			ReadHeaderTimeout: 30 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		ctx, cancel := context.WithCancel(context.Background())
		g.quotaCancel = cancel
		g.mu.Unlock()
		go g.Quotas.Run(ctx, func() map[string]*config.Plan { return g.Config.Snapshot().AllPlans })
		go func() {
			if serveErr := g.server.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
				// listener closed or stopped
			}
		}()
	})
	return err
}

// Stop gracefully shuts down the gateway HTTP server, transport pool, and usage.db.
func (g *Gateway) Stop() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	srv := g.server
	cancel := g.quotaCancel
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	var err error
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = srv.Shutdown(ctx)
		cancel()
	}
	if g.Executor != nil && g.Executor.Transports != nil {
		g.Executor.Transports.Close()
	}
	if g.Ledger != nil {
		_ = g.Ledger.Close()
	}
	return err
}

// Addr returns the bound TCP address (or configured ListenAddr before Start).
func (g *Gateway) Addr() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	ln := g.listener
	g.mu.Unlock()
	if ln != nil {
		return ln.Addr().String()
	}
	return g.ListenAddr
}

// ServeHTTP handles incoming gateway requests across all 6 protocols and /p/{profile}/...
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if g.Token != "" && !g.checkAuth(r) {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid gateway token")
		return
	}

	var body []byte
	if r.Body != nil && r.Method == http.MethodPost {
		lr := io.LimitReader(r.Body, g.maxBody+1)
		var err error
		body, err = io.ReadAll(lr)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_request", "failed reading request body")
			return
		}
		if int64(len(body)) > g.maxBody {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", fmt.Sprintf("request body exceeds %d bytes", g.maxBody))
			return
		}
	}

	ingress, err := codec.ParseIngress(r, body)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}

	snap := g.Config.Snapshot()
	if ingress.Action == "models" {
		g.serveModels(w, snap, ingress.Profile)
		return
	}
	g.Executor.ServeHTTP(w, r, snap, ingress, body)
}

func (g *Gateway) checkAuth(r *http.Request) bool {
	if auth := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		tok := strings.TrimSpace(auth[len("bearer "):])
		if subtle.ConstantTimeCompare([]byte(tok), []byte(g.Token)) == 1 {
			return true
		}
	}
	for _, h := range []string{r.Header.Get("x-api-key"), r.Header.Get("x-goog-api-key"), r.URL.Query().Get("key")} {
		if h != "" && subtle.ConstantTimeCompare([]byte(h), []byte(g.Token)) == 1 {
			return true
		}
	}
	return false
}

func (g *Gateway) serveModels(w http.ResponseWriter, snap *config.Snapshot, profile string) {
	items := map[string]map[string]any{}
	add := func(id, owner, kind, family string) {
		if id == "" || strings.ContainsAny(id, "*?") {
			return
		}
		if _, ok := items[id]; ok {
			return
		}
		items[id] = map[string]any{"id": id, "object": "model", "owned_by": owner, "crabbro_kind": kind, "family": family}
	}
	if snap != nil {
		for _, rt := range snap.Routes {
			if !rt.IsEnabled() {
				continue
			}
			if len(rt.Match.Profiles) > 0 {
				found := false
				for _, p := range rt.Match.Profiles {
					if p == profile {
						found = true
						break
					}
				}
				if !found {
					continue
				}
			}
			add(rt.ID, "crabbro", "route", rt.EffectiveFamily())
			for _, m := range rt.Match.Models {
				add(m, "crabbro", "route", rt.EffectiveFamily())
			}
		}
		for _, plan := range snap.AllPlans {
			if !plan.DirectEnabled() || !plan.AllowsDirectProfile(profile) {
				continue
			}
			for model, ms := range plan.Models {
				if ms == nil {
					continue
				}
				family := ""
				for proto, ep := range plan.Endpoints {
					if ep == nil {
						continue
					}
					allowed := len(ms.Endpoints) == 0
					for _, p := range ms.Endpoints {
						if p == proto {
							allowed = true
							break
						}
					}
					if !allowed {
						continue
					}
					if config.ProtocolFamily(proto) == config.FamilyGenerative {
						family = config.FamilyGenerative
						break
					}
					if config.ProtocolFamily(proto) == config.FamilyDecision {
						family = config.FamilyDecision
					}
				}
				if family != "" {
					add(plan.ID+"/"+model, plan.ID, "direct", family)
				}
			}
		}
	}
	ids := make([]string, 0, len(items))
	for id := range items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	data := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		data = append(data, items[id])
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// InvokeDecision routes an internal DecisionRequest through the named decision route.
func (g *Gateway) InvokeDecision(ctx context.Context, routeID string, req *ir.DecisionRequest) (*ir.DecisionResponse, error) {
	if g == nil || g.Config == nil || g.Executor == nil {
		return nil, fmt.Errorf("gateway is not initialized")
	}
	if routeID == "" {
		routeID = "decision"
	}
	return g.Executor.ExecuteDecision(ctx, g.Config.Snapshot(), routeID, req)
}

func planHasAnySecret(p *config.Plan, secrets *secret.Store) bool {
	if secrets == nil {
		return false
	}
	for _, ref := range p.CredentialRefs() {
		if secrets.Has(ref) {
			return true
		}
	}
	return false
}

// DecisionStatus reports whether an enabled decision route exists and its primary target.
func (g *Gateway) DecisionStatus(routeID string) (configured bool, provider, model, routeName string) {
	if g == nil || g.Config == nil {
		return false, "", "", ""
	}
	if routeID == "" {
		routeID = "decision"
	}
	snap := g.Config.Snapshot()
	for _, rt := range snap.Routes {
		if rt.ID == routeID && rt.IsEnabled() && rt.EffectiveFamily() == config.FamilyDecision {
			for _, tgt := range rt.Targets {
				if !tgt.IsEnabled() {
					continue
				}
				pl := snap.Plans[tgt.Plan]
				if pl == nil {
					continue
				}
				if g.Secrets != nil && !planHasAnySecret(pl, g.Secrets) {
					continue
				}
				ep := tgt.EndpointProtocol
				if ep == "" {
					for k := range pl.Endpoints {
						ep = k
						break
					}
				}
				return true, ep, tgt.Model, rt.ID
			}
		}
	}
	return false, "", "", ""
}

// Status returns the control-API view of the gateway subsystem.
func (g *Gateway) Status() shared.GatewayStatusResponse {
	if g == nil {
		return shared.GatewayStatusResponse{Enabled: false}
	}
	snap := g.Config.Snapshot()
	now := time.Now().UTC()
	healthy, cooling := 0, 0
	for _, p := range snap.Plans {
		if g.Health != nil {
			if c, _, _ := g.Health.IsPlanCooling(p.ID, now); c {
				cooling++
				continue
			}
		}
		healthy++
	}
	return shared.GatewayStatusResponse{
		Enabled:            true,
		ListenAddr:         g.Addr(),
		SnapshotGeneration: snap.Generation,
		PlanCount:          len(snap.AllPlans),
		RouteCount:         len(snap.AllRoutes),
		HealthyPlans:       healthy,
		CoolingPlans:       cooling,
		Diagnostics:        snap.Diagnostics,
	}
}

// Plans returns redacted plan configurations, live cooldowns, and rolling window usage.
func (g *Gateway) Plans() shared.GatewayPlanListResponse {
	snap := g.Config.Snapshot()
	now := time.Now().UTC()
	ids := make([]string, 0, len(snap.AllPlans))
	for id := range snap.AllPlans {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]shared.GatewayPlanStatus, 0, len(ids))
	for _, id := range ids {
		p := snap.AllPlans[id]
		eps := make([]string, 0, len(p.Endpoints))
		for k := range p.Endpoints {
			eps = append(eps, k)
		}
		sort.Strings(eps)
		mods := make([]string, 0, len(p.Models))
		for k := range p.Models {
			mods = append(mods, k)
		}
		sort.Strings(mods)
		var usableRefs []string
		for _, ref := range p.CredentialRefs() {
			if g.Secrets != nil && g.Secrets.Has(ref) {
				usableRefs = append(usableRefs, ref)
			}
		}
		hs := g.Health.SnapshotWithCredentials(id, usableRefs, now)
		hasSec := g.Secrets != nil && planHasAnySecret(p, g.Secrets)
		out = append(out, shared.GatewayPlanStatus{
			ID:      p.ID,
			Enabled: p.IsEnabled(),
			AutoReset: func() *shared.PlanAutoResetSpec {
				if !p.AutoReset.Enabled {
					return nil
				}
				return &shared.PlanAutoResetSpec{
					Enabled:        true,
					Scopes:         p.AutoReset.EffectiveAutoResetScopes(),
					ExpiringWithin: p.AutoReset.ExpiringWithin,
					MinInterval:    p.AutoReset.MinInterval,
				}
			}(),
			DirectEnabled: p.DirectEnabled(),
			DirectProfiles: func() []string {
				if p.Direct == nil {
					return nil
				}
				return p.Direct.Profiles
			}(),
			Description:   p.Description,
			Credential:    p.Credential,
			HasSecret:     hasSec,
			Endpoints:     eps,
			Models:        mods,
			Healthy:       hs.Healthy,
			CooldownUntil: hs.CooldownUntil,
			CooldownScope: hs.CooldownScope,
			LastError: func() string {
				if !hs.Healthy {
					return hs.LastError
				}
				return ""
			}(),
			HistoricalLastError: func() string {
				if hs.Healthy {
					return hs.LastError
				}
				return ""
			}(),
			InFlight:      hs.InFlight,
			ErrorEWMA:     hs.ErrorEWMA,
			LatencyEWMAMs: hs.LatencyEWMAMs,
			Windows:       g.Router.ComputeWindowUsage(p, now),
			RemoteQuota:   func() *quota.RemoteQuota { q, _ := g.Quotas.Get(id); return q }(),
		})
	}
	return shared.GatewayPlanListResponse{Plans: out}
}

// ResetCooldown clears active cooldown state for planID.
func (g *Gateway) ResetCooldown(planID string) error {
	snap := g.Config.Snapshot()
	if snap.AllPlans[planID] == nil {
		return fmt.Errorf("plan %q not found", planID)
	}
	g.Health.ResetCooldown(planID)
	return nil
}

// ResetQuota redeems at most one provider reset coupon for a plan, refreshes
// its remote quota snapshot, and clears plan cooling so traffic resumes
// immediately. Provider coupon identifiers never appear in the response.
func (g *Gateway) ResetQuota(ctx context.Context, req shared.GatewayQuotaResetRequest) shared.GatewayQuotaResetResponse {
	out, err := g.Quotas.Reset(ctx, g.Config.Snapshot().AllPlans, req.Plan, req.Scope, req.DryRun)
	if err != nil && out.Error == "" {
		out.Error = err.Error()
	}
	if out.Plan == "" {
		out.Plan = req.Plan
	}
	if out.Redeemed {
		g.Health.ResetCooldown(req.Plan)
	}
	statuses := g.Plans().Plans
	for i := range statuses {
		if statuses[i].ID == req.Plan {
			out.Status = &statuses[i]
			break
		}
	}
	return out
}

// Routes returns loaded gateway routes.
func (g *Gateway) Routes() shared.GatewayRouteListResponse {
	snap := g.Config.Snapshot()
	out := make([]shared.GatewayRouteStatus, 0, len(snap.AllRoutes))
	for _, rt := range snap.AllRoutes {
		tgts := make([]shared.GatewayRouteTargetStatus, 0, len(rt.Targets))
		for _, t := range rt.Targets {
			tgts = append(tgts, shared.GatewayRouteTargetStatus{
				Plan:             t.Plan,
				Model:            t.Model,
				EndpointProtocol: t.EndpointProtocol,
				Priority:         t.Priority,
				Enabled:          t.IsEnabled(),
				WhenTier:         t.WhenTier,
			})
		}
		out = append(out, shared.GatewayRouteStatus{
			ID:          rt.ID,
			Enabled:     rt.IsEnabled(),
			Description: rt.Description,
			Priority:    rt.Priority,
			Family:      rt.EffectiveFamily(),
			Profiles:    rt.Match.Profiles,
			Protocols:   rt.Match.Protocols,
			Models:      rt.Match.Models,
			Preset:      rt.Policy.EffectivePreset(),
			Targets:     tgts,
		})
	}
	return shared.GatewayRouteListResponse{Routes: out}
}

// Usage returns aggregated token and request statistics from usage.db.
func (g *Gateway) Usage(window, planFilter string) (shared.GatewayUsageResponse, error) {
	if window == "" {
		window = "24h"
	}
	dur, err := config.ParseDuration(window)
	if err != nil || dur <= 0 {
		return shared.GatewayUsageResponse{}, fmt.Errorf("invalid window %q", window)
	}
	until := time.Now().UTC()
	since := until.Add(-dur)
	return g.Ledger.Aggregate(window, since, until, planFilter)
}

// Preview runs router candidate selection without sending upstream bytes.
func (g *Gateway) Preview(ctx context.Context, req shared.GatewayPreviewRequest) shared.GatewayPreviewResponse {
	return g.Router.Preview(ctx, g.Config.Snapshot(), req)
}

// Reload reloads plans and routes from disk.
func (g *Gateway) Reload() ([]shared.Diagnostic, bool) {
	return g.Config.Reload()
}

func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg},
	})
}
