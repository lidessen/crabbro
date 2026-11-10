package main

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/chiling/pkg/api"
)

// The trajectory page: the tail of the timeline, newest first.

// trajStagger is how many leading rows join the page's staggered entrance;
// deeper rows come in at once rather than wait their turn.
const trajStagger = 12

// trajGroupGap is the pause between adjacent steps that opens a new group.
const trajGroupGap = 2 * time.Minute

// trajOldest is the order of the trace: 0 newest first, as a live tail
// reads, 1 oldest first, as a story does.
var trajOldest int

// trajectoryPage is the trace: the session's steps as a timeline, under a
// summary of what the session has done. A capture step shows its exchange
// where it happened — method, status, path, latency — so the story reads
// without leaving the page; a click opens it in Requests.
func (a *app) trajectoryPage(c *ui.Context, p *palette) {
	st := a.snapshotState()
	if len(st.traj) == 0 {
		emptyStateWith(c, p, iconClock, "No trace yet",
			"Start a recording and send traffic through the proxy. Captures, rule hits and verifications land here as a timeline.",
			"chiling record start", func() {
				a.fillButton(c, "trace-record", "Start recording", iconRecord).Clicked(func() { a.actStartRecording() })
			})
		return
	}
	byID := make(map[string]uiRequestRow, len(st.traceReqs))
	maxMs := int64(1)
	for _, r := range st.traceReqs {
		byID[string(r.ID)] = r
		maxMs = max(maxMs, r.DurationMs)
	}
	steps := make([]api.Activity, len(st.traj))
	copy(steps, st.traj)
	if trajOldest == 1 {
		for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
			steps[i], steps[j] = steps[j], steps[i]
		}
	}

	ui.Column(c).Grow(1).MinHeight(0).Children(func() {
		a.traceHeader(c, p, st, byID)
		ui.Scroll(c).Grow(1).Children(func() {
			ui.Column(c).FillWidth().Padding(6, 18, 24).Children(func() {
				for i, it := range steps {
					// A pause between steps is a chapter of its own: a
					// labelled hairline marks how long it was.
					if i > 0 {
						gap := steps[i-1].At.Sub(it.At)
						if trajOldest == 1 {
							gap = -gap
						}
						if gap > trajGroupGap {
							a.traceGap(c, p, it.ID, gap)
						}
					}
					a.traceStep(c, p, it, byID, maxMs, i == 0, i == len(steps)-1)
				}
			})
		})
	})
}

// traceHeader is the trace's top: the session it follows, the counts of
// what happened in it, and the order the steps read in.
func (a *app) traceHeader(c *ui.Context, p *palette, st store, byID map[string]uiRequestRow) {
	captures, rules, verifies, failed := 0, 0, 0, 0
	for _, it := range st.traj {
		switch it.Kind {
		case "request_captured", "request_completed":
			captures++
			if r, ok := byID[it.RequestID]; ok && r.Status >= 400 {
				failed++
			}
		case "rule_applied", "rule_created":
			rules++
		case "verify":
			verifies++
		}
	}
	ui.Row(c.Key("trace:head")).FillWidth().Shrink(0).Padding(14, 18, 10).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Gap(3).Shrink(1).MinWidth(0).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				title := "Trace"
				if st.session != nil && st.session.Name != "" {
					title = st.session.Name
				}
				ui.Text(c, title).FontSize(16).FontWeight(700).TextColor(p.text).SingleLine()
				if st.session != nil && st.session.Recording {
					recDot(c, p)
					ui.Text(c, "recording").FontSize(11.5).FontWeight(600).TextColor(p.removed).SingleLine()
				}
			})
			sub := fmt.Sprintf("%d steps", len(st.traj))
			if st.session != nil && st.session.ID != "" {
				sub = st.session.ID + " · " + sub
			}
			ui.Text(c, sub).FontSize(11.5).Font(mono()).TextColor(p.faint).SingleLine()
		})
		ui.Spacer(c)
		chip := func(label string, n int, col ui.Color) {
			if n == 0 {
				return
			}
			ui.Row(c).Gap(6).AlignItems(ui.Center).Padding(4, 10).Radius(9).Background(col.Alpha(0.11)).Children(func() {
				ui.Text(c, fmt.Sprint(n)).FontSize(13).FontWeight(700).Font(mono()).TextColor(col).SingleLine().
					ContentTransition(ui.ContentTransition{Numeric: true})
				ui.Text(c, label).FontSize(11.5).TextColor(col).SingleLine()
			})
		}
		chip("captures", captures, p.teal())
		chip("failed", failed, p.removed)
		chip("rules", rules, p.accent)
		chip("verified", verifies, p.added)
		ui.Segmented(c, &trajOldest, "Newest", "Oldest")
	})
}

// traceGap is the labelled hairline between two chapters of the trace.
func (a *app) traceGap(c *ui.Context, p *palette, id string, gap time.Duration) {
	ui.Row(c.Key("trajgap:"+id)).FillWidth().Gap(10).AlignItems(ui.Center).Padding(8, 0, 8, 34).Children(func() {
		ui.Box(c).Grow(1).Height(1).Background(p.border)
		ui.Text(c, "paused "+gap.Truncate(time.Second).String()).FontSize(10.5).TextColor(p.faint).SingleLine()
		ui.Box(c).Grow(1).Height(1).Background(p.border)
	})
}

// traceStep is one step: a dot on the rail, and a card of what it was.
func (a *app) traceStep(c *ui.Context, p *palette, it api.Activity, byID map[string]uiRequestRow, maxMs int64, first, last bool) {
	kindCol, kindBg := kindColor(p, it.Kind)
	ui.Row(c.Key("step:" + it.ID)).FillWidth().Gap(10).AlignItems(ui.Stretch).Children(func() {
		// The rail: a hairline through the steps, the dot on it.
		ui.Box(c).Width(24).Shrink(0).Children(func() {
			top, bottom := float32(0), float32(0)
			if first {
				top = 20
			}
			ui.Box(c).Absolute().Left(11).Top(top).Bottom(bottom).Width(2).Background(p.border)
			if last {
				ui.Box(c).Absolute().Left(11).Top(20).Bottom(0).Width(2).Background(p.editor)
			}
			ui.Box(c).Absolute().Left(6).Top(15).Width(12).Height(12).Radius(6).
				Background(p.editor).Border(2, kindCol)
		})
		var card ui.Element
		req, hasReq := byID[it.RequestID]
		if it.RequestID != "" {
			b := ui.ButtonBase(c.Key("traj:" + it.ID)).Label("Show the request " + it.RequestID).Tooltip("Show in Requests")
			card = ui.Element(b)
			if b.Hovered() {
				card.Background(p.surfaceHover)
			} else {
				card.Background(p.surface)
			}
			card.Transition(hoverFade)
		} else {
			card = ui.Column(c)
			card.Background(p.surface)
		}
		card.Grow(1).MinWidth(0).Margin(5, 0).Padding(9, 12).Gap(6).Radius(10).Border(1, p.border)
		card.Children(func() {
			ui.Column(c).Grow(1).MinWidth(0).Gap(6).Children(func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					a.pill(c, activityKind(it.Kind), kindCol, kindBg, false)
					ui.Text(c, trajTime(it.At)).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine().
						ContentTransition(ui.ContentTransition{Numeric: true})
					ui.Spacer(c)
					if it.RuleID != "" {
						ui.Badge(c, it.RuleID)
					}
					if it.RequestID != "" {
						ui.Text(c, it.RequestID).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine()
					}
				})
				if hasReq && (it.Kind == "request_captured" || it.Kind == "request_completed") {
					ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
						mcol, mbg := methodColor(p, req.Method)
						a.pill(c, methodLabel(req.Method), mcol, mbg, false)
						ui.Text(c, hostOf(req.URL)).FontSize(12.5).FontWeight(600).TextColor(p.text).SingleLine().Shrink(0)
						ui.Text(c, pathOf(req.URL)).FontSize(12).Font(mono()).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(20)
						ui.Spacer(c)
						ui.Text(c, durationLabel(req.DurationMs)).FontSize(11.5).Font(mono()).TextColor(p.muted).SingleLine().Shrink(0)
						if req.Status == 0 {
							ui.Spinner(c).Size(11, 11)
						} else {
							scol, sbg := statusColor(p, req.Status)
							a.pill(c, fmt.Sprint(req.Status), scol, sbg, false)
						}
					})
					latencyBar(c, p, req, maxMs)
					return
				}
				ui.Text(c, it.Summary).FontSize(12.5).TextColor(p.text).FillWidth()
			})
		})
	})
}

// latencyBar is how long an exchange took, as a bar against the slowest
// of the list, in the status' color.
func latencyBar(c *ui.Context, p *palette, r uiRequestRow, maxMs int64) {
	col, _ := statusColor(p, r.Status)
	ui.Box(c).FillWidth().Height(4).Radius(2).Background(p.faint.Alpha(0.12)).Children(func() {
		if r.DurationMs > 0 {
			frac := float32(r.DurationMs) / float32(maxMs)
			ui.Box(c).Absolute().Left(0).Top(0).Bottom(0).WidthPercent(max(frac*100, 3)).Radius(2).Background(col.Alpha(0.8))
		}
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
	case "request_captured", "request_completed":
		return p.teal(), p.teal().Alpha(0.12)
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
