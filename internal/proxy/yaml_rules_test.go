package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
)

func TestYAMLSelectParallelAndRunThroughProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Login-Kind") != "" {
			t.Errorf("sso header on password path: %s", r.Header.Get("X-Login-Kind"))
		}
		io.WriteString(w, "upstream")
	}))
	defer upstream.Close()

	script := `
version: 1
id: decode-login
on:
  request:
    method: POST
    path: /v1/login
request:
  pipeline:
    - id: decoded
      run:
        runtime: goja
        script: |
          export default function(ctx) {
            return { match: true, outputs: { kind: "password", ok: true } };
          }
    - id: route
      select:
        cases:
          - name: password
            if: pipeline.decoded.outputs.kind == "password"
            pipeline:
              - uses: crabbro.tag
                with:
                  tags: ["password-login"]
          - name: sso
            if: pipeline.decoded.outputs.kind == "sso"
            pipeline:
              - uses: crabbro.set-request-header
                with:
                  name: X-Login-Kind
                  value: sso
        default:
          pipeline:
            - uses: crabbro.tag
              with:
                tags: ["other-login"]
    - parallel:
        timeoutMs: 300
        stages:
          - id: logged
            uses: crabbro.log
            with:
              message: decoded-login
          - uses: crabbro.metric
            with:
              name: login_decoded
`
	rs := rule.NewRuleStore()
	if _, err := rs.Create("decode", 10, script); err != nil {
		t.Fatal(err)
	}
	pxy := New(":0", ring.NewRequestBuffer(8), rs, rule.NewRuleEngine())
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	resp, err := client.Post(upstream.URL+"/v1/login", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream" {
		t.Fatalf("body = %q", body)
	}
	var tagged bool
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !tagged {
		for _, rec := range pxy.RingBuffer.List(0) {
			for _, tag := range rec.Tags {
				if tag == "password-login" {
					tagged = true
				}
			}
		}
		if !tagged {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !tagged {
		t.Fatal("password branch did not tag the exchange")
	}
}

func TestYAMLResponseRewrite(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "yes")
		io.WriteString(w, "hello secret")
	}))
	defer upstream.Close()
	script := `
version: 1
id: hide-role
on:
  request:
    path: /v1/profile
response:
  pipeline:
    - uses: crabbro.rewrite-response-body
      with:
        find: secret
        replace: public
    - uses: crabbro.set-response-header
      with:
        name: X-Crabbro
        value: role-rewritten
    - uses: crabbro.remove-response-header
      with:
        name: X-Upstream
`
	rs := rule.NewRuleStore()
	if _, err := rs.Create("hide", 10, script); err != nil {
		t.Fatal(err)
	}
	pxy := New(":0", ring.NewRequestBuffer(4), rs, rule.NewRuleEngine())
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	resp, err := client.Get(upstream.URL + "/v1/profile")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello public" {
		t.Fatalf("body = %q", body)
	}
	if resp.Header.Get("X-Crabbro") != "role-rewritten" || resp.Header.Get("X-Upstream") != "" {
		t.Fatalf("headers = %v", resp.Header)
	}
}
