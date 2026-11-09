package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/lidessen/chiling/pkg/api"
	"github.com/lidessen/chiling/pkg/client"
)

// connState is how the app last saw the daemon.
type connState int

const (
	connConnecting connState = iota
	connConnected
	connDown
)

// refreshMsg asks the sync goroutine for new data. Query carries the search
// box's text, which only the view thread reads.
type refreshMsg struct {
	query string
}

// listRefreshDebounce folds a burst of events into one list refetch.
const listRefreshDebounce = 150 * time.Millisecond

// trajKeep is how many trajectory steps the feed keeps in memory.
const trajKeep = 400

// store is the state the view reads. The sync goroutine writes it under mu;
// the frame function reads it under mu. Neither holds it across a network
// call, and slices are replaced, never edited in place.
type store struct {
	conn            connState
	connErr         string
	status          api.DaemonStatus
	requests        []api.RequestSummary // newest first
	listHiddenCount int
	listScopeActive bool
	detail          *api.RequestDetailResponse
	detailID        string
	detailErr       string
	rules           []api.Rule
	scope           api.ScopeState
	hasScope        bool
	session         *api.Session
	traj            []api.Activity // newest first
	mitm            api.MITMStatusResponse
	history         api.HistoryStatus
	lastEvent       int64
}

// app is the state of the Chiling window: the client of the daemon, the
// store its goroutine keeps fresh, and the view's own state.
type app struct {
	c   *client.Client
	ctx context.Context

	mu    sync.Mutex
	st    store
	queue []func()
	sv    ui.Services
	hasSV bool

	// refresh asks the sync goroutine for a new snapshot; the query of the
	// search box rides along. It holds one pending ask at a time.
	refresh chan refreshMsg
	// refreshTimer folds event bursts before they ask.
	refreshPending bool

	// View state, read and written only on the main thread.
	page             string
	search           string
	searchSeen       string
	searchSent       string
	searchAt         time.Time
	showHidden       bool
	reqState         ui.ListState
	reqSel           int
	reqShownID       string
	pendingRequestID string
	detailTab        int
	ruleState        ui.ListState
	ruleSel          int
	ruleShownID      string
	ruleEditMode     int // 0 visual, 1 code
	scriptDraft      string
	section          int // the daemon pane's segmented choice
	sessionName      string
	trajAt           time.Time
	notes            []string
	busy             string // what an action is doing, shown in the status bar
	win              *mygo.Window
	pal              *palette // the frame's look, set as each frame begins
}

var hoverFade = ui.ElementTransition{Colors: true, Duration: 110 * time.Millisecond}

func newApp(c *client.Client, ctx context.Context) *app {
	a := &app{c: c, ctx: ctx, page: pageRequests}
	a.refresh = make(chan refreshMsg, 1)
	return a
}

// snapshotState copies what the frame draws. Slices keep their backing
// arrays, which the sync goroutine replaces rather than edits.
func (a *app) snapshotState() store {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st
}

// post runs fn on the main thread before its next frame. Safe from any
// goroutine.
func (a *app) post(fn func()) {
	a.mu.Lock()
	a.queue = append(a.queue, fn)
	sv := a.sv
	a.mu.Unlock()
	sv.Invalidate()
}

// drainQueue runs what was posted since the last frame. Main thread only.
func (a *app) drainQueue() {
	for {
		a.mu.Lock()
		q := a.queue
		a.queue = nil
		a.mu.Unlock()
		if len(q) == 0 {
			return
		}
		for _, fn := range q {
			fn()
		}
	}
}

func (a *app) setServices(sv ui.Services) {
	a.mu.Lock()
	a.sv, a.hasSV = sv, true
	a.mu.Unlock()
}

// invalidate draws a new frame. Safe from any goroutine.
func (a *app) invalidate() {
	a.mu.Lock()
	sv := a.sv
	a.mu.Unlock()
	sv.Invalidate()
}

// note queues a message for the next frame's toast.
func (a *app) note(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	a.post(func() {
		a.notes = append(a.notes, msg)
		a.invalidate()
	})
}

// nudge asks the sync goroutine for a new snapshot, folding repeats.
func (a *app) nudge(query string) {
	select {
	case a.refresh <- refreshMsg{query: query}:
	default:
	}
}

// nudgeLater asks for a snapshot after a short debounce, so a burst of
// events costs one refetch.
func (a *app) nudgeLater() {
	a.mu.Lock()
	if a.refreshPending {
		a.mu.Unlock()
		return
	}
	a.refreshPending = true
	a.mu.Unlock()
	time.AfterFunc(listRefreshDebounce, func() {
		a.mu.Lock()
		a.refreshPending = false
		a.mu.Unlock()
		a.nudge(currentQuery(a))
	})
}

// currentQuery reads the search box's text for a debounced refetch. The
// view thread owns the field; this runs on a timer, so it takes the lock.
func currentQuery(a *app) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.search
}

// run keeps the store fresh until the process ends: ensure the daemon,
// take a snapshot, follow the event stream, and start over when the
// stream drops. It is the only writer of the store outside the view's
// own queue.
func (a *app) run(ctx context.Context) {
	for ctx.Err() == nil {
		a.setConn(connConnecting, "")
		if err := a.c.EnsureDaemon(ctx); err != nil {
			a.setConn(connDown, err.Error())
			if !sleepCtx(ctx, 3*time.Second) {
				return
			}
			continue
		}
		if err := a.snapshot(ctx, ""); err != nil {
			a.setConn(connDown, err.Error())
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		a.setConn(connConnected, "")
		events, err := a.c.Subscribe(ctx, client.EventFilter{})
		if err != nil {
			a.setConn(connDown, err.Error())
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		a.follow(ctx, events)
	}
}

// setConn records how the daemon is and draws the change.
func (a *app) setConn(state connState, errStr string) {
	a.mu.Lock()
	a.st.conn = state
	a.st.connErr = errStr
	a.mu.Unlock()
	a.invalidate()
}

// showHiddenNow is the hidden-rows switch of the requests toolbar.
func (a *app) showHiddenNow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.showHidden
}

// snapshot fills the store from the list endpoints. It is the pattern the
// UI API doc prescribes: take what the screen shows, then follow events.
func (a *app) snapshot(ctx context.Context, query string) error {
	status, err := a.c.Status(ctx)
	if err != nil {
		return err
	}
	sess, _ := a.c.CurrentSession(ctx)
	scope, hasScope := a.scopeIfNeeded(ctx, sess)
	reqs, _ := a.c.SearchRequests(ctx, query, client.RequestQuery{Limit: 200, All: a.showHiddenNow()})
	rules, _ := a.c.ListRules(ctx)
	mitm, _ := a.c.CAStatus(ctx)
	hist, _ := a.c.History(ctx)
	traj, _ := a.c.Activity(ctx, "", "", 100)

	a.mu.Lock()
	a.st.status = status
	a.st.connErr = ""
	// Requests come oldest first; the table shows the newest at the top.
	a.st.requests = reverseRequests(reqs.Requests)
	a.st.listHiddenCount = reqs.HiddenCount
	a.st.listScopeActive = reqs.ScopeActive
	a.st.rules = rules
	a.st.scope = scope
	a.st.hasScope = hasScope
	a.st.session = sess
	a.st.mitm = mitm
	a.st.history = hist
	a.st.traj = reverseActivity(traj.Items)
	a.mu.Unlock()
	a.invalidate()
	return nil
}

// scopeIfNeeded fetches the scope when something can show it: the daemon
// page, or a session to classify. GET /v1/scope starts an implicit session
// when none is recording, so a passive window does not ask for it.
func (a *app) scopeIfNeeded(ctx context.Context, sess *api.Session) (api.ScopeState, bool) {
	if a.currentPage() == pageDaemon || sess != nil {
		sc, err := a.c.Scope(ctx)
		return sc, err == nil
	}
	return api.ScopeState{}, false
}

// currentPage is the page the sidebar shows.
func (a *app) currentPage() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.page
}

// follow applies events as they come, refetching on request changes, and
// returns when the stream drops or the context ends.
func (a *app) follow(ctx context.Context, events <-chan api.Event) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				a.setConn(connDown, "event stream dropped")
				return
			}
			a.applyEvent(ev)
		case <-a.refresh:
			a.refreshNow(ctx)
		case <-ticker.C:
			a.refreshNow(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// refreshNow refetches the light state: the list, the rules, the session,
// and the daemon status. The trajectory only grows by events and the
// snapshot; the detail follows its own request id.
func (a *app) refreshNow(ctx context.Context) {
	a.mu.Lock()
	query := a.search
	a.mu.Unlock()

	status, err := a.c.Status(ctx)
	reqs, reqErr := a.c.SearchRequests(ctx, query, client.RequestQuery{Limit: 200, All: a.showHiddenNow()})
	rules, _ := a.c.ListRules(ctx)
	sess, _ := a.c.CurrentSession(ctx)
	scope, hasScope := a.scopeIfNeeded(ctx, sess)
	mitm, _ := a.c.CAStatus(ctx)

	a.mu.Lock()
	if err == nil {
		a.st.status = status
		a.st.connErr = ""
	}
	if reqErr == nil {
		a.st.requests = reverseRequests(reqs.Requests)
		a.st.listHiddenCount = reqs.HiddenCount
		a.st.listScopeActive = reqs.ScopeActive
	}
	a.st.rules = rules
	a.st.session = sess
	a.st.scope = scope
	a.st.hasScope = hasScope
	a.st.mitm = mitm
	a.mu.Unlock()

	a.refreshDetail(ctx)
	a.invalidate()
}

// refreshDetail fetches the selected request's detail when it is stale.
func (a *app) refreshDetail(ctx context.Context) {
	a.mu.Lock()
	id := a.st.detailID
	a.mu.Unlock()
	if id == "" {
		return
	}
	d, err := a.c.GetRequest(ctx, id, client.Include{Headers: true, Body: true, RuleTrace: true})
	a.mu.Lock()
	if id == a.st.detailID {
		if err != nil {
			a.st.detail, a.st.detailErr = nil, err.Error()
		} else {
			a.st.detail, a.st.detailErr = &d, ""
		}
	}
	a.mu.Unlock()
	a.invalidate()
}

// applyEvent applies one event to the store, per the UI API doc: request
// events refetch their row, scope events replace the scope snapshot, and
// activity events append to the timeline.
func (a *app) applyEvent(ev api.Event) {
	if ev.ID > 0 {
		a.mu.Lock()
		a.st.lastEvent = ev.ID
		a.mu.Unlock()
	}
	switch ev.Type {
	case api.EventScopeChanged:
		var sc api.ScopeState
		if payloadJSON(ev.Payload, &sc) {
			a.mu.Lock()
			a.st.scope = sc
			a.st.hasScope = true
			a.mu.Unlock()
		}
		a.nudgeLater()
	case api.EventActivity:
		var act api.Activity
		if payloadJSON(ev.Payload, &act) {
			a.mu.Lock()
			if !hasActivity(a.st.traj, act.ID) {
				a.st.traj = append([]api.Activity{act}, a.st.traj...)
				if len(a.st.traj) > trajKeep {
					a.st.traj = a.st.traj[:trajKeep]
				}
			}
			a.mu.Unlock()
		}
		a.invalidate()
	case api.EventMITMEnabled, api.EventMITMDisabled:
		a.nudge("")
	default:
		// request_started … request_failed, rule_evaluated, rule_applied:
		// the list shows the row, the detail shows the exchange.
		if ev.RequestID != "" && a.currentDetailID() == string(ev.RequestID) {
			go a.refreshDetail(a.ctx)
		}
		a.nudgeLater()
	}
}

// currentDetailID is the request the detail pane shows, "" for none.
func (a *app) currentDetailID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st.detailID
}

// recordingNow says an explicit recording session is open. The daemon names
// the implicit session a scope command started "scope", which is how it
// keeps that apart from a recording window.
func (a *app) recordingNow() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.st.session
	return s != nil && s.Recording && s.Name != "scope"
}

// selectRequest makes id the detail's request, fetching what it shows.
func (a *app) selectRequest(id string) {
	a.mu.Lock()
	a.st.detailID = id
	a.st.detail = nil
	a.st.detailErr = ""
	a.mu.Unlock()
	if id == "" {
		a.invalidate()
		return
	}
	go func() {
		d, err := a.c.GetRequest(a.ctx, id, client.Include{Headers: true, Body: true, RuleTrace: true})
		a.mu.Lock()
		if string(a.st.detailID) == id {
			if err != nil {
				a.st.detail, a.st.detailErr = nil, err.Error()
			} else {
				a.st.detail, a.st.detailErr = &d, ""
			}
		}
		a.mu.Unlock()
		a.invalidate()
	}()
}

// The actions below run their client call on a goroutine and report back
// through the queue, so a slow daemon never blocks a frame.

func (a *app) actStartRecording() {
	a.setBusy("starting recording")
	go func() {
		_, err := a.c.StartRecording(a.ctx, a.sessionNameLocked())
		a.post(func() {
			a.setBusy("")
			if err != nil {
				a.note("record start: %v", err)
			}
			a.nudge("")
		})
	}()
}

func (a *app) actStopRecording() {
	a.setBusy("stopping recording")
	go func() {
		_, err := a.c.StopRecording(a.ctx, "")
		a.post(func() {
			a.setBusy("")
			if err != nil {
				a.note("record stop: %v", err)
			}
			a.nudge("")
		})
	}()
}

func (a *app) actToggleRule(id string, enable bool) {
	a.setBusy(map[bool]string{true: "enabling", false: "disabling"}[enable] + " " + id)
	go func() {
		var err error
		if enable {
			_, err = a.c.EnableRule(a.ctx, id)
		} else {
			_, err = a.c.DisableRule(a.ctx, id)
		}
		a.post(func() {
			a.setBusy("")
			if err != nil {
				a.note("rule toggle: %v", err)
			}
			a.nudge("")
		})
	}()
}

func (a *app) actDeleteRule(id string) {
	a.setBusy("deleting " + id)
	go func() {
		err := a.c.DeleteRule(a.ctx, id)
		a.post(func() {
			a.setBusy("")
			if err != nil {
				a.note("rule delete: %v", err)
			}
			a.nudge("")
		})
	}()
}

// actSaveRule writes the script draft and the form's priority back to the
// rule.
func (a *app) actSaveRule(id, script string, priority int) {
	a.setBusy("saving " + id)
	go func() {
		_, err := a.c.UpdateRule(a.ctx, id, api.UpdateRuleRequest{Script: &script, Priority: &priority})
		a.post(func() {
			a.setBusy("")
			if err != nil {
				a.note("rule save: %v", err)
			}
			a.nudge("")
		})
	}()
}

func (a *app) actReplay(id string) {
	a.setBusy("replaying " + id)
	go func() {
		res, err := a.c.Replay(a.ctx, api.VerifyRequest{RequestID: id})
		a.post(func() {
			a.setBusy("")
			switch {
			case err != nil:
				a.note("replay: %v", err)
			case res.OK:
				a.note("replay %s: ok (status %d)", id, res.Status)
			default:
				msg := "replay failed"
				if len(res.Errors) > 0 {
					msg = res.Errors[0]
				}
				a.note("replay %s: %s", id, msg)
			}
			a.nudge("")
		})
	}()
}

func (a *app) actScopeReset() {
	a.setBusy("resetting scope")
	go func() {
		_, err := a.c.ScopeReset(a.ctx)
		a.post(func() {
			a.setBusy("")
			if err != nil {
				a.note("scope reset: %v", err)
			}
			a.nudge("")
		})
	}()
}

func (a *app) actStartDaemon() {
	a.setBusy("starting daemon")
	go func() {
		err := a.c.EnsureDaemon(a.ctx)
		a.post(func() {
			a.setBusy("")
			if err != nil {
				a.note("daemon: %v", err)
			}
			a.nudge("")
		})
	}()
}

func (a *app) sessionNameLocked() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sessionName
}

func (a *app) setBusy(what string) {
	a.mu.Lock()
	a.busy = what
	a.mu.Unlock()
	a.invalidate()
}

// ---------------------------------------------------------------------------
// helpers

func payloadJSON(payload any, out any) bool {
	if payload == nil {
		return false
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, out) == nil
}

func hasActivity(items []api.Activity, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

// reverseRequests returns the list newest first.
func reverseRequests(in []api.RequestSummary) []api.RequestSummary {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.RequestSummary, len(in))
	for i, r := range in {
		out[len(in)-1-i] = r
	}
	return out
}

// reverseActivity returns the trajectory newest first.
func reverseActivity(in []api.Activity) []api.Activity {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.Activity, len(in))
	for i, r := range in {
		out[len(in)-1-i] = r
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// askQuestion shows the native dialog with OK first, Cancel second.
func (a *app) askQuestion(message, detail string) (bool, error) {
	res, err := mygo.Dialog.Message(mygo.MessageOptions{
		Parent:  a.win,
		Type:    mygo.MessageQuestion,
		Message: message,
		Detail:  detail,
		Buttons: []string{"Delete", "Cancel"},
	})
	return err == nil && res.Button == 0, err
}
