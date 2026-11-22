package main

import (
	"fmt"
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

func (a *app) gatewayPage(c *ui.Context, p *palette) {
	st := a.snapshotState()
	ui.Scroll(c).Grow(1).Background(p.chrome).Children(func() {
		ui.Column(c).FillWidth().MaxWidth(960).AlignSelf(ui.Center).Padding(20, 24, 32).Gap(12).Children(func() {
			a.gwStatusCard(c, p, st)
			a.gwPlansCard(c, p, st)
			a.gwRoutesCard(c, p, st)
			a.gwUsageCard(c, p, st)
			a.gwSecretsCard(c, p, st)
		})
	})
}

func (a *app) gwCard(c *ui.Context, p *palette, key, title string, icon *ui.SVG, right func(), body func()) {
	panel(c.Key("gw:"+key), p).FillWidth().Children(func() {
		ui.Row(c).FillWidth().Height(42).Padding(0, 14, 0, 16).Gap(9).AlignItems(ui.Center).
			BorderWidth(0, 0, 1, 0).BorderColor(p.border).Children(func() {
			ui.Icon(c, icon).FontSize(14).TextColor(p.faint)
			ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.text).SingleLine()
			ui.Spacer(c)
			if right != nil {
				right()
			}
		})
		ui.Column(c).FillWidth().Padding(14, 16, 16).Gap(10).Children(body)
	})
}

func (a *app) gwStatusCard(c *ui.Context, p *palette, st store) {
	// Without an answered status request there is no Enabled or Disabled to
	// assert: the zero value must not read as off. Only an answer may say.
	state, tone := "Unknown", p.faint
	answered := st.gwStatusFetch != gwUnknown
	if answered {
		state = "Disabled"
		if st.gwStatus.Enabled {
			state, tone = "Enabled", p.added
		}
	}
	a.gwCard(c, p, "status", "Gateway", iconServer, func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			badge(c, p, tone, state)
			if st.gwStatusFetch == gwStale {
				gwStaleTag(c, p)
			}
			if answered && st.gwStatus.Enabled {
				a.quietButton(c, "gw-reload", "Reload", iconReplay).Clicked(func() { a.actReloadGateway() })
			}
		})
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
		a.fact(c, p, "Listen addr", st.gwStatus.ListenAddr)
		a.fact(c, p, "Generation", fmt.Sprintf("v%d", st.gwStatus.SnapshotGeneration))
		a.fact(c, p, "Plans", gwPlanCounts(st.gwStatus))
		// RouteCount counts every parsed route, enabled and disabled alike.
		a.fact(c, p, "Routes", fmt.Sprintf("%d configured", st.gwStatus.RouteCount))
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
	a.gwCard(c, p, "plans", "Plans & Quotas", iconSliders, func() {
		if st.gwPlansFetch == gwUnknown {
			// No answer yet: no count to assert either, not even zero.
			tag(c, p, "plans unknown")
			return
		}
		tag(c, p, fmt.Sprintf("%d plans", len(plans)))
		a.ghostButton(c, "gw-refresh-quotas", "Refresh quotas", iconReplay).Clicked(func() { a.actRefreshGatewayQuotas() })
		if st.gwPlansFetch == gwStale {
			gwStaleTag(c, p)
		}
	}, func() {
		if st.gwPlansFetch == gwUnknown {
			ui.Text(c, "Plans unknown: crabbrod has not answered the plans request, so no plan is asserted configured or absent.").
				FontSize(13).TextColor(p.muted).FillWidth()
			return
		}
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
		for i, pl := range plans {
			if i > 0 {
				hairline(c, p)
			}
			ui.Column(c.Key("gw-plan:" + pl.ID)).FillWidth().Gap(6).Children(func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, pl.ID).FontSize(13).FontWeight(600).Font(mono()).TextColor(p.text).SingleLine()
					if !pl.Enabled {
						tag(c, p, "disabled")
					} else if !pl.HasSecret {
						// A plan whose secret is missing cannot be routed to
						// at all, whatever its cooldown state, so it must not
						// wear the green Ready badge: say why it is ineligible
						// in a warning tone instead.
						stateDot(c, p, p.warning(), "Missing credential")
					} else if pl.Healthy {
						// Healthy only means no plan-level cooldown: the API
						// reports no provider or per-model verdict here.
						stateDot(c, p, p.added, "Ready")
					} else {
						lbl, col := "Unhealthy", p.removed
						if pl.CooldownUntil == nil || pl.CooldownUntil.IsZero() {
							lbl = "Unhealthy (cooldown time not reported)"
						} else {
							rem := time.Until(*pl.CooldownUntil)
							if rem > 0 {
								lbl = fmt.Sprintf("Cooling (%s left)", rem.Round(time.Second))
							} else {
								// The window this answer describes has
								// already closed: the answer is older than
								// its own cooldown. Say that; do not assert
								// the cooldown is still active.
								col = p.warning()
								lbl = fmt.Sprintf("Cooldown ended %s ago (as last reported); plan state not refreshed",
									(-rem).Truncate(time.Second))
							}
						}
						stateDot(c, p, col, lbl)
					}
					if pl.HasSecret {
						tag(c, p, "secret:"+pl.Credential)
					} else {
						ui.Text(c, "missing:"+pl.Credential).FontSize(11).Font(mono()).TextColor(p.removed).
							Padding(2, 7).Radius(6).Background(p.removed.Alpha(p.wash())).SingleLine()
					}
					ui.Spacer(c)
					if !pl.Healthy {
						a.ghostButton(c, "gw-reset:"+pl.ID, "Reset cooldown", iconUndo).Clicked(func() {
							a.actResetCooldown(pl.ID)
						})
					}
				})
				ui.Row(c).FillWidth().Gap(6).Wrap().AlignItems(ui.Center).Children(func() {
					for _, ep := range pl.Endpoints {
						tag(c, p, ep)
					}
					for _, m := range pl.Models {
						ui.Text(c, m).FontSize(11).Font(mono()).TextColor(p.text).
							Padding(2, 7).Radius(6).Background(p.text.Alpha(0.06)).SingleLine()
					}
				})
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
						FontSize(11.5).Font(mono()).TextColor(p.removed).FillWidth()
				}
				if q := pl.RemoteQuota; q != nil {
					summary := "Remote quota"
					if q.Stale {
						summary += " (stale)"
					}
					if q.Balance != "" {
						summary += " · " + q.Balance
					}
					if q.UsagePct != nil {
						summary += fmt.Sprintf(" · %.0f%% used", *q.UsagePct)
					}
					for _, w := range q.Windows {
						// Windows whose usage is not a percentage (e.g. a
						// token pass reporting only remaining credit) surface
						// their sanitized note instead.
						if w.Note != "" {
							summary += " · " + w.Name + " " + w.Note
						}
					}
					summary += fmt.Sprintf(" · %d resets", q.ResetsRemaining)
					if q.SoonestResetExpiry != nil {
						summary += " (exp " + q.SoonestResetExpiry.Format("2006-01-02 15:04 MST") + ")"
					}
					ui.Text(c, summary).FontSize(11.5).TextColor(p.muted).FillWidth()
				}
				for wi, w := range pl.Windows {
					a.gwWindowBar(c, p, fmt.Sprintf("%s:%d", pl.ID, wi), w)
				}
			})
		}
		hairline(c, p)
		ui.Text(c, "Plan state above is plan-level cooldown only; model-level cooldowns are not reported, so a plan not cooling may still hold cooling models.").
			FontSize(11.5).TextColor(p.faint).FillWidth()
	})
}

func (a *app) gwWindowBar(c *ui.Context, p *palette, key string, w api.GatewayWindowUsage) {
	rem := w.RemainingRatio
	if rem < 0 {
		rem = 0
	}
	if rem > 1 {
		rem = 1
	}
	usedPct := float32((1.0 - rem) * 100.0)
	barCol := p.added
	if rem < 0.15 {
		barCol = p.removed
	} else if rem < 0.35 {
		barCol = p.warning()
	}
	label := w.Name
	if label == "" {
		label = w.Duration
	}
	ui.Column(c.Key("gw-win:"+key)).FillWidth().Gap(3).Padding(2, 0).Children(func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, fmt.Sprintf("%s (%s %s)", label, w.Duration, w.Mode)).FontSize(11.5).Font(mono()).TextColor(p.muted).SingleLine()
			ui.Spacer(c)
			detail := fmt.Sprintf("%.0f%% left · %d/%d tok · surplus %+.2f", rem*100, w.UsedTokens, w.MaxTokens, w.Surplus)
			if w.MaxRequests > 0 {
				detail = fmt.Sprintf("%.0f%% left · %d/%d req · surplus %+.2f", rem*100, w.UsedRequests, w.MaxRequests, w.Surplus)
			}
			ui.Text(c, detail).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine()
		})
		ui.Row(c).FillWidth().Height(6).Radius(3).Background(p.text.Alpha(0.07)).Clip().Children(func() {
			ui.Box(c).WidthPercent(usedPct).Height(6).Radius(3).Background(barCol)
		})
	})
}

func (a *app) gwRoutesCard(c *ui.Context, p *palette, st store) {
	routes := st.gwRoutes
	a.gwCard(c, p, "routes", "Routes", iconRule, func() {
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
		for i, rt := range routes {
			if i > 0 {
				hairline(c, p)
			}
			ui.Column(c.Key("gw-route:" + rt.ID)).FillWidth().Gap(5).Children(func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, rt.ID).FontSize(13).FontWeight(600).Font(mono()).TextColor(p.text).SingleLine()
					tag(c, p, rt.Family)
					if rt.Preset != "" {
						tag(c, p, rt.Preset)
					}
					if !rt.Enabled {
						tag(c, p, "disabled")
					}
					ui.Spacer(c)
					var tgts []string
					for _, t := range rt.Targets {
						tgts = append(tgts, t.Plan+"/"+t.Model)
					}
					ui.Text(c, strings.Join(tgts, " -> ")).FontSize(11.5).Font(mono()).TextColor(p.muted).SingleLine()
				})
				ui.Row(c).FillWidth().Gap(6).Wrap().Children(func() {
					if len(rt.Profiles) > 0 {
						tag(c, p, "profiles:"+strings.Join(rt.Profiles, ","))
					}
					if len(rt.Protocols) > 0 {
						tag(c, p, "proto:"+strings.Join(rt.Protocols, ","))
					}
					if len(rt.Models) > 0 {
						tag(c, p, "models:"+strings.Join(rt.Models, ","))
					}
				})
			})
		}
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
	selected := windowForIndex(a.gwUsageWinIdx)
	win := gwUsageWindowFact{selected: selected, asked: st.gwUsageAsked, reported: usage.Window}
	// Only an answered fetch leaves a range to describe at all.
	answered := st.gwUsageFetch != gwUnknown
	mismatch := answered && win.mismatch()
	unconfirmed := answered && win.unconfirmed()
	a.gwCard(c, p, "usage", "Usage Ledger", iconClock, func() {
		prev := a.gwUsageWinIdx
		a.words(c, "gw-usage-win", &a.gwUsageWinIdx, "1h", "24h", "7d")
		if a.gwUsageWinIdx != prev {
			a.mirrorView()
			a.nudge(a.search)
		}
		if st.gwUsageFetch == gwStale {
			gwStaleTag(c, p)
		}
	}, func() {
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
			a.fact(c, p, "Range", fmt.Sprintf("%s (as answered)", selected))
		}
		tot := usage.Totals
		a.fact(c, p, "Totals", fmt.Sprintf("%d reqs (%d err) · %d tokens (%d in / %d out / %d cache read)",
			tot.Requests, tot.Errors, tot.TotalTokens, tot.InputTokens, tot.OutputTokens, tot.CacheReadTokens))
		if tot.AvgTTFTMs > 0 {
			a.fact(c, p, "Cache & TTFT", fmt.Sprintf("cache hit %.0f%% · avg TTFT %.0fms · avg dur %.0fms",
				tot.CacheHitRatio*100, tot.AvgTTFTMs, tot.AvgDurationMs))
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
		hairline(c, p)
		for i, b := range usage.Buckets {
			ui.Row(c.Key(fmt.Sprintf("gw-use:%d:%s:%s", i, b.Plan, b.Model))).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Text(c, b.Plan+"/"+b.Model).FontSize(12).Font(mono()).TextColor(p.text).SingleLine().Grow(1).MinWidth(80)
				ui.Text(c, fmt.Sprintf("%d req · %d tok · hit %.0f%% · %.0fms",
					b.Requests, b.TotalTokens, b.CacheHitRatio*100, b.AvgTTFTMs)).
					FontSize(11.5).Font(mono()).TextColor(p.muted).SingleLine()
			})
		}
	})
}

func (a *app) gwSecretsCard(c *ui.Context, p *palette, st store) {
	secrets := st.gwSecrets
	a.gwCard(c, p, "secrets", "Credentials", iconLock, func() {
		if st.gwSecretsFetch == gwUnknown {
			tag(c, p, "secrets unknown")
			return
		}
		tag(c, p, fmt.Sprintf("%d stored", len(secrets)))
		if st.gwSecretsFetch == gwStale {
			gwStaleTag(c, p)
		}
	}, func() {
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
					a.ghostButton(c, "gw-sec-del:"+s.Name, "", iconTrash).Clicked(func() {
						a.actDeleteSecret(s.Name)
					})
				})
			}
			if len(secrets) == 0 {
				ui.Text(c, "No secrets stored yet under ~/.crabbro/secrets/.").FontSize(13).TextColor(p.muted).FillWidth()
			}
		}
		hairline(c, p)
		caption(c, p, "Store API Key (mode 0600)")
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.TextInput(c.Key("gw-sec-name"), &a.gwSecretName).Placeholder("secret_name").Width(180)
			ui.TextInput(c.Key("gw-sec-val"), &a.gwSecretVal).Placeholder("API key value").Password().Grow(1).MinWidth(120)
			a.fillButton(c, "gw-sec-save", "Save", iconSave, a.gwSecretName == "" || a.gwSecretVal == "").
				Clicked(func() {
					name := a.gwSecretName
					val := a.gwSecretVal
					a.gwSecretVal = ""
					a.actSaveSecret(name, val)
				})
		})
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
