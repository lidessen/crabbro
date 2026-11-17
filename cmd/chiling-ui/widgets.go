package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/chiling/pkg/api"
)

// Shared widgets and small formatters the pages draw with.

// emptyMark is the mark an empty page is known by: its icon in the accent,
// on a soft square of the same.
func emptyMark(c *ui.Context, p *palette, icon *ui.SVG) {
	ui.Row(c).Size(44, 44).Center().Radius(12).Background(p.accent.Alpha(0.09)).
		InsetShadow(0, 0, 0, 1, p.accent.Alpha(0.16)).Children(func() {
		ui.Icon(c, icon).FontSize(20).TextColor(p.accent)
	})
}

// ruleMark is the mark of a rule: a small bolt in the accent. Filled, it
// is a rule that is enabled — on a request, one that applied to it; in
// outline and faint, a rule that is written but did not apply.
func ruleMark(c *ui.Context, p *palette, size float32, on bool) {
	icon, col := iconRule, p.faint
	if on {
		icon, col = iconRuleOn, p.accent
	}
	ui.Row(c).Size(size, size).Center().Shrink(0).Children(func() {
		ui.Icon(c, icon).FontSize(size).TextColor(col)
	})
}

// caption heads a group of facts: small, quiet, and set a little apart
// from what it names. Groups are told apart by space and a hairline, not
// by boxes.
func caption(c *ui.Context, p *palette, title string) ui.Element {
	return ui.Text(c, title).FontSize(12).FontWeight(500).TextColor(p.faint).SingleLine()
}

// hairline is the rule between two groups.
func hairline(c *ui.Context, p *palette) ui.Element {
	return ui.Box(c).FillWidth().Height(1).Shrink(0).Background(p.border)
}

// words is a row of choices set as words, the chosen one in the text's color over a
// short accent stroke that slides in under it. It stands where tabs and
// segmented controls would: the window has one way to choose among views.
func (a *app) words(c *ui.Context, key string, sel *int, labels ...string) ui.Element {
	p := a.pal
	return ui.Row(c.Key(key)).Gap(22).Height(34).AlignItems(ui.Stretch).Shrink(0).Children(func() {
		for i, label := range labels {
			on := *sel == i
			b := ui.ButtonBase(c.Key(fmt.Sprint(key, ":", i))).AlignItems(ui.Center).Label(label)
			col := p.muted
			if on || b.Hovered() {
				col = p.text
			}
			under := float32(0)
			if on {
				under = 1
			}
			under = b.Animate("under", under, 160*time.Millisecond)
			b.Children(func() {
				ui.Text(c, label).FontSize(13).FontWeight(500).TextColor(col).SingleLine()
				// The stroke rests on the hairline under the row of words,
				// as the title bar's does on its own.
				ui.Box(c).Absolute().Left(0).Right(0).Bottom(-1).Height(2).Radius(1).
					Background(p.accent.Alpha(under))
			})
			if b.Clicked() {
				*sel = i
			}
		}
	})
}

// tag is a small neutral token: a rule id, a phase, a pattern. It carries
// no color of its own; a quiet wash sets it apart from the text around it.
func tag(c *ui.Context, p *palette, label string) ui.Element {
	return ui.Text(c, label).FontSize(11).Font(mono()).TextColor(p.muted).
		Padding(2, 7).Radius(6).Background(p.text.Alpha(0.055)).SingleLine().Shrink(0)
}

// stateDot is a state said plainly: a small dot in its color, and the word
// beside it in the text's own. Color marks the state; it does not fill it.
func stateDot(c *ui.Context, p *palette, col ui.Color, label string) {
	ui.Row(c).Gap(6).AlignItems(ui.Center).Shrink(0).Children(func() {
		// The dot keeps a faint halo of its own color, as a lamp does.
		ui.Box(c).Size(6, 6).Radius(3).Background(col).Shadow(0, 0, 0, 2, col.Alpha(0.16))
		ui.Text(c, label).FontSize(13).TextColor(p.muted).SingleLine()
	})
}

// panel is a raised sheet on a page's ground: the page's own color, a
// hairline round it and a soft shadow under it. It is what gives a page a
// surface to read on rather than a bare ground.
func panel(c *ui.Context, p *palette) ui.Element {
	return ui.Column(c).Background(p.editor).Border(1, p.border).Radius(12).
		Shadow(0, 1, 2, 0, p.shade()).Clip()
}

// statusText is a response status as a number in mono, in the color of
// how it went, on a wash of the same: a small pill, so a column of
// statuses has a shape to scan and a failure stands out of it.
func statusText(c *ui.Context, p *palette, status int) ui.Element {
	tone := statusTone(p, status)
	return ui.Text(c, fmt.Sprint(status)).FontSize(11).FontWeight(500).Font(mono()).
		TextColor(tone).Padding(2.5, 7).Radius(6).Background(tone.Alpha(p.wash())).SingleLine().Shrink(0)
}

// methodText is a request's verb as a small mono chip: GET, the common
// case, recedes; a verb that changes something reads a step stronger.
func methodText(c *ui.Context, p *palette, method string) ui.Element {
	col, bg := p.text, p.text.Alpha(0.075)
	if method == "GET" || method == "CONNECT" || method == "" {
		col, bg = p.muted, p.text.Alpha(0.045)
	}
	return ui.Text(c, methodLabel(method)).FontSize(11).FontWeight(500).Font(mono()).
		TextColor(col).Padding(2.5, 7).Radius(6).Background(bg).TextAlign(ui.Center).SingleLine().Shrink(0)
}

// ruleChip names a rule that applied to a request: its bolt and its name
// on a wash of the accent.
func ruleChip(c *ui.Context, p *palette, name string) {
	ui.Row(c).Gap(5).AlignItems(ui.Center).Shrink(0).Padding(2, 9, 2, 6).Radius(10).
		Background(p.accent.Alpha(p.wash() * 0.8)).Children(func() {
		ruleMark(c, p, 12, true)
		ui.Text(c, name).FontSize(12).FontWeight(500).TextColor(p.accent).SingleLine()
	})
}

// emptyState is what a page shows while it has nothing: a tinted mark, a
// title, a line of help and, optionally, the command that starts things
// off in a small code chip.
func emptyState(c *ui.Context, p *palette, icon *ui.SVG, title, help, cmd string) {
	emptyStateWith(c, p, icon, title, help, cmd, nil)
}

// emptyStateWith is emptyState with an action under the chip, drawn by act.
func emptyStateWith(c *ui.Context, p *palette, icon *ui.SVG, title, help, cmd string, act func()) {
	ui.Column(c).Grow(1).Center().Padding(24).Gap(10).Children(func() {
		emptyMark(c, p, icon)
		ui.Text(c, title).FontSize(15).FontWeight(600).TextColor(p.text).Margin(6, 0, 0, 0).SingleLine()
		ui.Text(c, help).FontSize(13).TextColor(p.muted).TextAlign(ui.Center).LineHeight(1.5).MaxWidth(340)
		if cmd != "" {
			ui.Text(c, cmd).FontSize(12).Font(mono()).TextColor(p.muted).Padding(7, 12).Radius(8).
				Background(p.text.Alpha(0.045)).InsetShadow(0, 0, 0, 1, p.text.Alpha(0.05)).
				Margin(2, 0, 0).SingleLine().Selectable()
		}
		if act != nil {
			ui.Row(c).Margin(4, 0, 0).Children(act)
		}
	})
}

// riseIn brings an element in as it is built: it fades and rises a little,
// Mayoi's welcome-page entrance, delayed by delay.
func riseIn(e ui.Element, delay time.Duration) ui.Element {
	k := e.Keyframes("rise", []ui.Keyframe{{Value: 0}, {At: 240 * time.Millisecond, Value: 1, Ease: ui.EaseOut}},
		ui.KeyframeOptions{Delay: delay})
	return e.Opacity(k).Translate(0, 8*(1-k))
}

// popIn is how a floating thing takes the screen, as Mayoi's are.
var popIn = ui.ElementTransition{Duration: 130 * time.Millisecond, Colors: true, Enter: &ui.Motion{Y: -6}, Exit: &ui.Motion{Y: -4}}

// statusTone is the color a response status reads in: the four the window
// keeps for how things went, and no others.
func statusTone(p *palette, status int) ui.Color {
	switch {
	case status == 0:
		return p.faint
	case status < 300:
		return p.added
	case status < 400:
		return p.changed
	case status < 500:
		return p.warning()
	default:
		return p.removed
	}
}

// barTone is the fill of a latency bar: the status' color, held back, so
// the bar agrees with the figure beside it without competing with it.
func barTone(p *palette, status int) ui.Color {
	if status >= 400 {
		return statusTone(p, status).Alpha(0.85)
	}
	return statusTone(p, status).Alpha(0.5)
}

func methodLabel(method string) string {
	if method == "" {
		return "—"
	}
	return method
}

func hostOf(url string) string {
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
	}
	if i := strings.IndexAny(url, "/?"); i >= 0 {
		url = url[:i]
	}
	return url
}

func pathOf(url string) string {
	if i := strings.Index(url, "://"); i >= 0 {
		if j := strings.Index(url[i+3:], "/"); j >= 0 {
			return url[i+3+j:]
		}
		return "/"
	}
	return url
}

// durationLabel is how long an exchange took, "—" while it runs.
func durationLabel(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// latencyLabel is durationLabel for an exchange: one that has its status
// and took under a millisecond, as a mocked response does, reads "<1ms",
// not the dash of one still running.
func latencyLabel(r api.RequestSummary) string {
	if r.DurationMs <= 0 && r.Status != 0 {
		return "<1ms"
	}
	return durationLabel(r.DurationMs)
}

// detailHead is the URL the detail's header names, "" while it loads.
func detailHead(d *api.RequestDetailResponse) string {
	if d == nil || d.Request.Method == "" {
		return ""
	}
	return d.Request.URL
}

// paramValue is one parameter of an action as it is read: a string as
// itself, anything else as JSON — %v would print a nested value as Go's
// map[...].
func paramValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprint(v)
}

func bodyOrNone(b *string) string {
	if b == nil || *b == "" {
		return "(none)"
	}
	return *b
}
