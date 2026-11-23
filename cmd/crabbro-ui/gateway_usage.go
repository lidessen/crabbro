package main

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/crabbro/pkg/api"
)

type gwUsagePlan struct {
	totals api.GatewayUsageBucket
	models []api.GatewayUsageBucket
}

func gwUsageByPlan(buckets []api.GatewayUsageBucket) []gwUsagePlan {
	var plans []gwUsagePlan
	indices := make(map[string]int)
	for _, bucket := range buckets {
		index, exists := indices[bucket.Plan]
		if !exists {
			index = len(plans)
			indices[bucket.Plan] = index
			plans = append(plans, gwUsagePlan{totals: api.GatewayUsageBucket{Plan: bucket.Plan}})
		}
		plan := &plans[index]
		plan.models = append(plan.models, bucket)
		plan.totals.Requests += bucket.Requests
		plan.totals.Errors += bucket.Errors
		plan.totals.Retries += bucket.Retries
		plan.totals.TotalTokens += bucket.TotalTokens
		plan.totals.InputTokens += bucket.InputTokens
		plan.totals.CacheReadTokens += bucket.CacheReadTokens
	}
	slices.SortFunc(plans, func(a, b gwUsagePlan) int {
		if order := cmp.Compare(b.totals.TotalTokens, a.totals.TotalTokens); order != 0 {
			return order
		}
		return cmp.Compare(a.totals.Plan, b.totals.Plan)
	})
	return plans
}

// gwUsageBreakdown keeps the ranking and model detail in view together on
// larger windows. At smaller widths, model detail opens under its plan.
func (a *app) gwUsageBreakdown(c *ui.Context, p *palette, buckets []api.GatewayUsageBucket) {
	plans := gwUsageByPlan(buckets)
	split := gwContentAreaWidth(c) >= 850
	selected := slices.IndexFunc(plans, func(plan gwUsagePlan) bool { return plan.key() == a.gwSelectedUsage })
	if split {
		selected = max(0, selected)
	}
	ranking := func() {
		panel(c.Key("gw-usage-ranking"), p).FillWidth().MinWidth(0).Children(func() {
			ui.Row(c).FillWidth().Padding(13, 14).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "Plans").FontSize(15).FontWeight(600).TextColor(p.text).SingleLine()
				ui.Spacer(c)
				ui.Text(c, "Ranked by tokens").FontSize(11).TextColor(p.muted).SingleLine()
			})
			hairline(c, p)
			gwUsageRankingHeader(c, p)
			for i, plan := range plans {
				active := i == selected
				if gwUsageRankingRow(c, p, plan, plans[0].totals.TotalTokens, active, split) {
					if active && !split {
						a.gwSelectedUsage, selected = "", -1
					} else {
						a.gwSelectedUsage, selected = plan.key(), i
					}
				}
				if !split && i == selected {
					gwUsageInspector(c.Key("inline:"+plan.key()), p, plan, false)
				}
				if i < len(plans)-1 {
					hairline(c, p)
				}
			}
		})
	}
	if !split {
		ranking()
		return
	}
	ui.Grid(c).FillWidth().ColumnTracks(ui.Fr(3), ui.Fr(2)).Gap(12).AlignItems(ui.Start).Children(func() {
		ranking()
		panel(c.Key("gw-usage-inspector"), p).FillWidth().MinWidth(0).Children(func() {
			gwUsageInspector(c.Key(plans[selected].key()), p, plans[selected], true)
		})
	})
}

func (p gwUsagePlan) key() string { return "gw-usage-plan:" + p.totals.Plan }

func (p gwUsagePlan) name() string {
	if p.totals.Plan == "" {
		return "Unassigned"
	}
	return p.totals.Plan
}

const (
	gwUsageTokenWidth   = 111
	gwUsageRequestWidth = 66
	gwUsageCacheWidth   = 58
	gwUsageErrorsWidth  = 46
)

func gwUsageRankingHeader(c *ui.Context, p *palette) {
	ui.Row(c).FillWidth().Padding(7, 14).Gap(10).AlignItems(ui.Center).Background(p.text.Alpha(0.03)).Children(func() {
		ui.Text(c, "Plan").FontSize(11).FontWeight(500).TextColor(p.muted).Grow(1).MinWidth(0)
		for _, col := range []struct {
			label string
			width float32
		}{
			{"Tokens", gwUsageTokenWidth},
			{"Requests", gwUsageRequestWidth},
			{"Cache hit", gwUsageCacheWidth},
			{"Errors", gwUsageErrorsWidth},
		} {
			ui.Text(c, col.label).Width(col.width).Shrink(0).TextAlign(ui.End).FontSize(11).FontWeight(500).TextColor(p.muted).SingleLine()
		}
		ui.Box(c).Width(12).Shrink(0)
	})
}

func gwUsageRankingRow(c *ui.Context, p *palette, plan gwUsagePlan, maxTokens int64, active, split bool) bool {
	action, icon := "Show models for ", iconChevronRight
	if active && !split {
		action, icon = "Hide models for ", iconChevronDown
	}
	b := ui.ButtonBase(c.Key(plan.key())).Label(action+plan.name()).Justify(ui.Start).
		FillWidth().MinWidth(0).Padding(9, 14).Gap(10).AlignItems(ui.Center)
	tone := p.teal()
	switch {
	case active:
		b.Background(tone.Alpha(0.10))
	case b.Hovered():
		b.Background(p.text.Alpha(0.055))
	default:
		tone = tone.Alpha(0.63)
	}
	b.Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(5).Children(func() {
			ui.Text(c, plan.name()).FontSize(12).FontWeight(600).Font(mono()).TextColor(p.text).FillWidth()
			if plan.totals.Retries > 0 {
				ui.Text(c, gwRetries(plan.totals.Retries)).FontSize(11).TextColor(p.muted)
			}
			if maxTokens > 0 {
				pct := 100 * float32(float64(plan.totals.TotalTokens)/float64(maxTokens))
				gwProgress(c, p, pct, tone)
			}
		})
		gwUsageCell(c, p, gwCount(plan.totals.TotalTokens), gwUsageTokenWidth)
		gwUsageCell(c, p, gwCount(plan.totals.Requests), gwUsageRequestWidth)
		cache := "—"
		if denominator := plan.totals.InputTokens + plan.totals.CacheReadTokens; denominator > 0 {
			cache = fmt.Sprintf("%.0f%%", 100*float64(plan.totals.CacheReadTokens)/float64(denominator))
		}
		gwUsageCell(c, p, cache, gwUsageCacheWidth)
		errorTone := p.faint
		if plan.totals.Errors > 0 {
			errorTone = p.removed
		}
		gwUsageCell(c, p, gwCount(plan.totals.Errors), gwUsageErrorsWidth).TextColor(errorTone)
		ui.Icon(c, icon).FontSize(12).TextColor(p.faint).Shrink(0)
	})
	return b.Clicked()
}

func gwUsageCell(c *ui.Context, p *palette, value string, width float32) ui.Element {
	return ui.Text(c, value).Width(width).Shrink(0).TextAlign(ui.End).FontSize(11.5).Font(mono()).TextColor(p.text).SingleLine()
}

func gwUsageInspector(c *ui.Context, p *palette, plan gwUsagePlan, split bool) {
	body := ui.Column(c).FillWidth().MinWidth(0).Padding(14).Gap(10)
	if !split {
		body.Background(p.text.Alpha(0.025))
	}
	body.Children(func() {
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			title := "Models"
			if split {
				title = plan.name()
			}
			ui.Text(c, title).FontSize(15).FontWeight(600).TextColor(p.text).Grow(1).MinWidth(0)
			tag(c, p, gwModelCount(len(plan.models)))
		})
		for _, model := range plan.models {
			hairline(c, p)
			ui.Column(c.Key("gw-usage-model:" + model.Model)).FillWidth().Gap(4).Children(func() {
				ui.Text(c, model.Model).FontSize(12).FontWeight(600).Font(mono()).TextColor(p.text).FillWidth()
				detail := fmt.Sprintf("%s tokens · %s req · %s cache · %s TTFT",
					gwCount(model.TotalTokens), gwCount(model.Requests), gwCacheHit(model), gwMilliseconds(model.AvgTTFTMs))
				ui.Text(c, detail).FontSize(11.5).TextColor(p.muted).FillWidth()
				if model.Errors > 0 {
					ui.Text(c, gwErrors(model.Errors)).FontSize(11).TextColor(p.removed)
				}
				if model.Retries > 0 {
					ui.Text(c, gwRetries(model.Retries)).FontSize(11).TextColor(p.muted)
				}
			})
		}
	})
}

func gwModelCount(count int) string {
	if count == 1 {
		return "1 model"
	}
	return fmt.Sprintf("%d models", count)
}
