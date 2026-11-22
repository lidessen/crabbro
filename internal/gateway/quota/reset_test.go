package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
)

// bigModelMock serves the three BigModel endpoints used by the quota syncer.
type bigModelMock struct {
	mu sync.Mutex

	usagePct      float64 // five-hour window usage reported by /quota/limit
	weeklyPct     float64
	afterUsePct   float64 // five-hour usage reported once a coupon is redeemed
	redeemed      bool
	coupons       string // /list payload
	useStatus     int    // /use HTTP status (0 means 200)
	useEnvelope   string // /use response body ("" means verified success)
	useDelay      time.Duration
	excludeWeekly bool

	limitCalls int
	listCalls  int
	useCalls   int
	lastUse    map[string]any
	lastAuth   string
}

func newBigModelMock(t *testing.T) *bigModelMock {
	t.Helper()
	return &bigModelMock{usagePct: 100, weeklyPct: 50, afterUsePct: 10, useStatus: http.StatusOK}
}

func shanghaiIn(d time.Duration) string {
	return time.Now().In(shanghaiZone).Add(d).Format("2006-01-02 15:04:05")
}

func (m *bigModelMock) listPayload() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.coupons != "" {
		return m.coupons
	}
	if m.redeemed {
		// A consumed coupon disappears from the inventory.
		return `{"code":200,"msg":"ok","success":true,"data":{}}`
	}
	body := fmt.Sprintf(`{"code":200,"msg":"ok","success":true,"data":{"fiveHourResets":[{"recordId":4242,"grantType":"DIRECT","expireTime":%q,"available":true}]}}`, shanghaiIn(90*time.Minute))
	if !m.excludeWeekly {
		body = strings.TrimSuffix(body, "}}") + fmt.Sprintf(`,"weekResets":[{"recordId":"99001","grantType":"DIRECT","expireTime":%q,"available":true}]}}`, shanghaiIn(6*time.Hour))
	}
	return body
}

func (m *bigModelMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/monitor/usage/quota/limit":
		m.mu.Lock()
		m.limitCalls++
		five, weekly := m.usagePct, m.weeklyPct
		if m.redeemed {
			five = m.afterUsePct
		}
		m.mu.Unlock()
		fmt.Fprintf(w, `{"code":0,"data":{"level":"lite","limits":[{"unit":3,"usage":100,"remaining":%.2f},{"unit":6,"usage":100,"remaining":%.2f}]}}`, 100-five, 100-weekly)
	case "/api/biz/customer-package-reset/list":
		m.mu.Lock()
		m.listCalls++
		m.lastAuth = r.Header.Get("Authorization")
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, m.listPayload())
	case "/api/biz/customer-package-reset/use":
		var body map[string]any
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		_ = dec.Decode(&body)
		m.mu.Lock()
		m.useCalls++
		m.lastUse = body
		m.lastAuth = r.Header.Get("Authorization")
		status, envelope, delay := m.useStatus, m.useEnvelope, m.useDelay
		if status == 0 {
			status = http.StatusOK
		}
		verified := status == http.StatusOK && (envelope == "" || strings.Contains(envelope, `"success":true`))
		if verified {
			m.redeemed = true
		}
		m.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if envelope == "" {
			fmt.Fprint(w, `{"code":200,"msg":"ok","success":true,"data":1}`)
			return
		}
		fmt.Fprint(w, envelope)
	default:
		http.NotFound(w, r)
	}
}

func (m *bigModelMock) stats() (listCalls, useCalls int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listCalls, m.useCalls
}

func (m *bigModelMock) useBody() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastUse
}

func (m *bigModelMock) auth() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastAuth
}

func zcodePlan(serverURL string, policy config.AutoResetSpec) *config.Plan {
	return &config.Plan{
		ID:         "zcode",
		Credential: "zcode_key",
		AutoReset:  policy,
		Endpoints: map[string]*config.EndpointSpec{
			"anthropic": {BaseURL: serverURL},
		},
	}
}

func newTestSyncer(t *testing.T) *Syncer {
	t.Helper()
	store := secret.New(t.TempDir())
	if err := store.Set("zcode_key", "sk-zcode-test"); err != nil {
		t.Fatal(err)
	}
	s := New(store, t.TempDir())
	s.client = &http.Client{Timeout: 5 * time.Second}
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
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

// TestZcodeExpiryParsedFromShanghaiWallClock verifies that naive
// "2006-01-02 15:04:05" expireTime strings are parsed as Asia/Shanghai
// (+08:00) local time and normalized to UTC.
func TestZcodeExpiryParsedFromShanghaiWallClock(t *testing.T) {
	soon := time.Now().Add(2 * time.Hour)
	past := time.Now().Add(-2 * time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/monitor/usage/quota/limit":
			fmt.Fprint(w, `{"code":0,"data":{"level":"lite","limits":[{"unit":3,"usage":100,"remaining":40}]}}`)
		case "/api/biz/customer-package-reset/list":
			fmt.Fprintf(w, `{"code":200,"success":true,"data":{"fiveHourResets":[
				{"recordId":4242,"grantType":"DIRECT","expireTime":%q,"available":true},
				{"recordId":4243,"grantType":"DIRECT","expireTime":%q,"available":true},
				{"recordId":4244,"grantType":"DIRECT","expireTime":"2026-11-22 21:40:36","available":false}
			]}}`, soon.In(shanghaiZone).Format("2006-01-02 15:04:05"), past.In(shanghaiZone).Format("2006-01-02 15:04:05"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	q, err := fetch(context.Background(), server.Client(), zcodePlan(server.URL, config.AutoResetSpec{}), "sk-zcode-test")
	if err != nil {
		t.Fatal(err)
	}
	if q.ResetsRemaining != 1 || len(q.Resets) != 1 {
		t.Fatalf("coupons = %+v", q.Resets)
	}
	want := soon.Truncate(time.Second).UTC()
	got := q.Resets[0].ExpiresAt
	if got == nil || !got.Equal(want) {
		t.Fatalf("expiresAt = %v, want %v (UTC)", got, want)
	}
	if got.Location() != time.UTC {
		t.Fatalf("expiresAt location = %v, want UTC", got.Location())
	}
	if q.SoonestResetExpiry == nil || !q.SoonestResetExpiry.Equal(want) {
		t.Fatalf("soonestResetExpiry = %v, want %v", q.SoonestResetExpiry, want)
	}
	// The wall-clock string "2026-11-22 21:40:36" must map to 13:40:36 UTC.
	if fix := shanghaiStamp("2026-11-22 21:40:36"); fix == nil || !fix.Equal(time.Date(2026, 11, 22, 13, 40, 36, 0, time.UTC)) {
		t.Fatalf("shanghaiStamp(2026-11-22 21:40:36) = %v", fix)
	}
	if shanghaiStamp("") != nil || shanghaiStamp("not-a-time") != nil || shanghaiStamp("2026-11-22T21:40:36Z") != nil {
		t.Fatal("shanghaiStamp accepted an unsupported value")
	}
}

// TestExplicitResetDryRunAndRedeemption covers the operator control path:
// dry-run selects without spending, a real reset redeems exactly one coupon
// with a unique request id, and the remote snapshot refreshes afterwards.
func TestExplicitResetDryRunAndRedeemption(t *testing.T) {
	mock := newBigModelMock(t)
	server := httptest.NewServer(mock)
	defer server.Close()

	s := newTestSyncer(t)
	plans := map[string]*config.Plan{"zcode": zcodePlan(server.URL, config.AutoResetSpec{})}

	out, err := s.Reset(context.Background(), plans, "zcode", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !out.DryRun || out.Redeemed || out.CouponsAvailable != 2 || out.Scope != ScopeFiveHour {
		t.Fatalf("dry-run outcome = %+v", out)
	}
	if _, useCalls := mock.stats(); useCalls != 0 {
		t.Fatalf("dry-run spent a coupon (%d /use calls)", useCalls)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "4242") || strings.Contains(string(raw), "recordId") {
		t.Fatalf("dry-run response leaks a coupon identifier: %s", raw)
	}

	out, err = s.Reset(context.Background(), plans, "zcode", ScopeFiveHour, false)
	if err != nil || !out.Redeemed || out.Scope != ScopeFiveHour || out.CouponsAvailable != 1 {
		t.Fatalf("reset outcome = %+v, err = %v", out, err)
	}
	body := mock.useBody()
	if body["targetType"] != "PERSONAL" || body["resetType"] != "FIVE_HOUR" || body["grantType"] != "DIRECT" {
		t.Fatalf("unexpected /use body: %v", body)
	}
	if fmt.Sprint(body["recordId"]) != "4242" {
		t.Fatalf("recordId passthrough = %v", body["recordId"])
	}
	if id, _ := body["requestId"].(string); len(id) != 36 {
		t.Fatalf("requestId = %v", body["requestId"])
	}
	if mock.auth() != "sk-zcode-test" {
		t.Fatalf("authorization header = %q", mock.auth())
	}
	raw, _ = json.Marshal(out)
	if strings.Contains(string(raw), "4242") || strings.Contains(string(raw), "recordId") {
		t.Fatalf("reset response leaks a coupon identifier: %s", raw)
	}

	// Remote quota refreshes immediately: the post-reset window is no longer
	// exhausted and the coupon inventory dropped to zero.
	q, ok := s.Get("zcode")
	if !ok || q.UsagePct == nil || *q.UsagePct != 10 || q.ResetsRemaining != 0 {
		t.Fatalf("post-reset snapshot = %+v", q)
	}
	if q.SoonestResetExpiry != nil {
		t.Fatalf("post-reset snapshot still reports a coupon expiry: %+v", q.SoonestResetExpiry)
	}

	// A second reset in the same window finds no coupons and fails closed.
	if _, err := s.Reset(context.Background(), plans, "zcode", ScopeFiveHour, false); !errors.Is(err, ErrResetNoCoupons) {
		t.Fatalf("second reset err = %v, want %v", err, ErrResetNoCoupons)
	}
	if _, useCalls := mock.stats(); useCalls != 1 {
		t.Fatalf("/use called %d times, want exactly 1", useCalls)
	}

	// Unknown plans and non-reset providers are rejected without I/O.
	if _, err := s.Reset(context.Background(), plans, "nope", "", false); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("unknown plan err = %v", err)
	}
	other := &config.Plan{ID: "deepseek", Credential: "deepseek_key"}
	plans["deepseek"] = other
	if _, err := s.Reset(context.Background(), plans, "deepseek", "", false); !errors.Is(err, ErrResetUnsupported) {
		t.Fatalf("unsupported provider err = %v", err)
	}
}

// TestResetWeeklyScopeAndValidation covers explicit scope selection plus the
// scope/plan guards of the control path.
func TestResetWeeklyScopeAndValidation(t *testing.T) {
	mock := newBigModelMock(t)
	mock.coupons = fmt.Sprintf(`{"code":200,"success":true,"data":{"weekResets":[{"recordId":"99001","grantType":"DIRECT","expireTime":%q,"available":true}]}}`, shanghaiIn(3*time.Hour))
	server := httptest.NewServer(mock)
	defer server.Close()

	s := newTestSyncer(t)
	plans := map[string]*config.Plan{"zcode": zcodePlan(server.URL, config.AutoResetSpec{})}
	if err := NormalizeResetScope("weekly"); err != nil {
		t.Fatal(err)
	}
	if err := NormalizeResetScope("bogus"); err == nil {
		t.Fatal("bogus scope accepted")
	}
	out, err := s.Reset(context.Background(), plans, "zcode", ScopeWeekly, false)
	if err != nil || !out.Redeemed || out.Scope != ScopeWeekly {
		t.Fatalf("weekly reset = %+v, err = %v", out, err)
	}
	if body := mock.useBody(); body["resetType"] != "WEEK" || fmt.Sprint(body["recordId"]) != "99001" {
		t.Fatalf("weekly /use body = %v", body)
	}
}

// TestResetSingleFlightProtections proves the per-plan mutex allows only one
// in-flight redemption cycle.
func TestResetSingleFlightProtections(t *testing.T) {
	mock := newBigModelMock(t)
	mock.useDelay = 250 * time.Millisecond
	server := httptest.NewServer(mock)
	defer server.Close()

	s := newTestSyncer(t)
	plans := map[string]*config.Plan{"zcode": zcodePlan(server.URL, config.AutoResetSpec{})}

	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Reset(context.Background(), plans, "zcode", "", false)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	busy, ok := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrResetInFlight):
			busy++
		default:
			t.Fatalf("unexpected reset error: %v", err)
		}
	}
	if ok != 1 || busy != 3 {
		t.Fatalf("single-flight results = %d ok, %d busy", ok, busy)
	}
	if _, useCalls := mock.stats(); useCalls != 1 {
		t.Fatalf("/use called %d times, want exactly 1", useCalls)
	}
}

// TestAutoResetOnExhaustion covers the opt-in policy end to end: a quota
// refresh that reports an exhausted window redeems exactly one coupon,
// respects minInterval, and never spends when the policy is disabled.
func TestAutoResetOnExhaustion(t *testing.T) {
	mock := newBigModelMock(t)
	server := httptest.NewServer(mock)
	defer server.Close()

	policy := config.AutoResetSpec{Enabled: true, Scopes: []string{ScopeFiveHour}, MinInterval: "10m"}
	s := newTestSyncer(t)
	var triggered []string
	var cbMu sync.Mutex
	s.OnAutoReset = func(planID, scope string, out ResetOutcome) {
		cbMu.Lock()
		defer cbMu.Unlock()
		triggered = append(triggered, planID+"/"+scope)
	}
	plans := map[string]*config.Plan{"zcode": zcodePlan(server.URL, policy)}

	// Disabled policy never redeems.
	off := config.AutoResetSpec{}
	s2 := newTestSyncer(t)
	s2.Refresh(context.Background(), map[string]*config.Plan{"zcode": zcodePlan(server.URL, off)})
	waitFor(t, "disabled-policy sync", func() bool { _, ok := s2.Get("zcode"); return ok })
	if _, useCalls := mock.stats(); useCalls != 0 {
		t.Fatalf("auto-reset spent a coupon with the policy disabled (%d calls)", useCalls)
	}

	// Enabled policy: the exhausted five-hour window is auto-redeemed once.
	s.Refresh(context.Background(), plans)
	waitFor(t, "automatic redemption", func() bool { _, useCalls := mock.stats(); return useCalls == 1 })
	cbMu.Lock()
	if len(triggered) != 1 || triggered[0] != "zcode/fiveHour" {
		t.Fatalf("auto-reset callbacks = %v", triggered)
	}
	cbMu.Unlock()
	if body := mock.useBody(); body["resetType"] != "FIVE_HOUR" {
		t.Fatalf("auto /use body = %v", body)
	}

	// The refreshed snapshot reflects the reset window.
	waitFor(t, "post-reset snapshot", func() bool {
		q, ok := s.Get("zcode")
		return ok && q.UsagePct != nil && *q.UsagePct == 10
	})

	// Re-arming the exhaustion does not double spend inside minInterval.
	mock.mu.Lock()
	mock.usagePct = 100
	mock.redeemed = false
	mock.mu.Unlock()
	s.Refresh(context.Background(), plans)
	waitFor(t, "second sync", func() bool {
		q, _ := s.Get("zcode")
		return q != nil && q.UsagePct != nil && *q.UsagePct == 100
	})
	if _, useCalls := mock.stats(); useCalls != 1 {
		t.Fatalf("/use called %d times inside minInterval, want 1", useCalls)
	}

	// Once minInterval elapses the same evidence may redeem again.
	s.SetClock(func() time.Time { return time.Now().Add(11 * time.Minute) })
	mock.mu.Lock()
	mock.usagePct = 100
	mock.mu.Unlock()
	s.Refresh(context.Background(), plans)
	waitFor(t, "redemption after minInterval", func() bool { _, useCalls := mock.stats(); return useCalls == 2 })
}

// TestAutoResetHonorsExpiringWithinAndFailsClosed covers the expiry horizon
// and the fail-closed rule for ambiguous provider responses.
func TestAutoResetHonorsExpiringWithinAndFailsClosed(t *testing.T) {
	t.Run("coupon outside horizon is skipped", func(t *testing.T) {
		mock := newBigModelMock(t)
		mock.coupons = fmt.Sprintf(`{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":4242,"grantType":"DIRECT","expireTime":%q,"available":true}]}}`, shanghaiIn(48*time.Hour))
		server := httptest.NewServer(mock)
		defer server.Close()
		policy := config.AutoResetSpec{Enabled: true, Scopes: []string{ScopeFiveHour}, ExpiringWithin: "24h", MinInterval: "10m"}
		s := newTestSyncer(t)
		s.Refresh(context.Background(), map[string]*config.Plan{"zcode": zcodePlan(server.URL, policy)})
		waitFor(t, "sync", func() bool { _, ok := s.Get("zcode"); return ok })
		if _, useCalls := mock.stats(); useCalls != 0 {
			t.Fatalf("redeemed a coupon outside expiringWithin (%d calls)", useCalls)
		}
	})
	t.Run("coupon inside horizon is redeemed", func(t *testing.T) {
		mock := newBigModelMock(t)
		mock.coupons = fmt.Sprintf(`{"code":200,"success":true,"data":{"fiveHourResets":[{"recordId":4242,"grantType":"DIRECT","expireTime":%q,"available":true}]}}`, shanghaiIn(30*time.Minute))
		server := httptest.NewServer(mock)
		defer server.Close()
		policy := config.AutoResetSpec{Enabled: true, Scopes: []string{ScopeFiveHour}, ExpiringWithin: "24h", MinInterval: "10m"}
		s := newTestSyncer(t)
		s.Refresh(context.Background(), map[string]*config.Plan{"zcode": zcodePlan(server.URL, policy)})
		waitFor(t, "redemption", func() bool { _, useCalls := mock.stats(); return useCalls == 1 })
	})
	ambiguity := []struct {
		name   string
		status int
		body   string
	}{
		{"http error", http.StatusInternalServerError, `{"code":200,"success":true}`},
		{"provider rejection", http.StatusOK, `{"code":1301,"msg":"denied","success":false}`},
		{"garbage envelope", http.StatusOK, `{"code":200}`},
	}
	for _, tc := range ambiguity {
		t.Run("fail closed on "+tc.name, func(t *testing.T) {
			mock := newBigModelMock(t)
			mock.useStatus = tc.status
			mock.useEnvelope = tc.body
			server := httptest.NewServer(mock)
			defer server.Close()
			policy := config.AutoResetSpec{Enabled: true, Scopes: []string{ScopeFiveHour}, MinInterval: "10m"}
			s := newTestSyncer(t)
			called := false
			s.OnAutoReset = func(planID, scope string, out ResetOutcome) { called = true }
			s.Refresh(context.Background(), map[string]*config.Plan{"zcode": zcodePlan(server.URL, policy)})
			waitFor(t, "single attempt", func() bool { _, useCalls := mock.stats(); return useCalls == 1 })
			time.Sleep(150 * time.Millisecond)
			if _, useCalls := mock.stats(); useCalls != 1 {
				t.Fatalf("/use retried %d times after an ambiguous result", useCalls-1)
			}
			if called {
				t.Fatal("auto-reset callback fired for an unverified redemption")
			}
			q, _ := s.Get("zcode")
			if q == nil || q.Stale {
				t.Fatalf("snapshot degraded after a failed redemption: %+v", q)
			}
		})
	}
}

// A repeated exhausted snapshot must not turn an ambiguous /use into a retry,
// even after the configured minimum interval has elapsed.
func TestAutoResetDoesNotRetryUnverifiedUse(t *testing.T) {
	mock := newBigModelMock(t)
	mock.useStatus = http.StatusInternalServerError
	server := httptest.NewServer(mock)
	defer server.Close()
	s := newTestSyncer(t)
	now := time.Now()
	s.SetClock(func() time.Time { return now })
	policy := config.AutoResetSpec{Enabled: true, Scopes: []string{ScopeFiveHour}, MinInterval: "5m"}
	p := zcodePlan(server.URL, policy)
	s.syncPlan(context.Background(), p, false)
	plans := map[string]*config.Plan{"zcode": p}
	s.AutoReset(context.Background(), plans, "zcode", false)
	now = now.Add(10 * time.Minute)
	// Simulate a new process reading the same persistent guard.
	restarted := New(s.secrets, filepath.Dir(s.resets.stateDir))
	restarted.client = s.client
	restarted.SetClock(func() time.Time { return now })
	restarted.syncPlan(context.Background(), p, false)
	restarted.AutoReset(context.Background(), plans, "zcode", true)
	s.AutoReset(context.Background(), plans, "zcode", true)
	if _, calls := mock.stats(); calls != 1 {
		t.Fatalf("unverified redemption was retried: %d calls", calls)
	}
}
