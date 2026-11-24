package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/rule"
	"github.com/lidessen/crabbro/internal/secret"
)

const offloadRule = `version: 1
id: offload
on:
  request:
    method: POST
    path: /v1/responses
request:
  pipeline:
    - uses: crabbro.gateway-offload
      with:
        protocol: responses
        model: mock-plan/mock-model
`

type offloadFunc func(*http.Request) (*http.Response, error)

func (f offloadFunc) Offload(r *http.Request) (*http.Response, error) { return f(r) }

func offloadProxy(t *testing.T, offloader GatewayOffloader) *Proxy {
	t.Helper()
	rs := rule.NewRuleStore()
	if _, err := rs.Create("offload", 10, offloadRule); err != nil {
		t.Fatal(err)
	}
	p := New("127.0.0.1:0", ring.NewRequestBuffer(20), rs, rule.NewRuleEngine())
	p.GatewayOffloader = offloader
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p
}

type lockedLog struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (b *lockedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.data.Write(p)
}
func (b *lockedLog) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.data.String() }

func TestGatewayOffloadEndToEnd(t *testing.T) {
	logs := &lockedLog{}
	originalLog := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(originalLog) })
	var subscriptionCalls atomic.Int32
	subscription := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		subscriptionCalls.Add(1)
		_, _ = io.WriteString(w, "subscription")
	}))
	defer subscription.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		all := r.URL.String() + fmt.Sprint(r.Header) + string(body)
		for _, private := range []string{"synthetic-auth", "synthetic-cookie", "synthetic-query", "synthetic-account", "subscription-model"} {
			if strings.Contains(all, private) {
				t.Error("subscription value reached gateway upstream")
			}
		}
		if r.URL.RawQuery != "" || !strings.Contains(string(body), `"model":"mock-model"`) {
			t.Errorf("unexpected gateway request: %s %s", r.URL, body)
		}
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[]}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[]}`)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	sec := secret.New(filepath.Join(dir, "secrets"))
	if err := sec.Set("gateway_key", "synthetic-gateway-key"); err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(gateway.Options{RootDir: filepath.Join(dir, "gateway"), Token: "synthetic-local-token"}, sec, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()
	plan := fmt.Sprintf("version: 1\nid: mock-plan\ncredential: gateway_key\nendpoints:\n  openai-responses:\n    baseUrl: %s\nmodels:\n  mock-model:\n    tier: strong\n", upstream.URL)
	if err := gw.Config.Draft(config.KindPlan, "mock-plan", []byte(plan)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Config.Commit(config.KindPlan, "mock-plan"); err != nil {
		t.Fatal(err)
	}
	p := offloadProxy(t, gw)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+p.Addr()))}}
	for _, stream := range []bool{false, true} {
		body := fmt.Sprintf(`{"model":"subscription-model","input":"hello synthetic-auth","stream":%t,"metadata":{"account":"synthetic-account"}}`, stream)
		req, _ := http.NewRequest(http.MethodPost, subscription.URL+"/v1/responses?token=synthetic-query", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer synthetic-auth")
		req.Header.Set("Cookie", "synthetic-cookie")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(got), "resp_1") {
			t.Fatalf("status %d body %s", resp.StatusCode, got)
		}
		if stream && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatal("missing SSE content type")
		}
	}
	resp, err := client.Get(subscription.URL + "/miss")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if subscriptionCalls.Load() != 1 {
		t.Fatalf("subscription calls = %d", subscriptionCalls.Load())
	}
	for _, rec := range p.DefaultSink().List(0) {
		if rec.Path != "/v1/responses" {
			continue
		}
		data, _ := json.Marshal(rec)
		body := p.DefaultSink().GetBody(rec.RequestBodyRef)
		for _, private := range []string{"synthetic-auth", "synthetic-cookie", "synthetic-query", "synthetic-account", "subscription-model"} {
			if strings.Contains(string(data)+string(body), private) {
				t.Errorf("subscription value reached capture")
			}
		}
		if rec.Query != "" || rec.Host != "gateway.internal" || !strings.Contains(string(body), `"model":"mock-plan/mock-model"`) {
			t.Fatalf("unexpected capture: %s %s", data, body)
		}
	}
	for _, private := range []string{"synthetic-auth", "synthetic-cookie", "synthetic-query", "synthetic-account", "subscription-model"} {
		if strings.Contains(logs.String(), private) {
			t.Error("subscription value reached logs")
		}
	}
}

func TestGatewayOffloadFailClosedAndCancellation(t *testing.T) {
	var calls atomic.Int32
	subscription := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer subscription.Close()
	p := offloadProxy(t, offloadFunc(func(r *http.Request) (*http.Response, error) { return nil, errors.New("synthetic failure") }))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+p.Addr()))}}
	resp, err := client.Post(subscription.URL+"/v1/responses", "application/json", strings.NewReader(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || calls.Load() != 0 {
		t.Fatalf("status %d; upstream calls %d", resp.StatusCode, calls.Load())
	}

	started, canceled := make(chan struct{}), make(chan struct{})
	p2 := offloadProxy(t, offloadFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		close(canceled)
		return nil, r.Context().Err()
	}))
	client2 := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+p2.Addr()))}}
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, subscription.URL+"/v1/responses", strings.NewReader(`{"input":"hello"}`))
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, _ := client2.Do(req)
		if r != nil {
			r.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway context not canceled")
	}
	<-done
	if calls.Load() != 0 {
		t.Fatal("canceled request reached subscription")
	}
}

func TestGatewayOffloadStreamingFlushAndCancel(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	p := offloadProxy(t, offloadFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.RawQuery != "" || len(r.Header) != 1 || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || strings.Contains(string(body), "synthetic-private") {
			t.Error("unclean offload ingress")
		}
		reader, writer := io.Pipe()
		go func() {
			defer close(stopped)
			_, _ = io.WriteString(writer, "data: first\n\n")
			close(started)
			<-r.Context().Done()
			_ = writer.Close()
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}, nil
	}))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+p.Addr()))}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://subscription.example/v1/responses?token=synthetic-private", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Authorization", "synthetic-private")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	first := make([]byte, len("data: first\n\n"))
	if _, err := io.ReadFull(resp.Body, first); err != nil || string(first) != "data: first\n\n" {
		t.Fatalf("first event: %q %v", first, err)
	}
	<-started
	cancel()
	resp.Body.Close()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stream cancellation did not reach gateway")
	}
}

func TestGatewayOffloadInvalidRequestCapture(t *testing.T) {
	p := offloadProxy(t, offloadFunc(func(*http.Request) (*http.Response, error) {
		t.Error("invalid request reached gateway")
		return nil, nil
	}))
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(mustParse(t, "http://"+p.Addr()))}}
	req, _ := http.NewRequest(http.MethodPost, "http://subscription.example/v1/responses?token=synthetic-private", strings.NewReader(`{"input":"synthetic-private"`))
	req.Header.Set("Authorization", "synthetic-private")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, rec := range p.DefaultSink().List(0) {
		data, _ := json.Marshal(rec)
		if strings.Contains(string(data)+string(p.DefaultSink().GetBody(rec.RequestBodyRef)), "synthetic-private") {
			t.Fatal("invalid request leaked to capture")
		}
	}
}
