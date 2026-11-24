package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"
)

const (
	pageRequests   = "requests"
	pageRules      = "rules"
	pageGateway    = "gateway"
	pageTrajectory = "trajectory"
	pageDaemon     = "daemon"
	workspaceInset = 24
)

var workspaces = [...]struct {
	key, title string
	icon       *ui.SVG
}{
	{pageTrajectory, "Trace", iconClock},
	{pageRequests, "Requests", iconList},
	{pageRules, "Rules", iconRule},
	{pageGateway, "Gateway", iconRoute},
	{pageDaemon, "Daemon", iconServer},
}

// mono is the family code, paths and ids draw in.
func mono() string { return fontMono }

var busyFade = ui.ElementTransition{Duration: 130 * time.Millisecond, Enter: &ui.Motion{}, Exit: &ui.Motion{}}

func (a *app) view(c *ui.Context) {
	a.setServices(c.Services())
	a.drainQueue()
	a.mirrorView()
	p := applyPal(c)
	a.pal = p
	st := a.snapshotState()

	ui.Column(c).Fill().Background(p.editor).Children(func() {
		a.titleBar(c, p, st)
		ui.Row(c).Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			a.sidebar(c, p)
			ui.Column(c.Key("page:" + a.page)).Grow(1).MinWidth(0).MinHeight(0).Clip().Children(func() {
				a.pageBody(c, p, st)
			})
		})
		a.statusBar(c, p, st)
	})
	a.toasts(c)
}

func (a *app) titleBar(c *ui.Context, p *palette, st store) {
	bar := c.TitleBar()
	ui.Row(c).Height(max(bar.Height, titleBarH)).Shrink(0).Padding(0, bar.Right+16, 0, bar.Left+16).
		AlignItems(ui.Center).Background(p.chrome).BorderWidth(0, 0, 1, 0).BorderColor(p.border).
		DragWindow().Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(8).Children(func() {
			brandMark(c, p, 23)
			ui.Text(c, "Crabbro").FontSize(14).FontWeight(600).TextColor(p.text).SingleLine()
		})
		ui.Spacer(c)
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() { a.recordControls(c, p, st) })
	})
}

// Keep the data panes wide enough to read at the minimum window size.
func sidebarWidth(c *ui.Context) float32 {
	if width, _ := c.Size(); width < 1060 {
		return 58
	}
	return 150
}

func (a *app) sidebar(c *ui.Context, p *palette) {
	width := sidebarWidth(c)
	compact := width < 100
	ui.Column(c.Key("workspaces")).Width(width).Shrink(0).Padding(12, 9).Gap(3).
		Background(p.chrome).BorderWidth(0, 1, 0, 0).BorderColor(p.border).Label("Workspaces").Children(func() {
		for _, workspace := range workspaces {
			on := a.page == workspace.key
			b := ui.ButtonBase(c.Key("nav:" + workspace.key)).FillWidth().Height(36).Shrink(0).
				Gap(9).Radius(7).AlignItems(ui.Center).Label(workspace.title).Tooltip(workspace.title).Selected(on)
			if compact {
				b.Justify(ui.Center)
			} else {
				b.Padding(0, 12).Justify(ui.Start)
			}
			ink, iconInk := p.muted, p.faint
			switch {
			case on:
				b.Background(p.surface).Border(1, p.border)
				ink, iconInk = p.text, p.accent
			case b.Pressed(), b.Hovered():
				b.Background(p.hover)
				ink, iconInk = p.text, p.text
			}
			b.Transition(hoverFade).Children(func() {
				if on && !compact {
					ui.Box(c).Absolute().Left(0).Top(10).Width(2).Height(16).Radius(1).Background(p.accent)
				}
				ui.Icon(c, workspace.icon).FontSize(16).TextColor(iconInk).Shrink(0)
				if !compact {
					ui.Text(c, workspace.title).FontSize(13).FontWeight(500).TextColor(ink).SingleLine()
				}
			})
			if b.Clicked() {
				a.setPage(workspace.key)
			}
		}
	})
}

// recordControls is the recording at the title bar's right: while one is
// open, its mark and how long it has run, and Stop; otherwise Record.
func (a *app) recordControls(c *ui.Context, p *palette, st store) {
	// Read the session from the one snapshot: asking the store twice lets it
	// change in between, and a session gone by the second look is a nil.
	if s := st.session; s != nil && s.Recording && s.Name != "scope" {
		ui.Row(c.Key("rec-pill")).Gap(7).AlignItems(ui.Center).Height(26).Padding(0, 6).Children(func() {
			// The dot breathes while the daemon records: 0.7s down and
			// back, a 1.4s round, as the daemon page's does.
			dot := ui.Box(c.Key("rec-pill-dot")).Width(7).Height(7).Radius(4).Background(p.removed).
				Shadow(0, 0, 0, 2.5, p.removed.Alpha(0.18))
			dot.Opacity(dot.Keyframes("breathe", []ui.Keyframe{
				{Value: 1},
				{At: 700 * time.Millisecond, Value: 0.55, Ease: ui.EaseInOut},
			}, ui.KeyframeOptions{Repeat: ui.RepeatForever, Alternate: true}))
			ui.Text(c, "Recording").FontSize(12).FontWeight(500).TextColor(p.text).SingleLine()
			if run := c.Now().Sub(s.StartedAt); run >= 0 {
				ui.Text(c, clockLabel(run)).FontSize(11).Font(mono()).TextColor(p.muted).SingleLine()
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
func (a *app) pageBody(c *ui.Context, p *palette, st store) {
	// Without a daemon there is nothing to show but how to get one: every
	// page but the daemon's says so, rather than an empty table that reads
	// as no traffic.
	if st.conn != connConnected && a.page != pageDaemon {
		a.offlinePage(c, p, st)
		return
	}
	switch a.page {
	case pageRequests:
		a.requestsPage(c, p, st)
	case pageRules:
		a.rulesPage(c, p, st)
	case pageGateway:
		a.gatewayPage(c, p, st)
	case pageDaemon:
		a.daemonPage(c, p, st)
	default:
		a.trajectoryPage(c, p, st)
	}
}

// offlinePage stands where a page would be while the daemon is not there.
func (a *app) offlinePage(c *ui.Context, p *palette, st store) {
	ui.Column(c.Key("offline")).Grow(1).MinHeight(0).Padding(18, workspaceInset, 16).Gap(12).Children(func() {
		for _, workspace := range workspaces {
			if workspace.key == a.page {
				workspaceTitle(c, p, workspace.title).Shrink(0)
				break
			}
		}
		panel(c, p).Grow(1).Center().Padding(24).Gap(10).Children(func() {
			emptyMark(c, p, iconServer)
			if st.conn == connConnecting {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Margin(4, 0, 0, 0).Children(func() {
					ui.Spinner(c).Size(13, 13)
					ui.Text(c, "Connecting to crabbrod…").FontSize(15).FontWeight(600).TextColor(p.text).SingleLine()
				})
				return
			}
			ui.Text(c, "Daemon offline").FontSize(15).FontWeight(600).TextColor(p.text).Margin(6, 0, 0, 0).SingleLine()
			ui.Text(c, "Start crabbrod to connect this window to the proxy and gateway.").
				FontSize(13).TextColor(p.muted).TextAlign(ui.Center).LineHeight(1.5).MaxWidth(380)
			a.fillButton(c, "offline-start", "Start crabbrod", iconPlay, false).Clicked(func() { a.actStartDaemon() })
			if st.connErr != "" {
				ui.Text(c, st.connErr).FontSize(11).Font(mono()).TextColor(p.faint).TextAlign(ui.Center).MaxWidth(520).Selectable()
			}
		})
	})
}

// statusBar is the chrome's bottom: how the daemon is, and the counts.
func (a *app) statusBar(c *ui.Context, p *palette, st store) {
	ui.Row(c).Height(statusBarH).Shrink(0).Padding(0, 16).Gap(12).AlignItems(ui.Center).
		Background(p.chrome).BorderWidth(1, 0, 0, 0).BorderColor(p.border).Children(func() {
		if st.conn != connConnected {
			dotCol, label := p.warning(), "Connecting"
			if st.conn == connDown {
				dotCol, label = p.removed, "Daemon offline"
			}
			ui.Box(c.Key("conn-dot")).Width(6).Height(6).Radius(3).Background(dotCol)
			ui.Text(c, label).FontSize(12).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(40)
		} else if a.page == pageGateway {
			gwConnectionState(c, p, st)
		} else {
			listenerStatus(c, p, "Proxy", st.status.ProxyAddr, "", p.added, false)
		}
		if a.busy != "" {
			ui.Row(c).Gap(5).AlignItems(ui.Center).Transition(busyFade).Children(func() {
				ui.Spinner(c).Size(11, 11)
				ui.Text(c, a.busy).FontSize(12).TextColor(p.muted).SingleLine()
			})
		}
		ui.Spacer(c)
		if a.page == pageRequests && st.listHiddenCount > 0 {
			ui.Text(c, fmt.Sprintf("%d hidden", st.listHiddenCount)).FontSize(12).TextColor(p.faint).SingleLine()
		}
		if st.conn == connConnected {
			if v := st.status.Version; v != "" {
				ui.Text(c, "v"+v).Font(mono()).FontSize(11).TextColor(p.faint).SingleLine()
			}
		}
	})
}

func gwConnectionState(c *ui.Context, p *palette, st store) {
	switch {
	case st.gwStatusFetch == gwUnknown:
		listenerStatus(c, p, "Gateway", "", "Unknown", p.faint, false)
	case !st.gwStatus.Enabled:
		listenerStatus(c, p, "Gateway", "", "Disabled", p.faint, st.gwStatusFetch == gwStale)
	default:
		listenerStatus(c, p, "Gateway", st.gwStatus.ListenAddr, "Enabled", p.added, st.gwStatusFetch == gwStale)
	}
}

func listenerStatus(c *ui.Context, p *palette, service, address, state string, tone ui.Color, stale bool) {
	if stale {
		tone = p.warning()
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Box(c.Key("conn-dot")).Size(6, 6).Radius(3).Shrink(0).Background(tone)
		ui.Text(c, service).FontSize(11.5).FontWeight(500).TextColor(p.muted).SingleLine().Shrink(0)
		if address != "" {
			hostText(c, address, p.faint, p.text.Mix(p.muted, 0.5)).FontSize(11).SingleLine().MinWidth(0)
		} else if state != "" {
			ui.Text(c, state).FontSize(11).TextColor(p.faint).SingleLine()
		}
		if stale {
			ui.Text(c, "Stale").FontSize(11).TextColor(tone).SingleLine().Shrink(0)
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
