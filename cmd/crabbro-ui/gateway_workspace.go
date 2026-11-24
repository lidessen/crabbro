package main

import (
	"fmt"

	"github.com/egoist/mygo/ui"
)

type gatewaySection int

const (
	gwCardGap      = 12
	gwContentWidth = 1200
)

const (
	gwOverview gatewaySection = iota
	gwPlans
	gwRoutes
	gwUsage
	gwCredentials
)

func (a *app) gatewayPage(c *ui.Context, p *palette, st store) {
	ui.Column(c.Key("gateway-workspace")).Grow(1).FillWidth().MinHeight(0).Children(func() {
		ui.Column(c).FillWidth().MaxWidth(gwContentWidth).AlignSelf(ui.Center).Shrink(0).Padding(18, workspaceInset, 0).Gap(6).Children(func() {
			workspaceTitle(c, p, "Gateway")
			section := int(a.gwSection)
			a.words(c, "gw-nav", &section, "Overview", "Plans & Quotas", "Routes", "Usage", "Credentials")
			a.gwSection = gatewaySection(section)
		})
		hairline(c, p)
		section := a.gwSection
		ui.Scroll(c.Key(fmt.Sprintf("gw-scroll:%d", section))).Grow(1).MinHeight(0).Children(func() {
			ui.Column(c).FillWidth().MaxWidth(gwContentWidth).AlignSelf(ui.Center).Padding(16, workspaceInset, 24).Gap(12).Children(func() {
				switch section {
				case gwOverview:
					a.gwStatusCard(c, p, st)
					a.gwOverviewHealth(c, p, st)
					a.gwOverviewUsage(c, p, st)
				case gwPlans:
					a.gwPlansCard(c, p, st)
				case gwRoutes:
					a.gwRoutesCard(c, p, st)
				case gwUsage:
					a.gwUsageCard(c, p, st)
				case gwCredentials:
					a.gwSecretsCard(c, p, st)
				}
			})
		})
	})
}

func gwGridColumns(c *ui.Context, minWidth float32, maxColumns int) int {
	return max(1, min(maxColumns, int((gwContentAreaWidth(c)+gwCardGap)/(minWidth+gwCardGap))))
}

func gwContentAreaWidth(c *ui.Context) float32 {
	width, _ := c.Size()
	return min(width-sidebarWidth(c), gwContentWidth) - 2*workspaceInset
}

// Cards stack at their own height in each column. Assign slots by data
// order so a quota refresh can change height without moving a card sideways.
func gwCardColumns(c *ui.Context, columns, count int, draw func(int)) {
	ui.Grid(c).FillWidth().Columns(columns).Gap(gwCardGap).AlignItems(ui.Start).Children(func() {
		for column := range min(columns, count) {
			ui.Column(c.Key(fmt.Sprintf("gw-column:%d", column))).MinWidth(0).Gap(gwCardGap).Children(func() {
				for i := column; i < count; i += columns {
					draw(i)
				}
			})
		}
	})
}

func gwMetric(c *ui.Context, p *palette, label, value, detail string, detailTone ui.Color) ui.Element {
	return ui.Column(c).Grow(1).Basis(0).MinWidth(0).Padding(12).Gap(4).
		Radius(8).Background(p.editor).Border(1, p.border).Children(func() {
		ui.Text(c, label).FontSize(11.5).FontWeight(500).TextColor(p.muted).FillWidth()
		ui.Text(c, value).FontSize(18).FontWeight(600).Font(mono()).TextColor(p.text).FillWidth()
		if detail != "" {
			ui.Text(c, detail).FontSize(11).TextColor(detailTone).FillWidth()
		}
	})
}

func (a *app) gwOverviewHealth(c *ui.Context, p *palette, st store) {
	if st.gwPlansFetch != gwFresh || len(st.gwPlans) == 0 || gwOffNow(st) {
		return
	}
	a.gwCard(c, p, "attention", "Plan health", iconSliders, func() {
		a.quietButton(c, "gw-open-plans", "View plans", nil).Clicked(func() { a.gwSection = gwPlans })
	}, func() {
		total := 0
		for _, plan := range st.gwPlans {
			if !plan.Enabled || (plan.Healthy && plan.HasSecret) {
				continue
			}
			total++
			if total > 4 {
				continue
			}
			ui.Row(c.Key("gw-attention:" + plan.ID)).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, plan.ID).FontSize(12.5).FontWeight(600).Font(mono()).TextColor(p.text).Grow(1).MinWidth(0)
				label, tone := gwPlanState(p, plan)
				ui.Text(c, label).WidthPercent(46).FontSize(12).TextAlign(ui.End).TextColor(tone)
			})
		}
		if total == 0 {
			ui.Text(c, "No plan-level cooldowns or missing credentials reported for enabled plans.").
				FontSize(13).TextColor(p.muted).FillWidth()
		} else if total > 4 {
			ui.Text(c, fmt.Sprintf("%d more plans need attention", total-4)).FontSize(12).TextColor(p.muted)
		}
	})
}

func (a *app) gwOverviewUsage(c *ui.Context, p *palette, st store) {
	usage := st.gwUsage
	if st.gwUsageFetch != gwFresh || usage.Window == "" || gwOffNow(st) {
		return
	}
	a.gwCard(c, p, "traffic", "Traffic", iconChart, func() {
		ui.Row(c).Gap(9).AlignItems(ui.Center).Children(func() {
			tag(c, p, usage.Window)
			a.quietButton(c, "gw-open-usage", "View usage", nil).Clicked(func() { a.gwSection = gwUsage })
		})
	}, func() {
		gwUsageMetrics(c, p, usage.Totals)
	})
}
