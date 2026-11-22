package main

import (
	"strings"
	"testing"
)

func TestRequestBodyDisplay(t *testing.T) {
	tests := []struct {
		name, source, want string
		truncated, raw     bool
	}{
		{"formatted", `{"model":"test","messages":[{"text":"hello"}]}`, "{\n  \"model\": \"test\",\n  \"messages\": [\n    {\n      \"text\": \"hello\"\n    }\n  ]\n}", false, false},
		{"raw", `{"model":"test"}`, `{"model":"test"}`, false, true},
		{"truncated", `{"model":"test","messages":[{"text":"hello, \\"world`, "{\n  \"model\": \"test\",\n  \"messages\": [\n    {\n      \"text\": \"hello, \\\\\"world", true, false},
		{"truncated raw", `{"model":"test","messages":[`, `{"model":"test","messages":[`, true, true},
		{"invalid untruncated", `{"model":`, `{"model":`, false, false},
		{"complete truncated preview", `{"model":"test"}`, "{\n  \"model\": \"test\"\n}", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			formatted, _ := readableBody(tt.source, "application/json", tt.truncated)
			got := bodyDisplay(tt.source, formatted, tt.raw)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadableBodyByteOrderMark(t *testing.T) {
	t.Run("complete JSON formats", func(t *testing.T) {
		source := "\\ufeff" + `{"model":"test"}`
		got, filename := readableBody(source, "application/json", false)
		want := "{\n  \"model\": \"test\"\n}"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if filename != "body.json" {
			t.Errorf("filename = %q, want body.json", filename)
		}
	})
	t.Run("broken JSON stays as captured", func(t *testing.T) {
		source := "\\ufeff" + `{"model":`
		got, filename := readableBody(source, "application/json", false)
		if got != source {
			t.Errorf("got %q, want the captured source %q", got, source)
		}
		if filename != "body.txt" {
			t.Errorf("filename = %q, want body.txt", filename)
		}
	})
	t.Run("empty body unchanged", func(t *testing.T) {
		got, filename := readableBody("", "application/json", false)
		if got != "" {
			t.Errorf("got %q, want unchanged empty body", got)
		}
		if filename != "body.txt" {
			t.Errorf("filename = %q, want body.txt", filename)
		}
	})
}

func TestTruncatedPreviewKeepsStringContents(t *testing.T) {
	source := `{"text":"braces {}, colon: and escaped \" quote","next":[1,2`
	got, _ := readableBody(source, "application/json", true)
	if !strings.Contains(got, `"text": "braces {}, colon: and escaped \" quote"`) {
		t.Fatalf("string changed: %q", got)
	}
}
