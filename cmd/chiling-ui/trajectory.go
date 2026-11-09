package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"
)

// The trajectory page: the tail of the timeline, newest first.

// trajStagger is how many leading rows join the page's staggered entrance;
// deeper rows come in at once rather than wait their turn.
const trajStagger = 12

// trajGroupGap is the pause between adjacent steps that opens a new group.
const trajGroupGap = 2 * time.Minute

// trajectoryPage is the tail of the timeline, newest first.
func (a *app) trajectoryPage(c *ui.Context, p *palette) {
	st := a.snapshotState()
	if len(st.traj) == 0 {
		emptyState(c, p, iconClock, "No steps yet",
			"Start recording, send traffic through the proxy, and every step lands here as a timeline.", "chiling record start")
		return
	}
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).FillWidth().Padding(14, 16, 22).Gap(2).Children(func() {
			for i, it := range st.traj {
				// A pause between steps is a chapter of its own: a
				// hairline keeps the read of it.
				if i > 0 && st.traj[i-1].At.Sub(it.At) > trajGroupGap {
					ui.Column(c.Key("trajgap:"+it.ID)).FillWidth().Padding(2, 0).Children(func() {
						ui.Box(c).FillWidth().Height(1).Background(p.faint.Alpha(0.28))
					})
				}
				row := ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Center).Padding(6, 10).Radius(8)
				if row.Hovered() {
					row.Background(p.hover)
				}
				row.Transition(hoverFade)
				row.Children(func() {
					ui.Text(c, trajTime(it.At)).FontSize(11).Font(mono()).MinWidth(54).
						TextColor(p.faint).SingleLine().ContentTransition(ui.ContentTransition{Numeric: true})
					kindCol, kindBg := kindColor(p, it.Kind)
					ui.Box(c).Width(7).Height(7).Radius(4).Background(kindCol).Shrink(0)
					a.pill(c, activityKind(it.Kind), kindCol, kindBg, false)
					ui.Text(c, it.Summary).FontSize(12.5).TextColor(p.text).SingleLine().Shrink(1).MinWidth(40)
					if it.RequestID != "" {
						link := ui.ButtonBase(c.Key("traj:"+it.ID)).Padding(2, 6).Radius(4).Shrink(0).
							AlignItems(ui.Center).Label("Show the request " + it.RequestID).Tooltip("Show the request")
						if link.Hovered() {
							link.Background(p.hover)
						}
						// Quiet until the row is under the pointer, then
						// the accent, fading as welcomeStar's does.
						linkCol := p.faint
						if row.Hovered() {
							linkCol = p.accent
						}
						linkCol = link.AnimateColor("text", linkCol, ui.Animation{Duration: hoverFade.Duration})
						link.Children(func() {
							ui.Text(c, string(it.RequestID)).FontSize(11).Font(mono()).TextColor(linkCol).SingleLine()
						})
						if link.Clicked() {
							a.showRequest(it.RequestID)
						}
					}
				})
			}
		})
	})
}

// trajTime is when a step happened: within the minute as its age, before
// that as the clock said. The sync goroutine's 5 s tick redraws the page,
// so the age keeps up without a timer of its own.
func trajTime(at time.Time) string {
	if age := time.Since(at); age >= 0 && age < time.Minute {
		return fmt.Sprintf("%ds", int(age.Seconds()))
	}
	return at.Local().Format("15:04:05")
}

// kindColor tints a trajectory step by what it is.
func kindColor(p *palette, kind string) (ui.Color, ui.Color) {
	switch kind {
	case "session_started", "session_stopped":
		return p.changed, p.changed.Alpha(0.11)
	case "rule_applied", "rule_created":
		return p.accent, p.accent.Alpha(0.14)
	case "verify":
		return p.added, p.added.Alpha(0.13)
	case "find":
		return p.warning(), p.warning().Alpha(0.13)
	}
	return p.muted, p.muted.Alpha(0.12)
}

// activityKind is the short label a trajectory step wears.
func activityKind(kind string) string {
	switch kind {
	case "session_started", "session_stopped":
		return "session"
	case "request_captured", "request_completed":
		return "capture"
	case "rule_applied", "rule_created":
		return "rule"
	case "verify":
		return "verify"
	case "find":
		return "find"
	}
	return kind
}

// showRequest jumps to the requests page with a request chosen.
func (a *app) showRequest(id string) {
	a.page = pageRequests
	a.pendingRequestID = string(id)
	a.invalidate()
}
