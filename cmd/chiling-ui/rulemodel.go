package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// The visual rule editor's model: a mirror of the YAML rule v1 document,
// kept in step with the script text. A document the visual form cannot
// fully express (hooks, vars, select, parallel, plugin middleware) marks
// itself advanced, and the Code tab stays the only editor for it.

// builtinStages are the chiling.* stages the visual editor offers, in the
// order the add-stage menu lists them.
var builtinStages = []string{
	"chiling.mock-response",
	"chiling.block",
	"chiling.abort",
	"chiling.set-request-header",
	"chiling.remove-request-header",
	"chiling.rewrite-url",
	"chiling.tag",
	"chiling.set-response-header",
	"chiling.remove-response-header",
	"chiling.set-response-status",
	"chiling.rewrite-response-body",
	"chiling.delay",
	"chiling.throttle",
}

// vmDoc mirrors the rule v1 document.
type vmDoc struct {
	Version     int            `yaml:"version"`
	ID          string         `yaml:"id"`
	Description string         `yaml:"description,omitempty"`
	Enabled     *bool          `yaml:"enabled,omitempty"`
	Priority    int            `yaml:"priority,omitempty"`
	FailureMode string         `yaml:"failureMode,omitempty"`
	Timeout     string         `yaml:"timeout,omitempty"`
	On          vmOn           `yaml:"on"`
	Vars        map[string]any `yaml:"vars,omitempty"`
	Request     *vmPhase       `yaml:"request,omitempty"`
	Response    *vmPhase       `yaml:"response,omitempty"`
	Frame       *vmPhase       `yaml:"frame,omitempty"`
}

type vmOn struct {
	Request *vmMatch `yaml:"request,omitempty"`
	Hook    string   `yaml:"hook,omitempty"`
}

type vmMatch struct {
	Method     string `yaml:"method,omitempty"`
	Host       string `yaml:"host,omitempty"`
	Path       string `yaml:"path,omitempty"`
	PathPrefix string `yaml:"pathPrefix,omitempty"`
}

type vmPhase struct {
	Pipeline []vmStage `yaml:"pipeline"`
}

type vmRun struct {
	Runtime string `yaml:"runtime,omitempty"`
	Script  string `yaml:"script"`
}

// vmStage mirrors one pipeline stage. Run stages and any advanced field are
// displayed read-only; they round-trip through the same fields.
type vmStage struct {
	ID              string         `yaml:"id,omitempty"`
	If              string         `yaml:"if,omitempty"`
	Uses            string         `yaml:"uses,omitempty"`
	Run             *vmRun         `yaml:"run,omitempty"`
	With            map[string]any `yaml:"with,omitempty"`
	Env             map[string]any `yaml:"env,omitempty"`
	ContinueOnError bool           `yaml:"continueOnError,omitempty"`
	TimeoutMs       int            `yaml:"timeoutMs,omitempty"`
	Select          yaml.Node      `yaml:"select,omitempty"`
	Parallel        yaml.Node      `yaml:"parallel,omitempty"`
}

// parseRuleDoc decodes the script into the visual model. knownFields
// matches the daemon's own strictness: an unknown field is an error, which
// is also what makes the advanced check trustworthy.
func parseRuleDoc(script string) (*vmDoc, error) {
	var doc vmDoc
	dec := yaml.NewDecoder(strings.NewReader(script))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if doc.Version == 0 {
		doc.Version = 1
	}
	return &doc, nil
}

// marshalRuleDoc writes the model back as the script text. yaml.v3 quotes
// the `on` key — YAML 1.1 reads a bare on as true — and the rule documents
// have always written it bare, so the quotes come back off.
func marshalRuleDoc(doc *vmDoc) (string, error) {
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	out := string(b)
	out = strings.ReplaceAll(out, "\"on\":", "on:")
	return out, nil
}

// docAdvanced reports why the visual editor cannot express the document,
// "" when it can. The stage-level checks come in stageAdvanced.
func (d *vmDoc) docAdvanced() string {
	switch {
	case d.On.Hook != "":
		return "hook rules are edited in code"
	case len(d.Vars) > 0:
		return "rules with vars are edited in code"
	case d.Timeout != "":
		return "rules with a timeout are edited in code"
	}
	return ""
}

// stageAdvanced reports why a stage draws read-only, "" when its fields
// are all editable in the visual form.
func (s *vmStage) stageAdvanced() string {
	switch {
	case s.Run != nil:
		return "run stage"
	case s.Select.Kind != 0:
		return "select"
	case s.Parallel.Kind != 0:
		return "parallel"
	case s.Env != nil:
		return "env"
	case s.ContinueOnError:
		return "continueOnError"
	case s.TimeoutMs != 0:
		return "timeoutMs"
	case s.Uses != "" && !isBuiltinStage(s.Uses):
		return "plugin stage"
	}
	return ""
}

func isBuiltinStage(uses string) bool {
	for _, b := range builtinStages {
		if uses == b {
			return true
		}
	}
	return false
}

// stageUses is what names a stage in the visual list: its uses, or "run".
func (s *vmStage) stageUses() string {
	if s.Run != nil {
		return "run"
	}
	return s.Uses
}

// scalarParams splits a stage's with into rows the visual editor can edit
// (scalar values: strings, numbers, bools) and rows it only shows (maps
// and lists, drawn as text).
func scalarParams(s *vmStage) (edit []string, shown []string) {
	for k, v := range s.With {
		switch v.(type) {
		case string, bool, int, int64, float64:
			edit = append(edit, k)
		default:
			shown = append(shown, k)
		}
	}
	sort.Strings(edit)
	sort.Strings(shown)
	return edit, shown
}

// paramString reads a with value as the text the visual editor shows: a
// scalar as itself, a map as its k: v pairs, a list as comma-separated.
func paramString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s: %v", k, t[k]))
		}
		return strings.Join(parts, ", ")
	case []any:
		parts := make([]string, 0, len(t))
		for _, item := range t {
			parts = append(parts, fmt.Sprintf("%v", item))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprintf("%v", v)
}

// coerceParam turns typed text into a with value: bools and numbers keep
// their YAML kinds, everything else is a string.
func coerceParam(text string) any {
	switch text {
	case "true":
		return true
	case "false":
		return false
	}
	if n, err := strconv.Atoi(text); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return f
	}
	return text
}

// ensurePhase returns the doc's phase of kind, creating it when missing.
func (d *vmDoc) ensurePhase(kind string) *vmPhase {
	switch kind {
	case "response":
		if d.Response == nil {
			d.Response = &vmPhase{}
		}
		return d.Response
	case "frame":
		if d.Frame == nil {
			d.Frame = &vmPhase{}
		}
		return d.Frame
	default:
		if d.Request == nil {
			d.Request = &vmPhase{}
		}
		return d.Request
	}
}

// phaseOf is the doc's phase of kind, nil when it holds none.
func (d *vmDoc) phaseOf(kind string) *vmPhase {
	switch kind {
	case "response":
		return d.Response
	case "frame":
		return d.Frame
	default:
		return d.Request
	}
}

// phaseEmpty reports whether a phase holds no stages yet.
func (d *vmDoc) phaseEmpty(kind string) bool {
	p := d.phaseOf(kind)
	return p == nil || len(p.Pipeline) == 0
}
