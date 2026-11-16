package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/chiling/pkg/api"
)

// The trace page: the session read as a timeline. Each line is one thing
// that happened, entered at its time; a request a rule touched bears that
// rule's mark.

// trajGroupGap is the pause between adjacent steps that opens a new group.
const trajGroupGap = 2 * time.Minute

// trajFetch is how many steps the snapshot asks the daemon for.
const trajFetch = 100

// trajOldest is the order of the trace: 0 newest first, as a live tail
// reads, 1 oldest first, as a story does.
var trajOldest int

// The measures of a line of the timeline.
const (
	traceRowH  = 36
	traceTimeW = 78
	traceRailW = 30
)

// traceView is what the lines of one frame are drawn from.
type traceView struct {
	byID  map[string]uiRequestRow
	maxMs int64
	// hits are the rules that touched each request, by its id, as the
	// names their marks are labelled with.
	hits map[string][]string
}

// trajectoryPage is the trace: under a heading that sums the session up,
// its steps in order of time.
func (a *app) trajectoryPage(c *ui.Context, p *palette) {
	st := a.snapshotState()
	if len(st.traj) == 0 {
		emptyStateWith(c, p, iconClock, "No activity yet",
			"Start a recording and send traffic through the proxy. Requests, the rules that touched them and each verification are entered here in order.",
			"chiling record start", func() {
				a.plainButton(c, "trace-record", "Start recording", iconRecord, true, &p.removed).
					Clicked(func() { a.actStartRecording() })
			})
		return
	}
	fetched := len(st.traj)
	all := foldTrace(st.traj)

	v := traceView{byID: make(map[string]uiRequestRow, len(st.traceReqs)), maxMs: 1, hits: map[string][]string{}}
	for _, r := range st.traceReqs {
		v.byID[string(r.ID)] = r
		v.maxMs = max(v.maxMs, r.DurationMs)
	}
	// A rule that ran on a request is not a line of its own: it is the
	// mark on that request's line. Only a rule hit with no request to sit
	// on keeps a line.
	shown := map[string]bool{}
	for _, it := range all {
		if isCapture(it) {
			shown[it.RequestID] = true
		}
	}
	steps := make([]api.Activity, 0, len(all))
	for _, it := range all {
		if it.Kind == "rule_applied" && shown[it.RequestID] {
			name := ruleNameOf(st.rules, it.RuleID)
			if !contains(v.hits[it.RequestID], name) {
				v.hits[it.RequestID] = append(v.hits[it.RequestID], name)
			}
			continue
		}
		steps = append(steps, it)
	}
	if trajOldest == 1 {
		for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
			steps[i], steps[j] = steps[j], steps[i]
		}
	}

	ui.Column(c).Grow(1).MinHeight(0).Children(func() {
		a.traceHeader(c, p, st, all, v)
		ui.Scroll(c).Grow(1).Children(func() {
			ui.Column(c).FillWidth().Padding(4, 28, 28, 8).Children(func() {
				for i, it := range steps {
					if i > 0 {
						gap := steps[i-1].At.Sub(it.At)
						if trajOldest == 1 {
							gap = -gap
						}
						if gap > trajGroupGap {
							a.traceGap(c, p, it.ID, gap)
						}
					}
					a.traceLine(c, p, it, v, i == 0, i == len(steps)-1)
				}
				if fetched >= trajFetch {
					ui.Text(c, fmt.Sprintf("The latest %d steps.", trajFetch)).FontSize(11).
						TextColor(p.faint).Padding(12, 0, 0, traceTimeW+traceRailW).SingleLine()
				}
			})
		})
	})
}

func isCapture(it api.Activity) bool {
	return it.RequestID != "" && (it.Kind == "request_captured" || it.Kind == "request_completed")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ruleNameOf is what a rule's mark is labelled: its name, else its id.
func ruleNameOf(rules []api.Rule, id string) string {
	if r := ruleOf(rules, id); r != nil {
		return ruleLabel(*r)
	}
	return id
}

// foldTrace drops the step that says a request arrived once the step that
// says it finished is there too. The daemon notes both, and both draw the
// same line, so unfolded every request reads twice and the counts double.
// A request still in flight keeps its arrival step.
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

// traceHeader is the trace's heading: the session by name, what happened
// in it as one line, the session drawn across time, and the order the
// lines read in.
func (a *app) traceHeader(c *ui.Context, p *palette, st store, all []api.Activity, v traceView) {
	requests, hits, verifies, failed := 0, 0, 0, 0
	for _, it := range all {
		switch it.Kind {
		case "request_captured", "request_completed":
			requests++
			if r, ok := v.byID[it.RequestID]; ok && r.Status >= 400 {
				failed++
			}
		case "rule_applied":
			hits++
		case "verify":
			verifies++
		}
	}
	ui.Row(c.Key("trace:head")).FillWidth().Shrink(0).Padding(22, 28, 16).Gap(24).AlignItems(ui.End).Children(func() {
		ui.Column(c).Gap(5).Shrink(1).MinWidth(0).Children(func() {
			title := "Trace"
			if st.session != nil && st.session.Name != "" && st.session.Name != "scope" {
				title = st.session.Name
			}
			ui.Text(c, title).FontSize(20).FontWeight(650).LetterSpacing(-0.2).TextColor(p.text).SingleLine()
			ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
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
				part(requests, "request", "requests", p.muted)
				part(failed, "failed", "failed", p.removed)
				part(hits, "hit a rule", "hit rules", p.accent)
				part(verifies, "verified", "verified", p.muted)
			})
		})
		ui.Spacer(c)
		a.traceStrip(c, p, all, v)
		label, icon := "Newest first", iconDown
		if trajOldest == 1 {
			label, icon = "Oldest first", iconUp
		}
		a.ghostButton(c, "trace-order", label, icon).Clicked(func() { trajOldest = 1 - trajOldest })
	})
	hairline(c, p)
}

// traceStrip is the session at a glance: its steps across time, left to
// right. A request is a stroke, taller the longer it took and in color
// only if it failed; a rule's hit is an accent block over it; a pause in
// the work is the gap it left. Nothing in it is decoration: it is the same
// timeline, read by its rhythm.
func (a *app) traceStrip(c *ui.Context, p *palette, all []api.Activity, v traceView) {
	if len(all) < 2 {
		return
	}
	ui.Box(c.Key("trace:strip")).Width(280).Height(30).Shrink(1).MinWidth(60).Margin(0, 0, 3, 0).
		Draw(func(pt *ui.Painter, r ui.Rect) {
			base := r.Y + r.H - 1
			pt.Fill(ui.Rect{X: r.X, Y: base, W: r.W, H: 1}, p.text.Alpha(0.16), 0)
			// all is newest first.
			t0, t1 := all[len(all)-1].At, all[0].At
			span := t1.Sub(t0)
			for i := len(all) - 1; i >= 0; i-- {
				it := all[i]
				f := float32(len(all)-1-i) / float32(len(all)-1)
				if span > 0 {
					f = float32(it.At.Sub(t0)) / float32(span)
				}
				x := r.X + f*(r.W-3)
				switch {
				case isCapture(it):
					req := v.byID[it.RequestID]
					h := float32(5)
					if req.DurationMs > 0 {
						h += (r.H - 13) * float32(math.Sqrt(float64(req.DurationMs)/float64(v.maxMs)))
					}
					col := p.text.Alpha(0.42)
					if req.Status >= 400 {
						col = statusTone(p, req.Status)
					}
					pt.Fill(ui.Rect{X: x, Y: base - h, W: 2, H: h}, col, 1)
				case it.Kind == "rule_applied":
					pt.Fill(ui.Rect{X: x - 1.5, Y: r.Y, W: 5, H: 5}, p.accent, 1.2)
				case it.Kind == "verify":
					pt.Fill(ui.Rect{X: x - 1.5, Y: r.Y, W: 5, H: 5}, p.added, 2.5)
				}
			}
		})
}

// traceGap is the note between two chapters of the trace: how long the
// pause was, in the margin where the times are.
func (a *app) traceGap(c *ui.Context, p *palette, id string, gap time.Duration) {
	ui.Row(c.Key("trajgap:" + id)).FillWidth().Height(26).AlignItems(ui.Center).Children(func() {
		ui.Text(c, gap.Truncate(time.Second).String()).FontSize(10.5).Font(mono()).TextColor(p.faint).
			Width(traceTimeW).TextAlign(ui.End).SingleLine().Shrink(0)
		ui.Box(c).Width(traceRailW).Shrink(0).FillHeight().Children(func() {
			ui.Box(c).Absolute().Left(traceRailW/2 - 0.5).Top(0).Bottom(0).Width(1).Background(p.text.Alpha(0.14))
		})
		ui.Text(c, "pause").FontSize(11).TextColor(p.faint).SingleLine()
	})
}

// traceLine is one line of the timeline: its time in the margin, its mark
// on the rail, and what it was.
func (a *app) traceLine(c *ui.Context, p *palette, it api.Activity, v traceView, first, last bool) {
	req, hasReq := v.byID[it.RequestID]
	capture := hasReq && isCapture(it)
	ui.Row(c.Key("step:" + it.ID)).FillWidth().Height(traceRowH).AlignItems(ui.Stretch).Children(func() {
		ui.Row(c).Width(traceTimeW).Shrink(0).AlignItems(ui.Center).Justify(ui.End).Children(func() {
			ui.Text(c, it.At.Local().Format("15:04:05")).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine()
		})
		ui.Box(c).Width(traceRailW).Shrink(0).Children(func() {
			top, bottom := float32(0), float32(0)
			if first {
				top = traceRowH / 2
			}
			if last {
				bottom = traceRowH / 2
			}
			mid := float32(traceRailW) / 2
			ui.Box(c).Absolute().Left(mid - 0.5).Top(top).Bottom(bottom).Width(1).Background(p.text.Alpha(0.14))
			a.traceMark(c, p, it, req, capture, mid)
		})
		if it.RequestID == "" {
			ui.Row(c).Grow(1).MinWidth(0).Padding(0, 10, 0, 6).Gap(10).AlignItems(ui.Center).Children(func() {
				a.traceEvent(c, p, it)
			})
			return
		}
		b := ui.ButtonBase(c.Key("traj:"+it.ID)).Grow(1).MinWidth(0).Padding(0, 10, 0, 6).Gap(10).Radius(6).
			AlignItems(ui.Center).Label("Show the request " + it.RequestID).Tooltip("Open in Requests")
		switch {
		case b.Pressed():
			b.Background(p.text.Alpha(0.06))
		case b.Hovered():
			b.Background(p.hover)
		}
		b.Transition(hoverFade)
		b.Children(func() {
			if capture {
				a.traceRequest(c, p, req, v)
			} else {
				a.traceEvent(c, p, it)
			}
		})
		if b.Clicked() {
			a.showRequest(it.RequestID)
		}
	})
}

// traceMark is a line's mark on the rail. A request is a ring in the color
// of how it came back; a rule is its bolt; the rest are small dark marks.
// Each sits on a patch of the ground, so the rail stops short of it.
func (a *app) traceMark(c *ui.Context, p *palette, it api.Activity, req uiRequestRow, capture bool, mid float32) {
	at := func(size float32) ui.Element {
		ui.Box(c).Absolute().Left(mid-size/2-2).Top((traceRowH-size)/2-2).Size(size+4, size+4).Background(p.editor)
		return ui.Row(c).Absolute().Left(mid-size/2).Top((traceRowH-size)/2).Size(size, size).Center()
	}
	switch {
	case capture:
		tone := p.faint
		if req.Status != 0 {
			tone = statusTone(p, req.Status)
		}
		at(9).Radius(4.5).Border(1.75, tone)
	case it.Kind == "rule_applied":
		at(13).Children(func() { ruleMark(c, p, 13, true) })
	case it.Kind == "rule_created":
		at(13).Children(func() { ruleMark(c, p, 13, false) })
	case it.Kind == "verify":
		at(8).Radius(4).Background(p.added)
	case it.Kind == "session_started", it.Kind == "session_stopped":
		at(7).Radius(1.5).Background(p.text)
	default:
		at(5).Radius(2.5).Background(p.faint)
	}
}

// traceRequest is a request's line: the verb, the URL with its host held
// back and its path in the text's color, the marks of the rules that touched it, and
// how long it took and what came back.
func (a *app) traceRequest(c *ui.Context, p *palette, req uiRequestRow, v traceView) {
	methodText(c, p, req.Method).Width(50)
	ui.Row(c).Shrink(1).MinWidth(40).AlignItems(ui.Center).Children(func() {
		ui.Text(c, hostOf(req.URL)).FontSize(12).Font(mono()).TextColor(p.faint).SingleLine().Shrink(2).MinWidth(0)
		ui.Text(c, pathOf(req.URL)).FontSize(12).Font(mono()).FontWeight(500).TextColor(p.text).SingleLine().Shrink(1).MinWidth(30)
	})
	for _, name := range v.hits[string(req.ID)] {
		ui.Row(c).Gap(6).AlignItems(ui.Center).Shrink(0).Margin(0, 0, 0, 4).Children(func() {
			ruleMark(c, p, 14, true)
			ui.Text(c, name).FontSize(12).TextColor(p.muted).SingleLine()
		})
	}
	ui.Spacer(c)
	latencyBar(c, p, req, v.maxMs)
	ui.Text(c, latencyLabel(req)).FontSize(11.5).Font(mono()).TextColor(p.muted).
		Width(50).TextAlign(ui.End).SingleLine().Shrink(0)
	ui.Row(c).Width(32).Justify(ui.End).AlignItems(ui.Center).Shrink(0).Children(func() {
		if req.Status == 0 {
			ui.Spinner(c).Size(11, 11)
		} else {
			statusText(c, p, req.Status)
		}
	})
}

// traceEvent is a line that is not a request: what it was, in the text's color, and the
// daemon's own words for it after.
func (a *app) traceEvent(c *ui.Context, p *palette, it api.Activity) {
	ui.Text(c, activityKind(it.Kind)).FontSize(12.5).FontWeight(600).TextColor(p.text).SingleLine().Shrink(0)
	ui.Text(c, eventSummary(it)).FontSize(12.5).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(20)
	if it.Kind == "session_started" || it.Kind == "session_stopped" {
		// A recording's start and stop rule the timeline off, as a
		// chapter does.
		ui.Box(c).Grow(1).Height(1).Background(p.border)
		return
	}
	ui.Spacer(c)
	if it.RuleID != "" {
		tag(c, p, it.RuleID)
	}
	if it.RequestID != "" {
		ui.Text(c, it.RequestID).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine().Shrink(0)
	}
}

// eventSummary is the daemon's summary of a step without the words the
// line's own name already says.
func eventSummary(it api.Activity) string {
	s := it.Summary
	switch it.Kind {
	case "session_started":
		s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "recording started"), " ("), ")")
	case "session_stopped":
		s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "recording stopped"), " ("), ")")
	}
	return strings.TrimSpace(s)
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
		return "Rule written"
	case "verify":
		return "Verified"
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
