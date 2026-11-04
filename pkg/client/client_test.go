package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	daemon "github.com/lidessen/chiling/internal/api"
	"github.com/lidessen/chiling/internal/fixture"
	"github.com/lidessen/chiling/internal/inspect"
	"github.com/lidessen/chiling/internal/proxy"
	"github.com/lidessen/chiling/internal/ring"
	"github.com/lidessen/chiling/internal/rule"
	"github.com/lidessen/chiling/internal/session"
	"github.com/lidessen/chiling/internal/shared"
	"github.com/lidessen/chiling/pkg/api"
)

func TestPackagesDoNotImportInternal(t *testing.T) {
	for _, pkg := range []string{
		"github.com/lidessen/chiling/pkg/api",
		"github.com/lidessen/chiling/pkg/client",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", pkg, err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "github.com/lidessen/chiling/internal") || strings.Contains(line, "mygo") {
				t.Fatalf("%s depends on %s", pkg, line)
			}
		}
	}
}

func testDaemon(t *testing.T) (*daemon.Server, *proxy.EventHub, *ring.RequestBuffer, *httptest.Server) {
	t.Helper()
	rb := ring.NewRequestBuffer(32)
	hub := proxy.NewEventHub()
	cache := proxy.NewBodyCache(1<<20, 1<<20)
	rules := rule.NewRuleStore()
	engine := rule.NewRuleEngine()
	fixtures, err := fixture.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := daemon.New("", "", rb, cache, hub, fixtures, rules, engine, "127.0.0.1:0", nil)
	srv.Sessions = session.New(128)
	srv.Sessions.SetObserver(func(a shared.Activity) {
		hub.Emit(&shared.Event{
			Type:      shared.EventActivity,
			RequestID: shared.RequestID(a.RequestID),
			Timestamp: a.At,
			Payload:   a,
		})
	})
	srv.Inspect = inspect.NewRuntime(t.TempDir())
	srv.Inspect.Scopes = srv.Sessions
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return srv, hub, rb, ts
}

func TestClientAgainstDaemon(t *testing.T) {
	srv, _, rb, ts := testDaemon(t)
	c := New(Options{BaseURL: ts.URL, Timeout: 2 * time.Second})
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Version == "" || st.ProxyAddr == "" {
		t.Fatalf("status = %+v", st)
	}

	kept := rb.Add("GET", "https", "app.example", "/login", "", "https://app.example/login", "127.0.0.1:1")
	kept.RequestHeaders = shared.Headers{"Accept": "application/json"}
	ref := srv.BodyCache.Store([]byte(`{"ok":true}`))
	kept.RequestBodyRef = ref
	noise := rb.Add("GET", "https", "cdn.example", "/pixel", "", "https://cdn.example/pixel", "127.0.0.1:1")
	noise.Hidden = true
	noise.HiddenReason = []string{"session-pick"}

	list, err := c.ListRequests(ctx, RequestQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if list.HiddenCount != 1 || len(list.Requests) != 1 || list.Requests[0].ID != kept.ID {
		t.Fatalf("list = %+v", list)
	}
	found, err := c.SearchRequests(ctx, "login", RequestQuery{})
	if err != nil || len(found.Requests) != 1 {
		t.Fatalf("search %v %+v", err, found)
	}
	detail, err := c.GetRequest(ctx, string(kept.ID), Include{Headers: true, Body: true, RuleTrace: true})
	if err != nil {
		t.Fatal(err)
	}
	if detail.Body == nil || *detail.Body != `{"ok":true}` {
		t.Fatalf("body = %v", detail.Body)
	}
	if detail.Headers == nil || (*detail.Headers)["Accept"] != "application/json" {
		t.Fatalf("headers = %v", detail.Headers)
	}
	if _, err := c.GetRequest(ctx, "req_missing", Include{}); err == nil {
		t.Fatal("expected missing request")
	}

	sources, err := c.ListSources(ctx)
	if err != nil || len(sources.Sources) != 0 {
		t.Fatalf("sources %v %+v", err, sources)
	}
	hist, err := c.History(ctx)
	if err != nil || hist.Enabled {
		t.Fatalf("history %v %+v", err, hist)
	}
	if _, err := c.CAStatus(ctx); err == nil {
		t.Fatal("expected MITM status to be absent")
	}
	if _, err := c.Replay(ctx, api.VerifyRequest{RequestID: string(kept.ID)}); err == nil {
		t.Fatal("expected replay to be absent")
	}

	scope, err := c.ScopePick(ctx, []string{"host:app.example"})
	if err != nil {
		t.Fatal(err)
	}
	if !scope.ImplicitSession || len(scope.Pick) != 1 || scope.Pick[0] != "host:app.example" {
		t.Fatalf("scope = %+v", scope)
	}
	off, err := c.ScopeOff(ctx, []string{"type:image"})
	if err != nil || len(off.Omit) != 1 {
		t.Fatalf("off %v %+v", err, off)
	}
	reset, err := c.ScopeReset(ctx)
	if err != nil || len(reset.Pick) != 0 || len(reset.Omit) != 0 {
		t.Fatalf("reset %v %+v", err, reset)
	}

	first, err := c.Activity(ctx, "", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.NextCursor == "" || first.Items[0].Kind != "session_started" {
		t.Fatalf("first page = %+v", first)
	}
	second, err := c.Activity(ctx, scope.SessionID, first.NextCursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, item := range second.Items {
		kinds[item.Kind] = true
		if item.SessionID != scope.SessionID {
			t.Fatalf("session = %s", item.SessionID)
		}
	}
	for _, kind := range []string{"scope_picked", "scope_omitted", "scope_reset"} {
		if !kinds[kind] {
			t.Fatalf("missing %s in %+v", kind, second.Items)
		}
	}
	if _, err := c.Activity(ctx, "", "bad", 1); err == nil {
		t.Fatal("expected bad cursor")
	}

	cur, err := c.CurrentSession(ctx)
	if err != nil || cur == nil || !cur.Recording {
		t.Fatalf("current %v %+v", err, cur)
	}
	stopped, err := c.StopRecording(ctx, "")
	if err != nil || stopped.Recording {
		t.Fatalf("stop %v %+v", err, stopped)
	}
	again, err := c.StartRecording(ctx, "manual")
	if err != nil || again.Name != "manual" {
		t.Fatalf("start %v %+v", err, again)
	}

	created, err := srv.RuleStore.Create("demo", 10, "function onRequest(){return {actions:[]}}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RuleStore.SetEnabled(created.ID, false); err != nil {
		t.Fatal(err)
	}
	enabled, err := c.EnableRule(ctx, string(created.ID))
	if err != nil || !enabled.Enabled {
		t.Fatalf("enable %v %+v", err, enabled)
	}
	disabled, err := c.DisableRule(ctx, string(created.ID))
	if err != nil || disabled.Enabled {
		t.Fatalf("disable %v %+v", err, disabled)
	}
	rules, err := c.ListRules(ctx)
	if err != nil || len(rules) != 1 {
		t.Fatalf("rules %v %+v", err, rules)
	}
	got, err := c.GetRule(ctx, string(created.ID))
	if err != nil || got.Name != "demo" {
		t.Fatalf("get rule %v %+v", err, got)
	}
	preview, err := c.PreviewRules(ctx, "GET", "app.example", "/login")
	if err != nil || !preview.Complete {
		t.Fatalf("preview %v %+v", err, preview)
	}

	if _, err := c.ListPlugins(ctx); err == nil {
		t.Fatal("expected plugins to be unconfigured")
	}
}

func TestEnsureDaemonWhenAlreadyUp(t *testing.T) {
	_, _, _, ts := testDaemon(t)
	c := New(Options{BaseURL: ts.URL, Timeout: 2 * time.Second})
	if err := c.EnsureDaemon(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEventStreamResume(t *testing.T) {
	_, hub, _, ts := testDaemon(t)
	var mu sync.Mutex
	var uris []string
	var once sync.Once
	base := ts.Client().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		uris = append(uris, r.URL.RequestURI())
		mu.Unlock()
		resp, err := base.RoundTrip(r)
		if err != nil {
			return nil, err
		}
		if r.URL.Path == "/v1/events" {
			once.Do(func() {
				resp.Body = &cutReader{r: resp.Body}
			})
		}
		return resp, nil
	})
	c := New(Options{BaseURL: ts.URL, Transport: rt, Timeout: 2 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.Subscribe(ctx, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	waitSubscribers(t, hub, 1)
	hub.Emit(&shared.Event{Type: shared.EventRequestStarted, RequestID: "req_1", Timestamp: time.Now().UTC()})
	first := readEvent(t, ch)
	if first.ID == 0 || first.Type != api.EventRequestStarted {
		t.Fatalf("first = %+v", first)
	}
	waitSubscribers(t, hub, 0)
	hub.Emit(&shared.Event{Type: shared.EventRuleApplied, RequestID: "req_1", Timestamp: time.Now().UTC(), Payload: map[string]any{"ruleId": "rule_1"}})
	second := readEvent(t, ch)
	if second.ID <= first.ID || second.Type != api.EventRuleApplied {
		t.Fatalf("second = %+v after %d", second, first.ID)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(uris) < 2 || !strings.Contains(uris[1], "after=") {
		t.Fatalf("requests = %v", uris)
	}
}

func waitSubscribers(t *testing.T, hub *proxy.EventHub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.Len() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("subscribers = %d, want %d", hub.Len(), n)
}

func readEvent(t *testing.T, ch <-chan api.Event) api.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event")
		return api.Event{}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type cutReader struct {
	r   io.ReadCloser
	buf bytes.Buffer
	cut bool
}

func (c *cutReader) Read(p []byte) (int, error) {
	if c.cut {
		return 0, io.EOF
	}
	n, err := c.r.Read(p)
	if n > 0 && bytes.Contains(append(c.buf.Bytes(), p[:n]...), []byte("\n\n")) {
		c.cut = true
	}
	if n > 0 {
		c.buf.Write(p[:n])
	}
	return n, err
}

func (c *cutReader) Close() error { return c.r.Close() }
