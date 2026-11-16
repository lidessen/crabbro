package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/chiling/pkg/api"
)

// Shared widgets and small formatters the pages draw with.

// emptyMark is the mark an empty page is known by: its icon in the accent,
// on a soft square of the same.
func emptyMark(c *ui.Context, p *palette, icon *ui.SVG) {
	ui.Row(c).Size(44, 44).Center().Radius(12).Background(p.accent.Alpha(0.1)).Children(func() {
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
	return ui.Text(c, title).FontSize(11.5).FontWeight(600).TextColor(p.faint).SingleLine()
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
	return ui.Row(c.Key(key)).Gap(20).Height(30).AlignItems(ui.Stretch).Shrink(0).Children(func() {
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
				ui.Text(c, label).FontSize(12.5).FontWeight(550).TextColor(col).SingleLine()
				ui.Box(c).Absolute().Left(0).Right(0).Bottom(2).Height(2).Radius(1).
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
		Padding(1, 6).Radius(5).Background(p.text.Alpha(0.055)).SingleLine().Shrink(0)
}

// stateDot is a state said plainly: a small dot in its color, and the word
// beside it in the text's own. Color marks the state; it does not fill it.
func stateDot(c *ui.Context, p *palette, col ui.Color, label string) {
	ui.Row(c).Gap(6).AlignItems(ui.Center).Shrink(0).Children(func() {
		ui.Box(c).Size(6, 6).Radius(3).Background(col)
		ui.Text(c, label).FontSize(12).TextColor(p.muted).SingleLine()
	})
}

// statusText is a response status as a number in mono, in the color of
// how it went. The color is in the figure itself — no badge behind it — so
// a column of statuses reads as a column of text, and a failure still
// stands out of it.
func statusText(c *ui.Context, p *palette, status int) ui.Element {
	return ui.Text(c, fmt.Sprint(status)).FontSize(12).FontWeight(600).Font(mono()).
		TextColor(statusTone(p, status)).SingleLine().Shrink(0)
}

// methodText is a request's verb in mono: GET, the common case, recedes;
// a verb that changes something reads a step stronger.
func methodText(c *ui.Context, p *palette, method string) ui.Element {
	col := p.text
	if method == "GET" || method == "CONNECT" || method == "" {
		col = p.muted
	}
	return ui.Text(c, methodLabel(method)).FontSize(11.5).FontWeight(600).Font(mono()).
		TextColor(col).SingleLine().Shrink(0)
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
		ui.Text(c, title).FontSize(14).FontWeight(600).TextColor(p.text).Margin(6, 0, 0, 0).SingleLine()
		ui.Text(c, help).FontSize(12.5).TextColor(p.muted).TextAlign(ui.Center).LineHeight(1.5).MaxWidth(340)
		if cmd != "" {
			ui.Text(c, cmd).FontSize(12).Font(mono()).TextColor(p.muted).Padding(5, 10).Radius(6).
				Background(p.text.Alpha(0.045)).Margin(2, 0, 0).SingleLine()
		}
		if act != nil {
			ui.Row(c).Margin(4, 0, 0).Children(act)
		}
	})
}

// stat is one headline number of the daemon's overview: a large value over
// a small label, on the page itself. The value rolls as it changes.
func stat(c *ui.Context, p *palette, key, label, value string) {
	ui.Column(c.Key("stat:" + key)).Grow(1).Basis(0).MinWidth(0).Gap(3).Children(func() {
		ui.Text(c, value).FontSize(22).FontWeight(650).LetterSpacing(-0.3).TextColor(p.text).SingleLine().
			ContentTransition(ui.ContentTransition{Numeric: true})
		ui.Text(c, label).FontSize(12).TextColor(p.muted).SingleLine()
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

// actionParams is an action's params as one line.
func actionParams(act api.RuleAction) string {
	parts := make([]string, 0, len(act.Params))
	for k, v := range act.Params {
		// JSON, not %v: a nested value would print as Go's map[...].
		val := fmt.Sprint(v)
		if _, plain := v.(string); !plain {
			if b, err := json.Marshal(v); err == nil {
				val = string(b)
			}
		}
		parts = append(parts, k+"="+val)
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return ""
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func bodyOrNone(b *string) string {
	if b == nil || *b == "" {
		return "(none)"
	}
	return *b
}
