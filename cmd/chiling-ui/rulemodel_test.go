package main

import (
	"strings"
	"testing"
)

// A body that holds `"on":` must come back byte-for-byte: only the
// document's own top-level on key loses its quotes.
func TestMarshalRuleDocKeepsOnInBodies(t *testing.T) {
	src := "version: 1\nid: r\non:\n  request:\n    host: example.com\nrequest:\n  pipeline:\n    - uses: chiling.mock-response\n      with:\n        status: 200\n        body: '{\"on\":true}'\n"
	doc, err := parseRuleDoc(src)
	if err != nil {
		t.Fatal(err)
	}
	out, err := marshalRuleDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\non:\n") && !strings.HasPrefix(out, "on:") {
		t.Errorf("top-level on key was not written bare:\n%s", out)
	}
	if !strings.Contains(out, `{"on":true}`) {
		t.Errorf("body text was rewritten:\n%s", out)
	}
	if again, err := parseRuleDoc(out); err != nil {
		t.Errorf("output does not parse: %v\n%s", err, out)
	} else if again.On.Request == nil || again.On.Request.Host != "example.com" {
		t.Errorf("match lost in round trip:\n%s", out)
	}
}
