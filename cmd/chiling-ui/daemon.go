package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"
)

// The daemon page: what the daemon is and holds — its status, the
// recording session, history, MITM, and the scope.

// daemonPage is what the daemon is and holds: its status, history, MITM,
// the recording session, and the scope. The cards rise in as the page
// shows, each a little after the one above it.
func (a *app) daemonPage(c *ui.Context, p *palette) {
	st := a.snapshotState()
	ui.Scroll(c).Grow(1).Background(p.chrome).Children(func() {
		// The page is a column of a readable measure, centered in the
		// window rather than left against its edge.
		ui.Column(c).FillWidth().MaxWidth(900).AlignSelf(ui.Center).Padding(20, 24, 32).Gap(12).Children(func() {
			switch {
			case st.conn == connDown:
				a.daemonSection(c, p, "Daemon", iconServer, "Stopped", p.removed, func() {
					ui.Column(c).FillWidth().Gap(8).Children(func() {
						ui.Text(c, st.connErr).FontSize(13).TextColor(p.removed).FillWidth()
						ui.Row(c).Gap(8).Margin(2, 0, 0, 0).Children(func() {
							a.fillButton(c, "daemon-start", "Start chilingd", iconPlay).
								Clicked(func() { a.actStartDaemon() })
						})
						ui.Text(c, "Or run chiling daemon start to choose the proxy address and MITM.").FontSize(13).TextColor(p.muted).FillWidth()
					})
				})
			case st.conn == connConnecting:
				a.daemonSection(c, p, "Daemon", iconServer, "", p.muted, func() {
					ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
						ui.Spinner(c).Size(12, 12)
						ui.Text(c, "looking for chilingd…").FontSize(13).TextColor(p.muted)
					})
				})
			default:
				a.daemonHero(c, p, st)
				recording := st.session != nil && st.session.Recording && st.session.Name != "scope"
				// The four things the daemon holds, two by two.
				pair := func(build func()) {
					ui.Row(c).FillWidth().Gap(12).AlignItems(ui.Stretch).Children(build)
				}
				pair(func() {
					recLabel, recCol := "Idle", p.muted
					if recording {
						recLabel, recCol = "Recording", p.removed
					}
					a.daemonSection(c, p, "Recording", iconRecord, recLabel, recCol, func() {
						if st.session != nil && st.session.Recording {
							a.factRow(c, p, "Session", func() {
								ui.Row(c).Gap(8).AlignItems(ui.Center).Shrink(1).Children(func() {
									factText(c, p, st.session.ID)
									if st.session.Recording && st.session.Name != "scope" {
										recDot(c, p)
									}
								})
							})
							a.factRow(c, p, "Started", func() {
								ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
									ui.Text(c, st.session.StartedAt.Local().Format("15:04:05")).FontSize(12).Font(mono()).
										TextColor(p.text).SingleLine().Shrink(0)
									ui.Text(c, elapsedLabel(st.session.StartedAt, c.Now())).FontSize(12).Font(mono()).
										TextColor(p.faint).SingleLine().ContentTransition(ui.ContentTransition{Numeric: true})
								})
								if run := c.Now().Sub(st.session.StartedAt); run >= 0 {
									// Draw again as the second flips on, so the
									// elapsed keeps counting while nothing else moves.
									c.After(time.Second - run%time.Second + 5*time.Millisecond)
								}
							})
							a.factRoll(c, p, "Captures", fmt.Sprint(len(st.session.RequestIDs)))
							if st.session.Name == "scope" {
								ui.Text(c, "Implicit scope session — no recording window is open.").FontSize(13).TextColor(p.muted)
							}
							ui.Row(c).Gap(8).Margin(4, 0, 0, 0).Children(func() {
								a.quietButton(c, "daemon-stop-record", "Stop recording", iconStop).
									Clicked(func() { a.actStopRecording() })
							})
						} else {
							ui.Text(c, "No recording is open. Start one to keep a named trace of what follows.").
								FontSize(13).LineHeight(1.45).TextColor(p.muted).FillWidth()
							ui.Row(c).Gap(8).Margin(4, 0, 0, 0).Children(func() {
								a.quietButton(c, "daemon-start-record", "Start recording", iconRecord).
									Clicked(func() { a.actStartRecording() })
							})
						}
					})
					histLabel, histCol := "Off", p.muted
					if st.history.Enabled {
						histLabel, histCol = "On", p.added
					}
					a.daemonSection(c, p, "History", iconClock, histLabel, histCol, func() {
						if !st.history.Enabled {
							ui.Text(c, "Exchanges live in the ring buffer only, and are gone when the daemon stops.").
								FontSize(13).LineHeight(1.45).TextColor(p.muted).FillWidth()
							return
						}
						a.fact(c, p, "Store", st.history.Path)
						a.factRoll(c, p, "Requests", fmt.Sprint(st.history.Requests))
						if st.history.Retention != "" {
							a.fact(c, p, "Retention", retentionLabel(st.history.Retention))
						}
					})
				})
				pair(func() {
					mitmLabel, mitmCol := "Off", p.muted
					switch {
					case !st.mitmKnown:
						mitmLabel, mitmCol = "Not set up", p.warning()
					case st.mitm.Enabled:
						mitmLabel, mitmCol = "On", p.added
					}
					a.daemonSection(c, p, "MITM", iconLock, mitmLabel, mitmCol, func() {
						if !st.mitmKnown {
							ui.Text(c, "This daemon was started without MITM, so HTTPS passes through unread. Restart it to read HTTPS:").
								FontSize(13).LineHeight(1.45).TextColor(p.muted).FillWidth()
							codeChip(c, p, "chiling daemon stop && chiling daemon start --mitm")
							return
						}
						a.factYesNo(c, p, "Enabled", st.mitm.Enabled)
						a.factYesNo(c, p, "CA installed", st.mitm.CertInstalled)
						if st.mitm.EmptyAllowDenies && len(st.mitm.AllowPatterns) == 0 {
							ui.Text(c, "The allowlist is empty, so nothing is intercepted.").FontSize(13).TextColor(p.muted)
						}
						a.factList(c, p, "Allow", st.mitm.AllowPatterns)
						a.factList(c, p, "Deny", st.mitm.DenyPatterns)
					})
					if st.hasScope {
						scopeLabel, scopeCol := "Open", p.muted
						if st.scope.Restricting {
							scopeLabel, scopeCol = "Restricting", p.accent
						}
						a.daemonSection(c, p, "Scope", iconFilter, scopeLabel, scopeCol, func() {
							if st.scope.Restricting {
								a.fact(c, p, "Session", st.scope.SessionID)
							} else {
								ui.Text(c, "Every exchange is in scope. Nothing is picked, omitted or bypassed.").
									FontSize(13).LineHeight(1.45).TextColor(p.muted).FillWidth()
							}
							a.factList(c, p, "Pick", st.scope.Pick)
							a.factList(c, p, "Omit", st.scope.Omit)
							a.factList(c, p, "Bypass", st.scope.Bypass)
							a.factList(c, p, "Durable allow", st.scope.ConfigAllow)
							a.factList(c, p, "Durable deny", st.scope.ConfigDeny)
							// Reset has something to undo only while the session
							// narrows the view.
							if !st.scope.Restricting && len(st.scope.Pick)+len(st.scope.Omit)+len(st.scope.Bypass) == 0 {
								return
							}
							ui.Row(c).Gap(8).Margin(4, 0, 0, 0).Children(func() {
								a.quietButton(c, "scope-reset", "Reset scope", iconUndo).
									Clicked(func() { a.actScopeReset() })
							})
						})
					}
				})
			}
		})
	})
}

// daemonHero heads the page with what the daemon is doing now: that it
// runs and where its proxy listens, and beside that its three counts.
func (a *app) daemonHero(c *ui.Context, p *palette, st store) {
	panel(c.Key("daemon:hero"), p).FillWidth().Children(func() {
		ui.Row(c).FillWidth().Padding(18, 22).Gap(20).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Gap(6).Shrink(1).MinWidth(0).Children(func() {
				ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
					ui.Box(c).Size(7, 7).Radius(3.5).Background(p.added)
					ui.Text(c, "Proxy listening on").FontSize(12).FontWeight(500).TextColor(p.muted).SingleLine()
				})
				ui.Text(c, st.status.ProxyAddr).FontSize(20).FontWeight(600).Font(mono()).LetterSpacing(-0.2).
					TextColor(p.text).SingleLine().Selectable()
			})
			ui.Spacer(c)
			metric := func(key, label, value string, rule bool) {
				if rule {
					ui.Box(c).Width(1).Height(34).Shrink(0).Background(p.border)
				}
				ui.Column(c.Key("stat:" + key)).Gap(4).MinWidth(64).Shrink(0).Children(func() {
					ui.Text(c, value).FontSize(18).FontWeight(600).FontFeatures("tnum").TextColor(p.text).SingleLine().
						ContentTransition(ui.ContentTransition{Numeric: true})
					ui.Text(c, label).FontSize(12).TextColor(p.muted).SingleLine()
				})
			}
			metric("req", "Requests", fmt.Sprint(st.status.RequestCount), false)
			metric("rules", "Rules", fmt.Sprint(st.status.RuleCount), true)
			metric("up", "Uptime", uptimeLabel(st.status.UptimeSeconds), true)
		})
	})
}

// daemonSection is one group of the daemon page, a sheet: its mark and
// name at the top with its state said in a badge at the right, then its
// facts.
func (a *app) daemonSection(c *ui.Context, p *palette, title string, icon *ui.SVG, state string, tone ui.Color, rows func()) {
	panel(c.Key("daemon:"+title), p).Grow(1).Basis(0).MinWidth(0).Children(func() {
		ui.Row(c).FillWidth().Height(42).Padding(0, 14, 0, 16).Gap(9).AlignItems(ui.Center).
			BorderWidth(0, 0, 1, 0).BorderColor(p.border).Children(func() {
			ui.Icon(c, icon).FontSize(14).TextColor(p.faint)
			ui.Text(c, title).FontSize(13).FontWeight(600).TextColor(p.text).SingleLine()
			ui.Spacer(c)
			if state != "" {
				badge(c, p, tone, state)
			}
		})
		ui.Column(c).FillWidth().Padding(14, 16, 16).Gap(8).Children(rows)
	})
}

// badge says a state in a word: a dot and the word in the state's color,
// on a wash of it.
func badge(c *ui.Context, p *palette, tone ui.Color, label string) {
	ui.Row(c).Gap(6).AlignItems(ui.Center).Shrink(0).Padding(2, 9, 2, 8).Radius(10).
		Background(tone.Alpha(p.wash())).Children(func() {
		ui.Box(c).Size(6, 6).Radius(3).Background(tone)
		ui.Text(c, label).FontSize(12).FontWeight(500).TextColor(tone).SingleLine()
	})
}

// codeChip is a command to run, set in mono on a quiet wash.
func codeChip(c *ui.Context, p *palette, cmd string) {
	ui.Text(c, cmd).FontSize(12).Font(mono()).TextColor(p.text).Padding(7, 10).Radius(6).
		Background(p.text.Alpha(0.05)).FillWidth().Selectable()
}

// fact is one label and its value on the daemon page.
func (a *app) fact(c *ui.Context, p *palette, label, value string) {
	a.factRow(c, p, label, func() { factText(c, p, value) })
}

// factRoll is a fact whose value changes as the daemon runs: the text
// rolls as it changes, so the jump each sync takes reads as counting.
func (a *app) factRoll(c *ui.Context, p *palette, label, value string) {
	a.factRow(c, p, label, func() {
		factText(c, p, value).ContentTransition(ui.ContentTransition{Numeric: true})
	})
}

// factYesNo is a fact whose value is whether something is on: it reads as
// a small pill, green for yes, faint for no.
func (a *app) factYesNo(c *ui.Context, p *palette, label string, yes bool) {
	a.factRow(c, p, label, func() {
		if yes {
			stateDot(c, p, p.added, "Yes")
		} else {
			stateDot(c, p, p.faint.Alpha(0.6), "No")
		}
	})
}

// factList is one label and its items on the daemon page: each item its
// own faint badge, and, past three, one badge counting the rest.
func (a *app) factList(c *ui.Context, p *palette, label string, items []string) {
	if len(items) == 0 {
		return
	}
	a.factRow(c, p, label, func() {
		ui.Row(c).Gap(4).Wrap().AlignItems(ui.Center).Shrink(1).Children(func() {
			shown := items
			if len(items) > 3 {
				shown = items[:3]
			}
			for _, it := range shown {
				tag(c, p, it)
			}
			if more := len(items) - len(shown); more > 0 {
				tag(c, p, fmt.Sprint("+", more))
			}
		})
	})
}

// factRow is one label and its value on the daemon page: the label in its
// column, the value after it. The row washes under the pointer, as one
// that could be pressed, though it reads only. The wash keeps the row's
// content where it was: the padding that rooms it is pulled back out by
// the margin.
func (a *app) factRow(c *ui.Context, p *palette, label string, value func()) {
	row := ui.Row(c.Key("fact:"+label)).FillWidth().Gap(10).AlignItems(ui.Center).
		Padding(3, 6).Margin(-3, -6).Radius(6)
	if row.Hovered() {
		row.Background(p.hover)
	}
	row.Transition(hoverFade)
	row.Children(func() {
		ui.Text(c, label).FontSize(13).TextColor(p.muted).SingleLine().Width(96).Shrink(0)
		value()
	})
}

// factText is the value of a fact row: mono, wrapping when it is long, as
// a path is.
func factText(c *ui.Context, p *palette, value string) ui.Element {
	if value == "" {
		value = "—"
	}
	return ui.Text(c, value).FontSize(12).Font(mono()).TextColor(p.text).Shrink(1).MinWidth(30)
}

// recDot is the small red mark beside a session that is recording: it
// breathes between full and faint, and, when the desktop asks for less
// motion, sits still at full.
func recDot(c *ui.Context, p *palette) {
	dot := ui.Box(c.Key("rec-dot")).Size(7, 7).Radius(4).Background(p.removed)
	k := dot.Keyframes("breathe", []ui.Keyframe{
		{Value: 1},
		{At: 1100 * time.Millisecond, Value: 0.35, Ease: ui.EaseInOut},
	}, ui.KeyframeOptions{Repeat: ui.RepeatForever, Alternate: true})
	dot.Opacity(k)
}

// elapsedLabel is how long a session has run, "6m32s" style, counted from
// from as the frame's now says.
func elapsedLabel(from, now time.Time) string {
	if now.Before(from) {
		return "0s"
	}
	return now.Sub(from).Truncate(time.Second).String()
}

// retentionLabel reads a Go duration as days when it is whole days:
// "168h0m0s" is "7 days". Anything else stays as the daemon wrote it.
func retentionLabel(s string) string {
	d, err := time.ParseDuration(s)
	if err != nil || d < 24*time.Hour || d%(24*time.Hour) != 0 {
		return s
	}
	if n := int(d / (24 * time.Hour)); n > 1 {
		return fmt.Sprintf("%d days", n)
	}
	return "1 day"
}

func uptimeLabel(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	return d.Truncate(time.Second).String()
}
