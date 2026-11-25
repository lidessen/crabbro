package secret

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreCRUDAndValidation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	s := New(dir)

	if err := s.Set("glm_key.1", "sk-secret-value-123456\n"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !s.Has("glm_key.1") {
		t.Fatal("Has returned false")
	}
	got, err := s.Get("glm_key.1")
	if err != nil || got != "sk-secret-value-123456" {
		t.Fatalf("Get = %q, %v", got, err)
	}

	fi, err := os.Stat(filepath.Join(dir, "glm_key.1"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %o, want 0600", perm)
	}

	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].Name != "glm_key.1" {
		t.Fatalf("List = %+v, %v", list, err)
	}

	for _, bad := range []string{"", "../escape", "a/b", ".hidden", "Upper", "a..b"} {
		if err := s.Set(bad, "val123456"); err == nil {
			t.Errorf("Set(%q) succeeded, expected error", bad)
		}
		if _, err := s.Get(bad); err == nil {
			t.Errorf("Get(%q) succeeded, expected error", bad)
		}
	}

	// Reject symlinks
	link := filepath.Join(dir, "sym_key")
	if err := os.Symlink(filepath.Join(dir, "glm_key.1"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("sym_key"); err == nil {
		t.Fatal("Get accepted symlink")
	}
	if err := s.Set("sym_key", "overwrite"); err == nil {
		t.Fatal("Set accepted symlink")
	}

	if err := s.Delete("glm_key.1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Has("glm_key.1") {
		t.Fatal("Has true after Delete")
	}
}

func TestScrubbers(t *testing.T) {
	u := ScrubURL("https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?key=AIzaSyDummySecret123&alt=sse")
	if strings.Contains(u, "AIzaSyDummySecret123") || strings.Contains(u, "key=") {
		t.Fatalf("ScrubURL leaked key: %s", u)
	}
	if !strings.Contains(u, "alt=sse") {
		t.Fatalf("ScrubURL dropped non-secret param: %s", u)
	}

	h := http.Header{}
	h.Set("Authorization", "Bearer sk-abc123456")
	h.Set("X-Api-Key", "ant-secret-123")
	h.Set("X-Goog-Api-Key", "AIza123456")
	h.Set("X-Custom-Auth", "token=my-secret-9999")
	h.Set("Content-Type", "application/json")
	scrubbed := ScrubHeaders(h, []string{"my-secret-9999", "short"}, "X-Trace-Secret")
	if scrubbed.Get("Authorization") != "" || scrubbed.Get("X-Api-Key") != "" || scrubbed.Get("X-Goog-Api-Key") != "" {
		t.Fatalf("auth headers not stripped: %v", scrubbed)
	}
	if got := scrubbed.Get("X-Custom-Auth"); got != "token=[REDACTED]" {
		t.Fatalf("X-Custom-Auth = %q", got)
	}
	if scrubbed.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type lost: %v", scrubbed)
	}

	// Short strings (<6 chars) are ignored to avoid corrupting normal text; longest matched first.
	txt := ScrubText("key=abcdef123456 sub=abcdef tiny=abc", "abcdef", "abcdef123456", "abc")
	if txt != "key=[REDACTED] sub=[REDACTED] tiny=abc" {
		t.Fatalf("ScrubText = %q", txt)
	}

	m := ScrubAny(map[string]any{
		"nested": []any{"val=abcdef123456", 42},
	}, "abcdef123456").(map[string]any)
	if m["nested"].([]any)[0] != "val=[REDACTED]" {
		t.Fatalf("ScrubAny = %#v", m)
	}
}

func TestSetRestoresDirectoryOwnerPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := New(dir).Set("example", "value"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %o", info.Mode().Perm())
	}
}
