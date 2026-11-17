package main

import (
	"fmt"
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

// mono is the family code, paths and ids draw in.
func mono() string { return fontMono }

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

// view builds the window: the title bar, which is also where the pages are
// chosen, the page, and the status line. There is no sidebar: four pages
// are four words, and the page keeps the window's whole width.
func (a *app) view(c *ui.Context) {
	a.setServices(c.Services())
	a.drainQueue()
	a.mirrorView()
	p := applyPal(c)
	a.pal = p

	ui.Column(c).Fill().Background(p.editor).Children(func() {
		a.titleBar(c, p)
		ui.Column(c.Key("page:" + a.page)).Grow(1).MinHeight(0).Children(func() {
			a.pageBody(c, p)
		})
		a.statusBar(c, p)
	})
	a.toasts(c)
}

// titleBar is the window's top: the mark, the pages as words, and the
// recording at its right.
func (a *app) titleBar(c *ui.Context, p *palette) {
	bar := c.TitleBar()
	st := a.snapshotState()
	ui.Row(c).Height(max(bar.Height, titleBarH)).Padding(0, bar.Right+12, 0, bar.Left+14).
		AlignItems(ui.Stretch).Background(p.chrome).BorderWidth(0, 0, 1, 0).BorderColor(p.border).
		DragWindow().Children(func() {
		ui.Row(c).AlignItems(ui.Center).Margin(0, 20, 0, 0).Children(func() { brandMark(c, p, 20) })
		ui.Row(c.Key("nav")).Gap(22).AlignItems(ui.Stretch).Children(func() {
			a.navWord(c, p, pageTrajectory, "Trace", len(foldTrace(st.traj)))
			a.navWord(c, p, pageRequests, "Requests", len(st.requests))
			a.navWord(c, p, pageRules, "Rules", len(st.rules))
			a.navWord(c, p, pageDaemon, "Daemon", 0)
		})
		ui.Spacer(c)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() { a.recordControls(c, p) })
	})
}

// navWord is one page of the title bar: its name, the count of what it
// holds set small beside it, and under the chosen one an accent stroke
// that rests on the bar's own hairline.
func (a *app) navWord(c *ui.Context, p *palette, key, label string, count int) {
	on := a.page == key
	b := ui.ButtonBase(c.Key("nav:" + key)).Gap(6).AlignItems(ui.Center).Label(label)
	col := p.muted
	if on || b.Hovered() {
		col = p.text
	}
	under := float32(0)
	if on {
		under = 1
	}
	under = b.Animate("under", under, 180*time.Millisecond)
	b.Children(func() {
		ui.Text(c, label).FontSize(13).FontWeight(500).TextColor(col).SingleLine()
		if count > 0 {
			ui.Text(c, fmt.Sprint(count)).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine().
				ContentTransition(ui.ContentTransition{Numeric: true})
		}
		ui.Box(c).Absolute().Left(0).Right(0).Bottom(-1).Height(2).Background(p.accent.Alpha(under))
	})
	if b.Clicked() {
		a.page = key
	}
}

// recordControls is the recording at the title bar's right: while one is
// open, its mark and how long it has run, and Stop; otherwise Record.
func (a *app) recordControls(c *ui.Context, p *palette) {
	st := a.snapshotState()
	// Read the session from the one snapshot: asking the store twice lets it
	// change in between, and a session gone by the second look is a nil.
	if s := st.session; s != nil && s.Recording && s.Name != "scope" {
		ui.Row(c.Key("rec-pill")).Gap(7).AlignItems(ui.Center).Height(26).Padding(0, 11).
			Radius(13).Background(p.removed.Alpha(0.09)).InsetShadow(0, 0, 0, 1, p.removed.Alpha(0.14)).Children(func() {
			// The dot breathes while the daemon records: 0.7s down and
			// back, a 1.4s round, as the daemon page's does.
			dot := ui.Box(c.Key("rec-pill-dot")).Width(7).Height(7).Radius(4).Background(p.removed).
				Shadow(0, 0, 0, 2.5, p.removed.Alpha(0.18))
			dot.Opacity(dot.Keyframes("breathe", []ui.Keyframe{
				{Value: 1},
				{At: 700 * time.Millisecond, Value: 0.35, Ease: ui.EaseInOut},
			}, ui.KeyframeOptions{Repeat: ui.RepeatForever, Alternate: true}))
			ui.Text(c, "Recording").FontSize(12).FontWeight(600).TextColor(p.removed).SingleLine()
			if run := c.Now().Sub(s.StartedAt); run >= 0 {
				ui.Text(c, clockLabel(run)).FontSize(11).Font(mono()).TextColor(p.removed.Alpha(0.8)).SingleLine().
					ContentTransition(ui.ContentTransition{Numeric: true})
				// Draw again as the second flips, so the clock keeps time
				// while nothing else moves.
				c.After(time.Second - run%time.Second + 5*time.Millisecond)
			}
		})
		a.quietButton(c, "stop-record", "Stop", iconStop).Clicked(func() { a.actStopRecording() })
		return
	}
	// Record is the window's first action, and red is what recording
	// means: its mark takes that color, on a button otherwise quiet.
	a.plainButton(c, "start-record", "Record", iconRecord, true, &p.removed).Disabled(st.conn != connConnected).
		Clicked(func() { a.actStartRecording() })
}

// clockLabel is a running time as a clock reads it: 0:42, 12:07, 1:02:33.
func clockLabel(d time.Duration) string {
	s := int(d / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// ---------------------------------------------------------------------------
// buttons

// buttonH is the height of a button: roomy enough to press without
// aiming, and still at home in the title bar.
const buttonH = 30

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
// text on it, as Mayoi's are. It stands a little off the page — lit along
// its top edge, a soft shadow of its own color under it. With nothing to
// act on (off) it gives the color up and waits as a plain face: a washed
// accent would read as a second, paler primary.
func (a *app) fillButton(c *ui.Context, key, label string, icon *ui.SVG, off bool) button {
	p := a.pal
	b := ui.ButtonBase(c.Key(key)).Height(buttonH).Padding(0, 13).Gap(7).Radius(9).
		AlignItems(ui.Center).Label(label).Tooltip(label)
	ink := p.fillText
	if off {
		ink = p.faint
		b.Background(p.text.Alpha(0.05)).Disabled(true)
	} else {
		under := p.fill.Mix(black, 0.5)
		b.Background(p.fill).
			InsetShadow(0, 1, 0, 0, ui.RGBA(255, 255, 255, 0.18)).
			Shadow(0, 1, 2.5, 0, under.Alpha(0.28))
		switch {
		case b.Pressed():
			b.Background(p.fillPressed).Shadow(0, 0, 1, 0, under.Alpha(0.2))
		case b.Hovered():
			b.Background(p.fillHover)
		}
	}
	b.Transition(hoverFade)
	b.Children(func() {
		if icon != nil {
			ui.Icon(c, icon).FontSize(13).TextColor(ink)
		}
		ui.Text(c, label).FontSize(12.5).FontWeight(500).TextColor(ink).SingleLine()
	})
	return button(b)
}

// ghostButton is a quiet button: no face until the pointer is on it,
// then a wash.
func (a *app) ghostButton(c *ui.Context, key, label string, icon *ui.SVG) button {
	return a.plainButton(c, key, label, icon, false, nil)
}

// quietButton is the title bar's button: a face of the surface under a
// hairline, for an action that should be found without shouting.
func (a *app) quietButton(c *ui.Context, key, label string, icon *ui.SVG) button {
	return a.plainButton(c, key, label, icon, true, nil)
}

// plainButton draws both. mark, when set, is the color its icon keeps
// whatever the button's state.
func (a *app) plainButton(c *ui.Context, key, label string, icon *ui.SVG, faced bool, mark *ui.Color) button {
	p := a.pal
	b := ui.ButtonBase(c.Key(key)).Height(buttonH).Padding(0, 11).Gap(7).Radius(9).
		AlignItems(ui.Center).Label(label).Tooltip(label)
	col := p.muted
	if faced {
		b.Background(p.surface).Border(1, p.border)
		p.lift(ui.Element(b))
		col = p.text
	}
	switch {
	case b.Pressed():
		b.Background(p.surfacePressed)
		col = p.text
	case b.Hovered():
		if faced {
			b.Background(p.surfaceHover)
		} else {
			b.Background(p.hover)
		}
		col = p.text
	}
	b.Transition(hoverFade)
	b.Children(func() {
		if icon != nil {
			iconCol := col.Alpha(0.8)
			if mark != nil {
				iconCol = *mark
			}
			ui.Icon(c, icon).FontSize(13).TextColor(iconCol)
		}
		if label != "" {
			ui.Text(c, label).FontSize(12).FontWeight(500).TextColor(col).SingleLine()
		}
	})
	return button(b)
}

// pageBody is the page the sidebar chose.
func (a *app) pageBody(c *ui.Context, p *palette) {
	// Without a daemon there is nothing to show but how to get one: every
	// page but the daemon's says so, rather than an empty table that reads
	// as no traffic.
	if st := a.snapshotState(); st.conn != connConnected && a.page != pageDaemon {
		a.offlinePage(c, p, st)
		return
	}
	switch a.page {
	case pageRequests:
		a.requestsPage(c, p)
	case pageRules:
		a.rulesPage(c, p)
	case pageDaemon:
		a.daemonPage(c, p)
	default:
		a.trajectoryPage(c, p)
	}
}

// offlinePage stands where a page would be while the daemon is not there.
func (a *app) offlinePage(c *ui.Context, p *palette, st store) {
	ui.Column(c.Key("offline")).Grow(1).Center().Padding(24).Gap(10).Children(func() {
		emptyMark(c, p, iconServer)
		if st.conn == connConnecting {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Margin(4, 0, 0, 0).Children(func() {
				ui.Spinner(c).Size(13, 13)
				ui.Text(c, "Looking for the daemon…").FontSize(15).FontWeight(600).TextColor(p.text).SingleLine()
			})
			return
		}
		ui.Text(c, "The daemon isn't running").FontSize(15).FontWeight(600).TextColor(p.text).Margin(6, 0, 0, 0).SingleLine()
		ui.Text(c, "Capture, rules and MITM live in chilingd. Start it and this window picks up where it left off.").
			FontSize(13).TextColor(p.muted).TextAlign(ui.Center).LineHeight(1.5).MaxWidth(380)
		a.fillButton(c, "offline-start", "Start chilingd", iconPlay, false).Clicked(func() { a.actStartDaemon() })
		if st.connErr != "" {
			ui.Text(c, st.connErr).FontSize(11).Font(mono()).TextColor(p.faint).TextAlign(ui.Center).MaxWidth(520)
		}
	})
}

// statusBar is the chrome's bottom: how the daemon is, and the counts.
func (a *app) statusBar(c *ui.Context, p *palette) {
	st := a.snapshotState()
	ui.Row(c).Height(statusBarH).Padding(0, 12).Gap(12).AlignItems(ui.Center).
		Background(p.chrome).BorderWidth(1, 0, 0, 0).BorderColor(p.border).Children(func() {
		if st.conn != connConnected {
			dotCol, label := p.faint, "connecting"
			if st.conn == connDown {
				dotCol, label = p.removed, "daemon down"
			}
			if st.connErr != "" {
				label += " — " + st.connErr
			}
			ui.Box(c.Key("conn-dot")).Width(6).Height(6).Radius(3).Background(dotCol)
			ui.Text(c, label).FontSize(12).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(40)
		} else {
			// The one place the window says where the proxy listens.
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
				ui.Box(c.Key("conn-dot")).Width(6).Height(6).Radius(3).Background(p.added).
					Shadow(0, 0, 0, 2, p.added.Alpha(0.16))
				ui.Text(c, "Listening on").FontSize(12).TextColor(p.faint).SingleLine()
				hostText(c, st.status.ProxyAddr, p.faint, p.text.Mix(p.muted, 0.5)).FontSize(11).SingleLine()
			})
		}
		if a.busy != "" {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Transition(busyFade).Children(func() {
				ui.Spinner(c).Size(11, 11)
				ui.Text(c, a.busy).FontSize(12).TextColor(p.muted).SingleLine()
			})
		}
		ui.Spacer(c)
		if st.listHiddenCount > 0 {
			ui.Text(c, fmt.Sprintf("%d hidden", st.listHiddenCount)).FontSize(12).TextColor(p.faint).SingleLine()
		}
		if st.conn == connConnected {
			if v := st.status.Version; v != "" {
				ui.Text(c, "v"+v).FontSize(12).TextColor(p.faint).SingleLine()
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
