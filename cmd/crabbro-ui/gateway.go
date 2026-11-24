package main

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/crabbro/pkg/api"
)

const gwUnclassifiedReason = "unclassified failure (details omitted)"

var (
	// gwClassifiedReasons mirrors the closed set of fixed summaries emitted
	// by internal/gateway/executor/classify.go. The UI keeps its own copy as
	// the last line of defense, so anything else becomes a fixed marker.
	gwClassifiedReasons = map[string]struct{}{
		"rate limited (429)":                       {},
		"quota exhausted":                          {},
		"quota exhausted (429)":                    {},
		"payment or quota required (402)":          {},
		"upstream auth error (401)":                {},
		"upstream permission denied (403)":         {},
		"model or endpoint not found":              {},
		"model or endpoint not found (404)":        {},
		"model capability or endpoint unavailable": {},
		"context length exceeded":                  {},
		"upstream timeout":                         {},
		"transport error":                          {},
		"mid-stream drop":                          {},
		"upstream SSE error":                       {},
		"upstream SSE error (rate_limit)":          {},
		"upstream SSE error (quota_exhausted)":     {},
		"upstream SSE error (auth_error)":          {},
		"429 rate limited":                         {},
		"insufficient balance":                     {},
	}
	gwClassifiedReasonPatterns = []*regexp.Regexp{
		regexp.MustCompile(`^upstream invalid request \(\d{3}\)$`),
		regexp.MustCompile(`^upstream rejected request \(\d{3}\)$`),
		regexp.MustCompile(`^upstream server error \(\d{3}\)$`),
	}
)

// gwSafeReason is the UI's last line of defense for LastError text: even if a
// caller hands the card an unsanitized snapshot, only the gateway classifier's
// fixed summaries reach rendered text. Whitespace and control characters are
// normalized first; an empty result stays empty. Any other non-empty input,
// including bodies, credentials, URLs, prompts, and emails, becomes a fixed
// unclassified marker and is never echoed.
func gwSafeReason(reason string) string {
	reason = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, reason)), " ")
	if reason == "" {
		return ""
	}
	if _, ok := gwClassifiedReasons[reason]; ok {
		return reason
	}
	for _, pattern := range gwClassifiedReasonPatterns {
		if pattern.MatchString(reason) {
			return reason
		}
	}
	return gwUnclassifiedReason
}

func (a *app) gwCard(c *ui.Context, p *palette, key, title string, icon *ui.SVG, right func(), body func()) {
	panel(c.Key("gw:"+key), p).FillWidth().Children(func() {
		ui.Row(c).FillWidth().Height(48).Padding(0, 16, 0, 18).Gap(9).AlignItems(ui.Center).
			BorderWidth(0, 0, 1, 0).BorderColor(p.border).Children(func() {
			ui.Icon(c, icon).FontSize(14).TextColor(p.faint)
			ui.Text(c, title).FontSize(14).FontWeight(600).TextColor(p.text).SingleLine()
			ui.Spacer(c)
			if right != nil {
				right()
			}
		})
		ui.Column(c).FillWidth().Padding(16, 18, 18).Gap(12).Children(body)
	})
}

func (a *app) gwStatusCard(c *ui.Context, p *palette, st store) {
	// Without an answered status request there is no Enabled or Disabled to
	// assert: the zero value must not read as off. Only an answer may say.
	answered := st.gwStatusFetch != gwUnknown
	a.gwCard(c, p, "status", "Configuration", iconServer, func() {
		if answered && st.gwStatus.Enabled {
			a.quietButton(c, "gw-reload", "Reload configuration", iconReplay).Clicked(func() { a.actReloadGateway() })
		}
	}, func() {
		if !answered {
			ui.Text(c, "Gateway state unknown: crabbrod has not answered the status request, so neither on nor off is asserted.").
				FontSize(13).TextColor(p.muted).FillWidth()
			return
		}
		if st.gwStatusFetch == gwStale {
			gwStaleNote(c, p, "status")
		}
		if !st.gwStatus.Enabled {
			ui.Text(c, "Start crabbrod without --gateway-addr=off to enable the multi-protocol gateway.").
				FontSize(13).TextColor(p.muted).FillWidth()
			return
		}
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Stretch).Children(func() {
			gwMetric(c, p, "Listen address", st.gwStatus.ListenAddr, "", p.faint).Grow(2)
			gwMetric(c, p, "Generation", fmt.Sprintf("v%d", st.gwStatus.SnapshotGeneration), "", p.faint)
			gwMetric(c, p, "Plans", fmt.Sprint(st.gwStatus.PlanCount), "Configured", p.faint)
			gwMetric(c, p, "Routes", fmt.Sprint(st.gwStatus.RouteCount), "Configured", p.faint)
		})
		ui.Text(c, gwPlanCounts(st.gwStatus)).FillWidth().FontSize(12).TextColor(p.muted)
		if len(st.gwStatus.Diagnostics) > 0 {
			hairline(c, p)
			caption(c, p, "Diagnostics")
			for i, d := range st.gwStatus.Diagnostics {
				ui.Text(c.Key(fmt.Sprintf("gw-diag-%d", i)), fmt.Sprintf("%s %s: %s", d.File, d.Path, d.Message)).
					FontSize(12).Font(mono()).TextColor(p.removed).FillWidth()
			}
		}
	})
}

// gwPlanCounts says the plan counts for what each counts: PlanCount counts
// every configured plan, while not-cooling/cooling cover enabled plans
// only, so the plans a config has disabled are said apart, not folded into
// the counts as though they were active.
func gwPlanCounts(st api.GatewayStatusResponse) string {
	enabled := st.HealthyPlans + st.CoolingPlans
	counts := fmt.Sprintf("%d configured · %d enabled (%d not cooling, %d cooling)",
		st.PlanCount, enabled, st.HealthyPlans, st.CoolingPlans)
	// Only a count that can exist is said: a disagreement is not clamped
	// into sense.
	if disabled := st.PlanCount - enabled; disabled > 0 {
		counts += fmt.Sprintf(" · %d disabled", disabled)
	}
	return counts
}

// gwStaleTag marks a card whose badge or count is the last successful
// answer rather than the latest one.
func gwStaleTag(c *ui.Context, p *palette) {
	ui.Text(c, "stale").FontSize(11).FontWeight(500).Font(mono()).TextColor(p.warning()).
		Padding(2, 7).Radius(6).Background(p.warning().Alpha(p.wash())).SingleLine().Shrink(0)
}

// gwStaleNote says a card shows kept data: the latest what request failed,
// so this is the last successful answer and nothing newer is asserted.
func gwStaleNote(c *ui.Context, p *palette, what string) {
	ui.Text(c, fmt.Sprintf("Stale: the latest %s request failed; this is the last successful answer.", what)).
		FontSize(11.5).Font(mono()).TextColor(p.warning()).FillWidth()
}

// gwOffNow says the status card's answer is fresh and says the gateway is
// off. Only that may read a missing plan list as "unavailable because the
// gateway is off"; a kept or missing status says nothing.
func gwOffNow(st store) bool {
	return st.gwStatusFetch == gwFresh && !st.gwStatus.Enabled
}

// gwEmptyNote is an empty list answer that may not be read as "none
// configured": the gateway being off is one cause, and the answer cannot
// rule out entries it simply does not include. The directory is named as
// where to look, never as a place known to be empty.
func gwEmptyNote(c *ui.Context, p *palette, what, dir string) {
	ui.Text(c, fmt.Sprintf("No %s reported in this answer; that alone does not prove none are configured. Look under %s.", what, dir)).
		FontSize(13).TextColor(p.muted).FillWidth()
	gwOffCaveat(c, p)
}

// gwOffCaveat says the empty-answer case that the answer itself hides: a
// disabled gateway answers these endpoints with an empty list, so an empty
// list is also what a config with entries can look like from here.
func gwOffCaveat(c *ui.Context, p *palette) {
	ui.Text(c, "A gateway started with --gateway-addr=off answers empty here too, so this does not rule out a config that has entries.").
		FontSize(11.5).TextColor(p.faint).FillWidth()
}

// gwOffNote is an answer whose emptiness is explained by a fresh off status:
// the endpoint reports nothing because the gateway is off, which is not the
// same as there being nothing to report.
func gwOffNote(c *ui.Context, p *palette, what string) {
	ui.Text(c, fmt.Sprintf("No %s reported: the gateway is off, and this endpoint answers empty rather than listing a config. Whether any are configured is not known from here.", what)).
		FontSize(13).TextColor(p.muted).FillWidth()
}

// gwOffUsageNote is the usage ledger's off case: an off gateway answers the
// usage endpoint empty whatever usage.db holds, so the empty answer is a
// placeholder and no absence of recorded usage may be inferred from it.
func gwOffUsageNote(c *ui.Context, p *palette, reported string) {
	ui.Text(c, "No usage reported: the gateway is off, and this endpoint answers empty whatever the ledger holds. Whether any usage is recorded is not known from here.").
		FontSize(13).TextColor(p.muted).FillWidth()
	if reported == "" {
		ui.Text(c, "The off answer did not name the range it covers.").
			FontSize(11.5).TextColor(p.faint).FillWidth()
	}
}

func (a *app) gwPlansCard(c *ui.Context, p *palette, st store) {
	plans := st.gwPlans
	ui.Column(c.Key("gw:plans")).FillWidth().Gap(12).Children(func() {
		if st.gwPlansFetch == gwUnknown {
			ui.Text(c, "Plans unknown: crabbrod has not answered the plans request, so no plan is asserted configured or absent.").
				FontSize(13).TextColor(p.muted).FillWidth()
			return
		}
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, fmt.Sprintf("%d provider plans", len(plans))).FontSize(13).TextColor(p.muted)
			if st.gwPlansFetch == gwStale {
				gwStaleTag(c, p)
			}
			ui.Spacer(c)
			a.quietButton(c, "gw-refresh-quotas", "Refresh quotas", iconReplay).Clicked(func() { a.actRefreshGatewayQuotas() })
		})
		if st.gwPlansFetch == gwStale {
			gwStaleNote(c, p, "plans")
		}
		if len(plans) == 0 {
			// What an empty answer proves depends on the status answer, which
			// is a different fetch: only a fresh off status may explain it as
			// the gateway being off, and no empty list proves none configured.
			switch {
			case st.gwPlansFetch == gwFresh && gwOffNow(st):
				ui.Text(c, "Plans unavailable: the gateway is off, so plan and quota state is not reported.").
					FontSize(13).TextColor(p.muted).FillWidth()
			case st.gwPlansFetch == gwStale && gwOffNow(st):
				// The kept empty list predates the current off state: it was
				// not freshly observed while the gateway is off, so it may
				// never be explained by it.
				ui.Text(c, "No plans in the kept answer: that empty list is stale and was not freshly observed while the gateway is off, so whether any plans are configured is not known from here.").
					FontSize(13).TextColor(p.muted).FillWidth()
			case st.gwStatusFetch != gwFresh:
				// A kept or missing status cannot explain the empty list as
				// the gateway being off: say that, then give the same empty
				// answer caveat rather than asserting an empty plan set.
				ui.Text(c, "No plans reported. The gateway's own on/off state is stale or unknown here, so this is neither confirmed as a disabled gateway nor as an empty plan set.").
					FontSize(13).TextColor(p.muted).FillWidth()
				gwEmptyNote(c, p, "plans", "~/.crabbro/gateway/plans/")
			default:
				// Fresh or kept, an empty list alongside a gateway on is an
				// observation, not a verdict on the config: a disabled plan
				// may simply be absent from the answer.
				gwEmptyNote(c, p, "plans", "~/.crabbro/gateway/plans/")
			}
			return
		}
		gwCardColumns(c, gwGridColumns(c, 390, 2), len(plans), func(i int) {
			a.gwPlanTile(c, p, plans[i])
		})
		ui.Text(c, "Plan state above is plan-level cooldown only; model-level cooldowns are not reported, so a plan not cooling may still hold cooling models.").
			FontSize(11.5).TextColor(p.faint).FillWidth()
	})
}

func (a *app) gwPlanTile(c *ui.Context, p *palette, pl api.GatewayPlanStatus) {
	panel(c.Key("gw-plan:"+pl.ID), p).MinWidth(0).Padding(16).Gap(10).Children(func() {
		ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
			statusIcon(c, p, iconServer, p.accent)
			ui.Text(c, pl.ID).Grow(1).MinWidth(0).FontSize(15).FontWeight(600).Font(mono()).TextColor(p.text)
		})
		label, tone := gwPlanState(p, pl)
		gwPlanStatusDot(c, tone, label)
		if len(pl.Windows) > 0 || pl.RemoteQuota != nil {
			hairline(c, p)
		}
		if reason := gwSafeReason(pl.LastError); reason != "" && !pl.Healthy {
			// LastError is populated only while the plan is
			// unhealthy; an expired timer makes the snapshot stale. The
			// reason is sanitized here as well as in the router so
			// raw snapshot text never reaches the frame.
			label := "Current error"
			if pl.CooldownUntil != nil && pl.CooldownUntil.After(time.Now()) {
				label = "Current cooldown reason"
			} else if pl.CooldownUntil != nil && !pl.CooldownUntil.IsZero() {
				label = "Last reported error (state not refreshed)"
			}
			ui.Text(c.Key("gw-plan-err:"+pl.ID),
				fmt.Sprintf("%s: %s", label, reason)).
				FontSize(11.5).Font(mono()).TextColor(p.removed).FillWidth()
		}
		if reason := gwSafeReason(pl.HistoricalLastError); reason != "" {
			// HistoricalLastError survives its cooldown and carries
			// no time or model: label it as the historical reason it
			// is, never as a live failure of a plan shown as fine.
			ui.Text(c.Key("gw-plan-hist-err:"+pl.ID),
				fmt.Sprintf("Last cooldown reason (historical, time & model not reported): %s", reason)).
				FontSize(11.5).Font(mono()).TextColor(p.muted).FillWidth()
		}
		if pl.RemoteQuota != nil {
			gwRemoteQuota(c, p, pl.RemoteQuota)
		}
		if len(pl.Windows) > 0 {
			caption(c, p, "Local limits")
		}
		for wi, w := range pl.Windows {
			a.gwWindowBar(c, p, fmt.Sprintf("%s:%d", pl.ID, wi), w)
		}
		hairline(c, p)
		gwMetadata(c, p, "Credential", []string{pl.Credential})
		gwMetadata(c, p, "Endpoints", pl.Endpoints)
		gwMetadata(c, p, "Models", pl.Models)
		if !pl.Healthy {
			a.ghostButton(c, "gw-reset:"+pl.ID, "Reset cooldown", iconUndo).Clicked(func() {
				a.actResetCooldown(pl.ID)
			})
		}
	})
}

func gwQuotaTone(p *palette, remaining float64) ui.Color {
	switch {
	case remaining < 0.15:
		return p.removed
	case remaining < 0.35:
		return p.warning()
	default:
		return p.added
	}
}

func gwPlanStatusDot(c *ui.Context, tone ui.Color, label string) {
	ui.Row(c).FillWidth().Gap(7).AlignItems(ui.Center).Children(func() {
		ui.Box(c).Size(6, 6).Radius(3).Shrink(0).Background(tone)
		ui.Text(c, label).Grow(1).MinWidth(0).FontSize(12).FontWeight(500).TextColor(tone)
	})
}

func gwPlanState(p *palette, plan api.GatewayPlanStatus) (string, ui.Color) {
	switch {
	case !plan.Enabled:
		return "disabled", p.faint
	case !plan.HasSecret:
		return "Missing credential", p.warning()
	case plan.Healthy:
		return "Ready", p.added
	case plan.CooldownUntil == nil || plan.CooldownUntil.IsZero():
		return "Unhealthy (cooldown time not reported)", p.removed
	}
	remaining := time.Until(*plan.CooldownUntil)
	if remaining > 0 {
		return fmt.Sprintf("Cooling (%s left)", remaining.Round(time.Second)), p.removed
	}
	return fmt.Sprintf("Cooldown ended %s ago (as last reported); plan state not refreshed",
		(-remaining).Truncate(time.Second)), p.warning()
}

func (a *app) gwWindowBar(c *ui.Context, p *palette, key string, w api.GatewayWindowUsage) {
	rem := max(0, min(1, w.RemainingRatio))
	barCol := gwQuotaTone(p, rem)
	label := w.Name
	if label == "" {
		label = w.Duration
	}
	ui.Column(c.Key("gw-win:"+key)).FillWidth().Gap(5).Padding(9, 10).Radius(7).
		Background(p.text.Alpha(0.04)).Children(func() {
		ui.Row(c).FillWidth().Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, label).Grow(1).MinWidth(0).FontSize(12).FontWeight(600).TextColor(p.text)
			ui.Text(c, fmt.Sprintf("%.0f%% used", (1-rem)*100)).FontSize(12).FontWeight(500).
				Font(mono()).TextColor(barCol).SingleLine().Shrink(0)
		})
		gwProgress(c, p, float32((1-rem)*100), barCol)
		detail := fmt.Sprintf("%s / %s tokens", gwCount(w.UsedTokens), gwCount(w.MaxTokens))
		if w.MaxRequests > 0 {
			detail = fmt.Sprintf("%s / %s requests", gwCount(w.UsedRequests), gwCount(w.MaxRequests))
		}
		ui.Text(c, fmt.Sprintf("%s · %s %s · surplus %+.2f", detail, w.Duration, w.Mode, w.Surplus)).
			FontSize(10.5).Font(mono()).TextColor(p.muted).FillWidth()
	})
}

func gwProgress(c *ui.Context, p *palette, pct float32, tone ui.Color) {
	ui.Row(c).FillWidth().Height(5).Radius(3).Background(p.text.Alpha(0.08)).Clip().Children(func() {
		ui.Box(c).WidthPercent(max(0, min(100, pct))).Height(5).Radius(3).Background(tone)
	})
}

func (a *app) gwRoutesCard(c *ui.Context, p *palette, st store) {
	routes := st.gwRoutes
	a.gwCard(c, p, "routes", "Configured routes", iconRoute, func() {
		if st.gwRoutesFetch == gwUnknown {
			tag(c, p, "routes unknown")
			return
		}
		tag(c, p, fmt.Sprintf("%d routes", len(routes)))
		if st.gwRoutesFetch == gwStale {
			gwStaleTag(c, p)
		}
	}, func() {
		if st.gwRoutesFetch == gwUnknown {
			ui.Text(c, "Routes unknown: crabbrod has not answered the routes request.").
				FontSize(13).TextColor(p.muted).FillWidth()
			return
		}
		if st.gwRoutesFetch == gwStale {
			gwStaleNote(c, p, "routes")
		}
		if len(routes) == 0 {
			// An off gateway answers the routes endpoint with an empty list
			// whatever the config holds: only a fresh off status explains
			// that, and no empty answer asserts an empty config.
			switch {
			case st.gwRoutesFetch == gwFresh && gwOffNow(st):
				gwOffNote(c, p, "routes")
			case st.gwRoutesFetch == gwStale && gwOffNow(st):
				// A kept empty list was not freshly observed while the
				// gateway is off, so the current off state does not explain it.
				ui.Text(c, "No routes in the kept answer: that empty list is stale and routes were not freshly observed while the gateway is off, so whether any are configured is not known from here.").
					FontSize(13).TextColor(p.muted).FillWidth()
			default:
				gwEmptyNote(c, p, "routes", "~/.crabbro/gateway/routes/")
			}
			return
		}
		for i, route := range routes {
			if i > 0 {
				hairline(c, p)
			}
			a.gwRouteRow(c, p, route)
		}
	})
}

func (a *app) gwRouteRow(c *ui.Context, p *palette, route api.GatewayRouteStatus) {
	ui.Column(c.Key("gw-route:" + route.ID)).FillWidth().Gap(8).Children(func() {
		expanded := a.gwExpandedRoute == route.ID
		chevron, action := iconChevronRight, "Show route "
		if expanded {
			chevron, action = iconChevronDown, "Hide route "
		}
		b := ui.ButtonBase(c.Key("gw-route-toggle:"+route.ID)).Label(action+route.ID).
			FillWidth().Padding(8, 10).Gap(10).Radius(7).AlignItems(ui.Center)
		if expanded || b.Hovered() {
			b.Background(p.text.Alpha(0.045))
		}
		b.Children(func() {
			ui.Icon(c, chevron).FontSize(14).TextColor(p.faint).Shrink(0)
			ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
				ui.Text(c, route.ID).FontSize(14).FontWeight(600).Font(mono()).TextColor(p.text).FillWidth()
				ui.Row(c).Gap(6).Wrap().AlignItems(ui.Center).Children(func() {
					if route.Family != "" {
						tag(c, p, route.Family)
					}
					if route.Preset != "" {
						tag(c, p, route.Preset)
					}
					if !route.Enabled {
						badge(c, p, p.warning(), "Disabled")
					}
				})
			})
			ui.Text(c, fmt.Sprintf("%d targets", len(route.Targets))).FontSize(12).TextColor(p.muted).SingleLine().Shrink(0)
		})
		if b.Clicked() {
			if expanded {
				a.gwExpandedRoute = ""
			} else {
				a.gwExpandedRoute = route.ID
			}
		}
		if expanded {
			a.gwRouteDetails(c, p, route)
		}
	})
}

func (a *app) gwRouteDetails(c *ui.Context, p *palette, route api.GatewayRouteStatus) {
	ui.Column(c).FillWidth().Padding(4, 10, 8, 34).Gap(10).Children(func() {
		if route.Description != "" {
			ui.Text(c, route.Description).FontSize(12.5).TextColor(p.muted).FillWidth()
		}
		gwMetadata(c, p, "Priority", []string{fmt.Sprint(route.Priority)})
		gwMetadata(c, p, "Profiles", route.Profiles)
		gwMetadata(c, p, "Protocols", route.Protocols)
		gwMetadata(c, p, "Models", route.Models)
		if len(route.Targets) == 0 {
			return
		}
		caption(c, p, "Targets in configuration order")
		for i, target := range route.Targets {
			ui.Row(c.Key(fmt.Sprintf("gw-target:%d", i))).FillWidth().Padding(8, 10).Gap(10).
				AlignItems(ui.Center).Radius(6).Background(p.text.Alpha(0.035)).Children(func() {
				ui.Text(c, fmt.Sprint(i+1)).Width(24).FontSize(11).Font(mono()).TextColor(p.faint)
				ui.Column(c).Grow(1).MinWidth(0).Gap(3).Children(func() {
					ui.Text(c, target.Model).FontSize(12.5).FontWeight(500).Font(mono()).TextColor(p.text).FillWidth()
					ui.Text(c, target.Plan).FontSize(11.5).Font(mono()).TextColor(p.muted).FillWidth()
					gwMetadata(c, p, "Tiers", target.WhenTier)
				})
				ui.Column(c).Width(132).Gap(4).AlignItems(ui.End).Children(func() {
					ui.Text(c, fmt.Sprintf("Priority %d", target.Priority)).FontSize(11).TextColor(p.muted).SingleLine()
					if target.EndpointProtocol != "" {
						ui.Text(c, target.EndpointProtocol).FontSize(11).Font(mono()).TextColor(p.muted).SingleLine()
					}
					if !target.Enabled {
						badge(c, p, p.warning(), "Disabled")
					}
				})
			})
		}
	})
}

func gwMetadata(c *ui.Context, p *palette, label string, values []string) {
	if len(values) == 0 || (len(values) == 1 && values[0] == "") {
		return
	}
	ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Start).Children(func() {
		ui.Text(c, label).Width(62).Shrink(0).FontSize(11.5).FontWeight(500).TextColor(p.muted)
		ui.Text(c, strings.Join(values, " · ")).Grow(1).MinWidth(0).FontSize(11.5).Font(mono()).TextColor(p.text)
	})
}

// gwUsageWindowFact is what the usage card can say about the range its kept
// numbers describe: whether that range is the one selected, and which the
// answer itself named. The selected range is view state and the answered
// range is store state, so both are passed rather than read again here.
type gwUsageWindowFact struct {
	selected string
	asked    string
	reported string
}

// unconfirmed says the selected range was requested but the answer did not
// name the window it covers, so the response itself cannot confirm the
// selection.
func (w gwUsageWindowFact) unconfirmed() bool {
	return w.selected == w.asked && w.reported == ""
}

// mismatch says the numbers are not confirmed as the selected range's totals:
// either the answer named another range, or the selected range was not the
// one requested.
func (w gwUsageWindowFact) mismatch() bool {
	return (w.reported != "" && w.reported != w.selected) || w.asked != w.selected
}

// described is the range to name when saying what the numbers cover: the
// range the answer named, else the one it was requested with.
func (w gwUsageWindowFact) described() string {
	if w.reported != "" {
		return w.reported
	}
	return w.asked
}

func (a *app) gwUsageCard(c *ui.Context, p *palette, st store) {
	usage := st.gwUsage
	answered := st.gwUsageFetch != gwUnknown
	hasTotals := answered && !(len(usage.Buckets) == 0 && st.gwUsageFetch == gwFresh && gwOffNow(st))
	pending := a.gwUsageRevision > st.gwUsageCompleted
	ui.Row(c.Key("gw:usage-header")).FillWidth().Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "Usage summary").FontSize(15).FontWeight(600).TextColor(p.text).SingleLine()
		ui.Spacer(c)
		prev := a.gwUsageWinIdx
		a.segments(c, "gw-usage-win", &a.gwUsageWinIdx, "1h", "24h", "7d")
		if a.gwUsageWinIdx != prev {
			a.gwUsageRevision++
			pending = true
			a.mirrorView()
			a.nudge(a.search)
		}
		if !pending && st.gwUsageFetch == gwStale {
			gwStaleTag(c, p)
		}
	})
	panel(c.Key("gw:usage-summary"), p).FillWidth().Padding(16).Gap(10).Children(func() {
		selected := windowForIndex(a.gwUsageWinIdx)
		if pending {
			label := "Loading " + selected
			if hasTotals && usage.Window != "" && usage.Window != selected {
				label += " · Showing " + usage.Window
			}
			a.fact(c, p, "Range", label)
			if hasTotals && usage.Window != "" {
				gwUsageMetrics(c, p, usage.Totals)
			}
			return
		}
		win := gwUsageWindowFact{selected: selected, asked: st.gwUsageAsked, reported: usage.Window}
		mismatch, unconfirmed := win.mismatch(), win.unconfirmed()
		if st.gwUsageFetch == gwUnknown {
			ui.Text(c, "Usage unknown: crabbrod has not answered the usage request.").
				FontSize(13).TextColor(p.muted).FillWidth()
			return
		}
		if st.gwUsageFetch == gwStale {
			gwStaleNote(c, p, "usage")
		}
		switch {
		case mismatch && win.reported != "":
			// The answer named another range: say both rather than let one
			// range's totals wear another's label.
			ui.Text(c, fmt.Sprintf("Range mismatch: %s is selected, but these numbers are the last %s answer. They are not totals for the selected range.",
				selected, win.reported)).
				FontSize(11.5).Font(mono()).TextColor(p.warning()).FillWidth()
		case mismatch:
			// The selected range was never requested, and the answer did not
			// name the window it covers: neither side confirms the range.
			ui.Text(c, fmt.Sprintf("Range mismatch: %s is selected, while this answer came from a %s request and did not name the window it covers; it is not confirmed as totals for the selected range.",
				selected, win.asked)).
				FontSize(11.5).Font(mono()).TextColor(p.warning()).FillWidth()
		case unconfirmed:
			// The selected range was requested, but the response did not
			// name its own window, so agreement cannot be claimed.
			ui.Text(c, fmt.Sprintf("Range unconfirmed: %s was selected and requested, but the response did not name the window it covers.",
				selected)).
				FontSize(11.5).Font(mono()).TextColor(p.warning()).FillWidth()
		default:
			rangeLabel := fmt.Sprintf("%s (as answered)", selected)
			if !usage.Since.IsZero() && !usage.Until.IsZero() {
				const dateTime = "Jan 2, 15:04"
				rangeLabel = usage.Since.Local().Format(dateTime) + " – " + usage.Until.Local().Format(dateTime)
			}
			ui.Text(c, rangeLabel).FontSize(11.5).TextColor(p.muted).FillWidth()
		}
		if hasTotals {
			gwUsageMetrics(c, p, usage.Totals)
		}
		if len(usage.Buckets) == 0 {
			// Zero buckets is what the endpoint answered. With the gateway
			// off that is a placeholder, and even on it proves no usage in
			// the answered range only — not none recorded anywhere, and not
			// none in the selected range while the ranges differ.
			switch {
			case st.gwUsageFetch == gwFresh && gwOffNow(st):
				gwOffUsageNote(c, p, usage.Window)
			case st.gwUsageFetch == gwStale && gwOffNow(st):
				// The kept empty buckets were not freshly observed while the
				// gateway is off, so the current off state does not explain
				// them and they prove no absence of usage.
				ui.Text(c, "No usage buckets in the kept answer: that empty list is a stale kept answer and was not freshly observed while the gateway is off, so whether any usage is recorded is not known from here.").
					FontSize(13).TextColor(p.muted).FillWidth()
			case unconfirmed:
				ui.Text(c, "No usage buckets in this answer; the window it covers was not named in the response, and zero buckets does not prove none are recorded elsewhere.").
					FontSize(13).TextColor(p.muted).FillWidth()
			case mismatch:
				ui.Text(c, fmt.Sprintf("No usage buckets in the %s answer; whether the selected %s range has any is not known yet.",
					win.described(), selected)).FontSize(13).TextColor(p.muted).FillWidth()
			default:
				ui.Text(c, fmt.Sprintf("No gateway usage buckets in the answered %s range; zero buckets does not prove no usage was recorded elsewhere.",
					win.described())).FontSize(13).TextColor(p.muted).FillWidth()
				gwOffCaveat(c, p)
			}
			return
		}
	})
	if answered && len(usage.Buckets) > 0 && (!pending || usage.Window != "") {
		a.gwUsageBreakdown(c, p, usage.Buckets)
	}
}

func gwUsageMetrics(c *ui.Context, p *palette, total api.GatewayUsageBucket) {
	errorTone := p.faint
	if total.Errors > 0 {
		errorTone = p.removed
	}
	requestDetail := gwErrors(total.Errors)
	if total.Retries > 0 {
		requestDetail += " · " + gwRetries(total.Retries)
	}
	ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Stretch).Children(func() {
		gwMetric(c, p, "Requests", gwCount(total.Requests), requestDetail, errorTone)
		gwMetric(c, p, "Tokens", gwCount(total.TotalTokens), "", p.faint)
		gwMetric(c, p, "Cache hit", gwCacheHit(total), "", p.faint)
		gwMetric(c, p, "Avg. TTFT", gwMilliseconds(total.AvgTTFTMs), "", p.faint)
		gwMetric(c, p, "Avg. tok/s", gwTokensPerSec(total.AvgTokensPerSec), "", p.faint)
	})
	ui.Row(c).FillWidth().Gap(8).Wrap().Children(func() {
		ui.Text(c, fmt.Sprintf("%s input · %s output · %s cache read", gwCount(total.InputTokens),
			gwCount(total.OutputTokens), gwCount(total.CacheReadTokens))).FontSize(11.5).TextColor(p.muted)
		if total.AvgDurationMs > 0 {
			ui.Text(c, "·  Avg. duration "+gwMilliseconds(total.AvgDurationMs)).FontSize(11.5).TextColor(p.muted)
		}
	})
}

func gwCount(n int64) string {
	digits := fmt.Sprint(n)
	start := 0
	if n < 0 {
		start = 1
	}
	for i := len(digits) - 3; i > start; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}

func gwErrors(count int64) string {
	if count == 1 {
		return "1 error"
	}
	return gwCount(count) + " errors"
}

func gwRetries(count int64) string {
	if count == 1 {
		return "1 retry"
	}
	return gwCount(count) + " retries"
}

func gwMilliseconds(ms float64) string {
	if ms <= 0 {
		return "—"
	}
	return fmt.Sprintf("%s ms", gwCount(int64(math.Round(ms))))
}

func gwCacheHit(bucket api.GatewayUsageBucket) string {
	if bucket.TotalTokens == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", bucket.CacheHitRatio*100)
}

func (a *app) gwSecretsCard(c *ui.Context, p *palette, st store) {
	secrets := st.gwSecrets
	a.gwCard(c, p, "secrets", "Stored credentials", iconLock, func() {
		if st.gwSecretsFetch == gwUnknown {
			tag(c, p, "secrets unknown")
			return
		}
		tag(c, p, fmt.Sprintf("%d stored", len(secrets)))
		if st.gwSecretsFetch == gwStale {
			gwStaleTag(c, p)
		}
	}, func() {
		caption(c, p, "Store an API key")
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c.Key("gw-sec-name"), &a.gwSecretName).Placeholder("Credential name").Width(160)
			ui.TextInput(c.Key("gw-sec-val"), &a.gwSecretVal).Placeholder("API key value").Password().Grow(1).MinWidth(120)
			a.fillButton(c, "gw-sec-save", "Save", iconSave, a.gwSecretName == "" || a.gwSecretVal == "").
				Clicked(func() {
					name := a.gwSecretName
					val := a.gwSecretVal
					a.gwSecretVal = ""
					a.actSaveSecret(name, val)
				})
		})
		hairline(c, p)
		if st.gwSecretsFetch == gwUnknown {
			ui.Text(c, "Credentials unknown: crabbrod has not answered the secrets request, so none is asserted stored or absent.").
				FontSize(13).TextColor(p.muted).FillWidth()
		} else {
			if st.gwSecretsFetch == gwStale {
				gwStaleNote(c, p, "secrets")
			}
			for _, sec := range secrets {
				s := sec
				ui.Row(c.Key("gw-sec:" + s.Name)).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Text(c, s.Name).FontSize(12.5).Font(mono()).TextColor(p.text).SingleLine().Grow(1).MinWidth(60)
					ui.Text(c, fmt.Sprintf("%d B · %s", s.SizeBytes, s.UpdatedAt.Local().Format("2006-01-02 15:04"))).
						FontSize(11).Font(mono()).TextColor(p.faint).SingleLine()
					remove := a.ghostButton(c, "gw-sec-del:"+s.Name, "", iconTrash)
					ui.Element(remove).Label("Delete " + s.Name).Tooltip("Delete credential")
					remove.Clicked(func() { a.actDeleteSecret(s.Name) })
				})
			}
			if len(secrets) == 0 {
				ui.Text(c, "No secrets stored yet under ~/.crabbro/secrets/.").FontSize(13).TextColor(p.muted).FillWidth()
			}
		}

	})
}

func (a *app) gatewayTraceSection(c *ui.Context, p *palette, gt *api.GatewayTrace) {
	if gt == nil {
		emptyState(c, p, iconServer, "Not a gateway exchange", "This exchange passed through the forward HTTP proxy.", "")
		return
	}
	caption(c, p, "Routing Outcome")
	protoLine := gt.IngressProtocol + " -> " + gt.UpstreamProtocol
	if gt.Translated {
		protoLine += " (translated)"
	} else {
		protoLine += " (passthrough)"
	}
	a.fact(c, p, "Route / Family", gt.RouteID+" ("+gt.Family+")")
	a.fact(c, p, "Chosen Target", gt.ChosenPlan+" / "+gt.ChosenModel)
	a.fact(c, p, "Protocols", protoLine)
	if gt.Profile != "" {
		a.fact(c, p, "Profile", gt.Profile)
	}
	if gt.TurnPhase != "" {
		a.fact(c, p, "Turn Phase", fmt.Sprintf("%s · gravity=%.2f · session=%s", gt.TurnPhase, gt.CacheGravity, gt.SessionKey))
	}
	if gt.DecisionMode != "" {
		cal := false
		if gt.Calibrated != nil {
			cal = *gt.Calibrated
		}
		a.fact(c, p, "Decision Mode", fmt.Sprintf("%s (calibrated=%t)", gt.DecisionMode, cal))
	}
	if gt.Usage != nil {
		u := gt.Usage
		a.fact(c, p, "Tokens", fmt.Sprintf("in=%d out=%d cacheRead=%d cacheWrite=%d reasoning=%d (total=%d)",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ReasoningTokens, u.Total()))
	}

	if len(gt.Attempts) > 0 {
		hairline(c, p)
		caption(c, p, "Upstream Attempts")
		for i, att := range gt.Attempts {
			msg := fmt.Sprintf("#%d %s/%s (%s) status=%d ttft=%dms dur=%dms",
				i+1, att.Plan, att.Model, att.Protocol, att.Status, att.TTFTMs, att.LatencyMs)
			if att.ErrorKind != "" {
				msg += " err=" + att.ErrorKind
			}
			if att.CooldownApplied != "" {
				msg += " cooldown=" + att.CooldownApplied
			}
			col := p.text
			if att.ErrorKind != "" {
				col = p.removed
			}
			ui.Text(c.Key(fmt.Sprintf("gw-att:%d", i)), msg).FontSize(12).Font(mono()).TextColor(col).FillWidth()
		}
	}

	if gt.RouteDecision != nil && len(gt.RouteDecision.Candidates) > 0 {
		hairline(c, p)
		caption(c, p, "Router Candidate Scores")
		for i, cand := range gt.RouteDecision.Candidates {
			line := fmt.Sprintf("%s/%s (%s) total=%.3f Q=%.2f C=%.2f U=%.2f P=%.2f H=%.2f",
				cand.Plan, cand.Model, cand.Protocol, cand.Scores.Total,
				cand.Scores.Quality, cand.Scores.Cache, cand.Scores.Quota, cand.Scores.Protocol, cand.Scores.Health)
			col := p.text
			if !cand.Eligible {
				col = p.muted
				line = fmt.Sprintf("%s/%s rejected: %s", cand.Plan, cand.Model, cand.RejectReason)
			}
			ui.Text(c.Key(fmt.Sprintf("gw-cand:%d", i)), line).FontSize(11.5).Font(mono()).TextColor(col).FillWidth()
		}
	}
}

func gwTokensPerSec(rate float64) string {
	if rate <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f", rate)
}
