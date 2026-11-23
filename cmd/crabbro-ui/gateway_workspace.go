package main

import (
	"fmt"

	"github.com/egoist/mygo/ui"
)

type gatewaySection int

const (
	gwCardGap      = 12
	gwSidebarWidth = 190
	gwContentWidth = 1200
	gwContentInset = 28
)

const (
	gwOverview gatewaySection = iota
	gwPlans
	gwRoutes
	gwUsage
	gwCredentials
)

var gatewaySections = [...]struct {
	title string
	icon  *ui.SVG
}{
	{"Overview", iconServer},
	{"Plans & Quotas", iconSliders},
	{"Routes", iconRoute},
	{"Usage", iconChart},
	{"Credentials", iconLock},
}

func (a *app) gatewayPage(c *ui.Context, p *palette) {
	st := a.snapshotState()
	ui.Row(c.Key("gateway-workspace")).Grow(1).FillWidth().MinHeight(0).AlignItems(ui.Stretch).Background(p.chrome).Clip().Children(func() {
		a.gatewaySidebar(c, p)
		section := a.gwSection
		ui.Scroll(c.Key(fmt.Sprintf("gw-scroll:%d", section))).Grow(1).MinWidth(0).MinHeight(0).Children(func() {
			ui.Column(c).FillWidth().MaxWidth(gwContentWidth).AlignSelf(ui.Center).Padding(28, gwContentInset, 36).Gap(16).Children(func() {
				if section != gwUsage {
					ui.Text(c, gatewaySections[section].title).FontSize(26).FontWeight(600).TextColor(p.text).SingleLine()
				}
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

func (a *app) gatewaySidebar(c *ui.Context, p *palette) {
	ui.Column(c).Width(gwSidebarWidth).Shrink(0).Padding(20, 12, 16).Gap(5).
		Background(p.editor).BorderWidth(0, 1, 0, 0).BorderColor(p.border).Children(func() {
		ui.Row(c).Padding(0, 8, 15).Gap(9).AlignItems(ui.Center).Children(func() {
			gwGlyph(c, p, iconRoute, p.accent)
			ui.Text(c, "Gateway").FontSize(15).FontWeight(600).TextColor(p.text).SingleLine()
		})
		for i, section := range gatewaySections {
			b := ui.ButtonBase(c.Key(fmt.Sprintf("gw-nav:%d", i))).Label(section.title).
				FillWidth().Height(36).Radius(7).Padding(0, 9).Gap(9).AlignItems(ui.Center).Justify(ui.Start)
			active := a.gwSection == gatewaySection(i)
			tone := p.muted
			switch {
			case active:
				b.Background(p.accent.Alpha(p.wash()))
				tone = p.accent
			case b.Hovered():
				b.Background(p.text.Alpha(0.05))
				tone = p.text
			}
			b.Children(func() {
				ui.Icon(c, section.icon).FontSize(15).TextColor(tone)
				ui.Text(c, section.title).FontSize(13).FontWeight(500).TextColor(tone).SingleLine()
			})
			if b.Clicked() {
				a.gwSection = gatewaySection(i)
			}
		}
	})
}

func gwGridColumns(c *ui.Context, minWidth float32, maxColumns int) int {
	return max(1, min(maxColumns, int((gwContentAreaWidth(c)+gwCardGap)/(minWidth+gwCardGap))))
}

func gwContentAreaWidth(c *ui.Context) float32 {
	width, _ := c.Size()
	return min(width-gwSidebarWidth, gwContentWidth) - 2*gwContentInset
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

func gwGlyph(c *ui.Context, p *palette, icon *ui.SVG, tone ui.Color) {
	ui.Row(c).Size(30, 30).Shrink(0).Center().Radius(8).Background(tone.Alpha(p.wash())).Children(func() {
		ui.Icon(c, icon).FontSize(15).TextColor(tone)
	})
}

func gwMetric(c *ui.Context, p *palette, label, value, detail string, detailTone ui.Color) ui.Element {
	return ui.Column(c).Grow(1).Basis(0).MinWidth(0).Padding(10, 12).Gap(4).
		Radius(8).Background(p.text.Alpha(0.035)).Children(func() {
		ui.Text(c, label).FontSize(11.5).FontWeight(500).TextColor(p.muted).FillWidth()
		ui.Text(c, value).FontSize(17).FontWeight(600).Font(mono()).TextColor(p.text).FillWidth()
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
