package config

import (
	"fmt"
	"testing"
)

func TestSIWCPlanValidation(t *testing.T) {
	base := "version: 1\nid: siwc-plan\n%s\nendpoints:\n%s\nmodels:\n  model: {}\n"
	endpoint := "  openai-responses:\n    baseUrl: https://api.openai.com/v1"
	for _, tc := range []struct {
		name, auth, endpoints string
		valid                 bool
	}{
		{"session references", "siwc:\n  sessionRef: session\n  hostIdRef: host", endpoint, true},
		{"same reference", "siwc:\n  sessionRef: session\n  hostIdRef: session", endpoint, false},
		{"missing host", "siwc:\n  sessionRef: session", endpoint, false},
		{"plaintext field", "siwc:\n  sessionRef: session\n  hostIdRef: host\n  accessToken: secret", endpoint, false},
		{"mixed auth", "credential: key\nsiwc:\n  sessionRef: session\n  hostIdRef: host", endpoint, false},
		{"untrusted endpoint", "siwc:\n  sessionRef: session\n  hostIdRef: host", "  openai-responses:\n    baseUrl: https://example.com/v1", false},
		{"wrong protocol", "siwc:\n  sessionRef: session\n  hostIdRef: host", "  openai-chat:\n    baseUrl: https://api.openai.com/v1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, diags := ValidatePlanBytes("plan.yaml", "siwc-plan", []byte(fmt.Sprintf(base, tc.auth, tc.endpoints)))
			if (len(diags) == 0) != tc.valid {
				t.Fatalf("valid = %v, diagnostics = %+v", tc.valid, diags)
			}
			if tc.valid && (plan.SIWC.HostIDRef != "host" || len(plan.CredentialRefs()) != 1 || plan.CredentialRefs()[0] != "session") {
				t.Fatalf("incorrect SIWC references: %+v", plan.SIWC)
			}
		})
	}
}

func TestSIWCProxyURLRejected(t *testing.T) {
	base := `version: 1
id: test
%s
proxyUrl: https://proxy.example.com
endpoints:
  openai-responses:
    baseUrl: https://api.openai.com/v1
models:
  model: {}
`
	for _, tc := range []struct {
		name, auth string
		wantError  bool
	}{
		{"siwc", "siwc:\n  sessionRef: session\n  hostIdRef: host", true},
		{"credential", "credential: key", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, diags := ValidatePlanBytes("test.yaml", "test", []byte(fmt.Sprintf(base, tc.auth)))
			if tc.wantError {
				if len(diags) != 1 || diags[0].Path != "$.proxyUrl" || diags[0].Message != "proxyUrl is not allowed for SIWC plans" {
					t.Fatalf("unexpected diagnostics: %+v", diags)
				}
			} else if len(diags) != 0 {
				t.Fatalf("unexpected diagnostics: %+v", diags)
			}
		})
	}
}
