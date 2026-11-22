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

func TestTruncatedPreviewKeepsStringContents(t *testing.T) {
	source := `{"text":"braces {}, colon: and escaped \" quote","next":[1,2`
	got, _ := readableBody(source, "application/json", true)
	if !strings.Contains(got, `"text": "braces {}, colon: and escaped \" quote"`) {
		t.Fatalf("string changed: %q", got)
	}
}
