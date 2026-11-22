package config

import (
	"strings"
	"testing"
	"time"
)

const autoResetPlanYAML = `version: 1
id: zcode
credential: zcode_key
autoReset:
  enabled: true
  scopes: [fiveHour]
  expiringWithin: 24h
  minInterval: 5m
endpoints:
  anthropic:
    baseUrl: https://open.bigmodel.cn/api/anthropic
models:
  glm-4.7:
    tier: strong
`

func TestAutoResetSpecDefaults(t *testing.T) {
	var empty AutoResetSpec
	if got := empty.EffectiveAutoResetScopes(); len(got) != 2 || got[0] != AutoResetScopeFiveHour || got[1] != AutoResetScopeWeekly {
		t.Fatalf("default scopes = %v", got)
	}
	if empty.ExpiringWithinDuration() != 0 {
		t.Fatal("empty expiringWithin must mean any expiry")
	}
	if empty.MinIntervalDuration() != 5*time.Minute {
		t.Fatalf("default minInterval = %v", empty.MinIntervalDuration())
	}
	spec := AutoResetSpec{Enabled: true, Scopes: []string{"weekly", "bogus", "weekly"}, ExpiringWithin: "2h", MinInterval: "30s"}
	if got := spec.EffectiveAutoResetScopes(); len(got) != 1 || got[0] != AutoResetScopeWeekly {
		t.Fatalf("scopes = %v", got)
	}
	if !spec.AllowsAutoResetScope(AutoResetScopeWeekly) || spec.AllowsAutoResetScope(AutoResetScopeFiveHour) {
		t.Fatal("scope filter is wrong")
	}
	if spec.ExpiringWithinDuration() != 2*time.Hour || spec.MinIntervalDuration() != 30*time.Second {
		t.Fatal("duration parsing is wrong")
	}
	// Unparsable durations fall back to safe defaults.
	broken := AutoResetSpec{ExpiringWithin: "soon", MinInterval: "-5m"}
	if broken.ExpiringWithinDuration() != 0 || broken.MinIntervalDuration() != 5*time.Minute {
		t.Fatal("broken durations must fail closed")
	}
}

func TestAutoResetValidation(t *testing.T) {
	plan, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(autoResetPlanYAML))
	if len(diags) != 0 {
		t.Fatalf("valid autoReset plan rejected: %v", diags)
	}
	if plan == nil || !plan.AutoReset.Enabled || len(plan.AutoReset.Scopes) != 1 || plan.AutoReset.Scopes[0] != "fiveHour" {
		t.Fatalf("parsed autoReset policy = %+v", plan.AutoReset)
	}

	badScope := strings.Replace(autoResetPlanYAML, "scopes: [fiveHour]", "scopes: [hourly]", 1)
	if _, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(badScope)); len(diags) == 0 || !strings.Contains(diags[0].Path, "autoReset.scopes") {
		t.Fatalf("bad scope diagnostics = %v", diags)
	}
	badHorizon := strings.Replace(autoResetPlanYAML, "expiringWithin: 24h", "expiringWithin: soon", 1)
	if _, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(badHorizon)); len(diags) == 0 || !strings.Contains(diags[0].Path, "autoReset.expiringWithin") {
		t.Fatalf("bad horizon diagnostics = %v", diags)
	}
	badInterval := strings.Replace(autoResetPlanYAML, "minInterval: 5m", "minInterval: 0s", 1)
	if _, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(badInterval)); len(diags) == 0 || !strings.Contains(diags[0].Path, "autoReset.minInterval") {
		t.Fatalf("bad interval diagnostics = %v", diags)
	}
	// The schema itself rejects unknown keys inside autoReset.
	unknownKey := strings.Replace(autoResetPlanYAML, "  minInterval: 5m", "  minInterval: 5m\n  couponId: 42", 1)
	if _, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(unknownKey)); len(diags) == 0 {
		t.Fatal("unknown autoReset key accepted")
	}
}
