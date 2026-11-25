package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/lidessen/crabbro/internal/shared"
)

//go:embed gateway-plan.v1.json
var planSchemaJSON []byte

//go:embed gateway-route.v1.json
var routeSchemaJSON []byte

var (
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	schemaOnce  sync.Once
	planSchema  *jsonschema.Schema
	routeSchema *jsonschema.Schema
	schemaErr   error
)

func loadSchemas() (*jsonschema.Schema, *jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		var pDoc, rDoc any
		if err := json.Unmarshal(planSchemaJSON, &pDoc); err != nil {
			schemaErr = err
			return
		}
		if err := json.Unmarshal(routeSchemaJSON, &rDoc); err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("https://crabbro.dev/schemas/gateway-plan.v1.json", pDoc); err != nil {
			schemaErr = err
			return
		}
		if err := c.AddResource("https://crabbro.dev/schemas/gateway-route.v1.json", rDoc); err != nil {
			schemaErr = err
			return
		}
		planSchema, schemaErr = c.Compile("https://crabbro.dev/schemas/gateway-plan.v1.json")
		if schemaErr != nil {
			return
		}
		routeSchema, schemaErr = c.Compile("https://crabbro.dev/schemas/gateway-route.v1.json")
	})
	return planSchema, routeSchema, schemaErr
}

// ValidatePlanBytes parses and schema-validates one plan YAML document.
func ValidatePlanBytes(filename, expectedID string, data []byte) (*Plan, []shared.Diagnostic) {
	if int64(len(data)) > MaxFileBytes {
		return nil, []shared.Diagnostic{{File: filename, Path: "$", Message: "file exceeds 256 KiB"}}
	}
	norm, err := yamlToJSONAny(data)
	if err != nil {
		return nil, []shared.Diagnostic{{File: filename, Path: "$", Message: "invalid YAML"}}
	}
	ps, _, err := loadSchemas()
	if err != nil {
		return nil, []shared.Diagnostic{{File: filename, Path: "$", Message: "schema unavailable"}}
	}
	if err := ps.Validate(norm); err != nil {
		return nil, schemaDiags(filename, err)
	}
	var plan Plan
	if err := yaml.Unmarshal(data, &plan); err != nil {
		return nil, []shared.Diagnostic{{File: filename, Path: "$", Message: "invalid YAML"}}
	}
	var diags []shared.Diagnostic
	if expectedID != "" && plan.ID != expectedID {
		diags = append(diags, shared.Diagnostic{File: filename, Path: "$.id", Message: "id does not match the filename"})
	}
	if plan.SIWC != nil {
		if plan.ProxyURL != "" {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.proxyUrl", Message: "proxyUrl is not allowed for SIWC plans"})
		}
		if plan.SIWC.SessionRef == plan.SIWC.HostIDRef {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.siwc.hostIdRef", Message: "host ID and session must use distinct references"})
		}
		if len(plan.Endpoints) != 1 || plan.Endpoints[ProtocolOpenAIResponses] == nil || plan.Endpoints[ProtocolOpenAIResponses].BaseURL != "https://api.openai.com/v1" {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.endpoints", Message: "SIWC requires only the OpenAI Responses endpoint at https://api.openai.com/v1"})
		}
	}
	if plan.ProxyURL != "" {
		u, err := url.Parse(plan.ProxyURL)
		if err != nil || u.Host == "" {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.proxyUrl", Message: "invalid proxy URL"})
		}
	}
	for proto, ep := range plan.Endpoints {
		if ep == nil {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.endpoints." + proto, Message: "endpoint is empty"})
			continue
		}
		if err := ep.RequestTransform.Validate(ep.AuthHeader); err != nil {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.endpoints." + proto + ".requestTransform", Message: err.Error()})
		}
		if err := ep.ResponseTransform.Validate(ep.AuthHeader); err != nil {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.endpoints." + proto + ".responseTransform", Message: err.Error()})
		}
		u, err := url.Parse(ep.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.endpoints." + proto + ".baseUrl", Message: "invalid endpoint baseUrl"})
		}
	}
	for model, ms := range plan.Models {
		if ms == nil {
			continue
		}
		for proto, ep := range plan.Endpoints {
			if ep != nil && (len(ms.Endpoints) == 0 || containsProtocol(ms.Endpoints, proto)) {
				if err := ms.ResponseTransform.Validate(ep.AuthHeader); err != nil {
					diags = append(diags, shared.Diagnostic{File: filename, Path: "$.models." + model + ".responseTransform", Message: err.Error()})
				}
				if err := ms.RequestTransform.Validate(ep.AuthHeader); err != nil {
					diags = append(diags, shared.Diagnostic{File: filename, Path: "$.models." + model + ".requestTransform", Message: err.Error()})
					break
				}
			}
		}
		for i, proto := range ms.Endpoints {
			if plan.Endpoints[proto] == nil {
				diags = append(diags, shared.Diagnostic{File: filename, Path: fmt.Sprintf("$.models.%s.endpoints[%d]", model, i), Message: fmt.Sprintf("endpoint %q is not configured on plan", proto)})
			}
		}
	}
	for i, w := range plan.Limits.Windows {
		d, err := ParseDuration(w.Duration)
		if err != nil || d <= 0 {
			diags = append(diags, shared.Diagnostic{File: filename, Path: fmt.Sprintf("$.limits.windows[%d].duration", i), Message: "invalid window duration"})
		}
	}
	for i, scope := range plan.AutoReset.Scopes {
		if scope != AutoResetScopeFiveHour && scope != AutoResetScopeWeekly {
			diags = append(diags, shared.Diagnostic{File: filename, Path: fmt.Sprintf("$.autoReset.scopes[%d]", i), Message: fmt.Sprintf("unknown reset scope %q (want fiveHour or weekly)", scope)})
		}
	}
	if plan.AutoReset.ExpiringWithin != "" {
		if d, err := ParseDuration(plan.AutoReset.ExpiringWithin); err != nil || d <= 0 {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.autoReset.expiringWithin", Message: "invalid reset expiry horizon"})
		}
	}
	if plan.AutoReset.MinInterval != "" {
		if d, err := ParseDuration(plan.AutoReset.MinInterval); err != nil || d <= 0 {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.autoReset.minInterval", Message: "invalid reset minimum interval"})
		}
	}
	if plan.AutoReset.Safeguard.LeadTime != "" {
		if d, err := ParseDuration(plan.AutoReset.Safeguard.LeadTime); err != nil || d <= 0 {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.autoReset.safeguard.leadTime", Message: "invalid safeguard lead time"})
		}
	}
	if v := plan.AutoReset.Safeguard.MinUsedPercent; v != 0 && (v < 1 || v > 99) {
		diags = append(diags, shared.Diagnostic{File: filename, Path: "$.autoReset.safeguard.minUsedPercent", Message: "safeguard minUsedPercent must be between 1 and 99"})
	}
	if plan.QuotaReminder.LeadTime != "" {
		if d, err := ParseDuration(plan.QuotaReminder.LeadTime); err != nil || d <= 0 {
			diags = append(diags, shared.Diagnostic{File: filename, Path: "$.quotaReminder.leadTime", Message: "invalid quota reminder lead time"})
		}
	}
	if len(diags) > 0 {
		return nil, diags
	}
	if plan.Credentials != nil {
		plan.Credential = plan.Credentials.Refs[0]
	}
	return &plan, nil
}

// ValidateRouteBytes parses and schema-validates one route YAML document.
func ValidateRouteBytes(filename, expectedID string, data []byte) (*Route, []shared.Diagnostic) {
	if int64(len(data)) > MaxFileBytes {
		return nil, []shared.Diagnostic{{File: filename, Path: "$", Message: "file exceeds 256 KiB"}}
	}
	norm, err := yamlToJSONAny(data)
	if err != nil {
		return nil, []shared.Diagnostic{{File: filename, Path: "$", Message: "invalid YAML"}}
	}
	_, rs, err := loadSchemas()
	if err != nil {
		return nil, []shared.Diagnostic{{File: filename, Path: "$", Message: "schema unavailable"}}
	}
	if err := rs.Validate(norm); err != nil {
		return nil, schemaDiags(filename, err)
	}
	var route Route
	if err := yaml.Unmarshal(data, &route); err != nil {
		return nil, []shared.Diagnostic{{File: filename, Path: "$", Message: "invalid YAML"}}
	}
	var diags []shared.Diagnostic
	if expectedID != "" && route.ID != expectedID {
		diags = append(diags, shared.Diagnostic{File: filename, Path: "$.id", Message: "id does not match the filename"})
	}
	fam := route.EffectiveFamily()
	for _, proto := range route.Match.Protocols {
		pf := ProtocolFamily(proto)
		if pf != "" && pf != fam {
			diags = append(diags, shared.Diagnostic{
				File:    filename,
				Path:    "$.match.protocols",
				Message: fmt.Sprintf("protocol %q belongs to family %q, route family is %q", proto, pf, fam),
			})
		}
	}
	for i, tgt := range route.Targets {
		if tgt.MinContextTokens > 0 && tgt.MaxContextTokens > 0 && tgt.MinContextTokens > tgt.MaxContextTokens {
			diags = append(diags, shared.Diagnostic{
				File:    filename,
				Path:    fmt.Sprintf("$.targets[%d]", i),
				Message: "minContextTokens exceeds maxContextTokens",
			})
		}
		if fam == FamilyGenerative && ProtocolFamily(tgt.EndpointProtocol) == FamilyDecision {
			diags = append(diags, shared.Diagnostic{
				File:    filename,
				Path:    fmt.Sprintf("$.targets[%d].endpointProtocol", i),
				Message: "generative route cannot target decision-family endpoint",
			})
		}
		if fam == FamilyDecision && tgt.EndpointProtocol != "" && ProtocolFamily(tgt.EndpointProtocol) != FamilyDecision && !route.Policy.AllowEmulation {
			diags = append(diags, shared.Diagnostic{
				File:    filename,
				Path:    fmt.Sprintf("$.targets[%d].endpointProtocol", i),
				Message: "decision route targeting generative endpoint requires policy.allowEmulation: true",
			})
		}
	}
	if len(diags) > 0 {
		return nil, diags
	}
	return &route, nil
}

// ValidateCrossFile checks referential integrity and semantic-family compatibility
// between routes and plans.
func ValidateCrossFile(plans map[string]*Plan, routes map[string]*Route) []shared.Diagnostic {
	var diags []shared.Diagnostic
	for _, plan := range plans {
		if plan == nil || plan.DirectFallbackRoute() == "" {
			continue
		}
		fallback, ok := routes[plan.DirectFallbackRoute()]
		if !ok || !fallback.IsEnabled() {
			diags = append(diags, shared.Diagnostic{
				File:    plan.ID + ".yaml",
				Path:    "$.direct.fallbackRoute",
				Message: fmt.Sprintf("fallback route %q must name an enabled route", plan.DirectFallbackRoute()),
			})
		}
	}
	for _, route := range routes {
		if route == nil {
			continue
		}
		file := route.ID + ".yaml"
		fam := route.EffectiveFamily()
		for i, tgt := range route.Targets {
			plan, ok := plans[tgt.Plan]
			if !ok {
				diags = append(diags, shared.Diagnostic{
					File:    file,
					Path:    fmt.Sprintf("$.targets[%d].plan", i),
					Message: fmt.Sprintf("unknown plan %q", tgt.Plan),
				})
				continue
			}
			ms, ok := plan.Models[tgt.Model]
			if !ok || ms == nil {
				diags = append(diags, shared.Diagnostic{
					File:    file,
					Path:    fmt.Sprintf("$.targets[%d].model", i),
					Message: fmt.Sprintf("model %q is not declared on plan %q", tgt.Model, tgt.Plan),
				})
			}
			if tgt.EndpointProtocol != "" {
				if ms != nil && len(ms.Endpoints) > 0 && !containsProtocol(ms.Endpoints, tgt.EndpointProtocol) {
					diags = append(diags, shared.Diagnostic{File: file, Path: fmt.Sprintf("$.targets[%d].endpointProtocol", i), Message: "endpoint protocol not allowed for model"})
				}
				if _, ok := plan.Endpoints[tgt.EndpointProtocol]; !ok {
					diags = append(diags, shared.Diagnostic{
						File:    file,
						Path:    fmt.Sprintf("$.targets[%d].endpointProtocol", i),
						Message: fmt.Sprintf("endpoint %q is not configured on plan %q", tgt.EndpointProtocol, tgt.Plan),
					})
				}
			} else {
				hasCompatible := false
				for epProto := range plan.Endpoints {
					if ms != nil && len(ms.Endpoints) > 0 && !containsProtocol(ms.Endpoints, epProto) {
						continue
					}
					if fam == FamilyGenerative && ProtocolFamily(epProto) == FamilyGenerative {
						hasCompatible = true
						break
					}
					if fam == FamilyDecision && (ProtocolFamily(epProto) == FamilyDecision || route.Policy.AllowEmulation) {
						hasCompatible = true
						break
					}
				}
				if !hasCompatible {
					diags = append(diags, shared.Diagnostic{
						File:    file,
						Path:    fmt.Sprintf("$.targets[%d]", i),
						Message: fmt.Sprintf("plan %q has no endpoint compatible with family %q", tgt.Plan, fam),
					})
				}
			}
		}
		if route.Policy.SemanticTiering.Enabled {
			decName := route.Policy.SemanticTiering.EffectiveDecisionRoute()
			decRoute, ok := routes[decName]
			if !ok || !decRoute.IsEnabled() || decRoute.EffectiveFamily() != FamilyDecision {
				diags = append(diags, shared.Diagnostic{
					File:    file,
					Path:    "$.policy.semanticTiering.decisionRoute",
					Message: fmt.Sprintf("decisionRoute %q must name an enabled decision-family route", decName),
				})
			}
		}
	}
	return diags
}

func yamlToJSONAny(data []byte) (any, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var norm any
	if err := json.Unmarshal(b, &norm); err != nil {
		return nil, err
	}
	return norm, nil
}

func schemaDiags(file string, err error) []shared.Diagnostic {
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []shared.Diagnostic{{File: file, Path: "$", Message: err.Error()}}
	}
	var out []shared.Diagnostic
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			p := "$"
			if len(e.InstanceLocation) > 0 {
				p = "$." + strings.Join(e.InstanceLocation, ".")
			}
			msg := "schema constraint violated"
			if e.ErrorKind != nil {
				msg = fmt.Sprint(e.ErrorKind)
			}
			out = append(out, shared.Diagnostic{File: file, Path: p, Message: msg})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(out) == 0 {
		out = append(out, shared.Diagnostic{File: file, Path: "$", Message: "schema validation failed"})
	}
	return out
}

func containsProtocol(list []string, value string) bool {
	for _, p := range list {
		if p == value {
			return true
		}
	}
	return false
}
