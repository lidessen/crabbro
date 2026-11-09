package main

import (
	"fmt"
	"runtime"
	"time"

	"github.com/egoist/mygo/ui"
)

// The pages the sidebar chooses among.
const (
	pageRequests   = "requests"
	pageRules      = "rules"
	pageTrajectory = "trajectory"
	pageDaemon     = "daemon"
)

// mono is the family code and bodies draw in, or "" for the system's.
func mono() string {
	if runtime.GOOS == "darwin" {
		return "Menlo"
	}
	return ""
}

// fadePage brings a page in as the sidebar chooses it, as Mayoi's are: it
// fades in, without moving. Give the element a key that carries the page,
// so the fade replays at each switch.
func fadePage(e ui.Element) ui.Element {
	return e.Opacity(e.Keyframes("show", []ui.Keyframe{{Value: 0}, {At: 150 * time.Millisecond, Value: 1, Ease: ui.EaseOut}}))
}

// pillMotion is how a small thing of the chrome comes and goes, as the
// sidebar's count does, after Mayoi's terminal pill: it drops in from a
// little above where it settles, and goes back up as it leaves. Only its
// colors follow it after.
var pillMotion = ui.ElementTransition{Duration: 160 * time.Millisecond, Colors: true, Enter: &ui.Motion{Y: -6}, Exit: &ui.Motion{Y: -4}}

// busyFade is how the status bar's busy line comes and goes: it fades in
// place, as Mayoi's reveals do, rather than popping or shoving its
// neighbors about.
var busyFade = ui.ElementTransition{Duration: 130 * time.Millisecond, Enter: &ui.Motion{}, Exit: &ui.Motion{}}

// view builds the window: a chrome shell — the title bar, the sidebar and
// the status bar — around the page the sidebar chose.
func (a *app) view(c *ui.Context) {
	a.setServices(c.Services())
	a.drainQueue()
	p := applyPal(c)
	a.pal = p

	ui.Column(c).Fill().Background(p.chrome).Children(func() {
		a.titleBar(c, p)
		ui.Row(c).Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			a.sidebar(c, p)
			ui.Column(c.Key("page")).Grow(1).MinWidth(0).Background(p.editor).
				BorderWidth(0, 0, 0, 1).BorderColor(p.border).Children(func() {
				ui.Column(c.Key("page:" + a.page)).Grow(1).Children(func() {
					a.pageBody(c, p)
				})
			})
		})
		a.statusBar(c, p)
	})
	a.toasts(c)
}

// titleBar is the chrome's top: the mark and name, and the recording
// controls at its right.
func (a *app) titleBar(c *ui.Context, p *palette) {
	bar := c.TitleBar()
	ui.Row(c).Height(max(bar.Height, titleBarH)).Padding(0, bar.Right+10, 0, bar.Left+12).
		AlignItems(ui.Center).Background(p.chrome).BorderWidth(0, 0, 1, 0).BorderColor(p.border).
		DragWindow().Children(func() {
		ui.Row(c).Gap(9).AlignItems(ui.Center).Children(func() {
			ui.Row(c).Size(22, 22).Center().Radius(7).Background(p.accent.Alpha(0.16)).Children(func() {
				ui.Icon(c, iconShell).FontSize(13).TextColor(p.accent)
			})
			ui.Text(c, "Chiling").FontSize(13.5).FontWeight(700).LetterSpacing(0.2).TextColor(p.text).SingleLine()
			ui.Text(c, pageTitle(a.page)).FontSize(12).TextColor(p.faint).SingleLine()
		})
		ui.Spacer(c)
		a.recordControls(c, p)
	})
}

// pageTitle is the name the title bar gives the page in view.
func pageTitle(page string) string {
	switch page {
	case pageRules:
		return "/ Rules"
	case pageTrajectory:
		return "/ Trajectory"
	case pageDaemon:
		return "/ Daemon"
	}
	return "/ Requests"
}

// recordControls is the recording pill and its start and stop.
func (a *app) recordControls(c *ui.Context, p *palette) {
	st := a.snapshotState()
	if a.recordingNow() {
		ui.Row(c.Key("rec-pill")).Gap(6).AlignItems(ui.Center).Height(24).Padding(0, 9).
			Radius(12).Background(p.removed.Alpha(0.14)).Children(func() {
			// The dot breathes while the daemon records: 0.7s down and
			// back, a 1.4s round, as the daemon page's does.
			dot := ui.Box(c.Key("rec-pill-dot")).Width(7).Height(7).Radius(4).Background(p.removed)
			dot.Opacity(dot.Keyframes("breathe", []ui.Keyframe{
				{Value: 1},
				{At: 700 * time.Millisecond, Value: 0.35, Ease: ui.EaseInOut},
			}, ui.KeyframeOptions{Repeat: ui.RepeatForever, Alternate: true}))
			ui.Text(c, "recording").FontSize(11.5).FontWeight(600).TextColor(p.removed).SingleLine()
			if id := st.session.ID; id != "" {
				ui.Text(c, id).FontSize(11).Font(mono()).TextColor(p.removed.Alpha(0.75)).SingleLine()
			}
		})
		a.ghostButton(c, "stop-record", "Stop", nil).Clicked(func() { a.actStopRecording() })
		return
	}
	a.fillButton(c, "start-record", "Record", iconRecord).Clicked(func() { a.actStartRecording() })
}

// ---------------------------------------------------------------------------
// buttons

type button ui.Element

// Clicked reports the button was pressed this frame and runs fn then.
func (b button) Clicked(fn func()) {
	if ui.Element(b).Clicked() {
		fn()
	}
}

// Disabled grays a button out, as one whose action has nothing to act on.
func (b button) Disabled(off bool) button {
	ui.Element(b).Disabled(off)
	return b
}

// fillButton is the primary button: the accent as its background, white
// text on it, as Mayoi's are.
func (a *app) fillButton(c *ui.Context, key, label string, icon *ui.SVG) button {
	p := a.pal
	b := ui.ButtonBase(c.Key(key)).Height(26).Padding(0, 11).Gap(6).Radius(7).
		AlignItems(ui.Center).Label(label).Tooltip(label).
		Background(p.fill).TextColor(p.fillText)
	switch {
	case b.Pressed():
		b.Background(p.fillPressed)
	case b.Hovered():
		b.Background(p.fillHover)
	}
	b.Transition(hoverFade)
	b.Children(func() {
		if icon != nil {
			ui.Icon(c, icon).FontSize(13).TextColor(p.fillText)
		}
		ui.Text(c, label).FontSize(12).FontWeight(600).TextColor(p.fillText).SingleLine()
	})
	return button(b)
}

// ghostButton is a quiet button: no face until the pointer is on it,
// then a wash.
func (a *app) ghostButton(c *ui.Context, key, label string, icon *ui.SVG) button {
	p := a.pal
	b := ui.ButtonBase(c.Key(key)).Height(26).Padding(0, 9).Gap(6).Radius(7).
		AlignItems(ui.Center).Label(label).Tooltip(label)
	col := p.muted
	switch {
	case b.Pressed():
		b.Background(p.surfacePressed)
		col = p.text
	case b.Hovered():
		b.Background(p.hover)
		col = p.text
	}
	b.Transition(hoverFade)
	b.Children(func() {
		if icon != nil {
			ui.Icon(c, icon).FontSize(13).TextColor(col)
		}
		if label != "" {
			ui.Text(c, label).FontSize(12).FontWeight(500).TextColor(col).SingleLine()
		}
	})
	return button(b)
}

// sidebar is the pages of the window, as Mayoi's are: rows on the chrome,
// the chosen one in a tint of the accent and under the rail at its left.
func (a *app) sidebar(c *ui.Context, p *palette) {
	st := a.snapshotState()
	ui.Column(c.Key("sidebar")).Width(sidebarW).Background(p.chrome).
		Padding(12, 10).Gap(3).Children(func() {
		ui.Text(c, "WORKSPACE").FontSize(10).FontWeight(650).LetterSpacing(0.8).TextColor(p.faint).
			Padding(2, 9, 6).SingleLine()
		item := func(key, label string, icon *ui.SVG, count int) {
			on := a.page == key
			row := ui.ButtonBase(c.Key("side:"+key)).FillWidth().Height(32).Padding(0, 10).Gap(10).
				Radius(8).AlignItems(ui.Center).Label(label)
			switch {
			case on:
				row.Background(p.active)
			case row.Pressed():
				row.Background(p.surfacePressed)
			case row.Hovered():
				row.Background(p.hover)
			}
			row.Transition(hoverFade)
			col := p.muted
			if on {
				col = p.text
			}
			// The chosen row wears the accent as a rail at its left edge,
			// as Mayoi's rail marks its square: it grows from the row's
			// middle as the choice lands, and away as it leaves.
			railH := float32(0)
			if on {
				railH = 16
			}
			railH = row.Animate("rail", railH, 160*time.Millisecond)
			row.Children(func() {
				ui.Box(c.Key("rail")).Absolute().Left(0).Top((32 - railH) / 2).
					Width(3).Height(railH).Radius(1.5).Background(p.accent)
				iconCol := col
				if on {
					iconCol = p.accent
				}
				ui.Icon(c, icon).FontSize(15).TextColor(iconCol)
				ui.Text(c, label).FontSize(13).FontWeight(550).TextColor(col).SingleLine().Grow(1)
				if count > 0 {
					ui.Text(c, fmt.Sprint(count)).FontSize(10.5).FontWeight(600).Font(mono()).
						TextColor(p.muted).Padding(1, 7).Radius(9).Background(p.faint.Alpha(0.14)).
						SingleLine().Transition(pillMotion)
				}
			})
			if row.Clicked() {
				a.page = key
			}
		}
		item(pageRequests, "Requests", iconList, len(st.requests))
		item(pageRules, "Rules", iconSliders, len(st.rules))
		item(pageTrajectory, "Trajectory", iconClock, 0)
		item(pageDaemon, "Daemon", iconServer, 0)
		ui.Spacer(c)
		a.sidebarFooter(c, p, st)
	})
}

// sidebarFooter is the daemon in brief at the sidebar's foot: a dot for
// how it is, and the proxy address it listens on.
func (a *app) sidebarFooter(c *ui.Context, p *palette, st store) {
	dotCol, label := p.faint, "Connecting…"
	switch st.conn {
	case connConnected:
		dotCol, label = p.added, "Proxy running"
	case connDown:
		dotCol, label = p.removed, "Daemon offline"
	}
	ui.Column(c.Key("side:foot")).FillWidth().Padding(9, 11).Gap(3).Radius(9).
		Background(p.surface).Border(1, p.border).Children(func() {
		ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
			dot := ui.Box(c.Key("side-dot")).Width(7).Height(7).Radius(4)
			dot.Background(dot.AnimateColor("conn", dotCol, ui.Animation{Duration: hoverFade.Duration}))
			ui.Text(c, label).FontSize(12).FontWeight(600).TextColor(p.text).SingleLine()
		})
		if st.conn == connConnected && st.status.ProxyAddr != "" {
			ui.Text(c, st.status.ProxyAddr).FontSize(11).Font(mono()).TextColor(p.muted).SingleLine()
		}
	})
}

// pageBody is the page the sidebar chose.
func (a *app) pageBody(c *ui.Context, p *palette) {
	switch a.page {
	case pageRules:
		a.rulesPage(c, p)
	case pageTrajectory:
		a.trajectoryPage(c, p)
	case pageDaemon:
		a.daemonPage(c, p)
	default:
		a.requestsPage(c, p)
	}
}

// statusBar is the chrome's bottom: how the daemon is, and the counts.
func (a *app) statusBar(c *ui.Context, p *palette) {
	st := a.snapshotState()
	ui.Row(c).Height(statusBarH).Padding(0, 12).Gap(12).AlignItems(ui.Center).
		Background(p.chrome).BorderWidth(1, 0, 0, 0).BorderColor(p.border).Children(func() {
		dotCol, label := p.faint, "connecting"
		switch st.conn {
		case connConnected:
			dotCol, label = p.added, "daemon connected"
		case connDown:
			dotCol, label = p.removed, "daemon down"
		}
		if st.connErr != "" {
			label += " — " + st.connErr
		}
		// The dot turns from one state's color to the next as the text
		// crossfades, as the trajectory's links do.
		dot := ui.Box(c.Key("conn-dot")).Width(7).Height(7).Radius(4)
		dot.Background(dot.AnimateColor("conn", dotCol, ui.Animation{Duration: hoverFade.Duration}))
		ui.Text(c, label).FontSize(11.5).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(40).
			ContentTransition(ui.ContentTransition{})
		if a.busy != "" {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Transition(busyFade).Children(func() {
				ui.Spinner(c).Size(11, 11)
				ui.Text(c, a.busy).FontSize(11.5).TextColor(p.muted).SingleLine()
			})
		}
		ui.Spacer(c)
		if st.listHiddenCount > 0 {
			ui.Text(c, fmt.Sprintf("%d hidden", st.listHiddenCount)).FontSize(11.5).TextColor(p.faint).SingleLine()
		}
		if st.conn == connConnected {
			if v := st.status.Version; v != "" {
				ui.Text(c, "v"+v).FontSize(11.5).TextColor(p.faint).SingleLine()
			}
		}
	})
}

// toasts shows the notes the actions left, and drops them.
func (a *app) toasts(c *ui.Context) {
	for _, msg := range a.notes {
		c.Toast(msg)
	}
	a.notes = a.notes[:0]
}
