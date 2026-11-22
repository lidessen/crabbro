package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/capture"
	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/session"
	"github.com/lidessen/crabbro/internal/shared"
)

// resetSink records emitted events without touching any store.
type resetSink struct {
	mu     sync.Mutex
	events []string
}

func (s *resetSink) Start(rec *shared.Request) *shared.Request { return rec }
func (s *resetSink) StoreBody(data []byte) shared.BodyRef      { return "" }
func (s *resetSink) GetBody(ref shared.BodyRef) []byte         { return nil }
func (s *resetSink) NoteActivity(kind, sessionID, reqID, ruleID, sum string) {
}
func (s *resetSink) Complete(rec *shared.Request)                {}
func (s *resetSink) Resolve(id shared.RequestID) *shared.Request { return nil }
func (s *resetSink) List(limit int) []*shared.Request            { return nil }
func (s *resetSink) SessionStore() *session.Store                { return nil }
func (s *resetSink) Emit(typ shared.EventType, id shared.RequestID, payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, string(typ)+fmt.Sprint(payload))
}

func (s *resetSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

var _ capture.ExchangeSink = (*resetSink)(nil)

// newResetGateway builds a gateway whose only plan is a zcode plan backed by
// the mock BigModel server, with opt-in auto-reset enabled.
func newResetGateway(t *testing.T, serverURL string, policy config.AutoResetSpec, sink capture.ExchangeSink) (*Gateway, *secret.Store) {
	t.Helper()
	sec := secret.New(t.TempDir())
	if err := sec.Set("zcode_key", "sk-zcode-test"); err != nil {
		t.Fatal(err)
	}
	gw, err := New(Options{RootDir: t.TempDir(), ListenAddr: "127.0.0.1:0"}, sec, sink)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Stop() })
	snap := gw.Config.Snapshot()
	snap.AllPlans["zcode"] = &config.Plan{
		ID:         "zcode",
		Credential: "zcode_key",
		AutoReset:  policy,
		Endpoints: map[string]*config.EndpointSpec{
			"anthropic": {BaseURL: serverURL},
		},
	}
	return gw, sec
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestGatewayAutoResetClearsCooldown proves that an opt-in plan recovers by
// itself: a quota refresh that reports an exhausted window redeems one coupon
// and immediately clears the plan cooldown.
func TestGatewayAutoResetClearsCooldown(t *testing.T) {
	var mu sync.Mutex
	useCalls := 0
	exhausted := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/monitor/usage/quota/limit":
			used := 10.0
			if exhausted {
				used = 100.0
			}
			fmt.Fprintf(w, `{"code":0,"data":{"level":"lite","limits":[{"unit":3,"usage":100,"remaining":%.2f}]}}`, 100-used)
		case "/api/biz/customer-package-reset/list":
			mu.Lock()
			available := useCalls == 0
			mu.Unlock()
			if available {
				fmt.Fprintf(w, `{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":4242,"grantType":"DIRECT","expireTime":%q,"available":true}]}}`,
					time.Now().Add(time.Hour).Format("2006-01-02 15:04:05"))
				return
			}
			fmt.Fprint(w, `{"code":200,"success":true,"data":{}}`)
		case "/api/biz/customer-package-reset/use":
			mu.Lock()
			useCalls++
			mu.Unlock()
			fmt.Fprint(w, `{"code":200,"msg":"ok","success":true,"data":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	sink := &resetSink{}
	policy := config.AutoResetSpec{Enabled: true, Scopes: []string{config.AutoResetScopeFiveHour}, MinInterval: "10m"}
	gw, _ := newResetGateway(t, server.URL, policy, sink)

	gw.Health.EnterPlanCooldown("zcode", "plan", "quota exhausted (429)", 5*time.Minute, time.Now().UTC())
	if cooling, _, _ := gw.Health.IsPlanCooling("zcode", time.Now().UTC()); !cooling {
		t.Fatal("plan did not enter cooldown")
	}

	gw.Quotas.Refresh(context.Background(), gw.Config.Snapshot().AllPlans)

	waitUntil(t, "automatic redemption", func() bool { mu.Lock(); defer mu.Unlock(); return useCalls == 1 })
	mu.Lock()
	exhausted = false
	mu.Unlock()
	waitUntil(t, "cooldown cleared", func() bool {
		cooling, _, _ := gw.Health.IsPlanCooling("zcode", time.Now().UTC())
		return !cooling
	})

	events := sink.snapshot()
	var resetEvents []string
	for _, ev := range events {
		if strings.Contains(ev, string(shared.EventGatewayQuotaReset)) {
			resetEvents = append(resetEvents, ev)
		}
	}
	if len(resetEvents) != 1 || !strings.Contains(resetEvents[0], "zcode") || !strings.Contains(resetEvents[0], "fiveHour") {
		t.Fatalf("unexpected reset events: %v", resetEvents)
	}
	if strings.Contains(resetEvents[0], "4242") || strings.Contains(resetEvents[0], "recordId") {
		t.Fatalf("reset event leaks a coupon identifier: %v", resetEvents)
	}

	// A failed redemption keeps the cooldown in place (fail closed).
	mu.Lock()
	useCalls = 0
	exhausted = true
	mu.Unlock()
	gw.Health.EnterPlanCooldown("zcode", "plan", "quota exhausted (429)", 5*time.Minute, time.Now().UTC())
	gw.Executor.AutoResetSignal("zcode")
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		calls := useCalls
		mu.Unlock()
		if calls != 0 {
			t.Fatalf("unexpected redemption after the coupon inventory was drained: %d", calls)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cooling, _, _ := gw.Health.IsPlanCooling("zcode", time.Now().UTC()); !cooling {
		t.Fatal("cooldown cleared without a verified redemption")
	}
}

// TestGatewayResetQuotaControlPath covers the operator path used by
// POST /v1/gateway/quotas/reset and `crabbro gw quotas reset`.
func TestGatewayResetQuotaControlPath(t *testing.T) {
	var mu sync.Mutex
	useCalls := 0
	drained := false
	var lastBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/monitor/usage/quota/limit":
			fmt.Fprint(w, `{"code":0,"data":{"level":"lite","limits":[{"unit":3,"usage":100,"remaining":100}]}}`)
		case "/api/biz/customer-package-reset/list":
			mu.Lock()
			empty := drained
			mu.Unlock()
			if empty {
				fmt.Fprint(w, `{"code":200,"success":true,"data":{}}`)
				return
			}
			fmt.Fprintf(w, `{"code":200,"success":true,"data":{"weekResets":[{"recordId":7,"grantType":"DIRECT","expireTime":%q,"available":true}]}}`,
				time.Now().Add(2*time.Hour).Format("2006-01-02 15:04:05"))
		case "/api/biz/customer-package-reset/use":
			var body map[string]any
			dec := json.NewDecoder(r.Body)
			dec.UseNumber()
			_ = dec.Decode(&body)
			mu.Lock()
			useCalls++
			lastBody = body
			drained = true
			mu.Unlock()
			fmt.Fprint(w, `{"code":200,"msg":"ok","success":true,"data":2}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	gw, _ := newResetGateway(t, server.URL, config.AutoResetSpec{}, nil)

	gw.Health.EnterPlanCooldown("zcode", "plan", "rate limited (429)", 5*time.Minute, time.Now().UTC())

	out := gw.ResetQuota(context.Background(), shared.GatewayQuotaResetRequest{Plan: "zcode", Scope: "weekly"})
	if out.Error != "" || !out.Redeemed || out.Scope != "weekly" || out.CouponsAvailable != 1 {
		t.Fatalf("reset response = %+v", out)
	}
	mu.Lock()
	calls, body := useCalls, lastBody
	mu.Unlock()
	if calls != 1 || body["resetType"] != "WEEK" {
		t.Fatalf("use calls = %d, body = %v", calls, body)
	}
	if cooling, _, _ := gw.Health.IsPlanCooling("zcode", time.Now().UTC()); cooling {
		t.Fatal("cooldown not cleared after a verified redemption")
	}
	if out.Status == nil || out.Status.ID != "zcode" || out.Status.RemoteQuota == nil {
		t.Fatalf("refreshed plan status missing: %+v", out.Status)
	}

	dry := gw.ResetQuota(context.Background(), shared.GatewayQuotaResetRequest{Plan: "zcode", Scope: "", DryRun: true})
	if dry.Redeemed || dry.Error == "" {
		t.Fatalf("dry-run on a drained inventory = %+v", dry)
	}
	if unknown := gw.ResetQuota(context.Background(), shared.GatewayQuotaResetRequest{Plan: "ghost"}); unknown.Error == "" {
		t.Fatalf("unknown plan accepted: %+v", unknown)
	}
}
