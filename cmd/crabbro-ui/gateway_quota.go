package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/crabbro/pkg/api"
)

func gwRemoteQuota(c *ui.Context, p *palette, quota *api.RemoteQuotaJSON) {
	summary := "Remote quota"
	tone := p.muted
	if quota.Stale {
		summary += " (stale)"
		tone = p.warning()
	}
	if quota.Balance != "" {
		summary += " · " + quota.Balance
	}
	for _, window := range quota.Windows {
		if window.Note != "" {
			summary += " · " + window.Name + " " + window.Note
		}
	}
	summary += fmt.Sprintf(" · %d resets", quota.ResetsRemaining)
	if quota.SoonestResetExpiry != nil {
		summary += " (exp " + quota.SoonestResetExpiry.Format("2006-01-02 15:04 MST") + ")"
	}

	var windows []api.RemoteQuotaWindow
	for _, window := range quota.Windows {
		if window.UsagePct != nil {
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 && quota.UsagePct != nil {
		windows = append(windows, api.RemoteQuotaWindow{Name: "Quota", UsagePct: quota.UsagePct, ResetAt: quota.ResetAt})
	}

	ui.Column(c).FillWidth().Gap(10).Padding(12).Radius(8).Background(p.text.Alpha(0.04)).Children(func() {
		ui.Text(c, summary).FontSize(11.5).TextColor(tone).FillWidth()
		if len(windows) == 0 {
			return
		}
		ui.Grid(c).FillWidth().Columns(3).Gap(6).Children(func() {
			for i, window := range windows {
				remaining := 1 - max(0, min(100, *window.UsagePct))/100
				gwQuotaDial(c.Key(fmt.Sprint(i)), p, window.Name, remaining, window.ResetAt, quota.Stale)
			}
		})
	})
}

func gwQuotaDial(c *ui.Context, p *palette, name string, remaining float64, resetAt *time.Time, stale bool) {
	tone, number := gwQuotaTone(p, remaining), p.text
	if stale {
		tone, number = p.faint, p.muted
	} else if remaining < 0.35 {
		number = tone
	}
	reset := ""
	if resetAt != nil && !resetAt.IsZero() {
		reset = "Resets " + resetAt.Local().Format("Jan 2, 15:04")
	}
	ui.Column(c).MinWidth(0).Gap(5).AlignItems(ui.Center).Children(func() {
		ui.Text(c, name).FillWidth().FontSize(11.5).FontWeight(600).TextAlign(ui.Center).TextColor(p.text)
		ui.Column(c).Size(68, 68).Center().Gap(0).Draw(func(painter *ui.Painter, bounds ui.Rect) {
			const stroke = 5
			x, y := bounds.X+bounds.W/2, bounds.Y+bounds.H/2
			r := (min(bounds.W, bounds.H) - stroke - 4) / 2
			k := r * 0.5522847498
			var ring ui.Path
			// Start at twelve o'clock so the remaining allowance reads clockwise.
			ring.MoveTo(x, y-r).
				CubeTo(x+k, y-r, x+r, y-k, x+r, y).
				CubeTo(x+r, y+k, x+k, y+r, x, y+r).
				CubeTo(x-k, y+r, x-r, y+k, x-r, y).
				CubeTo(x-r, y-k, x-k, y-r, x, y-r).Close()
			painter.StrokePath(&ring, stroke, p.text.Alpha(0.11))
			switch {
			case remaining >= 1:
				painter.StrokePath(&ring, stroke, tone)
			case remaining > 0:
				painter.StrokePath(ring.Trim(0, float32(remaining)), stroke, tone)
			}
		}).Children(func() {
			ui.Text(c, fmt.Sprintf("%.0f%%", remaining*100)).FontSize(15).FontWeight(600).
				Font(mono()).TextColor(number).SingleLine()
			ui.Text(c, "left").FontSize(10).TextColor(p.muted).SingleLine()
		})
		if reset != "" {
			ui.Text(c, reset).FillWidth().TextAlign(ui.Center).FontSize(10).TextColor(p.muted)
		}
	})
}
