package main

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The rule overview's model: the YAML rule v1 document, parsed to be read.
// Nothing writes it back — the script text is the only thing a save sends,
// so comments, key order and anything the model does not know survive.

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

// stageUses is what names a stage in the visual list: its uses, or "run".
func (s *vmStage) stageUses() string {
	if s.Run != nil {
		return "run"
	}
	return s.Uses
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

// stageNote names what a stage does that its uses does not say.
func (s *vmStage) stageNote() string {
	switch {
	case s.Select.Kind != 0:
		return "select"
	case s.Parallel.Kind != 0:
		return "parallel"
	case s.ContinueOnError:
		return "continueOnError"
	case s.TimeoutMs != 0:
		return fmt.Sprintf("timeout %dms", s.TimeoutMs)
	}
	return ""
}

// sortedKeys is a map's keys in order, for rows that must not shuffle
// between frames.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
