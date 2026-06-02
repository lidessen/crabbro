package fixture

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNew_DefaultDir(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatalf("New() with empty dir: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".chiling", "fixtures")
	if s.BaseDir() != want {
		t.Errorf("BaseDir = %q, want %q", s.BaseDir(), want)
	}
	// Cleanup
	os.RemoveAll(filepath.Join(home, ".chiling", "fixtures"))
}

func TestNew_CustomDir(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New() with custom dir: %v", err)
	}
	if s.BaseDir() != dir {
		t.Errorf("BaseDir = %q, want %q", s.BaseDir(), dir)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("base directory was not created")
	}
}

func TestSaveAndGet(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	f := FixtureData{
		CreatedAt: now,
		Request: FixtureRequest{
			Method:      "POST",
			Host:        "example.com",
			Path:        "/api/test",
			URL:         "https://example.com/api/test",
			RequestHeaders: map[string]string{"Content-Type": "application/json"},
			RequestBody: `{"key":"value"}`,
			Status:      200,
			ResponseBody: `{"ok":true}`,
		},
	}

	if err := s.Save("test-fixture", f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify the file exists
	fpath := filepath.Join(dir, "test-fixture.json")
	if _, err := os.Stat(fpath); os.IsNotExist(err) {
		t.Error("fixture file was not created")
	}

	// Retrieve and verify
	got, err := s.Get("test-fixture")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "test-fixture" {
		t.Errorf("Name = %q, want %q", got.Name, "test-fixture")
	}
	if got.Request.Method != "POST" {
		t.Errorf("Method = %q, want %q", got.Request.Method, "POST")
	}
	if got.Request.ResponseBody != `{"ok":true}` {
		t.Errorf("ResponseBody = %q, want %q", got.Request.ResponseBody, `{"ok":true}`)
	}
}

func TestSave_Overwrite(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	f1 := FixtureData{Request: FixtureRequest{Method: "GET"}}
	f2 := FixtureData{Request: FixtureRequest{Method: "DELETE"}}

	if err := s.Save("same-name", f1); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("same-name", f2); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get("same-name")
	if got.Request.Method != "DELETE" {
		t.Errorf("after overwrite Method = %q, want %q", got.Request.Method, "DELETE")
	}
}

func TestGet_NotFound(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	_, err := s.Get("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent fixture")
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	// Empty directory
	summaries, err := s.List()
	if err != nil {
		t.Fatalf("List on empty: %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("expected 0 summaries, got %d", len(summaries))
	}

	// Add some fixtures
	names := []string{"z-last", "a-first", "m-middle"}
	for _, name := range names {
		s.Save(name, FixtureData{
			Request: FixtureRequest{
				Method: "GET",
				Host:   "example.com",
				Path:   "/" + name,
			},
		})
	}

	summaries, _ = s.List()
	if len(summaries) != 3 {
		t.Fatalf("expected 3 summaries, got %d", len(summaries))
	}
	// Verify alphabetical order
	if summaries[0].Name != "a-first" || summaries[1].Name != "m-middle" || summaries[2].Name != "z-last" {
		t.Errorf("wrong order: %v", summaries)
	}
}

func TestList_SkipsCorruptFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	s.Save("good", FixtureData{Request: FixtureRequest{Method: "GET"}})
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{not json}"), 0644)
	os.WriteFile(filepath.Join(dir, "not-json.txt"), []byte("hello"), 0644)

	summaries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Errorf("expected 1 valid summary, got %d", len(summaries))
	}
}

func TestDelete(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	s.Save("to-delete", FixtureData{Request: FixtureRequest{Method: "GET"}})

	if err := s.Delete("to-delete"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify it's gone
	_, err := s.Get("to-delete")
	if err == nil {
		t.Error("expected error after delete")
	}

	// Double-delete should fail
	err = s.Delete("to-delete")
	if err == nil {
		t.Error("expected error for double-delete")
	}
}

func TestValidateName_RejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	err := s.Save("", FixtureData{})
	if err == nil {
		t.Error("expected error for empty name")
	}
}

func TestValidateName_RejectsPathSeparator(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	tests := []string{"a/b", "a\\b", "../etc"}
	for _, name := range tests {
		if err := s.Save(name, FixtureData{}); err == nil {
			t.Errorf("expected error for name %q", name)
		}
		if _, err := s.Get(name); err == nil {
			t.Errorf("expected error for Get with name %q", name)
		}
		if err := s.Delete(name); err == nil {
			t.Errorf("expected error for Delete with name %q", name)
		}
	}
}

func TestSave_SetsNameAndTimestamp(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)

	f := FixtureData{Request: FixtureRequest{Method: "GET"}}
	if err := s.Save("auto-fields", f); err != nil {
		t.Fatal(err)
	}

	got, _ := s.Get("auto-fields")
	if got.Name != "auto-fields" {
		t.Errorf("Name = %q, want %q", got.Name, "auto-fields")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
}
