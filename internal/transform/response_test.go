package transform

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestResponseNormalization(t *testing.T) {
	h := http.Header{"X-Remove": {"old"}, "X-Order": {"old"}}
	input := []byte(`{"code":0,"data":{"usage":{"in":7,"out":2},"drop":true}}`)
	endpoint := ResponseSpec{UnwrapPath: "data", StripHeaders: []string{"x-remove"}, SetHeaders: map[string]string{"X-Order": "endpoint"}, RenameBodyFields: map[string]string{"usage.in": "usage.prompt_tokens"}}
	model := ResponseSpec{RenameBodyFields: map[string]string{"usage.out": "usage.completion_tokens"}, DropBodyFields: []string{"drop"}, SetBodyFields: map[string]any{"model": "test"}, SetHeaders: map[string]string{"X-Order": "model"}}
	outH, out, err := Response(h, input, "", endpoint, model)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"model":"test","usage":{"completion_tokens":2,"prompt_tokens":7}}` || outH.Get("X-Order") != "model" || outH.Get("X-Remove") != "" {
		t.Fatalf("headers=%v body=%s", outH, out)
	}
	if h.Get("X-Remove") != "old" || !bytes.Contains(input, []byte(`"code"`)) {
		t.Fatal("input mutated")
	}
	_, out, err = Response(nil, input, "", ResponseSpec{ExtractField: "data"})
	if err != nil || !bytes.Contains(out, []byte(`"usage"`)) {
		t.Fatalf("extract: %s %v", out, err)
	}
	for _, spec := range []ResponseSpec{{UnwrapPath: "data", ExtractField: "data"}, {UnwrapPath: "missing"}, {UnwrapPath: "data.usage.in"}, {SetHeaders: map[string]string{"Authorization": "x"}}} {
		if _, _, err := Response(nil, input, "", spec); err == nil {
			t.Errorf("accepted %#v", spec)
		}
	}
	if _, err := ReadResponse(strings.NewReader(strings.Repeat("x", MaxResponseBody+1))); err == nil {
		t.Fatal("oversized input accepted")
	}
	if _, _, err := Response(nil, []byte(`{}`), "", ResponseSpec{SetBodyFields: map[string]any{"large": strings.Repeat("x", MaxResponseBody)}}); err == nil {
		t.Fatal("oversized output accepted")
	}
}
