package rule

import (
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/shared"
)

func TestNewRuleStore(t *testing.T) {
	s := NewRuleStore()
	if s.Count() != 0 {
		t.Errorf("expected 0 rules, got %d", s.Count())
	}
}

func TestCreateAndList(t *testing.T) {
	s := NewRuleStore()

	r1, err := s.Create("rule1", 100, yamlRun("function rule(ctx) { return { match: true }; }"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	r2, err := s.Create("rule2", 50, yamlRun("function rule(ctx) { return { match: false }; }"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if r1.ID == r2.ID {
		t.Error("expected distinct IDs")
	}

	if s.Count() != 2 {
		t.Errorf("expected 2 rules, got %d", s.Count())
	}

	rules := s.List()
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules in List, got %d", len(rules))
	}
}

func TestCreate_EmptyScriptFails(t *testing.T) {
	s := NewRuleStore()
	_, err := s.Create("test", 100, "")
	if err == nil {
		t.Error("expected error for empty script")
	}
}

func TestCreate_DefaultPriority(t *testing.T) {
	s := NewRuleStore()
	r, err := s.Create("test", 0, yamlRun("function rule(ctx) { return { match: true }; }"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.Priority != shared.DefaultRulePriority {
		t.Errorf("expected priority %d, got %d", shared.DefaultRulePriority, r.Priority)
	}
}

func TestGet(t *testing.T) {
	s := NewRuleStore()
	created, _ := s.Create("test", 100, yamlRun("function rule(ctx) { return { match: true }; }"))

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("Get returned wrong rule: %s vs %s", got.ID, created.ID)
	}
	if got.Script != created.Script {
		t.Errorf("script mismatch")
	}
}

func TestGet_NotFound(t *testing.T) {
	s := NewRuleStore()
	_, err := s.Get("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent rule")
	}
}

func TestUpdate(t *testing.T) {
	s := NewRuleStore()
	created, _ := s.Create("original", 100, yamlRun("function rule(ctx) { return { match: true }; }"))

	name := "updated"
	priority := 50
	script := yamlRun("function rule(ctx) { return { match: false }; }")

	updated, err := s.Update(created.ID, &name, &priority, &script)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "updated" {
		t.Errorf("name = %q, want %q", updated.Name, "updated")
	}
	if updated.Priority != 50 {
		t.Errorf("priority = %d, want %d", updated.Priority, 50)
	}
	if updated.Script != script {
		t.Errorf("script mismatch")
	}
}

func TestUpdate_Partial(t *testing.T) {
	s := NewRuleStore()
	created, _ := s.Create("original", 100, yamlRun("function rule(ctx) { return { match: true }; }"))

	name := "only-name"
	updated, err := s.Update(created.ID, &name, nil, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Name != "only-name" {
		t.Errorf("name = %q, want %q", updated.Name, "only-name")
	}
	if updated.Priority != 100 {
		t.Errorf("priority changed unexpectedly: %d", updated.Priority)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	s := NewRuleStore()
	name := "test"
	_, err := s.Update("nonexistent", &name, nil, nil)
	if err == nil {
		t.Error("expected error for nonexistent rule")
	}
}

func TestUpdate_EmptyScriptFails(t *testing.T) {
	s := NewRuleStore()
	created, _ := s.Create("test", 100, yamlRun("function rule(ctx) { return { match: true }; }"))

	empty := ""
	_, err := s.Update(created.ID, nil, nil, &empty)
	if err == nil {
		t.Error("expected error for empty script")
	}
}

func TestDelete(t *testing.T) {
	s := NewRuleStore()
	created, _ := s.Create("test", 100, yamlRun("function rule(ctx) { return { match: true }; }"))

	if err := s.Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Count() != 0 {
		t.Errorf("expected 0 rules after delete, got %d", s.Count())
	}

	// Double delete should fail.
	if err := s.Delete(created.ID); err == nil {
		t.Error("expected error for double delete")
	}
}

func TestDelete_NotFound(t *testing.T) {
	s := NewRuleStore()
	if err := s.Delete("nonexistent"); err == nil {
		t.Error("expected error for nonexistent rule")
	}
}

func TestSetEnabled(t *testing.T) {
	s := NewRuleStore()
	created, _ := s.Create("test", 100, yamlRun("function rule(ctx) { return { match: true }; }"))

	if !created.Enabled {
		t.Error("expected new rule to be enabled")
	}

	updated, err := s.SetEnabled(created.ID, false)
	if err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if updated.Enabled {
		t.Error("expected rule to be disabled")
	}

	// Re-enable.
	updated, _ = s.SetEnabled(created.ID, true)
	if !updated.Enabled {
		t.Error("expected rule to be re-enabled")
	}
}

func TestSetEnabled_NotFound(t *testing.T) {
	s := NewRuleStore()
	_, err := s.SetEnabled("nonexistent", false)
	if err == nil {
		t.Error("expected error for nonexistent rule")
	}
}

func TestEnabledRules_SortedByPriority(t *testing.T) {
	s := NewRuleStore()
	s.Create("low", 200, yamlRun("function rule(ctx) { return { match: true }; }"))
	s.Create("high", 50, yamlRun("function rule(ctx) { return { match: true }; }"))
	s.Create("mid", 100, yamlRun("function rule(ctx) { return { match: true }; }"))

	enabled := s.EnabledRules()
	if len(enabled) != 3 {
		t.Fatalf("expected 3 enabled rules, got %d", len(enabled))
	}

	// Check priority order: high(50) < mid(100) < low(200)
	if enabled[0].Priority != 50 {
		t.Errorf("expected first priority 50, got %d", enabled[0].Priority)
	}
	if enabled[1].Priority != 100 {
		t.Errorf("expected second priority 100, got %d", enabled[1].Priority)
	}
	if enabled[2].Priority != 200 {
		t.Errorf("expected third priority 200, got %d", enabled[2].Priority)
	}
}

func TestEnabledRules_OnlyEnabled(t *testing.T) {
	s := NewRuleStore()
	s.Create("e1", 100, yamlRun("function rule(ctx) { return { match: true }; }"))
	r2, _ := s.Create("e2", 100, yamlRun("function rule(ctx) { return { match: true }; }"))
	s.SetEnabled(r2.ID, false)

	enabled := s.EnabledRules()
	if len(enabled) != 1 {
		t.Errorf("expected 1 enabled rule, got %d", len(enabled))
	}
}

func TestCreate_RejectsLegacyJavaScript(t *testing.T) {
	s := NewRuleStore()

	// Legacy JavaScript is not a rule format anymore: the script must be a
	// YAML rule v1 document.
	_, err := s.Create("legacy", 100, "function rule(ctx) { return { match: true }; }")
	if err == nil {
		t.Fatal("expected error when the script is legacy JavaScript")
	}
	if !strings.Contains(err.Error(), "crabbro rules migrate") {
		t.Errorf("error should point to crabbro rules migrate, got: %s", err.Error())
	}
	if s.Count() != 0 {
		t.Errorf("expected no rule to be stored, got %d", s.Count())
	}
}

func TestUpdate_RejectsLegacyJavaScript(t *testing.T) {
	s := NewRuleStore()
	created, err := s.Create("test", 100, yamlRun("function rule(ctx) { return { match: true }; }"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	js := "function rule(ctx) { return { match: false }; }"
	_, err = s.Update(created.ID, nil, nil, &js)
	if err == nil {
		t.Fatal("expected error when the script is legacy JavaScript")
	}
	if !strings.Contains(err.Error(), "crabbro rules migrate") {
		t.Errorf("error should point to crabbro rules migrate, got: %s", err.Error())
	}

	// The rejected script must not replace the stored one.
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Script != yamlRun("function rule(ctx) { return { match: true }; }") {
		t.Error("script should be unchanged after a rejected update")
	}
}
