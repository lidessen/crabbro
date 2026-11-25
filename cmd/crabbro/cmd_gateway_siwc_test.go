package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGatewaySIWCStatus(t *testing.T) {
	input := `{"plans":[{"id":"account","healthy":true,"siwc":{"sessionRef":"session","hostIdRef":"host","accessToken":"private-token"},"lastError":"private-error"},{"id":"api","healthy":false,"siwc":null,"credential":"private-key"}]}`
	for _, pretty := range []bool{false, true} {
		var buf bytes.Buffer
		if err := renderGatewaySIWCStatus(&buf, strings.NewReader(input), pretty); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		for _, secret := range []string{"private-token", "private-error", "private-key", "api"} {
			if strings.Contains(out, secret) {
				t.Errorf("output contains %q: %s", secret, out)
			}
		}
		if pretty {
			if !strings.Contains(out, "planId=account sessionRef=session hostIdRef=host healthy=true") {
				t.Errorf("unexpected pretty output: %s", out)
			}
		} else {
			var entries []gatewaySIWCStatusEntry
			if err := json.Unmarshal([]byte(out), &entries); err != nil || len(entries) != 1 || entries[0] != (gatewaySIWCStatusEntry{"account", "session", "host", true}) {
				t.Errorf("unexpected JSON output: %s", out)
			}
		}
	}
}

func TestGatewaySIWCStatusRejectsMissingMetadata(t *testing.T) {
	var out bytes.Buffer
	err := renderGatewaySIWCStatus(&out, strings.NewReader(`{"plans":[{"id":"account","healthy":true}]}`), false)
	if err == nil || !strings.Contains(err.Error(), "daemon update required") {
		t.Errorf("expected unsupported daemon error, got %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("unexpected output: %q", out.String())
	}
}

func TestGatewaySIWCStatusEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := renderGatewaySIWCStatus(&out, strings.NewReader(`{"plans":[{"id":"api","siwc":null}]}`), false); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("unexpected output: %q", out.String())
	}
}

func TestGatewaySIWCStatusRejectsArguments(t *testing.T) {
	if err := cmdGateway(&clientConfig{}, []string{"siwc-status", "extra"}); err == nil {
		t.Fatal("expected argument error")
	}
}
