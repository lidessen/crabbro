package transform

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
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

func TestSetBodyFieldsDoesNotMutateSpec(t *testing.T) {
	u, _ := url.Parse("https://example.com/")
	original := map[string]any{
		"nested": map[string]any{"existing": "value"},
		"items":  []any{map[string]any{"key": []any{"value"}}},
	}
	s := Spec{SetBodyFields: map[string]any{
		"a":            original,
		"a.nested.new": true,
	}}
	want := map[string]any{
		"nested": map[string]any{"existing": "value"},
		"items":  []any{map[string]any{"key": []any{"value"}}},
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, body, err := Apply(u, nil, []byte(`{}`), s)
			if err != nil {
				t.Errorf("Apply: %v", err)
				return
			}
			if string(body) != `{"a":{"items":[{"key":["value"]}],"nested":{"existing":"value","new":true}}}` {
				t.Errorf("unexpected body: %s", body)
			}
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(original, want) {
		t.Fatalf("spec mutated: %#v", original)
	}

	clone := cloneBodyValue(original).(map[string]any)
	clone["items"].([]any)[0].(map[string]any)["key"].([]any)[0] = "changed"
	if !reflect.DeepEqual(original, want) {
		t.Fatalf("nested slice or map shared with clone: %#v", original)
	}

	s.SetBodyFields["a.nested.new.child"] = true
	if _, _, _, err := Apply(u, nil, []byte(`{}`), s); err == nil {
		t.Fatal("expected error for non-object parent")
	}
	if !reflect.DeepEqual(original, want) {
		t.Fatalf("spec mutated on error: %#v", original)
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
