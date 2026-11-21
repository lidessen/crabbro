package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"github.com/lidessen/crabbro/pkg/api"
	"github.com/lidessen/crabbro/pkg/client"
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

// gwFetch says how fresh one gateway card's data is. The sync goroutine
// refetches the five gateway endpoints together, but each answers on its
// own, so a card whose fetch failed keeps its last-good data as stale
// beside a card whose fetch answered.
type gwFetch int

const (
	// gwUnknown: no successful answer yet. Nothing may be asserted — not
	// even that the gateway is off, which a zero value would read as.
	gwUnknown gwFetch = iota
	// gwFresh: the latest fetch answered.
	gwFresh
	// gwStale: the latest fetch failed; the last-good answer is kept and
	// shown as stale, never as current.
	gwStale
)

// gwAnswers is one round of the five gateway endpoint answers, each
// paired with whether that endpoint answered at all. usageWindow is the
// range that round asked usage for: without it the response's own window
// is all that says which range the numbers describe.
type gwAnswers struct {
	status      api.GatewayStatusResponse
	statusOK    bool
	plans       []api.GatewayPlanStatus
	plansOK     bool
	routes      []api.GatewayRouteStatus
	routesOK    bool
	usage       api.GatewayUsageResponse
	usageOK     bool
	usageWindow string
	secrets     []api.SecretSummary
	secretsOK   bool
}

// gwNext moves one card's fetch state across a round: an answer is fresh,
// a failed round keeps the last-good answer as stale, and only a card that
// has never answered stays unknown.
func gwNext(prev gwFetch, ok bool) gwFetch {
	if ok {
		return gwFresh
	}
	if prev == gwUnknown {
		return gwUnknown
	}
	return gwStale
}

// applyGateway writes one round of gateway answers into the store. A
// failed endpoint keeps its last-good data, marked stale, so a kept answer
// never passes for a current one, and an endpoint that has never answered
// leaves nothing to assert.
func (st *store) applyGateway(ans gwAnswers) {
	st.gwStatusFetch = gwNext(st.gwStatusFetch, ans.statusOK)
	if ans.statusOK {
		st.gwStatus = ans.status
	}
	st.gwPlansFetch = gwNext(st.gwPlansFetch, ans.plansOK)
	if ans.plansOK {
		st.gwPlans = ans.plans
	}
	st.gwRoutesFetch = gwNext(st.gwRoutesFetch, ans.routesOK)
	if ans.routesOK {
		st.gwRoutes = ans.routes
	}
	st.gwUsageFetch = gwNext(st.gwUsageFetch, ans.usageOK)
	if ans.usageOK {
		st.gwUsage = ans.usage
		// Keep the range that was asked for beside the answer: the selected
		// range may move on before this answer, and a stale answer keeps the
		// range it was fetched for, not the range asked for now.
		st.gwUsageAsked = ans.usageWindow
	}
	st.gwSecretsFetch = gwNext(st.gwSecretsFetch, ans.secretsOK)
	if ans.secretsOK {
		st.gwSecrets = ans.secrets
	}
}

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
	traceReqs       []api.RequestSummary // every exchange, oldest first, for the trace
	rules           []api.Rule
	scope           api.ScopeState
	hasScope        bool
	session         *api.Session
	traj            []api.Activity // newest first
	mitm            api.MITMStatusResponse
	// mitmKnown is false while the daemon has no MITM to report on: one
	// started without its flags answers 404 for the status.
	mitmKnown bool
	history   api.HistoryStatus
	lastEvent int64

	gwStatus      api.GatewayStatusResponse
	gwStatusFetch gwFetch
	gwPlans       []api.GatewayPlanStatus
	gwPlansFetch  gwFetch
	gwRoutes      []api.GatewayRouteStatus
	gwRoutesFetch gwFetch
	gwUsage       api.GatewayUsageResponse
	gwUsageFetch  gwFetch
	// gwUsageAsked is the range the kept usage answer was requested with,
	// "" before any answer. The view reads the selected range itself; the
	// two together say whether these numbers cover that range.
	gwUsageAsked   string
	gwSecrets      []api.SecretSummary
	gwSecretsFetch gwFetch
}

// app is the state of the Crabbro window: the client of the daemon, the
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
	ruleEditMode     int // 0 overview, 1 code
	scriptDraft      string
	section          int // the daemon pane's segmented choice
	sessionName      string
	reqKindIdx       int // 0: all, 1: proxy, 2: gateway
	gwUsageWinIdx    int // 0: 1h, 1: 24h, 2: 7d
	gwSecretName     string
	gwSecretVal      string

	// mirror* are the view thread's search, hidden switch and page as of the
	// last frame, under mu: the sync goroutine reads these, never the fields
	// the widgets write.
	mirrorSearch   string
	mirrorHidden   bool
	mirrorPage     string
	mirrorKind     string
	mirrorUsageWin string
	trajAt         time.Time
	notes          []string
	busy           string // what an action is doing, shown in the status bar
	win            *mygo.Window
	pal            *palette // the frame's look, set as each frame begins
}

var hoverFade = ui.ElementTransition{Colors: true, Duration: 150 * time.Millisecond}

func newApp(c *client.Client, ctx context.Context) *app {
	a := &app{
		c:              c,
		ctx:            ctx,
		page:           pageTrajectory,
		gwUsageWinIdx:  1,
		mirrorUsageWin: "24h",
	}
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

// mirrorView copies the view thread's own state where the sync goroutine
// can read it under the lock. Called once a frame.
func (a *app) mirrorView() {
	a.mu.Lock()
	a.mirrorSearch, a.mirrorHidden, a.mirrorPage = a.search, a.showHidden, a.page
	a.mirrorKind = kindForIndex(a.reqKindIdx)
	a.mirrorUsageWin = windowForIndex(a.gwUsageWinIdx)
	a.mu.Unlock()
}

func kindForIndex(idx int) string {
	switch idx {
	case 1:
		return "proxy"
	case 2:
		return "gateway"
	default:
		return ""
	}
}

func windowForIndex(idx int) string {
	switch idx {
	case 0:
		return "1h"
	case 2:
		return "7d"
	default:
		return "24h"
	}
}

func (a *app) currentKind() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mirrorKind
}

func (a *app) currentUsageWindow() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mirrorUsageWin == "" {
		return "24h"
	}
	return a.mirrorUsageWin
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
	return a.mirrorSearch
}

// run keeps the store fresh until the process ends: ensure the daemon,
// take a snapshot, follow the event stream, and start over when the
// stream drops. It is the only writer of the store outside the view's
// own queue.
func (a *app) run(ctx context.Context) {
	// The window starts the daemon when it opens, once. After that a daemon
	// that went away stays away until the person starts it (the button, or
	// crabbro daemon start): respawning it here would undo a deliberate
	// crabbro daemon stop.
	first := true
	for ctx.Err() == nil {
		a.setConn(connConnecting, "")
		if first {
			first = false
			if err := a.c.EnsureDaemon(ctx); err != nil {
				a.setConn(connDown, err.Error())
				if !sleepCtx(ctx, 3*time.Second) {
					return
				}
				continue
			}
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
	return a.mirrorHidden
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
	if query == "" {
		query = currentQuery(a)
	}
	reqs, reqErr := a.c.SearchRequests(ctx, query, client.RequestQuery{Limit: requestListLimit, All: a.showHiddenNow(), Kind: a.currentKind()})
	traceReqs, _ := a.traceRequests(ctx, query, reqs, reqErr)
	rules, _ := a.c.ListRules(ctx)
	mitm, mitmErr := a.c.CAStatus(ctx)
	hist, _ := a.c.History(ctx)
	traj, _ := a.c.Activity(ctx, "", "", trajFetch)
	gwSt, gwStErr := a.c.GatewayStatus(ctx)
	gwPlans, gwPlansErr := a.c.ListGatewayPlans(ctx)
	gwRoutes, gwRoutesErr := a.c.ListGatewayRoutes(ctx)
	// Take the range once, and keep it with the answer: reading it again
	// after the fetch could pair one range's numbers with another's label.
	usageWindow := a.currentUsageWindow()
	gwUsage, gwUsageErr := a.c.GatewayUsage(ctx, usageWindow, "")
	gwSecrets, gwSecretsErr := a.c.ListSecrets(ctx)

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
	a.st.mitm, a.st.mitmKnown = mitm, mitmErr == nil
	a.st.history = hist
	a.st.traj = reverseActivity(traj.Items)
	a.st.traceReqs = traceReqs
	a.st.applyGateway(gwAnswers{
		status:      gwSt,
		statusOK:    gwStErr == nil,
		plans:       gwPlans,
		plansOK:     gwPlansErr == nil,
		routes:      gwRoutes,
		routesOK:    gwRoutesErr == nil,
		usage:       gwUsage,
		usageOK:     gwUsageErr == nil,
		usageWindow: usageWindow,
		secrets:     gwSecrets,
		secretsOK:   gwSecretsErr == nil,
	})
	a.mu.Unlock()
	a.invalidate()
	return nil
}

// traceRequests is the exchanges the trace joins its capture steps to. It
// is the unfiltered list: the table's search and its hidden switch narrow
// what the table shows, not what the trace can describe. When the table's
// own fetch already is that list, it is reused.
func (a *app) traceRequests(ctx context.Context, query string, table api.ListRequestsResponse, tableErr error) ([]api.RequestSummary, error) {
	if tableErr == nil && query == "" && a.showHiddenNow() {
		return table.Requests, nil
	}
	all, err := a.c.ListRequests(ctx, client.RequestQuery{Limit: requestListLimit, All: true})
	return all.Requests, err
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
	return a.mirrorPage
}

// follow applies events as they come, refetching on request changes, and
// returns when the stream drops or the context ends.
func (a *app) follow(ctx context.Context, events <-chan api.Event) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	ticks := 0
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
			// Events drive the lists; the tick is the safety net, and the
			// uptime on the daemon page. Off that page it runs every third.
			ticks++
			pg := a.currentPage()
			if pg == pageDaemon || pg == pageGateway || ticks%3 == 0 {
				a.refreshNow(ctx)
			}
		case <-ctx.Done():
			return
		}
	}
}

// refreshNow refetches the light state: the list, the rules, the session,
// and the daemon status. The trajectory only grows by events and the
// snapshot; the detail follows its own request id.
func (a *app) refreshNow(ctx context.Context) {
	query := currentQuery(a)

	status, err := a.c.Status(ctx)
	reqs, reqErr := a.c.SearchRequests(ctx, query, client.RequestQuery{Limit: requestListLimit, All: a.showHiddenNow(), Kind: a.currentKind()})
	rules, rulesErr := a.c.ListRules(ctx)
	sess, sessErr := a.c.CurrentSession(ctx)
	scope, hasScope := a.scopeIfNeeded(ctx, sess)
	mitm, mitmErr := a.c.CAStatus(ctx)
	traceReqs, traceErr := a.traceRequests(ctx, query, reqs, reqErr)
	gwSt, gwStErr := a.c.GatewayStatus(ctx)
	gwPlans, gwPlansErr := a.c.ListGatewayPlans(ctx)
	gwRoutes, gwRoutesErr := a.c.ListGatewayRoutes(ctx)
	// Take the range once, and keep it with the answer: reading it again
	// after the fetch could pair one range's numbers with another's label.
	usageWindow := a.currentUsageWindow()
	gwUsage, gwUsageErr := a.c.GatewayUsage(ctx, usageWindow, "")
	gwSecrets, gwSecretsErr := a.c.ListSecrets(ctx)

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
	// A failed fetch keeps what the screen had: an empty answer from a
	// hiccup would read as "no rules" or "not recording".
	if rulesErr == nil {
		a.st.rules = rules
	}
	if sessErr == nil {
		a.st.session = sess
		a.st.scope = scope
		a.st.hasScope = hasScope
	}
	a.st.mitmKnown = mitmErr == nil
	if mitmErr == nil {
		a.st.mitm = mitm
	}
	if traceErr == nil {
		a.st.traceReqs = traceReqs
	}
	a.st.applyGateway(gwAnswers{
		status:      gwSt,
		statusOK:    gwStErr == nil,
		plans:       gwPlans,
		plansOK:     gwPlansErr == nil,
		routes:      gwRoutes,
		routesOK:    gwRoutesErr == nil,
		usage:       gwUsage,
		usageOK:     gwUsageErr == nil,
		usageWindow: usageWindow,
		secrets:     gwSecrets,
		secretsOK:   gwSecretsErr == nil,
	})
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
	case api.EventGatewayConfigChanged, api.EventGatewayPlanHealthChanged:
		a.nudgeLater()
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
		name := a.sessionNameLocked()
		if name == "" {
			// An unnamed session would head the trace as just "Trace".
			name = "Recording " + time.Now().Format("15:04")
		}
		_, err := a.c.StartRecording(a.ctx, name)
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

// actSaveRule writes the script draft back to the rule. Only the script is
// sent: the priority and everything else stay as the daemon has them.
func (a *app) actSaveRule(id, script string) {
	a.setBusy("saving " + id)
	go func() {
		_, err := a.c.UpdateRule(a.ctx, id, api.UpdateRuleRequest{Script: &script})
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
			if replayingID == id {
				replayingID = ""
			}
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

func (a *app) actResetCooldown(planID string) {
	a.setBusy("clearing cooldown")
	go func() {
		err := a.c.ResetGatewayPlanCooldown(a.ctx, planID)
		a.post(func() {
			a.setBusy("")
			if err != nil {
				a.note("reset cooldown: %v", err)
			} else {
				a.note("cleared cooldown for %s", planID)
			}
			a.nudge("")
		})
	}()
}

func (a *app) actReloadGateway() {
	a.setBusy("reloading gateway")
	go func() {
		complete, diags, err := a.c.ReloadGateway(a.ctx)
		a.post(func() {
			a.setBusy("")
			switch {
			case err != nil:
				a.note("gateway reload: %v", err)
			case !complete && len(diags) > 0:
				a.note("gateway reloaded with %d diagnostic(s)", len(diags))
			default:
				a.note("gateway config reloaded")
			}
			a.nudge("")
		})
	}()
}

func (a *app) actSaveSecret(name, val string) {
	if name == "" || val == "" {
		a.note("secret name and value are required")
		return
	}
	a.setBusy("saving secret")
	go func() {
		err := a.c.SetSecret(a.ctx, name, val)
		a.post(func() {
			a.setBusy("")
			a.gwSecretVal = ""
			if err != nil {
				a.note("save secret: %v", err)
			} else {
				a.gwSecretName = ""
				a.note("saved secret %s", name)
			}
			a.nudge("")
		})
	}()
}

func (a *app) actDeleteSecret(name string) {
	a.setBusy("deleting secret")
	go func() {
		err := a.c.DeleteSecret(a.ctx, name)
		a.post(func() {
			a.setBusy("")
			if err != nil {
				a.note("delete secret: %v", err)
			} else {
				a.note("deleted secret %s", name)
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
