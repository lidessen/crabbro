package router

import (
	"reflect"
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
