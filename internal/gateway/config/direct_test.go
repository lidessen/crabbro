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
