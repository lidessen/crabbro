package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lidessen/chiling/internal/inspect"
	"github.com/lidessen/chiling/internal/ring"
	"github.com/lidessen/chiling/internal/rule"
)

func TestScopeHidesNoiseAndKeepsTheTarget(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Chiling-Initiator") != "" {
			t.Errorf("initiator stamp was forwarded")
		}
		io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	rs := rule.NewRuleStore()
	script := `
version: 1
id: always
on:
  request: {}
request:
  pipeline:
    - id: ran
      uses: chiling.tag
      with:
        tags: ["ran"]
`
	if _, err := rs.Create("always", 10, script); err != nil {
		t.Fatal(err)
	}
	pxy := New(":0", ring.NewRequestBuffer(16), rs, rule.NewRuleEngine())
	rt := inspect.NewRuntime(t.TempDir())
	rt.Scopes = pxy.Sessions
	pxy.Inspect = rt
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()

	_, scope, implicit := pxy.Sessions.EnsureScope()
	if !implicit {
		t.Fatal("expected an implicit recording session")
	}
	if err := scope.AddOmit([]string{"type:image", "type:font", "type:script"}); err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	u := upstream.URL
	send := func(path string, hdr map[string]string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, u+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	send("/logo.png", map[string]string{"Sec-Fetch-Dest": "image"})
	send("/font.woff", map[string]string{"Sec-Fetch-Dest": "font"})
	send("/app.js", map[string]string{"Sec-Fetch-Dest": "script"})
	send("/v1/checkout", map[string]string{
		"Sec-Fetch-Dest":      "empty",
		"Sec-Fetch-Mode":      "cors",
		"Referer":             u + "/pay",
		"X-Chiling-Initiator": "https://app.example/static/pay.js",
	})

	var target *struct {
		hidden bool
		typ    string
		init   string
		tags   []string
	}
	hidden := 0
	for _, rec := range pxy.RingBuffer.List(0) {
		if rec.Method == "CONNECT" {
			continue
		}
		if rec.Hidden {
			hidden++
			for _, tag := range rec.Tags {
				if tag == "ran" && rec.Attribution != nil && rec.Attribution.ResourceType == "image" {
					// rules still ran on a hidden exchange
				}
			}
			continue
		}
		if strings.Contains(rec.Path, "/v1/checkout") {
			target = &struct {
				hidden bool
				typ    string
				init   string
				tags   []string
			}{hidden: rec.Hidden, tags: rec.Tags}
			if rec.Attribution != nil {
				target.typ = rec.Attribution.ResourceType
				target.init = rec.Attribution.InitiatorURL
			}
		}
	}
	if target == nil || target.hidden || target.typ != "xhr" || target.init != "https://app.example/static/pay.js" {
		t.Fatalf("target = %+v hidden=%d", target, hidden)
	}
	tagged := false
	for _, tag := range target.tags {
		if tag == "ran" {
			tagged = true
		}
	}
	if !tagged {
		t.Fatal("scope hid the target from rules")
	}
	if hidden < 3 {
		t.Fatalf("hidden = %d, want image, font, and script", hidden)
	}
	var hiddenTagged bool
	for _, rec := range pxy.RingBuffer.List(0) {
		if rec.Hidden {
			for _, tag := range rec.Tags {
				if tag == "ran" {
					hiddenTagged = true
				}
			}
		}
	}
	if !hiddenTagged {
		t.Fatal("a hidden exchange did not run rules")
	}
}
