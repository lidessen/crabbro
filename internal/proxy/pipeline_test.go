package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/shared"
)

func TestPlainHTTPForwardsBodyBeyondCaptureLimit(t *testing.T) {
	const size = 80 * 1024
	payload := bytes.Repeat([]byte("abc"), size/3)
	var got []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credential was forwarded")
		}
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	rb := ring.NewRequestBuffer(16)
	pxy := New(":0", rb, nil, nil)
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	req, _ := http.NewRequest(http.MethodPost, upstream.URL+"/echo", bytes.NewReader(payload))
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("upstream got %d bytes, want %d", len(got), len(payload))
	}
	cookies := resp.Header.Values("Set-Cookie")
	if len(cookies) != 2 {
		t.Fatalf("Set-Cookie values = %v", cookies)
	}

	found := false
	for _, r := range rb.List(0) {
		if r.Method == http.MethodPost {
			found = true
			if !r.RequestBodyTruncated {
				t.Error("expected truncated capture")
			}
			if r.RequestBodyBytes != int64(len(payload)) {
				t.Errorf("forwarded byte count = %d, want %d", r.RequestBodyBytes, len(payload))
			}
			cached := pxy.DefaultSink().GetBody(r.RequestBodyRef)
			if len(cached) == 0 || len(cached) >= len(payload) {
				t.Fatalf("cached %d bytes, want a short preview", len(cached))
			}
			var cookieVals []string
			for _, h := range r.ResponseHeaderList {
				if h.Name == "Set-Cookie" {
					cookieVals = h.Values
				}
			}
			if len(cookieVals) != 2 {
				t.Fatalf("captured Set-Cookie = %v", cookieVals)
			}
		}
	}
	if !found {
		t.Fatal("request was not captured")
	}
}

func TestInvalidRuleAppliesNothing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("upstream-" + r.Header.Get("X-From-Rule")))
	}))
	defer upstream.Close()

	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	// A valid header action plus an invalid action must apply neither.
	if _, err := rs.Create("bad", 100, yamlRule("bad", `export default function(ctx) {
		return { match: true, actions: [
			{ type: "setRequestHeader", params: { name: "X-From-Rule", value: "yes" } },
			{ type: "nope", params: {} }
		] };
	}`)); err != nil {
		t.Fatal(err)
	}
	pxy := New(":0", ring.NewRequestBuffer(8), rs, re)
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	resp, err := client.Get(upstream.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream-" {
		t.Fatalf("body = %q, want untouched upstream", body)
	}
}

func TestRuleOnionSeesOuterPatch(t *testing.T) {
	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	outer := `
version: 1
id: outer
on:
  request: {}
request:
  pipeline:
    - id: stage
      uses: crabbro.set-request-header
      with:
        name: X-Stage
        value: outer
`
	if _, err := rs.Create("outer", 10, outer); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Create("inner", 20, yamlRule("inner", `export default function(ctx) {
		if (ctx.request.headers["X-Stage"] !== "outer") return { match: false };
		return { match: true, actions: [{ type: "mockResponse", params: { status: 201, body: "saw-outer" } }] };
	}`)); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	pxy := New(":0", ring.NewRequestBuffer(8), rs, re)
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	resp, err := client.Get(upstream.URL + "/chain")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 || string(body) != "saw-outer" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestAgentLoopRecordInspectRuleVerify(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/login" && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"token":"user-token"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("missing"))
	}))
	defer upstream.Close()

	rs := rule.NewRuleStore()
	re := rule.NewRuleEngine()
	pxy := New(":0", ring.NewRequestBuffer(32), rs, re)
	sess := pxy.DefaultSink().Sessions.Start("login-hunt")
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	resp, err := client.Post(upstream.URL+"/v1/login", "application/json", strings.NewReader(`{"user":"ada"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var loginID string
	for _, rec := range pxy.DefaultSink().List(0) {
		if strings.Contains(rec.URL, "/v1/login") {
			loginID = string(rec.ID)
			if rec.SessionID != sess.ID {
				t.Fatalf("session = %s, want %s", rec.SessionID, sess.ID)
			}
		}
	}
	if loginID == "" {
		t.Fatal("login request was not recorded")
	}
	// Prefix resolves when it is unique.
	if pxy.DefaultSink().Resolve(shared.RequestID(loginID)) == nil {
		t.Fatal("resolve failed")
	}

	captured := pxy.DefaultSink().RingBuffer.GetByID(shared.RequestID(loginID))
	script := rule.Scaffold(captured, "mock", `{"token":"mocked"}`, "application/json")
	if _, err := rs.Create("mock-login", 100, script); err != nil {
		t.Fatal(err)
	}

	result, err := pxy.Replay(loginID, 200, "mocked")
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("verify not ok: %#v", result)
	}
	if !strings.Contains(result.ResponseBody, "mocked") {
		t.Fatalf("body = %s", result.ResponseBody)
	}
	if len(result.MatchedRules) == 0 {
		t.Fatal("expected a matched rule")
	}
}

func TestEmptyAllowlistDeniesWhenPublished(t *testing.T) {
	pxy := New(":0", ring.NewRequestBuffer(4), nil, nil)
	pxy.PublishPolicy(NetworkPolicy{Enabled: true, EmptyAllowDenies: true})
	if pxy.policyForTunnel().ShouldIntercept("example.com:443") {
		t.Fatal("empty allowlist should deny interception")
	}
	pxy.PublishPolicy(NetworkPolicy{Enabled: true, EmptyAllowDenies: true, Allow: []string{"*.example.com"}})
	if !pxy.policyForTunnel().ShouldIntercept("api.example.com:443") {
		t.Fatal("wildcard allow should intercept")
	}
	if pxy.policyForTunnel().ShouldIntercept("evil.test:443") {
		t.Fatal("unlisted host should not be intercepted")
	}
}

func TestStreamingResponseFlushesBeforeEnd(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("first-chunk"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		<-release
		w.Write([]byte("-second"))
	}))
	defer upstream.Close()

	pxy := New(":0", ring.NewRequestBuffer(4), nil, nil)
	if err := pxy.Start(); err != nil {
		t.Fatal(err)
	}
	defer pxy.Stop()
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+pxy.Addr()))}}
	resp, err := client.Get(upstream.URL + "/slow")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, len("first-chunk"))
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk was not delivered before the origin finished")
	}
	if string(buf) != "first-chunk" {
		t.Fatalf("chunk = %q", buf)
	}
	close(release)
	rest, _ := io.ReadAll(resp.Body)
	if string(rest) != "-second" {
		t.Fatalf("rest = %q", rest)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// yamlRule wraps a JavaScript rule body as the single run stage of a YAML
// rule v1 document that matches every request. The script text is kept
// verbatim; each line is re-indented so the block scalar stays valid.
func yamlRule(id, js string) string {
	var b strings.Builder
	b.WriteString("version: 1\n")
	b.WriteString("id: " + id + "\n")
	b.WriteString("on:\n  request: {}\n")
	b.WriteString("request:\n  pipeline:\n")
	b.WriteString("    - id: r\n      run:\n        runtime: goja\n        script: |\n")
	for _, line := range strings.Split(js, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("          " + line + "\n")
	}
	return b.String()
}
