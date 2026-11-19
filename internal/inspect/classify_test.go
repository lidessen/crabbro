package inspect

import (
	"net/http"
	"testing"
)

func TestEmptyFetchDestIsXHR(t *testing.T) {
	h := http.Header{}
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	got := Classify(h, "https://app.example.com/v1/checkout")
	if got.ResourceType != "xhr" {
		t.Fatalf("type = %s", got.ResourceType)
	}
}

func TestMissingFetchDestIsUnknown(t *testing.T) {
	got := Classify(http.Header{}, "https://app.example.com/v1/checkout")
	if got.ResourceType != "unknown" {
		t.Fatalf("type = %s", got.ResourceType)
	}
}

func TestPageFromRefererAndExtensionInitiator(t *testing.T) {
	h := http.Header{}
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Referer", "https://app.example.com/dashboard")
	h.Set("X-Crabbro-Initiator", "https://app.example.com/static/app.js")
	got := Classify(h, "https://app.example.com/v1/checkout")
	if got.Origin != "https://app.example.com" || got.PageURL != "https://app.example.com/dashboard" {
		t.Fatalf("page = %+v", got)
	}
	if got.InitiatorURL != "https://app.example.com/static/app.js" || got.InitiatorConfidence != "extension" {
		t.Fatalf("initiator = %+v", got)
	}
	StripStamps(h)
	if h.Get("X-Crabbro-Initiator") != "" {
		t.Fatal("stamp was not stripped")
	}
}

func TestScopePickAndOmit(t *testing.T) {
	var sc Scope
	if err := sc.ReplacePick([]string{"app:Chrome"}); err != nil {
		t.Fatal(err)
	}
	if err := sc.AddOmit([]string{"type:image", "type:font"}); err != nil {
		t.Fatal(err)
	}
	target := Subject{App: "Google Chrome", ResourceType: "xhr", Host: "app.example.com", Path: "/v1/checkout"}
	if v := Decide(target, &sc, DurableSnapshot{}); v.Hidden || !v.InScope {
		t.Fatalf("target = %+v", v)
	}
	image := Subject{App: "Google Chrome", ResourceType: "image", Host: "app.example.com"}
	v := Decide(image, &sc, DurableSnapshot{})
	if !v.Hidden || v.Reasons[0] != "session-omit" {
		t.Fatalf("image = %+v", v)
	}
	other := Subject{App: "Spotify", ResourceType: "xhr"}
	v = Decide(other, &sc, DurableSnapshot{})
	if !v.Hidden {
		t.Fatal("expected pick to hide other apps")
	}
	if err := sc.ReplacePick([]string{"not a token"}); err == nil {
		t.Fatal("expected invalid expression")
	}
}

func TestDurableDenyDoesNotDropBypassConfusion(t *testing.T) {
	d := DurableSnapshot{Deny: []Expr{{Key: "host", Value: "*.analytics.example"}}}
	v := Decide(Subject{Host: "stats.analytics.example", ResourceType: "script", App: "Chrome"}, nil, d)
	if !v.Hidden || v.Bypass {
		t.Fatalf("deny = %+v", v)
	}
	d.Bypass = []Expr{{Key: "host", Value: "updates.example"}}
	v = Decide(Subject{Host: "updates.example", App: "Updater"}, nil, d)
	if !v.Bypass || v.Hidden {
		t.Fatalf("bypass = %+v", v)
	}
}

func TestEmptyAllowDoesNotNarrow(t *testing.T) {
	v := Decide(Subject{Host: "app.example.com", ResourceType: "xhr"}, nil, DurableSnapshot{})
	if !v.InScope {
		t.Fatalf("empty allow narrowed: %+v", v)
	}
	d := DurableSnapshot{Allow: []Expr{{Key: "host", Value: "app.example.com"}}, AllowRestricts: true}
	v = Decide(Subject{Host: "other.example", ResourceType: "xhr"}, nil, d)
	if !v.Hidden {
		t.Fatal("non-empty allow should hide other hosts")
	}
}
