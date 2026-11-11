package main

import (
	"fmt"
	"strings"
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

// trajFetch is how many steps the snapshot asks the daemon for.
const trajFetch = 100

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
	fetched := len(st.traj)
	st.traj = foldTrace(st.traj)
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
			ui.Column(c).FillWidth().Padding(2, 14, 24, 12).Children(func() {
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
				if fetched >= trajFetch {
					ui.Text(c, fmt.Sprintf("Showing the latest %d steps.", trajFetch)).FontSize(11).
						TextColor(p.faint).Padding(10, 0, 0, 30).SingleLine()
				}
			})
		})
	})
}

// foldTrace drops the step that says a request arrived once the step that
// says it finished is there too. The daemon notes both, and both draw the
// same exchange card, so unfolded every request reads twice and the counts
// double. A request still in flight keeps its arrival step.
func foldTrace(in []api.Activity) []api.Activity {
	done := make(map[string]bool)
	for _, it := range in {
		if it.Kind == "request_completed" && it.RequestID != "" {
			done[it.RequestID] = true
		}
	}
	if len(done) == 0 {
		return in
	}
	out := make([]api.Activity, 0, len(in))
	for _, it := range in {
		if it.Kind == "request_captured" && done[it.RequestID] {
			continue
		}
		out = append(out, it)
	}
	return out
}

// traceHeader is the trace's top: the session it follows, what happened in
// it as one quiet line, and the order the steps read in. Only what failed
// takes a color.
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
	ui.Row(c.Key("trace:head")).FillWidth().Shrink(0).Padding(16, 20, 12).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Column(c).Gap(4).Shrink(1).MinWidth(0).Children(func() {
			title := "Trace"
			if st.session != nil && st.session.Name != "" && st.session.Name != "scope" {
				title = st.session.Name
			}
			ui.Text(c, title).FontSize(15).FontWeight(650).TextColor(p.text).SingleLine()
			ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
				first := true
				part := func(n int, one, many string, col ui.Color) {
					if n == 0 {
						return
					}
					if !first {
						ui.Text(c, "·").FontSize(12).TextColor(p.faint).SingleLine()
					}
					first = false
					word := many
					if n == 1 {
						word = one
					}
					ui.Text(c, fmt.Sprintf("%d %s", n, word)).FontSize(12).TextColor(col).SingleLine().
						ContentTransition(ui.ContentTransition{Numeric: true})
				}
				part(len(st.traj), "step", "steps", p.muted)
				part(captures, "request", "requests", p.muted)
				part(failed, "failed", "failed", p.removed)
				part(rules, "rule hit", "rule hits", p.muted)
				part(verifies, "verified", "verified", p.muted)
			})
		})
		ui.Spacer(c)
		ui.Segmented(c, &trajOldest, "Newest", "Oldest")
	})
}

// traceGap is the labelled hairline between two chapters of the trace.
func (a *app) traceGap(c *ui.Context, p *palette, id string, gap time.Duration) {
	ui.Row(c.Key("trajgap:"+id)).FillWidth().Gap(10).AlignItems(ui.Center).Padding(8, 10, 8, 30).Children(func() {
		ui.Text(c, gap.Truncate(time.Second).String()+" later").FontSize(11).TextColor(p.faint).SingleLine()
		ui.Box(c).Grow(1).Height(1).Background(p.border)
	})
}

// traceRowH is the height of one step of the trace.
const traceRowH = 34

// traceStep is one step: a dot on the rail and, beside it, one line of
// what it was. A capture reads as its exchange — verb, host, path, how
// long, what came back — and a click opens it in Requests.
func (a *app) traceStep(c *ui.Context, p *palette, it api.Activity, byID map[string]uiRequestRow, maxMs int64, first, last bool) {
	req, hasReq := byID[it.RequestID]
	capture := hasReq && (it.Kind == "request_captured" || it.Kind == "request_completed")
	ui.Row(c.Key("step:" + it.ID)).FillWidth().Height(traceRowH).AlignItems(ui.Stretch).Children(func() {
		// The rail: a hairline through the steps, the dot on it, ringed in
		// the page's ground so the line stops short of it.
		ui.Box(c).Width(18).Shrink(0).Children(func() {
			top, bottom := float32(0), float32(0)
			if first {
				top = traceRowH / 2
			}
			if last {
				bottom = traceRowH / 2
			}
			ui.Box(c).Absolute().Left(8.5).Top(top).Bottom(bottom).Width(1).Background(p.text.Alpha(0.14))
			// A step that is an event of its own — a rule, a check, the
			// recording — is a full dot; a request is a ring, so the eye
			// tells the story's beats from the traffic between them.
			tone := stepTone(p, it.Kind, req, capture)
			ui.Box(c).Absolute().Left(2).Top((traceRowH-14)/2).Size(14, 14).Radius(7).Background(p.editor)
			dot := ui.Box(c).Absolute().Left(4).Top((traceRowH-10)/2).Size(10, 10).Radius(5)
			if capture {
				dot.Background(p.editor).Border(2, tone)
			} else {
				dot.Background(tone)
			}
		})
		line := func() {
			ui.Text(c, trajTime(it.At)).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine().
				Width(58).Shrink(0).ContentTransition(ui.ContentTransition{Numeric: true})
			if capture {
				methodText(c, p, req.Method).Width(46)
				ui.Text(c, hostOf(req.URL)).FontSize(12.5).FontWeight(550).TextColor(p.text).SingleLine().Shrink(0)
				ui.Text(c, pathOf(req.URL)).FontSize(12).Font(mono()).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(20)
				ui.Spacer(c)
				latencyBar(c, p, req, maxMs)
				ui.Text(c, latencyLabel(req)).FontSize(11.5).Font(mono()).TextColor(p.muted).
					Width(48).TextAlign(ui.End).SingleLine().Shrink(0)
				ui.Row(c).Width(30).Justify(ui.End).AlignItems(ui.Center).Shrink(0).Children(func() {
					if req.Status == 0 {
						ui.Spinner(c).Size(11, 11)
					} else {
						statusText(c, p, req.Status)
					}
				})
				return
			}
			ui.Text(c, activityKind(it.Kind)).FontSize(12.5).FontWeight(600).TextColor(p.text).SingleLine().Shrink(0)
			ui.Text(c, it.Summary).FontSize(12.5).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(20)
			ui.Spacer(c)
			if it.RuleID != "" {
				tag(c, p, it.RuleID)
			}
			if it.RequestID != "" {
				ui.Text(c, it.RequestID).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine().Shrink(0)
			}
		}
		if it.RequestID == "" {
			ui.Row(c).Grow(1).MinWidth(0).Padding(0, 10, 0, 8).Gap(10).AlignItems(ui.Center).Children(line)
			return
		}
		b := ui.ButtonBase(c.Key("traj:"+it.ID)).Grow(1).MinWidth(0).Padding(0, 10, 0, 8).Gap(10).Radius(6).
			AlignItems(ui.Center).Label("Show the request " + it.RequestID).Tooltip("Show in Requests")
		switch {
		case b.Pressed():
			b.Background(p.text.Alpha(0.065))
		case b.Hovered():
			b.Background(p.hover)
		}
		b.Transition(hoverFade)
		b.Children(line)
		if b.Clicked() {
			a.showRequest(it.RequestID)
		}
	})
}

// stepTone is the color of a step's mark: the accent for a rule at work,
// green for a check that passed, and for a request the color of how it
// came back — the same four the status figures use.
func stepTone(p *palette, kind string, req uiRequestRow, capture bool) ui.Color {
	switch kind {
	case "rule_applied", "rule_created":
		return p.accent
	case "verify":
		return p.added
	case "session_started", "session_stopped":
		return p.text
	}
	if capture && req.Status != 0 {
		return statusTone(p, req.Status)
	}
	return p.faint
}

// latencyBar is how long an exchange took, as a short bar against the
// slowest of the list.
func latencyBar(c *ui.Context, p *palette, r uiRequestRow, maxMs int64) {
	const barW = 56
	ui.Box(c).Width(barW).Height(3).Radius(1.5).Shrink(0).Background(p.text.Alpha(0.07)).Children(func() {
		if r.DurationMs > 0 {
			w := max(float32(r.DurationMs)/float32(maxMs)*barW, 3)
			ui.Box(c).Absolute().Left(0).Top(0).Bottom(0).Width(w).Radius(1.5).Background(barTone(p, r.Status))
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

// activityKind is the short name a trajectory step reads by.
func activityKind(kind string) string {
	switch kind {
	case "session_started":
		return "Recording started"
	case "session_stopped":
		return "Recording stopped"
	case "request_captured", "request_completed":
		return "Request"
	case "rule_applied":
		return "Rule"
	case "rule_created":
		return "Rule created"
	case "verify":
		return "Verify"
	case "find":
		return "Find"
	}
	if rest, ok := strings.CutPrefix(kind, "scope_"); ok {
		return "Scope " + rest
	}
	return kind
}

// showRequest jumps to the requests page with a request chosen.
func (a *app) showRequest(id string) {
	a.page = pageRequests
	a.pendingRequestID = string(id)
	a.invalidate()
}
