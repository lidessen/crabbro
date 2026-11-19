package rule

import (
	"os"
	"path/filepath"
	"testing"
)

func ruleText(id string) string {
	return "version: 1\nid: " + id + "\non:\n  request:\n    host: example.com\nrequest:\n  pipeline:\n    - uses: crabbro.tag\n      with:\n        tags: [x]\n"
}

func has(s *RuleStore, id string) bool {
	for _, r := range s.EnabledRules() {
		if string(r.ID) == id {
			return true
		}
	}
	return false
}

// A rule whose file breaks in an edit keeps running as it was; deleting the
// file is what removes it.
func TestSyncKeepsLastGoodVersionOfABrokenFile(t *testing.T) {
	dir := t.TempDir()
	d := OpenDir(dir)
	store := NewRuleStore()
	path := filepath.Join(dir, "guard.yaml")
	if err := os.WriteFile(path, []byte(ruleText("guard")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, complete := d.Sync(store); !complete || !has(store, "guard") {
		t.Fatal("a good rule did not load")
	}
	if err := os.WriteFile(path, []byte("version: 1\nid: guard\nbogus: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diags, complete := d.Sync(store)
	if complete || len(diags) == 0 {
		t.Errorf("a broken file was not reported: complete=%v diags=%v", complete, diags)
	}
	if !has(store, "guard") {
		t.Error("the rule stopped running because its file broke")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	d.Sync(store)
	if has(store, "guard") {
		t.Error("a deleted file's rule is still running")
	}
}

// Ids come from the API. None may reach outside the rules directory.
func TestConfigOperationsRefuseBadIDs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rules")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d := OpenDir(dir)
	outside := filepath.Join(root, "x.yaml.draft")
	if err := os.WriteFile(outside, []byte(ruleText("x")), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../x", "../../x", "a/b", "", "UPPER", ".hidden"} {
		if err := d.Discard(id); err == nil {
			t.Errorf("Discard(%q) accepted", id)
		}
		if err := d.Commit(id); err == nil {
			t.Errorf("Commit(%q) accepted", id)
		}
		if diags := d.ValidateFile(id, true, nil); len(diags) == 0 {
			t.Errorf("ValidateFile(%q) accepted", id)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("a file outside the directory was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "x.yaml")); err == nil {
		t.Error("Commit wrote outside the directory")
	}
}

// Commit leaves the live file complete and no temporary file behind.
func TestCommitReplacesTheLiveFileWhole(t *testing.T) {
	dir := t.TempDir()
	d := OpenDir(dir)
	if err := os.WriteFile(filepath.Join(dir, "r.yaml"), []byte(ruleText("r")), 0o600); err != nil {
		t.Fatal(err)
	}
	next := ruleText("r") + "# v2\n"
	if err := d.Draft("r", []byte(next)); err != nil {
		t.Fatal(err)
	}
	if err := d.Commit("r"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "r.yaml"))
	if err != nil || string(got) != next {
		t.Fatalf("live file = %q, %v", got, err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "r.yaml" {
			t.Errorf("leftover %q", e.Name())
		}
	}
}

// The rules directory's limits are enforced, not just reported.
func TestFileLimitIsEnforced(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxRuleFiles+3; i++ {
		id := "r" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		if err := os.WriteFile(filepath.Join(dir, id+".yaml"), []byte(ruleText(id)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rules, diags, complete := OpenDir(dir).Load()
	if complete || len(rules) != maxRuleFiles || len(diags) != 3 {
		t.Errorf("loaded %d, diags %d, complete %v", len(rules), len(diags), complete)
	}
}
