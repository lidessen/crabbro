package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

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

// requestListLimit is how many exchanges the table asks the daemon for; at
// that count the toolbar says the list is the latest of more.
const requestListLimit = 200

// reqMaxMs is the slowest exchange of the table's rows, which the latency
// bars are drawn against.
var reqMaxMs int64 = 1

// replayingID is the request whose replay the detail's button is showing
// its wait for, "" for none, and startReplayAt when the wait began. Both
// live on the view thread, set where the click is and read as the frame
// draws. The replay's answer clears it; the timeout below is only the
// backstop for an answer that never comes, past the daemon's own 20 s.
var (
	replayingID   string
	startReplayAt time.Time
)

// replayBusyFor is how long the replay button may spin without word of
// the replay.
const replayBusyFor = 25 * time.Second

// requestsPage is the live table of captured exchanges and, below it, the
// detail of the one chosen.
func (a *app) requestsPage(c *ui.Context, p *palette) {
	st := a.snapshotState()

	// The rows are the search's matches; the state keeps the table's own
	// sort, which reorders them.
	rows := a.sortRequests(st.requests)
	reqMaxMs = 1
	for _, r := range rows {
		reqMaxMs = max(reqMaxMs, r.DurationMs)
	}
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
					// The columns read as a line of the trace does: when,
					// what, where, then how long and what came back.
					cols := []ui.TableColumn{
						{Title: "Time", ID: "at", Width: 92},
						{Title: "Method", ID: "method", Width: 84, Sortable: true},
						{Title: "Host", ID: "host", Width: 190, Sortable: true},
						{Title: "Path", ID: "path"},
						{Title: "Latency", ID: "time", Width: 140, Align: ui.End, Sortable: true},
						{Title: "Status", ID: "status", Width: 72, Align: ui.End, Sortable: true},
					}
					sealed := sealedRequests(st.traj)
					ui.Table(c, &a.reqState, cols, len(rows), func(row, col int) {
						a.requestCell(c, p, rows[row], col, sealed[string(rows[row].ID)])
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
	ui.Row(c).FillWidth().Height(48).Shrink(0).Padding(0, 12).
		Gap(10).AlignItems(ui.Center).Children(func() {
		ui.SearchField(c, &a.search).Grow(1).Shrink(1).MinWidth(40).MaxWidth(300)
		wasHidden := a.showHidden
		ui.Checkbox(c, &a.showHidden, "Show hidden").FontSize(12.5)
		if a.showHidden != wasHidden {
			// Refetch now: waiting for the next tick leaves the table
			// unchanged for seconds after the click.
			a.mirrorView()
			a.nudge(a.search)
		}
		ui.Spacer(c)
		st := a.snapshotState()
		failed := 0
		for _, rq := range st.requests {
			if rq.Status >= 400 {
				failed++
			}
		}
		// The counts read as a line of text, not as badges: only the
		// failures take a color.
		count := fmt.Sprintf("%d requests", len(st.requests))
		if len(st.requests) >= requestListLimit {
			count = fmt.Sprintf("Latest %d", requestListLimit)
		}
		ui.Text(c, count).FontSize(12).TextColor(p.muted).SingleLine().
			ContentTransition(ui.ContentTransition{Numeric: true})
		if failed > 0 {
			ui.Text(c, "·").FontSize(12).TextColor(p.faint).SingleLine()
			ui.Text(c, fmt.Sprintf("%d failed", failed)).FontSize(12).TextColor(p.removed).SingleLine().
				ContentTransition(ui.ContentTransition{Numeric: true})
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

// sealedRequests is the requests a rule touched, as the trace has them.
func sealedRequests(traj []api.Activity) map[string]bool {
	out := map[string]bool{}
	for _, it := range traj {
		if it.Kind == "rule_applied" && it.RequestID != "" {
			out[it.RequestID] = true
		}
	}
	return out
}

// requestCell draws one cell of the requests table.
func (a *app) requestCell(c *ui.Context, p *palette, r uiRequestRow, col int, sealed bool) {
	switch col {
	case 0:
		ui.Text(c, clockOf(r.StartedAt)).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine()
	case 1:
		methodText(c, p, r.Method)
	case 2:
		ui.Text(c, hostOf(r.URL)).FontSize(12).Font(mono()).TextColor(p.muted).SingleLine()
	case 3:
		ui.Row(c).Grow(1).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, pathOf(r.URL)).FontSize(12).Font(mono()).FontWeight(500).TextColor(p.text).SingleLine().Shrink(1).MinWidth(20)
			if sealed {
				seal(c, p, 13, true)
			}
			for i, name := range r.Tags {
				if i > 2 {
					break
				}
				tag(c, p, name)
			}
			if r.Hidden {
				ui.Text(c, "hidden").FontSize(11).TextColor(p.faint).SingleLine()
			}
		})
	case 4:
		ui.Row(c).Gap(8).AlignItems(ui.Center).Justify(ui.End).Children(func() {
			latencyBar(c, p, r, reqMaxMs)
			ui.Text(c, latencyLabel(r)).FontSize(11.5).Font(mono()).
				TextColor(p.muted).Width(48).TextAlign(ui.End).SingleLine()
		})
	case 5:
		if r.Status == 0 {
			ui.Spinner(c).Size(10, 10)
			return
		}
		statusText(c, p, r.Status)
	}
}

// clockOf is an exchange's start as the clock said it, "" when the daemon
// gave none that reads.
func clockOf(startedAt string) string {
	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return ""
	}
	return t.Local().Format("15:04:05")
}

// ---------------------------------------------------------------------------
// request detail

// detailTabs are the views of an exchange.
var detailTabs = []string{"Headers", "Body", "Rules"}

// bodyRaw shows the bodies as captured rather than formatted: one switch
// for both sides.
var bodyRaw bool

// requestDetail is the exchange the table chose. Its heading is the
// request line itself, set large, with what came back at its right; under
// it, what was sent and what came back stand side by side, as the two
// halves of one exchange.
func (a *app) requestDetail(c *ui.Context, p *palette, st store, rows []uiRequestRow) {
	id := a.chosenRequestID(rows)
	if id == "" {
		if len(rows) == 0 {
			return
		}
		emptyState(c, p, iconList, "Select a request",
			"Choose a line to read what it sent, what came back, and which rules sealed it.", "")
		return
	}
	var row uiRequestRow
	for _, r := range rows {
		if string(r.ID) == id {
			row = r
		}
	}
	d, errText := st.detail, st.detailErr
	ui.Column(c).Grow(1).MinHeight(0).BorderWidth(1, 0, 0, 0).BorderColor(p.border).Children(func() {
		ui.Row(c).FillWidth().Shrink(0).Padding(14, 16, 4, 24).Gap(16).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Gap(3).Shrink(1).MinWidth(0).Children(func() {
				ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
					ui.Text(c, methodLabel(row.Method)).FontSize(14).FontWeight(650).Font(mono()).TextColor(p.text).SingleLine().Shrink(0)
					ui.Text(c, pathOf(row.URL)).FontSize(14).FontWeight(500).Font(mono()).TextColor(p.text).SingleLine().Shrink(1).MinWidth(40)
				})
				ui.Text(c, hostOf(row.URL)+"  ·  "+id).FontSize(11.5).Font(mono()).TextColor(p.faint).SingleLine()
			})
			ui.Spacer(c)
			if row.Status != 0 {
				ui.Text(c, latencyLabel(row)).FontSize(12).Font(mono()).TextColor(p.muted).SingleLine().Shrink(0)
				// The status animates as it changes, as when the
				// exchange completes or a replay lands.
				ui.Text(c, fmt.Sprint(row.Status)).FontSize(20).FontWeight(650).Font(mono()).
					TextColor(statusTone(p, row.Status)).SingleLine().Shrink(0).
					ContentTransition(ui.ContentTransition{Numeric: true})
			}
			a.replayButton(c, p, id)
		})
		ui.Row(c).FillWidth().Shrink(0).Padding(0, 16, 0, 24).AlignItems(ui.Center).Children(func() {
			a.words(c, "detail-tabs", &a.detailTab, detailTabs...)
			ui.Spacer(c)
			if a.detailTab == 1 {
				label := "Formatted"
				if bodyRaw {
					label = "Raw"
				}
				a.ghostButton(c, "body-raw", label, iconSwitch).Clicked(func() { bodyRaw = !bodyRaw })
			}
		})
		hairline(c, p)
		ui.Scroll(c).Grow(1).MinHeight(0).Children(func() {
			// Keyed by the request and the tab, so the content fades in
			// again as either changes.
			tabFade(ui.Column(c.Key(fmt.Sprintf("detail:content:%s:%d", id, a.detailTab))).
				FillWidth().Padding(16, 24, 24)).Children(func() {
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
	})
}

// sides lays the two halves of an exchange side by side under their
// names, a hairline between them.
func sides(c *ui.Context, p *palette, request, response func()) {
	side := func(title string, body func()) {
		ui.Column(c).Grow(1).Basis(0).MinWidth(0).Gap(8).Children(func() {
			caption(c, p, title)
			body()
		})
	}
	ui.Row(c).FillWidth().AlignItems(ui.Stretch).Children(func() {
		side("Request", request)
		ui.Box(c).Width(1).Shrink(0).Margin(0, 24).Background(p.border)
		side("Response", response)
	})
}

// tabFade is how the detail's content comes in as its tab, or its request,
// changes: a fade, without moving, as Mayoi's pages do.
func tabFade(e ui.Element) ui.Element {
	return e.Opacity(e.Keyframes("tab", []ui.Keyframe{{Value: 0}, {At: 150 * time.Millisecond, Value: 1, Ease: ui.EaseOut}}))
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

// detailSkeleton is what the detail shows while its exchange loads: quiet
// bars where the two sides will be.
func (a *app) detailSkeleton(c *ui.Context, p *palette) {
	bars := func() {
		for _, w := range []float32{220, 160, 250, 120} {
			ui.Box(c).Width(w).Height(9).Radius(4.5).Margin(4, 0).Background(p.text.Alpha(0.05))
		}
	}
	sides(c, p, bars, bars)
}

// detailBody is what the detail's tabs show of one exchange.
func (a *app) detailBody(c *ui.Context, p *palette, d *api.RequestDetailResponse) {
	switch a.detailTab {
	case 1:
		sides(c, p,
			func() {
				a.bodyPane(c, p, "request", "Request body", d.Body, d.Request.RequestHeaders, d.Request.RequestHeaderList)
			},
			func() {
				a.bodyPane(c, p, "response", "Response body", d.ResponseBody, d.Request.ResponseHeaders, d.Request.ResponseHeaderList)
			})
	case 2:
		a.ruleTraceSection(c, p, d.Request.RuleTrace)
	default:
		sides(c, p,
			func() { a.headerRows(c, p, d.Request.RequestHeaderList, d.Request.RequestHeaders) },
			func() { a.headerRows(c, p, d.Request.ResponseHeaderList, d.Request.ResponseHeaders) })
	}
}

// headerRows is one side's headers, list first, map as the fallback. The
// names keep one column, so the values line up to be read down; the name
// recedes and the value is what is read.
func (a *app) headerRows(c *ui.Context, p *palette, list []api.HeaderField, m api.Headers) {
	if len(list) == 0 && len(m) == 0 {
		ui.Text(c, "None").FontSize(12).TextColor(p.faint)
		return
	}
	ui.Column(c).FillWidth().Children(func() {
		draw := func(name, value string) {
			ui.Row(c).FillWidth().Gap(12).AlignItems(ui.Start).Padding(3, 0).Children(func() {
				ui.Text(c, name).FontSize(12).Font(mono()).TextColor(p.muted).SingleLine().Width(150).Shrink(0)
				ui.Text(c, value).FontSize(12).Font(mono()).TextColor(p.text).Shrink(1).MinWidth(40)
			})
		}
		for _, h := range list {
			for _, v := range h.Values {
				draw(h.Name, v)
			}
		}
		if len(list) == 0 {
			names := make([]string, 0, len(m))
			for name := range m {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				draw(name, m[name])
			}
		}
	})
}

// bodyView keeps the source and display buffers stable across frames.
type bodyView struct {
	source      string
	input       string
	contentType string
	formatted   string
	filename    string
	state       *editor.State
}

var bodyViews = map[string]*bodyView{}

// bodyPane is one side's body in a read-only editor, formatted unless the
// raw switch is on. Its buffer is kept across frames.
func (a *app) bodyPane(c *ui.Context, p *palette, key, title string, body *string, headers api.Headers, list []api.HeaderField) {
	if body == nil || *body == "" {
		ui.Text(c, "No body").FontSize(12).TextColor(p.faint)
		return
	}
	v := bodyViews[key]
	if v == nil {
		v = &bodyView{}
		bodyViews[key] = v
	}
	source := *body
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
	if bodyRaw {
		display = source
	}
	cacheKey := fmt.Sprintf("%t:%s:%s", bodyRaw, filename, display)
	if v.state == nil || v.source != cacheKey {
		buf := editor.NewBuffer(display)
		buf.SetLanguage(editor.LanguageFor(filename))
		v.state = &editor.State{Buffer: buf, ReadOnly: true, SoftWrap: true, NoScrollPastEnd: true, Label: title}
		v.source = cacheKey
	}
	editor.View(c.Key("body:"+key), v.state).FillWidth().Height(240).MinWidth(0).
		Radius(6).Border(1, p.border)
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

// ruleTraceSection is which rules ran on the exchange and what they did:
// each rule under its seal — solid if it matched, in outline if it was
// only asked — with its stages below as a pipeline down a rail.
func (a *app) ruleTraceSection(c *ui.Context, p *palette, traces []api.RuleTrace) {
	if len(traces) == 0 {
		ui.Text(c, "No rule ran on this exchange. It passed through untouched.").FontSize(12.5).TextColor(p.muted)
		return
	}
	rules := a.snapshotState().rules
	for i, tr := range traces {
		if i > 0 {
			hairline(c, p).Margin(12, 0)
		}
		ui.Row(c.Key(fmt.Sprintf("rt:%s:%s", tr.RuleID, tr.Phase))).FillWidth().Gap(10).AlignItems(ui.Center).Children(func() {
			seal(c, p, 16, tr.Matched)
			col := p.text
			if !tr.Matched {
				col = p.muted
			}
			ui.Text(c, ruleNameOf(rules, string(tr.RuleID))).FontSize(13).FontWeight(600).TextColor(col).SingleLine()
			if tr.Phase != "" {
				ui.Text(c, "on the "+tr.Phase).FontSize(12).TextColor(p.faint).SingleLine()
			}
			ui.Spacer(c)
			if tr.DurationMs > 0 {
				ui.Text(c, durationLabel(tr.DurationMs)).FontSize(11).Font(mono()).TextColor(p.faint).SingleLine()
			}
			if !tr.Matched {
				ui.Text(c, "did not match").FontSize(12).TextColor(p.faint).SingleLine()
			}
		})
		if tr.Error != "" {
			ui.Text(c, tr.Error).FontSize(12).Font(mono()).TextColor(p.removed).FillWidth().Margin(6, 0, 0, 26)
		}
		// The pipeline: a rail down from the seal, a mark per stage in the
		// color of how it went, then what the rule did at the end.
		node := func(key string, col ui.Color, body func()) {
			ui.Row(c.Key(key)).FillWidth().Gap(10).AlignItems(ui.Stretch).Children(func() {
				ui.Box(c).Width(16).Shrink(0).Children(func() {
					ui.Box(c).Absolute().Left(7.5).Top(0).Bottom(0).Width(1).Background(p.text.Alpha(0.14))
					ui.Box(c).Absolute().Left(2).Top(6).Size(12, 12).Background(p.editor)
					ui.Box(c).Absolute().Left(4.5).Top(8.5).Size(7, 7).Radius(3.5).Background(col)
				})
				ui.Column(c).Grow(1).MinWidth(0).Gap(2).Padding(4, 0, 6).Children(body)
			})
		}
		for i, stg := range tr.Stages {
			if stg.ID == "" && stg.Uses == "" {
				continue
			}
			col := p.added
			switch {
			case stg.Error != "":
				col = p.removed
			case stg.Skipped:
				col = p.faint.Alpha(0.6)
			}
			node(fmt.Sprintf("stage:%s:%s:%d", tr.RuleID, tr.Phase, i), col, func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					name := stg.ID
					if name == "" {
						name = stg.Uses
					}
					ui.Text(c, name).FontSize(12.5).Font(mono()).FontWeight(600).TextColor(p.text).SingleLine().Shrink(1).MinWidth(20)
					if stg.Uses != "" && stg.ID != "" {
						ui.Text(c, stg.Uses).FontSize(11.5).Font(mono()).TextColor(p.faint).SingleLine()
					}
					if stg.Skipped {
						tag(c, p, "skipped")
					}
				})
				switch {
				case stg.Error != "":
					ui.Text(c, stg.Error).FontSize(12).Font(mono()).TextColor(p.removed).FillWidth()
				case stg.Skipped && stg.SkipReason != "":
					ui.Text(c, skipLabel(stg.SkipReason)).FontSize(12).TextColor(p.faint).FillWidth()
				}
			})
		}
		for i, act := range tr.Actions {
			node(fmt.Sprintf("act:%s:%s:%d", tr.RuleID, tr.Phase, i), p.accent, func() {
				ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, string(act.Type)).FontSize(12).FontWeight(600).Font(mono()).TextColor(p.text).SingleLine().Shrink(0)
					ui.Text(c, actionParams(act)).FontSize(11.5).Font(mono()).TextColor(p.muted).SingleLine().Shrink(1).MinWidth(20)
				})
			})
		}
	}
}

// skipLabel says in words why a stage did not run.
func skipLabel(reason string) string {
	switch reason {
	case "if":
		return "its condition was false"
	case "unselected":
		return "another branch was selected"
	}
	return reason
}
