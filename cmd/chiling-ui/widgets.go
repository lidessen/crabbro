package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/chiling/pkg/api"
)

// Shared widgets and small formatters the pages draw with.

// pill is a rounded badge: a wash of the color behind text in it. On a
// chosen row the wash drops, for the row's own tint to show.
func (a *app) pill(c *ui.Context, label string, col, bg ui.Color, chosen bool) {
	t := ui.Text(c, label).FontSize(11).FontWeight(650).Font(mono()).TextColor(col).
		Padding(2, 7).Radius(10).SingleLine()
	if !chosen {
		t.Background(bg)
	}
}

// pageWash is the quiet vertical wash a page's own background draws, from
// the editor toward the chrome, which the cards catch the light from.
func pageWash(p *palette) ui.LinearGradient {
	return ui.LinearGradient{From: p.editor, To: p.chrome.Mix(p.editor, 0.5), Angle: 180}
}

// card is a surface of the page: a rounded face the sections sit on.
func card(c *ui.Context, p *palette, fn func()) {
	ui.Column(c).FillWidth().Padding(10, 12).Radius(8).
		Background(p.surface).Border(1, p.border).Children(fn)
}

// cardTitle is what a card is of.
func cardTitle(c *ui.Context, p *palette, title string) {
	ui.Text(c, strings.ToUpper(title)).FontSize(10.5).FontWeight(650).LetterSpacing(0.6).
		TextColor(p.faint).SingleLine()
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

// statusColor is the color and wash of a response status.
func statusColor(p *palette, status int) (ui.Color, ui.Color) {
	switch {
	case status == 0:
		return p.faint, p.faint.Alpha(0.12)
	case status < 300:
		return p.added, p.added.Alpha(0.13)
	case status < 400:
		return p.changed, p.changed.Alpha(0.13)
	case status < 500:
		return p.warning(), p.warning().Alpha(0.13)
	default:
		return p.removed, p.removed.Alpha(0.13)
	}
}

// methodColor tints the verbs by what they do.
func methodColor(p *palette, method string) (ui.Color, ui.Color) {
	switch method {
	case "GET":
		return p.changed, p.changed.Alpha(0.11)
	case "POST":
		return p.accent, p.accent.Alpha(0.14)
	case "PUT", "PATCH":
		return p.warning(), p.warning().Alpha(0.13)
	case "DELETE":
		return p.removed, p.removed.Alpha(0.12)
	case "CONNECT":
		return p.faint, p.faint.Alpha(0.12)
	}
	return p.muted, p.muted.Alpha(0.12)
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
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
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
