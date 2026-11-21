package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/secret"
)

type rewriteTransport struct{ base *url.URL }

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = t.base.Scheme
	u.Host = t.base.Host
	r2.URL = &u
	return http.DefaultTransport.RoundTrip(r2)
}
func TestProvidersAndStalePreservation(t *testing.T) {
	failed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failed {
			w.WriteHeader(500)
			fmt.Fprint(w, "<html>private@example.com secret-token</html>")
			return
		}
		switch r.URL.Path {
		case "/api/monitor/usage/quota/limit":
			fmt.Fprint(w, `{"code":0,"data":{"level":"lite","limits":[{"unit":3,"usage":20,"remaining":80}]}}`)
		case "/api/biz/customer-package-reset/list":
			fmt.Fprintf(w, `{"code":200,"data":{"fiveHourResets":[{"available":true,"expiresAt":%q}]}}`, time.Now().Add(24*time.Hour).UTC().Format(time.RFC3339))
		case "/alpha/billing/credits":
			fmt.Fprint(w, `{"windowLimits":{"fiveHour":{"used":1,"cap":4}},"monthlyCredits":10,"purchasedCredits":2,"freeCredits":3}`)
		case "/v1/credits":
			fmt.Fprint(w, `{"balance":17.65}`)
		case "/user/balance":
			fmt.Fprint(w, `{"balance_infos":[{"currency":"CNY","total_balance":"97.85"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := secret.New(t.TempDir())
	plans := map[string]*config.Plan{}
	for _, name := range []string{"zcode", "commandcode", "vercel", "deepseek"} {
		if err := store.Set(name, "test-key"); err != nil {
			t.Fatal(err)
		}
		plans[name] = &config.Plan{ID: name, Credential: name}
	}
	s := New(store)
	u, _ := url.Parse(server.URL)
	s.client.Transport = rewriteTransport{u}
	s.Refresh(context.Background(), plans)
	for name, want := range map[string]string{"commandcode": "15.00 credits", "vercel": "$17.65", "deepseek": "CNY 97.85"} {
		q, ok := s.Get(name)
		if !ok || q.Balance != want || q.Stale {
			t.Fatalf("%s: %+v", name, q)
		}
	}
	q, _ := s.Get("zcode")
	if q.PlanTier != "lite" || q.ResetsRemaining != 1 || len(q.Windows) != 1 || q.UsagePct == nil || *q.UsagePct != 20 {
		t.Fatalf("zcode: %+v", q)
	}
	failed = true
	s.Refresh(context.Background(), plans)
	q, _ = s.Get("zcode")
	raw, _ := json.Marshal(q)
	if !q.Stale || q.PlanTier != "lite" || q.SyncSource != "cached" || strings.Contains(string(raw), "example.com") || strings.Contains(string(raw), "secret-token") {
		t.Fatalf("stale: %s", raw)
	}
}
func TestUnsafeLabels(t *testing.T) {
	for _, v := range []string{"user@example.com", "/Users/private", "0123456789abcdef", "abcdefghijklmnopqrstuvwx", "<html>"} {
		if safe(v) != "" {
			t.Fatalf("unsafe label accepted")
		}
	}
}
