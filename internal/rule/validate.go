package rule

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	_ "embed"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed rule.v1.json
var ruleSchemaJSON []byte

var (
	schemaOnce sync.Once
	ruleSchema *jsonschema.Schema
	schemaErr  error
	timeoutPat = regexp.MustCompile(`^[0-9]+(ms|s)$`)
	usesPat    = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)+$`)
	stageIDPat = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// Diagnostic is one problem. Message does not repeat expression values.
type Diagnostic struct {
	File    string `json:"file,omitempty"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (d Diagnostic) Error() string {
	if d.File != "" {
		return d.File + ": " + d.Path + ": " + d.Message
	}
	return d.Path + ": " + d.Message
}

func loadSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		var doc any
		if err := json.Unmarshal(ruleSchemaJSON, &doc); err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("https://chiling.dev/schemas/rule.v1.json", doc); err != nil {
			schemaErr = err
			return
		}
		ruleSchema, schemaErr = c.Compile("https://chiling.dev/schemas/rule.v1.json")
	})
	return ruleSchema, schemaErr
}

// Validate checks the JSON Schema and the semantic rules. When offline is
// false and rt is set, plugin instances must be installed.
func Validate(script string, offline bool, rt Runtime) []Diagnostic {
	doc, err := ParseYAML(script)
	if err != nil {
		return []Diagnostic{{Path: "$", Message: err.Error()}}
	}
	var out []Diagnostic
	out = append(out, schemaDiagnostics(script)...)
	out = append(out, semanticDiagnostics(doc, offline, rt)...)
	return out
}

func schemaDiagnostics(script string) []Diagnostic {
	sch, err := loadSchema()
	if err != nil {
		return []Diagnostic{{Path: "$", Message: "schema unavailable"}}
	}
	var raw any
	if err := yamlToGeneric(script, &raw); err != nil {
		return []Diagnostic{{Path: "$", Message: "invalid YAML"}}
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return []Diagnostic{{Path: "$", Message: "invalid YAML"}}
	}
	var inst any
	if err := json.Unmarshal(b, &inst); err != nil {
		return []Diagnostic{{Path: "$", Message: "invalid YAML"}}
	}
	err = sch.Validate(inst)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Diagnostic{{Path: "$", Message: "schema rejected the document"}}
	}
	var out []Diagnostic
	collectSchemaErrors(ve, &out)
	if len(out) == 0 {
		return []Diagnostic{{Path: "$", Message: "schema rejected the document"}}
	}
	return out
}

func collectSchemaErrors(ve *jsonschema.ValidationError, out *[]Diagnostic) {
	if ve == nil {
		return
	}
	if len(ve.Causes) == 0 {
		*out = append(*out, Diagnostic{
			Path:    jsonPath(ve.InstanceLocation),
			Message: "schema " + strings.Join(ve.ErrorKind.KeywordPath(), "/"),
		})
		return
	}
	for _, c := range ve.Causes {
		collectSchemaErrors(c, out)
	}
}

func jsonPath(loc []string) string {
	if len(loc) == 0 {
		return "$"
	}
	var b strings.Builder
	b.WriteByte('$')
	for _, p := range loc {
		if p == "" {
			continue
		}
		if _, err := fmt.Sscanf(p, "%d", new(int)); err == nil && isDigits(p) {
			b.WriteByte('[')
			b.WriteString(p)
			b.WriteByte(']')
			continue
		}
		b.WriteByte('.')
		b.WriteString(p)
	}
	return b.String()
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func yamlToGeneric(script string, dest *any) error {
	return yamlUnmarshalGeneric(script, dest)
}

func semanticDiagnostics(doc *Document, offline bool, rt Runtime) []Diagnostic {
	var out []Diagnostic
	if !idPattern.MatchString(doc.ID) {
		out = append(out, Diagnostic{Path: "$.id", Message: "id does not match the rule id pattern"})
	}
	if doc.Timeout != "" && !timeoutPat.MatchString(doc.Timeout) {
		out = append(out, Diagnostic{Path: "$.timeout", Message: "timeout must be a duration in ms or s"})
	}
	if doc.FailureMode != "" && doc.FailureMode != "fail-open" && doc.FailureMode != "fail-closed" {
		out = append(out, Diagnostic{Path: "$.failureMode", Message: "failureMode must be fail-open or fail-closed"})
	}
	if err := checkVars(doc.Vars); err != nil {
		out = append(out, Diagnostic{Path: "$.vars", Message: err.Error()})
	}
	known := map[string]bool{}
	if doc.Request != nil {
		out = append(out, walkPipeline("$.request.pipeline", doc.Request.Pipeline, "request", known, false, offline, rt)...)
	}
	if doc.Response != nil {
		out = append(out, walkPipeline("$.response.pipeline", doc.Response.Pipeline, "response", known, false, offline, rt)...)
	}
	if doc.Frame != nil {
		out = append(out, walkPipeline("$.frame.pipeline", doc.Frame.Pipeline, "frame", known, false, offline, rt)...)
	}
	return out
}

func checkVars(vars map[string]any) error {
	if len(vars) > maxVars {
		return fmt.Errorf("too many vars")
	}
	refs := map[string][]string{}
	for name, val := range vars {
		s, ok := val.(string)
		if !ok {
			continue
		}
		for _, inner := range exprBodies(s) {
			e, err := parseExpr(inner)
			if err != nil {
				return fmt.Errorf("invalid expression")
			}
			var paths [][]string
			exprRefs(e, &paths)
			for _, p := range paths {
				if len(p) == 0 {
					continue
				}
				if p[0] == "secrets" || p[0] == "pipeline" {
					return fmt.Errorf("vars cannot reference %s", p[0])
				}
				if p[0] == "vars" && len(p) > 1 {
					refs[name] = append(refs[name], p[1])
				}
			}
		}
	}
	visiting := map[string]bool{}
	var walk func(string) error
	walk = func(name string) error {
		if visiting[name] {
			return fmt.Errorf("vars cycle")
		}
		visiting[name] = true
		for _, dep := range refs[name] {
			if _, ok := vars[dep]; !ok {
				return fmt.Errorf("vars reference an unknown name")
			}
			if err := walk(dep); err != nil {
				return err
			}
		}
		visiting[name] = false
		return nil
	}
	for name := range vars {
		if err := walk(name); err != nil {
			return err
		}
	}
	return nil
}

func exprBodies(s string) []string {
	var out []string
	rest := s
	for {
		i := strings.Index(rest, "${{")
		if i < 0 {
			return out
		}
		rest = rest[i+3:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			return append(out, rest)
		}
		out = append(out, strings.TrimSpace(rest[:j]))
		rest = rest[j+2:]
	}
}

func walkPipeline(path string, stages []Stage, phase string, known map[string]bool, inSelect bool, offline bool, rt Runtime) []Diagnostic {
	var out []Diagnostic
	terminals := map[string]bool{}
	for i, st := range stages {
		p := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case st.Select != nil:
			if inSelect {
				out = append(out, Diagnostic{Path: p, Message: "nested select is not allowed"})
			}
			if st.ID == "" || !stageIDPat.MatchString(st.ID) {
				out = append(out, Diagnostic{Path: p + ".id", Message: "select requires an id"})
			}
			out = append(out, checkIf(p+".if", st.If)...)
			armIDs := map[string]bool{}
			for j, c := range st.Select.Cases {
				cp := fmt.Sprintf("%s.select.cases[%d]", p, j)
				if !stageIDPat.MatchString(c.Name) {
					out = append(out, Diagnostic{Path: cp + ".name", Message: "invalid case name"})
				}
				out = append(out, checkIf(cp+".if", c.If)...)
				local := cloneKnown(known)
				out = append(out, walkPipeline(cp+".pipeline", c.Pipeline, phase, local, true, offline, rt)...)
				for id := range local {
					if !known[id] {
						armIDs[id] = true
					}
				}
			}
			if st.Select.Default != nil {
				local := cloneKnown(known)
				out = append(out, walkPipeline(p+".select.default.pipeline", st.Select.Default.Pipeline, phase, local, true, offline, rt)...)
				for id := range local {
					if !known[id] {
						armIDs[id] = true
					}
				}
			}
			for id := range armIDs {
				known[id] = true
			}
			if st.ID != "" {
				known[st.ID] = true
			}
		case st.Parallel != nil:
			out = append(out, checkIf(p+".if", st.If)...)
			if st.ID != "" && !stageIDPat.MatchString(st.ID) {
				out = append(out, Diagnostic{Path: p + ".id", Message: "invalid id"})
			}
			for j, child := range st.Parallel.Stages {
				cp := fmt.Sprintf("%s.parallel.stages[%d]", p, j)
				if child.Select != nil || child.Parallel != nil {
					out = append(out, Diagnostic{Path: cp, Message: "parallel stages are actions"})
					continue
				}
				out = append(out, checkAction(cp, child, phase, known, true, offline, rt)...)
			}
		default:
			out = append(out, checkAction(p, st, phase, known, false, offline, rt)...)
			if st.Uses == "chiling.mock-response" || st.Uses == "chiling.block" || st.Uses == "chiling.abort" {
				if terminals[st.Uses] || (st.Uses == "chiling.mock-response" && terminals["chiling.block"]) || (st.Uses == "chiling.block" && terminals["chiling.mock-response"]) || (terminals["chiling.abort"] && st.Uses != "chiling.abort") || (st.Uses == "chiling.abort" && len(terminals) > 0) {
					out = append(out, Diagnostic{Path: p, Message: "multiple terminal actions"})
				}
				terminals[st.Uses] = true
			}
		}
	}
	return out
}

func checkAction(path string, st Stage, phase string, known map[string]bool, inParallel, offline bool, rt Runtime) []Diagnostic {
	var out []Diagnostic
	if st.ID != "" && !stageIDPat.MatchString(st.ID) {
		out = append(out, Diagnostic{Path: path + ".id", Message: "invalid id"})
	}
	out = append(out, checkIf(path+".if", st.If)...)
	if st.Uses != "" && st.Run != nil {
		out = append(out, Diagnostic{Path: path, Message: "a stage has both uses and run"})
	}
	if st.Uses == "" && st.Run == nil {
		out = append(out, Diagnostic{Path: path, Message: "a stage needs uses or run"})
	}
	if st.Run != nil {
		if st.Run.Runtime != "goja" || strings.TrimSpace(st.Run.Script) == "" {
			out = append(out, Diagnostic{Path: path + ".run", Message: "run.runtime must be goja"})
		}
		if inParallel {
			out = append(out, Diagnostic{Path: path, Message: "parallel stages must be observe-only"})
		}
	}
	if st.Uses != "" {
		if !usesPat.MatchString(st.Uses) {
			out = append(out, Diagnostic{Path: path + ".uses", Message: "invalid uses name"})
		}
		if ph, ok := builtinPhase[st.Uses]; ok {
			if ph != "any" && ph != phase {
				out = append(out, Diagnostic{Path: path + ".uses", Message: "uses is not valid in this phase"})
			}
			if inParallel && builtinEffect(st.Uses) != "observe" {
				out = append(out, Diagnostic{Path: path, Message: "parallel stages must be observe-only"})
			}
		} else if inParallel {
			// Plugin effect is checked at runtime. Validation rejects an explicit mutate only
			// when the caller does not know; runtime still refuses mutate inside parallel.
		}
		if !offline && isPluginUses(st.Uses) && rt != nil {
			inst := st.Instance
			if inst == "" {
				inst = "local"
			}
			if !strings.Contains(inst, "${{") && !rt.HasInstance(pluginID(st.Uses), inst) {
				out = append(out, Diagnostic{Path: path + ".instance", Message: "instance is not installed"})
			}
		}
	}
	out = append(out, checkTemplates(path+".with", st.With, false, known)...)
	out = append(out, checkTemplates(path+".env", st.Env, false, known)...)
	out = append(out, checkTemplates(path+".instance", map[string]any{"instance": st.Instance}, false, known)...)
	if st.ID != "" && !inParallel {
		known[st.ID] = true
	}
	return out
}

func checkIf(path, src string) []Diagnostic {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	body := src
	if inner, ok := wholeExpr(src); ok {
		body = inner
	}
	if _, err := parseExpr(body); err != nil {
		return []Diagnostic{{Path: path, Message: "invalid expression"}}
	}
	return nil
}

func checkTemplates(path string, v any, allowEmbed bool, known map[string]bool) []Diagnostic {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		if strings.Contains(t, "${{") && !allowEmbed {
			if _, ok := wholeExpr(t); !ok {
				return []Diagnostic{{Path: path, Message: "embedded expression"}}
			}
		}
		for _, body := range exprBodies(t) {
			e, err := parseExpr(body)
			if err != nil {
				return []Diagnostic{{Path: path, Message: "invalid expression"}}
			}
			var refs [][]string
			exprRefs(e, &refs)
			for _, r := range refs {
				if len(r) > 0 && r[0] == "pipeline" && len(r) > 1 && !known[r[1]] {
					return []Diagnostic{{Path: path, Message: "pipeline output is not in scope"}}
				}
			}
		}
		return nil
	case map[string]any:
		var out []Diagnostic
		for k, child := range t {
			out = append(out, checkTemplates(path+"."+k, child, allowEmbed, known)...)
		}
		return out
	case []any:
		var out []Diagnostic
		for i, child := range t {
			out = append(out, checkTemplates(fmt.Sprintf("%s[%d]", path, i), child, allowEmbed, known)...)
		}
		return out
	default:
		return nil
	}
}

func cloneKnown(in map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func isPluginUses(uses string) bool {
	return !strings.HasPrefix(uses, "chiling.")
}

func pluginID(uses string) string {
	i := strings.LastIndex(uses, ".")
	if i < 0 {
		return uses
	}
	return uses[:i]
}

func capabilityName(uses string) string {
	i := strings.LastIndex(uses, ".")
	if i < 0 {
		return uses
	}
	return uses[i+1:]
}
