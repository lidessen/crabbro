package transform

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func str(s string) *string { return &s }
func TestApply(t *testing.T) {
	u, _ := url.Parse("https://example.com/v1/messages?keep=1&remove=2")
	h := http.Header{"Anthropic-Beta": {"feature"}, "X-Stainless-Lang": {"go"}, "User-Agent": {"agent"}, "Content-Length": {"200"}}
	input := []byte(`{"nested":{"old":12345678901234567890,"remove":1},"drop":true}`)
	s := Spec{StripHeaders: []string{"ANTHROPIC-beta", "x-stainless-*", "user-agent"}, SetHeaders: map[string]string{"X-Test": "one"}, Path: str("/custom"), PathPrefix: str("/api"), Query: map[string]*string{"remove": nil, "keep": str("new")}, DropBodyFields: []string{"drop", "nested.remove"}, RenameBodyFields: map[string]string{"nested.old": "renamed.value"}, SetBodyFields: map[string]any{"nested.new": true}}
	got, headers, body, err := Apply(u, h, input, s)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "https://example.com/api/custom?keep=new" {
		t.Fatal(got)
	}
	if headers.Get("Anthropic-Beta") != "" || headers.Get("X-Stainless-Lang") != "" || headers.Get("User-Agent") != "" || headers.Get("Content-Length") != "" || headers.Get("X-Test") != "one" {
		t.Fatal(headers)
	}
	var obj map[string]any
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"nested": map[string]any{"new": true}, "renamed": map[string]any{"value": json.Number("12345678901234567890")}}
	if !reflect.DeepEqual(obj, want) {
		t.Fatalf("%v", obj)
	}
	if u.Path != "/v1/messages" || h.Get("Anthropic-Beta") != "feature" || string(input) != `{"nested":{"old":12345678901234567890,"remove":1},"drop":true}` {
		t.Fatal("input mutated")
	}
}
func TestErrors(t *testing.T) {
	u, _ := url.Parse("https://example.com/")
	for _, tc := range []struct {
		body      string
		s         Spec
		protected string
	}{
		{`{}`, Spec{SetHeaders: map[string]string{"aUtHoRiZaTiOn": "bad"}}, ""},
		{`{}`, Spec{SetHeaders: map[string]string{"X-Custom": "bad"}}, "x-custom"},
		{`{}`, Spec{SetHeaders: map[string]string{"Content-Length": "1"}}, ""},
		{`[]`, Spec{SetBodyFields: map[string]any{"x": 1}}, ""},
		{`{} {}`, Spec{DropBodyFields: []string{"x"}}, ""},
		{`{"x":1}`, Spec{SetBodyFields: map[string]any{"x.y": 1}}, ""},
		{`{}`, Spec{RenameBodyFields: map[string]string{"a": "a.b"}}, ""},
	} {
		if _, _, _, err := Apply(u, nil, []byte(tc.body), tc.s, tc.protected); err == nil {
			t.Errorf("expected error: %+v", tc.s)
		}
	}
}
