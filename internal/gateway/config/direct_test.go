package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestDirectPlanValidation(t *testing.T) {
	base := `version: 1
id: test
credential: key
%s
endpoints:
  anthropic: {baseUrl: "https://example.com"}
models:
  model:
    endpoints: [%s]
`
	for _, tc := range []struct {
		direct, endpoints string
		valid             bool
	}{
		{"direct: {enabled: false, profiles: [work]}", "anthropic", true},
		{"direct: {profiles: [INVALID]}", "anthropic", false},
		{"", "openai-chat", false}, {"", "unknown", false}, {"", "anthropic, anthropic", false},
	} {
		_, diags := ValidatePlanBytes("test.yaml", "test", []byte(fmt.Sprintf(base, tc.direct, tc.endpoints)))
		if (len(diags) == 0) != tc.valid {
			t.Fatalf("%+v: %+v", tc, diags)
		}
	}
	p, diags := ValidatePlanBytes("test.yaml", "test", []byte(fmt.Sprintf(base, "direct: {enabled: false, profiles: [work]}", "anthropic")))
	if len(diags) > 0 || p.DirectEnabled() || !p.AllowsDirectProfile("work") || p.AllowsDirectProfile("") {
		t.Fatalf("plan=%+v diags=%+v", p, diags)
	}
	p.Endpoints[ProtocolOpenAIChat] = &EndpointSpec{}
	route := &Route{ID: "r", Targets: []Target{{Plan: "test", Model: "model", EndpointProtocol: ProtocolOpenAIChat}}}
	diags = ValidateCrossFile(map[string]*Plan{"test": p}, map[string]*Route{"r": route})
	if len(diags) == 0 || !strings.Contains(diags[0].Path, "endpointProtocol") {
		t.Fatalf("diags=%+v", diags)
	}
}

func TestDirectFallbackRouteOptInValidation(t *testing.T) {
	base := `version: 1
id: test
credential: key
direct: {fallbackRoute: %s}
endpoints:
  anthropic: {baseUrl: "https://example.com"}
models:
  model:
    endpoints: [anthropic]
`
	p, diags := ValidatePlanBytes("test.yaml", "test", []byte(fmt.Sprintf(base, "fallback")))
	if len(diags) > 0 {
		t.Fatalf("fallbackRoute should validate: %+v", diags)
	}
	fallback := &Route{ID: "fallback", Targets: []Target{{Plan: "test", Model: "model"}}}
	if diags := ValidateCrossFile(map[string]*Plan{"test": p}, map[string]*Route{"fallback": fallback}); len(diags) > 0 {
		t.Fatalf("configured fallback route should resolve: %+v", diags)
	}
	if diags := ValidateCrossFile(map[string]*Plan{"test": p}, map[string]*Route{}); len(diags) != 1 ||
		diags[0].Path != "$.direct.fallbackRoute" || !strings.Contains(diags[0].Message, "enabled route") {
		t.Fatalf("missing fallback route diagnostics = %+v", diags)
	}
	disabled := false
	fallback.Enabled = &disabled
	if diags := ValidateCrossFile(map[string]*Plan{"test": p}, map[string]*Route{"fallback": fallback}); len(diags) != 1 {
		t.Fatalf("disabled fallback route diagnostics = %+v", diags)
	}
	if _, diags := ValidatePlanBytes("test.yaml", "test", []byte(fmt.Sprintf(base, "INVALID"))); len(diags) == 0 {
		t.Fatal("invalid fallbackRoute id accepted")
	}
}

func TestRouteSchemaAcceptsSixProtocols(t *testing.T) {
	_, schema, err := loadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"version": float64(1), "id": "test", "match": map[string]any{"protocols": []any{ProtocolAnthropic, ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolGemini, ProtocolTypeSafeJev, ProtocolOpenAIDecisions}}, "targets": []any{map[string]any{"plan": "test", "model": "model"}}}
	if err := schema.Validate(doc); err != nil {
		t.Fatal(err)
	}
}
