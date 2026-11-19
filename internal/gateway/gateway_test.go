package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lidessen/crabbro/internal/capture"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/session"
)

func TestFastPathPassthrough429AndFake200SSEPeekFailover(t *testing.T) {
	dir := t.TempDir()
	sec := secret.New(filepath.Join(dir, "secrets"))
	if err := sec.Set("key1", "sk-secret-plan-one"); err != nil {
		t.Fatal(err)
	}
	if err := sec.Set("key2", "sk-secret-plan-two"); err != nil {
		t.Fatal(err)
	}

	var p1Calls atomic.Int32
	var p1Mode atomic.Int32 // 0 = 429 JSON, 1 = fake 200 SSE rate_limit_error
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p1Calls.Add(1)
		if r.Header.Get("x-api-key") != "sk-secret-plan-one" {
			t.Errorf("up1 got wrong key %q", r.Header.Get("x-api-key"))
		}
		if p1Mode.Load() == 0 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"429 limit"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, ": keepalive\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"fake 200 quota\"}}\n\n")
	}))
	defer up1.Close()

	var p2GotBody string
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p2GotBody = string(b)
		if r.Header.Get("x-api-key") != "sk-secret-plan-two" {
			t.Errorf("up2 got wrong key %q", r.Header.Get("x-api-key"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":50,\"cache_read_input_tokens\":200}}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":12}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer up2.Close()

	rb := ring.NewRequestBuffer(32)
	bc := capture.NewBodyCache(1<<20, 64*1024)
	hub := capture.NewEventHub()
	sink := capture.NewDefaultSink(rb, bc, hub, session.New(32), nil)

	gw, err := New(Options{
		ListenAddr: "127.0.0.1:0",
		RootDir:    filepath.Join(dir, "gateway"),
	}, sec, sink)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()

	plan1YAML := fmt.Sprintf(`version: 1
id: plan-one
credential: key1
endpoints:
  anthropic:
    baseUrl: %s
models:
  m-one:
    tier: strong
    contextWindow: 200000
`, up1.URL)
	plan2YAML := fmt.Sprintf(`version: 1
id: plan-two
credential: key2
endpoints:
  anthropic:
    baseUrl: %s
models:
  m-two:
    tier: strong
    contextWindow: 200000
`, up2.URL)
	routeYAML := `version: 1
id: coding-main
priority: 10
family: generative
match:
  protocols: [anthropic]
  models: ["coding-main"]
policy:
  preset: strict-failover
  maxAttempts: 3
targets:
  - plan: plan-one
    model: m-one
    priority: 10
  - plan: plan-two
    model: m-two
    priority: 20
`
	if err := gw.Config.Draft(config.KindPlan, "plan-one", []byte(plan1YAML)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Config.Commit(config.KindPlan, "plan-one"); err != nil {
		t.Fatal(err)
	}
	if err := gw.Config.Draft(config.KindPlan, "plan-two", []byte(plan2YAML)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Config.Commit(config.KindPlan, "plan-two"); err != nil {
		t.Fatal(err)
	}
	if err := gw.Config.Draft(config.KindRoute, "coding-main", []byte(routeYAML)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Config.Commit(config.KindRoute, "coding-main"); err != nil {
		t.Fatal(err)
	}

	if err := gw.Start(); err != nil {
		t.Fatal(err)
	}

	// Case 1: 429 on plan-one silently fails over to plan-two and preserves unknown vendor fields!
	reqBody := `{"model":"coding-main","stream":true,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post("http://"+gw.Addr()+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	sseBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("X-Crabbro-Plan") != "plan-two" {
		t.Fatalf("status=%d plan=%q body=%s", resp.StatusCode, resp.Header.Get("X-Crabbro-Plan"), sseBytes)
	}
	if !strings.Contains(p2GotBody, `"budget_tokens":2048`) || !strings.Contains(p2GotBody, `"model":"m-two"`) {
		t.Fatalf("patched body dropped vendor field or model: %s", p2GotBody)
	}
	if !strings.Contains(string(sseBytes), `"hello"`) {
		t.Fatalf("missing streamed text: %s", sseBytes)
	}

	// Reset cooldown, switch up1 to fake-200 SSE error, verify First-SSE-Event Peek catches it and fails over!
	if err := gw.ResetCooldown("plan-one"); err != nil {
		t.Fatal(err)
	}
	p1Mode.Store(1)
	resp2, err := http.Post("http://"+gw.Addr()+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	sc := bufio.NewScanner(resp2.Body)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if resp2.StatusCode != 200 || resp2.Header.Get("X-Crabbro-Plan") != "plan-two" {
		t.Fatalf("fake-200 peek did not fail over: status=%d plan=%q", resp2.StatusCode, resp2.Header.Get("X-Crabbro-Plan"))
	}
	if !strings.Contains(strings.Join(lines, "\n"), `"hello"`) {
		t.Fatalf("missing streamed payload after fake-200 failover: %v", lines)
	}

	// Verify GET /v1/models and usage.db accounting and zero secret leaks in ring buffer.
	mResp, err := http.Get("http://" + gw.Addr() + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	mBody, _ := io.ReadAll(mResp.Body)
	mResp.Body.Close()
	if !strings.Contains(string(mBody), `"coding-main"`) {
		t.Fatalf("/v1/models = %s", mBody)
	}

	usage, err := gw.Usage("1h", "")
	if err != nil {
		t.Fatal(err)
	}
	if usage.Totals.CacheReadTokens != 400 || usage.Totals.OutputTokens != 24 {
		t.Fatalf("usage.db mismatch: %+v", usage.Totals)
	}

	if p1Calls.Load() != 2 {
		t.Fatalf("expected plan-one to be attempted on both requests (429 and fake-200 peek), got %d calls", p1Calls.Load())
	}
	captured := rb.List(0)
	if len(captured) < 2 {
		t.Fatalf("expected captured exchanges, got %d", len(captured))
	}
	rawCap, _ := json.Marshal(captured)
	if bytes.Contains(rawCap, []byte("sk-secret-plan")) {
		t.Fatalf("secret leaked into captured exchange records: %s", rawCap)
	}
	if captured[len(captured)-1].Kind != "gateway" || captured[len(captured)-1].Gateway == nil || len(captured[len(captured)-1].Gateway.Attempts) != 2 {
		t.Fatalf("unexpected gateway trace: %+v", captured[len(captured)-1])
	}
}

func TestNonLoopbackRequiresToken(t *testing.T) {
	dir := t.TempDir()
	gw, err := New(Options{ListenAddr: "0.0.0.0:0", RootDir: dir}, secret.New(filepath.Join(dir, "secrets")), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()
	if err := gw.Start(); err == nil {
		t.Fatal("expected non-loopback listener without token to be rejected")
	}
}
