package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/egoist/mygo/plugins/glass"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/editor"
	"golang.org/x/net/html"

	"github.com/lidessen/chiling/pkg/api"
)

// The requests page: the live table of captured exchanges and the detail
// of the one chosen.

// detailSplitH is the height the request detail takes below the table, as
// the page's splitter keeps it.
var detailSplitH float32 = 270

// replayingID is the request whose replay the detail's button is showing
// its wait for, "" for none, and startReplayAt when the wait began. Both
// live on the view thread, set where the click is and read as the frame
// draws: the replay action reports no callback, so the wait ends by the
// timeout below, in a frame of its own.
var (
	replayingID   string
	startReplayAt time.Time
)

// replayBusyFor is how long the replay button may spin without word of
// the replay.
const replayBusyFor = 3 * time.Second

// requestsPage is the live table of captured exchanges and, below it, the
// detail of the one chosen.
func (a *app) requestsPage(c *ui.Context, p *palette) {
	st := a.snapshotState()

	// The rows are the search's matches; the state keeps the table's own
	// sort, which reorders them.
	rows := a.sortRequests(st.requests)
	if len(rows) > 0 {
		a.reqState.Key = func(row int) any { return rows[row].ID }
		a.reqState.Selected = &a.reqSel
	}

	// A link from the trajectory lands here: choose the row it names.
	if a.pendingRequestID != "" {
		for i, r := range rows {
			if string(r.ID) == a.pendingRequestID {
				a.reqSel = i
				break
			}
		}
		a.pendingRequestID = ""
	}

	// The table takes a quiet theme: its choice is a tint of the accent,
	// not the accent itself, which would swallow the pills' colors.
	base := *c.Theme()
	p.quiet(c)
	ui.SplitVertical(c, &detailSplitH,
		func() {
			ui.Column(c).Fill().MinHeight(0).Children(func() {
				a.requestsToolbar(c, p)
				if len(rows) == 0 {
					a.requestsEmpty(c, p)
				} else {
					cols := []ui.TableColumn{
						{Title: "Status", ID: "status", Width: 74, Sortable: true},
						{Title: "Method", ID: "method", Width: 84, Sortable: true},
						{Title: "Host", ID: "host", Width: 200, Sortable: true},
						{Title: "Path", ID: "path"},
						{Title: "Time", ID: "time", Width: 72, Align: ui.End, Sortable: true},
					}
					ui.Table(c, &a.reqState, cols, len(rows), func(row, col int) {
						a.requestCell(c, p, rows[row], col, row == a.reqSel)
					}).Grow(1).MinHeight(0).ContextMenu(func(m *ui.Menu) {
						if id := a.chosenRequestID(rows); id != "" && m.Item("Replay "+id).Chosen() {
							a.actReplay(id)
						}
					})
				}
			})
		},
		func() { a.requestDetail(c, p, st, rows) },
	).Grow(1)
	c.SetTheme(&base)

	// The detail follows the row chosen, by id, so the choice survives the
	// row moving up as newer exchanges arrive.
	if id := a.chosenRequestID(rows); id != a.reqShownID {
		a.reqShownID = id
		a.selectRequest(id)
	}
}

// requestsEmpty is what the table's place shows while nothing has been
// captured yet, as Mayoi's empty folder is: how to send the first request
// through the proxy.
func (a *app) requestsEmpty(c *ui.Context, p *palette) {
	emptyState(c, p, iconList, "No traffic yet",
		"Point a client at the proxy and send a request. Every exchange shows up here as it happens.",
		"export HTTP_PROXY=http://127.0.0.1:8080")
}

// chosenRequestID is the request the table's choice names, "" for none.
func (a *app) chosenRequestID(rows []uiRequestRow) string {
	if a.reqSel < 0 || a.reqSel >= len(rows) {
		return ""
	}
	return string(rows[a.reqSel].ID)
}

// requestsToolbar occupies its own row above the table header.
func (a *app) requestsToolbar(c *ui.Context, p *palette) {
	ui.Row(c).FillWidth().Height(56).Shrink(0).Padding(8, 12).
		Gap(10).AlignItems(ui.Center).Children(func() {
		ui.SearchField(c, &a.search).Grow(1).Shrink(1).MinWidth(40).MaxWidth(300)
		ui.Checkbox(c, &a.showHidden, "Show hidden").FontSize(12.5)
		ui.Spacer(c)
		st := a.snapshotState()
		failed := 0
		for _, rq := range st.requests {
			if rq.Status >= 400 {
				failed++
			}
		}
		a.pill(c, fmt.Sprintf("%d requests", len(st.requests)), p.muted, p.muted.Alpha(0.12), false)
		if failed > 0 {
			a.pill(c, fmt.Sprintf("%d failed", failed), p.removed, p.removed.Alpha(0.13), false)
		}
		if a.bounceSearch(c) {
			a.nudge(a.search)
		}
	})
}

// bounceSearch sends the query once it has settled for a moment, so
// typing does not search per keystroke. It reports whether it did.
func (a *app) bounceSearch(c *ui.Context) bool {
	now := c.Now()
	if a.search != a.searchSeen {
		a.searchSeen = a.search
		a.searchAt = now
		return false
	}
	if a.search != a.searchSent && now.Sub(a.searchAt) >= 300*time.Millisecond {
		a.searchSent = a.search
		return true
	}
	return false
}

// uiRequestRow is one sorted row of the requests table.
type uiRequestRow = api.RequestSummary

// sortRequests reorders the rows as the table's header says. A stable sort
// keeps the newest-first order within equal rows.
func (a *app) sortRequests(in []api.RequestSummary) []uiRequestRow {
	rows := make([]uiRequestRow, len(in))
	copy(rows, in)
	sortCol, desc := "", false
	if a.reqState.Sort != nil {
		sortCol, desc = a.reqState.Sort.Column, a.reqState.Sort.Descending
	}
	less := func(i, j int) bool { return false }
	switch sortCol {
	case "status":
		less = func(i, j int) bool { return rows[i].Status < rows[j].Status }
	case "method":
		less = func(i, j int) bool { return rows[i].Method < rows[j].Method }
	case "host":
		less = func(i, j int) bool { return rows[i].URL < rows[j].URL }
	case "time":
		less = func(i, j int) bool { return rows[i].DurationMs < rows[j].DurationMs }
	default:
		return rows
	}
	sort.SliceStable(rows, func(i, j int) bool { return less(i, j) != desc })
	return rows
}

// requestCell draws one cell of the requests table.
func (a *app) requestCell(c *ui.Context, p *palette, r uiRequestRow, col int, chosen bool) {
	switch col {
	case 0:
		if r.Status == 0 {
			ui.Spinner(c).Size(10, 10)
			return
		}
		col, bg := statusColor(p, r.Status)
		a.pill(c, fmt.Sprint(r.Status), col, bg, chosen)
	case 1:
		col, bg := methodColor(p, r.Method)
		a.pill(c, methodLabel(r.Method), col, bg, chosen)
	case 2:
		ui.Text(c, hostOf(r.URL)).FontSize(12.5).TextColor(p.text).SingleLine()
	case 3:
		ui.Row(c).Grow(1).Gap(6).AlignItems(ui.Center).Children(func() {
			ui.Text(c, pathOf(r.URL)).FontSize(12).Font(mono()).TextColor(p.text).SingleLine().Shrink(1).MinWidth(20)
			for i, tag := range r.Tags {
				if i > 2 {
					break
				}
				ui.Badge(c, tag)
			}
			if r.Hidden {
				ui.Text(c, "hidden").FontSize(11).TextColor(p.faint).SingleLine()
			}
		})
	case 4:
		ui.Text(c, durationLabel(r.DurationMs)).FontSize(11.5).Font(mono()).
			TextColor(p.faint).TextAlign(ui.End).SingleLine()
	}
}

// ---------------------------------------------------------------------------
// request detail

// requestDetail is the exchange the table chose: what it sent, what came
// back, and which rules touched it.
func (a *app) requestDetail(c *ui.Context, p *palette, st store, rows []uiRequestRow) {
	id := a.chosenRequestID(rows)
	if id == "" {
		emptyState(c, p, iconReplay, "Select a request",
			"Choose a row to see what it sent, what came back, and which rules touched it.", "")
		return
	}
	d, errText := st.detail, st.detailErr
	// The header and the tabs float on glass over the content, which
	// scrolls under them and fades into the scroll edge.
	ui.Box(c).Grow(1).MinHeight(0).BorderWidth(1, 0, 0, 0).BorderColor(p.border).Children(func() {
		ui.Scroll(c).Fill().Padding(80, 14, 16).ScrollbarInsets(80, 0, 0).Children(func() {
			// Keyed by the request and the tab, so the content fades in
			// again as either changes.
			tabFade(ui.Column(c.Key(fmt.Sprintf("detail:content:%s:%d", id, a.detailTab))).
				FillWidth().Padding(0, 0, 8).Gap(12)).Children(func() {
				switch {
				case errText != "":
					ui.Text(c, errText).FontSize(12).TextColor(p.removed)
				case d == nil:
					a.detailSkeleton(c, p)
				default:
					a.detailBody(c, p, d)
				}
			})
		})
		ui.Box(c).Absolute().Top(0).Left(0).Right(0).Height(80).PassThrough().
			Material(glass.ScrollEdge{})
		ui.Row(c).Absolute().Top(8).Left(12).Right(12).Height(34).Padding(0, 10).
			Gap(8).AlignItems(ui.Center).Radius(14).Material(glass.Glass{}).Children(func() {
			if d != nil && d.Request.Status != 0 {
				col, bg := statusColor(p, d.Request.Status)
				detailPill(c, fmt.Sprint(d.Request.Status), col, bg)
			}
			if d != nil && d.Request.Method != "" {
				mcol, mbg := methodColor(p, d.Request.Method)
				detailPill(c, methodLabel(d.Request.Method), mcol, mbg)
			}
			if head := detailHead(d); head != "" {
				ui.Text(c, head).FontSize(12).Font(mono()).TextColor(p.text).SingleLine().Shrink(1).MinWidth(40)
			}
			ui.Text(c, id).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine()
			ui.Spacer(c)
			a.replayButton(c, p, id)
		})
		ui.Row(c).Absolute().Top(46).Left(16).Right(16).Height(30).
			AlignItems(ui.Center).Gap(8).PassThrough().Children(func() {
			ui.Tabs(c, &a.detailTab, "Headers", "Bodies", "Rule trace")
		})
	})
}

// tabFade is how the detail's content comes in as its tab, or its request,
// changes: a fade, without moving, as Mayoi's pages do.
func tabFade(e ui.Element) ui.Element {
	return e.Opacity(e.Keyframes("tab", []ui.Keyframe{{Value: 0}, {At: 150 * time.Millisecond, Value: 1, Ease: ui.EaseOut}}))
}

// pillEl is a pill that answers with its element, for the ones the page
// animates.
func pillEl(c *ui.Context, label string, col, bg ui.Color) ui.Element {
	return ui.Text(c, label).FontSize(11).FontWeight(650).Font(mono()).TextColor(col).
		Padding(2, 7).Radius(10).Background(bg).SingleLine()
}

// detailPill is a pill of the detail's header, whose label animates as it
// changes, as the status does when the exchange completes or a replay
// lands.
func detailPill(c *ui.Context, label string, col, bg ui.Color) {
	pillEl(c, label, col, bg).ContentTransition(ui.ContentTransition{})
}

// replayButton is the detail header's replay button, drawn here rather
// than barButton's, for the wait: while the replay it started is going, a
// spinner turns in place of its icon and the button takes no clicks,
// until the timeout below gives the wait up.
func (a *app) replayButton(c *ui.Context, p *palette, id string) {
	busy := a.replayBusy(c, id)
	b := ui.ButtonBase(c.Key("replay")).Size(28, 24).Radius(6).Justify(ui.Center).AlignItems(ui.Center).
		Label("Replay this request through the proxy").Tooltip("Replay this request through the proxy").
		Disabled(busy)
	if !busy {
		switch {
		case b.Pressed():
			b.Background(p.surfacePressed)
		case b.Hovered():
			b.Background(p.hover)
		}
	}
	b.Transition(hoverFade)
	col := p.muted
	if !busy && (b.Hovered() || b.Pressed()) {
		col = p.text
	}
	b.Children(func() {
		if busy {
			ui.Spinner(c).Size(14, 14).TextColor(col).Label("Replaying")
		} else {
			ui.Icon(c, iconReplay).FontSize(14).TextColor(col)
		}
	})
	if !busy && b.Clicked() {
		replayingID, startReplayAt = id, c.Now()
		// The wait's backstop: the spinner repaints itself without
		// rebuilding the view, so a replay whose word never comes spins
		// for ever unless something draws a frame past the timeout.
		time.AfterFunc(replayBusyFor, a.invalidate)
		a.invalidate()
		a.actReplay(id)
	}
}

// replayBusy reports the replay of id is showing its wait in the button,
// giving the wait up once replayBusyFor has gone.
func (a *app) replayBusy(c *ui.Context, id string) bool {
	if replayingID != id {
		return false
	}
	if c.Now().Sub(startReplayAt) >= replayBusyFor {
		replayingID = ""
		return false
	}
	return true
}

// detailSkeleton is what the detail shows while its exchange loads: two
// cards of quiet bars where the sections will be.
func (a *app) detailSkeleton(c *ui.Context, p *palette) {
	// bar draws one; w of 0 or less takes the card's width.
	bar := func(w float32) {
		b := ui.Box(c).Height(12).Radius(6).Background(p.faint.Alpha(0.06))
		if w <= 0 {
			b.FillWidth()
		} else {
			b.Width(w)
		}
	}
	section := func(title float32, lines ...float32) {
		card(c, p, func() {
			ui.Column(c).FillWidth().Gap(8).Children(func() {
				bar(title)
				for _, w := range lines {
					bar(w)
				}
			})
		})
	}
	section(88, 0, 176)
	section(64, 0, 120)
}

// detailBody is what the detail's tabs show of one exchange.
func (a *app) detailBody(c *ui.Context, p *palette, d *api.RequestDetailResponse) {
	switch a.detailTab {
	case 1:
		a.bodySection(c, p, "request", "Request body", d.Body, d.Request.RequestHeaders, d.Request.RequestHeaderList)
		a.bodySection(c, p, "response", "Response body", d.ResponseBody, d.Request.ResponseHeaders, d.Request.ResponseHeaderList)
	case 2:
		a.ruleTraceSection(c, p, d.Request.RuleTrace)
	default:
		a.headersSection(c, p, "Request headers", d.Request.RequestHeaderList, d.Request.RequestHeaders)
		a.headersSection(c, p, "Response headers", d.Request.ResponseHeaderList, d.Request.ResponseHeaders)
	}
}

// headersSection is one group of headers, list first, map as the fallback.
func (a *app) headersSection(c *ui.Context, p *palette, title string, list []api.HeaderField, m api.Headers) {
	card(c, p, func() {
		cardTitle(c, p, title)
		if len(list) == 0 && len(m) == 0 {
			ui.Text(c, "none").FontSize(12).Font(mono()).TextColor(p.faint)
			return
		}
		ui.Column(c).FillWidth().Gap(1).Children(func() {
			draw := func(name, value string) {
				ui.Row(c).FillWidth().Gap(10).AlignItems(ui.Start).Padding(2, 0).Children(func() {
					ui.Text(c, name).FontSize(12).Font(mono()).FontWeight(600).TextColor(p.text).SingleLine().Shrink(0)
					ui.Text(c, value).FontSize(12).Font(mono()).TextColor(p.muted).Shrink(1).MinWidth(40)
				})
			}
			for _, h := range list {
				for _, v := range h.Values {
					draw(h.Name, v)
				}
			}
			if len(list) == 0 {
				for name, value := range m {
					draw(name, value)
				}
			}
		})
	})
}

// bodyView keeps the source and display buffers stable across frames.
type bodyView struct {
	source      string
	input       string
	contentType string
	formatted   string
	filename    string
	mode        int
	state       *editor.State
}

var bodyViews = map[string]*bodyView{}

func (a *app) bodySection(c *ui.Context, p *palette, key, title string, body *string, headers api.Headers, list []api.HeaderField) {
	card(c, p, func() {
		v := bodyViews[key]
		if v == nil {
			v = &bodyView{}
			bodyViews[key] = v
		}
		source := bodyOrNone(body)
		contentType := ""
		for k, value := range headers {
			if strings.EqualFold(k, "content-type") {
				contentType = value
				break
			}
		}
		for _, h := range list {
			if strings.EqualFold(h.Name, "content-type") && len(h.Values) > 0 {
				contentType = h.Values[0]
				break
			}
		}
		if v.state == nil || v.input != source || v.contentType != contentType {
			v.formatted, v.filename = readableBody(source, contentType)
			v.input, v.contentType = source, contentType
		}
		display, filename := v.formatted, v.filename
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
			cardTitle(c, p, title)
			ui.Spacer(c)
			ui.Segmented(c, &v.mode, "Readable", "Raw")
		})
		if v.mode == 1 {
			display = source
		}
		cacheKey := fmt.Sprintf("%d:%s:%s", v.mode, filename, display)
		if v.state == nil || v.source != cacheKey {
			buf := editor.NewBuffer(display)
			buf.SetLanguage(editor.LanguageFor(filename))
			v.state = &editor.State{Buffer: buf, ReadOnly: true, SoftWrap: true, NoScrollPastEnd: true, Label: title}
			v.source = cacheKey
		}
		editor.View(c.Key("body:"+key), v.state).FillWidth().Height(200).MinWidth(0).Margin(8, 0, 0)
	})
}

// readableBody formats JSON and HTML without loading or executing content.
// Raw always retains the captured bytes represented by the API's string.
func readableBody(source, contentType string) (string, string) {
	var formatted bytes.Buffer
	if json.Indent(&formatted, []byte(source), "", "  ") == nil {
		return formatted.String(), "body.json"
	}
	trimmed := strings.TrimSpace(source)
	if strings.Contains(strings.ToLower(contentType), "html") || strings.HasPrefix(strings.ToLower(trimmed), "<!doctype html") || strings.HasPrefix(strings.ToLower(trimmed), "<html") {
		z := html.NewTokenizer(strings.NewReader(source))
		var out strings.Builder
		lastNewline := true
		for {
			t := z.Next()
			if t == html.ErrorToken {
				if z.Err() == io.EOF {
					break
				}
				return source, "body.html"
			}
			raw := string(z.Raw())
			if (t == html.StartTagToken || t == html.EndTagToken || t == html.SelfClosingTagToken) && out.Len() > 0 && !lastNewline {
				out.WriteByte('\n')
			}
			out.WriteString(raw)
			if len(raw) > 0 {
				lastNewline = raw[len(raw)-1] == '\n'
			}
		}
		return out.String(), "body.html"
	}
	return source, "body.txt"
}

// ruleTraceSection is which rules ran on the exchange and what they did.
func (a *app) ruleTraceSection(c *ui.Context, p *palette, traces []api.RuleTrace) {
	if len(traces) == 0 {
		ui.Text(c, "No rule ran on this exchange.").FontSize(12).TextColor(p.faint)
		return
	}
	for _, tr := range traces {
		card(c, p, func() {
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				// The verdict rises in as the card shows it, a no match
				// after its beat, so a list of cards settles in order.
				if tr.Matched {
					pillEl(c, "matched", p.added, p.added.Alpha(0.13))
				} else {
					pillEl(c, "no match", p.faint, p.faint.Alpha(0.12))
				}
				ui.Text(c, string(tr.RuleID)).FontSize(12.5).FontWeight(600).TextColor(p.text).SingleLine()
				if tr.Phase != "" {
					ui.Badge(c, tr.Phase)
				}
				ui.Spacer(c)
				ui.Text(c, durationLabel(tr.DurationMs)).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine()
			})
			if tr.Error != "" {
				ui.Text(c, tr.Error).FontSize(12).Font(mono()).TextColor(p.removed).FillWidth()
			}
			for _, act := range tr.Actions {
				ui.Text(c, "→ "+string(act.Type)+" "+actionParams(act)).FontSize(12).Font(mono()).
					TextColor(p.muted).FillWidth()
			}
			for _, stg := range tr.Stages {
				line := stg.ID
				if stg.Uses != "" {
					if line != "" {
						line += " · "
					}
					line += stg.Uses
				}
				if stg.Skipped {
					line += " (skipped: " + stg.SkipReason + ")"
				}
				if stg.Effect != "" {
					line += " — " + stg.Effect
				}
				if stg.Error != "" {
					line += " — error: " + stg.Error
				}
				if line != "" {
					ui.Text(c, line).FontSize(11.5).Font(mono()).TextColor(p.faint).FillWidth()
				}
			}
		})
	}
}
