package config

import (
	"strings"
	"testing"
)

func TestCredentialPoolValidation(t *testing.T) {
	base := "version: 1\nid: pool\n%s\nendpoints:\n  openai-chat:\n    baseUrl: https://example.com\nmodels:\n  model:\n    endpoints: [openai-chat]\n"
	cases := []struct {
		name, credential string
		valid            bool
	}{
		{"single", "credential: first", true},
		{"pool", "credentials:\n  refs: [first, second]\n  strategy: round-robin", true},
		{"default", "credentials:\n  refs: [first]", true},
		{"both", "credential: first\ncredentials:\n  refs: [second]", false},
		{"neither", "", false},
		{"duplicate", "credentials:\n  refs: [first, first]", false},
		{"empty", "credentials:\n  refs: []", false},
		{"blank", "credentials:\n  refs: ['']", false},
		{"strategy", "credentials:\n  refs: [first]\n  strategy: random", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, diags := ValidatePlanBytes("pool.yaml", "pool", []byte(strings.Replace(base, "%s", tc.credential, 1)))
			if (len(diags) == 0) != tc.valid {
				t.Fatalf("diagnostics: %+v", diags)
			}
			if tc.valid && (plan.Credential != "first" || len(plan.CredentialRefs()) == 0) {
				t.Fatalf("pool not normalized: %+v", plan)
			}
		})
	}
}
