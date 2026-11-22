package router

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/quota"
)

type keyQuotaMap map[string]*quota.RemoteQuota

func (m keyQuotaMap) Get(id string) (*quota.RemoteQuota, bool) { q, ok := m[id]; return q, ok }

func TestKeyPoolOrderingAndAvailability(t *testing.T) {
	r, snap, _, sec := setupRouterFixture(t)
	p := snap.Plans["kimi-coding"]
	if err := sec.Set("key_extra", "fixture"); err != nil {
		t.Fatal(err)
	}
	p.Credentials = &config.CredentialPool{Refs: []string{"k1", "key_extra", "missing"}}
	rt := snap.Routes[0]
	now := time.Now()
	pick := func() Candidate {
		return r.evalTargets(snap, rt, Signals{Protocol: config.ProtocolAnthropic}, config.TierStrong, SessionCacheEntry{}, false, now, false)[0]
	}
	if got := pick().CredentialRefs; !reflect.DeepEqual(got, []string{"k1", "key_extra"}) {
		t.Fatalf("priority: %v", got)
	}
	p.Credentials.Strategy = "round-robin"
	if got := pick().CredentialRefs; !reflect.DeepEqual(got, []string{"k1", "key_extra"}) {
		t.Fatalf("first rotation: %v", got)
	}
	if got := pick().CredentialRefs; !reflect.DeepEqual(got, []string{"key_extra", "k1"}) {
		t.Fatalf("second rotation: %v", got)
	}
	p.Credentials.Strategy = "quota-weighted"
	used := 80.0
	r.Quotas = keyQuotaMap{p.ID + "/k1": {UsagePct: &used}}
	if got := pick().CredentialRefs; !reflect.DeepEqual(got, []string{"key_extra", "k1"}) {
		t.Fatalf("quota order: %v", got)
	}
	r.Health.EnterKeyCooldown(p.ID, "k1", time.Minute, now)
	if got := pick().CredentialRefs; !reflect.DeepEqual(got, []string{"key_extra"}) {
		t.Fatalf("one cooling: %v", got)
	}
	r.Health.EnterKeyCooldown(p.ID, "key_extra", time.Minute, now)
	if got := pick(); got.Eligible || got.RejectReason != "no available credentials" {
		t.Fatalf("all unavailable: %+v", got)
	}
	r.Health.ResetCooldown(p.ID)
	if got := pick().CredentialRefs; len(got) != 2 {
		t.Fatalf("reset: %v", got)
	}
}

func TestQuotaWeightedUsesSyncedCredentialSnapshots(t *testing.T) {
	r, snap, _, sec := setupRouterFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		remaining := 20
		if strings.Contains(req.Header.Get("Authorization"), "second-value") {
			remaining = 90
		}
		fmt.Fprintf(w, `{"code":0,"data":{"limits":[{"unit":3,"usage":100,"remaining":%d}]}}`, remaining)
	}))
	defer server.Close()
	if err := sec.Set("second", "second-value"); err != nil {
		t.Fatal(err)
	}
	p := snap.Plans["kimi-coding"]
	p.ID = "zcode"
	p.Endpoints[config.ProtocolAnthropic].BaseURL = server.URL
	p.Credentials = &config.CredentialPool{Strategy: "quota-weighted", Refs: []string{"k1", "second"}}
	syncer := quota.New(sec, t.TempDir())
	syncer.Refresh(context.Background(), map[string]*config.Plan{p.ID: p})
	r.Quotas = syncer
	got := r.evalTargets(snap, snap.Routes[0], Signals{Protocol: config.ProtocolAnthropic}, config.TierStrong, SessionCacheEntry{}, false, time.Now(), false)[0].CredentialRefs
	if !reflect.DeepEqual(got, []string{"second", "k1"}) {
		t.Fatalf("quota order: %v", got)
	}
}
