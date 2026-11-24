package main

import (
	"context"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/lidessen/crabbro/pkg/api"
)

func TestRequestSearchRefreshesAfterTypingStops(t *testing.T) {
	a := newApp(nil, context.Background())
	a.page, a.st.conn = pageRequests, connConnected
	tt := ui.NewTester(a.view, 860, 540)
	tt.Frame()
	if err := tt.Click("Search requests"); err != nil {
		t.Fatal(err)
	}
	tt.Type("orders")
	tt.Advance(299 * time.Millisecond)
	select {
	case <-a.refresh:
		t.Fatal("search refreshed before the debounce elapsed")
	default:
	}
	tt.Advance(time.Millisecond)
	select {
	case msg := <-a.refresh:
		if msg.query != "orders" || currentQuery(a) != "orders" {
			t.Fatalf("refresh did not carry the search text: %+v", msg)
		}
	default:
		t.Fatal("search did not refresh after typing stopped")
	}
}

func TestRequestInspectorAdaptsToWindowWidth(t *testing.T) {
	a := newApp(nil, context.Background())
	a.page, a.st = pageRequests, gatewayViewFixture(time.Now())
	a.reqShownID = string(a.st.detailID)
	tt := ui.NewTester(a.view, 1280, 820)
	check := func(beside bool) {
		t.Helper()
		tt.Frame()
		list, listOK := tt.Find("Time")
		tabs, tabsOK := tt.Find("Headers")
		request, requestOK := tt.Find("Request")
		response, responseOK := tt.Find("Response")
		if !listOK || !tabsOK || !requestOK || !responseOK {
			t.Fatal("request list or inspector sections missing")
		}
		if beside && (tabs.X < list.X+250 || response.Y <= request.Y) {
			t.Fatal("wide inspector should sit beside the table and stack the two sections")
		}
		if !beside && (tabs.Y < list.Y+100 || response.X <= request.X) {
			t.Fatal("narrow inspector should sit below the table and compare the two sections side by side")
		}
		if a.reqShownID != string(a.st.detailID) {
			t.Fatal("resizing changed the selected request")
		}
	}
	check(true)
	tt.SetSize(860, 540)
	check(false)
	tt.SetSize(1280, 820)
	check(true)
}

func TestRulesKeepDraftWhenListChanges(t *testing.T) {
	a := newApp(nil, context.Background())
	a.page, a.st.conn = pageRules, connConnected
	first := api.Rule{ID: "first", Name: "First rule", Priority: 10, Script: "version: 1\n"}
	second := api.Rule{ID: "second", Name: "Second rule", Priority: 20, Script: "version: 1\n"}
	a.st.rules = []api.Rule{first, second}
	tt := ui.NewTester(a.view, 860, 540)
	tt.Frame()
	if err := tt.Click("Second rule"); err != nil {
		t.Fatal(err)
	}
	if a.ruleShownID != string(second.ID) || a.scriptDraft != second.Script {
		t.Fatal("selecting a rule did not load its script")
	}
	draft := second.Script + "# draft\n"
	a.scriptDraft = draft
	a.st.rules = []api.Rule{second, first}
	tt.Frame()
	if a.ruleShownID != string(second.ID) || a.scriptDraft != draft {
		t.Fatal("refreshing the list replaced the selected rule or its draft")
	}
}
