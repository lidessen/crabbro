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

func TestSafeguardAndReminderDefaults(t *testing.T) {
	var guard SafeguardSpec
	if guard.Enabled {
		t.Fatal("safeguard must be off by default")
	}
	if guard.LeadTimeDuration() != 30*time.Minute || guard.EffectiveMinUsedPercent() != 80 {
		t.Fatalf("safeguard defaults = %v / %v", guard.LeadTimeDuration(), guard.EffectiveMinUsedPercent())
	}
	custom := SafeguardSpec{Enabled: true, LeadTime: "45m", MinUsedPercent: 65}
	if custom.LeadTimeDuration() != 45*time.Minute || custom.EffectiveMinUsedPercent() != 65 {
		t.Fatalf("safeguard parsing = %v / %v", custom.LeadTimeDuration(), custom.EffectiveMinUsedPercent())
	}
	broken := SafeguardSpec{LeadTime: "soon", MinUsedPercent: -1}
	if broken.LeadTimeDuration() != 30*time.Minute || broken.EffectiveMinUsedPercent() != 80 {
		t.Fatal("broken safeguard values must fall back to defaults")
	}

	var reminder QuotaReminderSpec
	if reminder.LeadTimeDuration() != 24*time.Hour {
		t.Fatalf("default reminder lead = %v", reminder.LeadTimeDuration())
	}
	if got := (QuotaReminderSpec{LeadTime: "2h"}).LeadTimeDuration(); got != 2*time.Hour {
		t.Fatalf("reminder lead = %v", got)
	}
	if got := (QuotaReminderSpec{LeadTime: "0s"}).LeadTimeDuration(); got != 24*time.Hour {
		t.Fatalf("broken reminder lead = %v", got)
	}
}

func TestSafeguardAndReminderValidation(t *testing.T) {
	withGuard := strings.Replace(autoResetPlanYAML, "  minInterval: 5m", "  minInterval: 5m\n  safeguard:\n    enabled: true\n    leadTime: 30m\n    minUsedPercent: 80", 1)
	if _, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(withGuard)); len(diags) != 0 {
		t.Fatalf("valid safeguard rejected: %v", diags)
	}
	badLead := strings.Replace(withGuard, "leadTime: 30m", "leadTime: soon", 1)
	if _, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(badLead)); len(diags) == 0 || !strings.Contains(diags[0].Path, "safeguard.leadTime") {
		t.Fatalf("bad safeguard lead diagnostics = %v", diags)
	}
	badPct := strings.Replace(withGuard, "minUsedPercent: 80", "minUsedPercent: 0.5", 1)
	if _, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(badPct)); len(diags) == 0 || !strings.Contains(diags[0].Path, "safeguard.minUsedPercent") {
		t.Fatalf("bad safeguard threshold diagnostics = %v", diags)
	}
	withReminder := strings.Replace(autoResetPlanYAML, "  minInterval: 5m", "  minInterval: 5m\nquotaReminder:\n  leadTime: 6h", 1)
	if _, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(withReminder)); len(diags) != 0 {
		t.Fatalf("valid quotaReminder rejected: %v", diags)
	}
	badReminder := strings.Replace(withReminder, "leadTime: 6h", "leadTime: never", 1)
	if _, diags := ValidatePlanBytes("zcode.yaml", "zcode", []byte(badReminder)); len(diags) == 0 || !strings.Contains(diags[0].Path, "quotaReminder.leadTime") {
		t.Fatalf("bad quotaReminder diagnostics = %v", diags)
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
