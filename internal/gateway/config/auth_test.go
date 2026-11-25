package config

import (
	"bytes"
	"fmt"
	"github.com/lidessen/crabbro/internal/gateway/provider"
	"os"
	"testing"
)

func TestAuthNormalization(t *testing.T) {
	base := "version: 1\nid: test\nprovider: openai\n%s\nendpoints:\n  openai-responses:\n    baseUrl: https://api.openai.com/v1\nmodels:\n  model: {}\n"
	tests := []struct {
		name, input string
		kind        provider.AuthKind
		ref         string
		valid       bool
	}{
		{"legacy key", "credential: key", provider.APIKey, "key", true},
		{"legacy pool", "credentials:\n  refs: [key, other]\n  strategy: round-robin", provider.KeyPool, "key", true},
		{"legacy siwc", "siwc:\n  sessionRef: session\n  hostIdRef: host", provider.OAuthSIWC, "session", true},
		{"new key", "auth: {kind: api-key, ref: key}", provider.APIKey, "key", true},
		{"new pool", "auth: {kind: key-pool, refs: [key, other], strategy: round-robin}", provider.KeyPool, "key", true},
		{"new siwc", "auth: {kind: oauth-siwc, sessionRef: session, hostIdRef: host}", provider.OAuthSIWC, "session", true},
		{"mixed", "credential: key\nauth: {kind: api-key, ref: key}", "", "", false},
		{"wrong field", "auth: {kind: api-key, ref: key, sessionRef: session}", "", "", false},
		{"duplicate", "auth: {kind: key-pool, refs: [key, key]}", "", "", false},
		{"identical siwc refs", "auth: {kind: oauth-siwc, sessionRef: session, hostIdRef: session}", "", "", false},
		{"siwc proxy", "auth: {kind: oauth-siwc, sessionRef: session, hostIdRef: host}\nproxyUrl: https://example.com", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, d := ValidatePlanBytes("test.yaml", "test", []byte(fmt.Sprintf(base, tc.input)))
			if (len(d) == 0) != tc.valid {
				t.Fatalf("diagnostics: %+v", d)
			}
			if tc.valid && (plan.Provider != "openai" || plan.Auth == nil || plan.Auth.Kind != tc.kind || plan.CredentialRefs()[0] != tc.ref) {
				t.Fatalf("incorrect normalization: %+v", plan)
			}
			if tc.valid && tc.kind == provider.KeyPool && plan.CredentialStrategy() != "round-robin" {
				t.Fatal("pool strategy lost")
			}
		})
	}
	untrusted := fmt.Sprintf(base, "auth: {kind: oauth-siwc, sessionRef: session, hostIdRef: host}")
	untrusted = string(bytes.ReplaceAll([]byte(untrusted), []byte("https://api.openai.com/v1"), []byte("https://example.com/v1")))
	if _, d := ValidatePlanBytes("test.yaml", "test", []byte(untrusted)); len(d) == 0 {
		t.Fatal("unpinned endpoint accepted")
	}
}

func TestPlanSchemasMatch(t *testing.T) {
	external, err := os.ReadFile("../../../schemas/gateway-plan.v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(external, planSchemaJSON) {
		t.Fatal("embedded and external plan schemas differ")
	}
}
