package rule

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxRuleFileBytes = 256 * 1024
	maxRuleDirBytes  = 8 * 1024 * 1024
	maxRuleFiles     = 128
	maxVars          = 64
	maxVarsBytes     = 64 * 1024
)

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// IsYAMLRule reports whether script is a YAML rule document rather than
// legacy JavaScript.
func IsYAMLRule(script string) bool {
	return strings.HasPrefix(strings.TrimSpace(script), "version:")
}

// Document is one rule file.
type Document struct {
	Version     int            `yaml:"version" json:"version"`
	ID          string         `yaml:"id" json:"id"`
	Enabled     *bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Description string         `yaml:"description,omitempty" json:"description,omitempty"`
	Priority    int            `yaml:"priority,omitempty" json:"priority,omitempty"`
	FailureMode string         `yaml:"failureMode,omitempty" json:"failureMode,omitempty"`
	Timeout     string         `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	On          On             `yaml:"on" json:"on"`
	Vars        map[string]any `yaml:"vars,omitempty" json:"vars,omitempty"`
	Request     *PhaseDoc      `yaml:"request,omitempty" json:"request,omitempty"`
	Response    *PhaseDoc      `yaml:"response,omitempty" json:"response,omitempty"`
	Frame       *PhaseDoc      `yaml:"frame,omitempty" json:"frame,omitempty"`
}

// On is the match. A request match and a hook are alternatives.
type On struct {
	Request *RequestMatch `yaml:"request,omitempty" json:"request,omitempty"`
	Hook    string        `yaml:"hook,omitempty" json:"hook,omitempty"`
}

// RequestMatch is evaluated in Go before any stage runs.
type RequestMatch struct {
	Method     string `yaml:"method,omitempty" json:"method,omitempty"`
	Host       string `yaml:"host,omitempty" json:"host,omitempty"`
	Path       string `yaml:"path,omitempty" json:"path,omitempty"`
	PathPrefix string `yaml:"pathPrefix,omitempty" json:"pathPrefix,omitempty"`
}

// PhaseDoc is one of request, response, or frame.
type PhaseDoc struct {
	Pipeline []Stage `yaml:"pipeline" json:"pipeline"`
}

// MiddlewareRef is an explicit plugin middleware selection.
type MiddlewareRef struct {
	Plugin   string `yaml:"plugin" json:"plugin"`
	Instance string `yaml:"instance" json:"instance"`
	Name     string `yaml:"name" json:"name"`
}

// RunSpec is the fallback inline JavaScript stage.
type RunSpec struct {
	Runtime string `yaml:"runtime" json:"runtime"`
	Script  string `yaml:"script" json:"script"`
}

// Stage is an action, a select, or a parallel block.
type Stage struct {
	ID              string          `yaml:"id,omitempty" json:"id,omitempty"`
	If              string          `yaml:"if,omitempty" json:"if,omitempty"`
	Uses            string          `yaml:"uses,omitempty" json:"uses,omitempty"`
	Instance        string          `yaml:"instance,omitempty" json:"instance,omitempty"`
	Middleware      []MiddlewareRef `yaml:"middleware,omitempty" json:"middleware,omitempty"`
	Run             *RunSpec        `yaml:"run,omitempty" json:"run,omitempty"`
	With            map[string]any  `yaml:"with,omitempty" json:"with,omitempty"`
	Env             map[string]any  `yaml:"env,omitempty" json:"env,omitempty"`
	ContinueOnError bool            `yaml:"continueOnError,omitempty" json:"continueOnError,omitempty"`
	TimeoutMs       int             `yaml:"timeoutMs,omitempty" json:"timeoutMs,omitempty"`
	Select          *SelectSpec     `yaml:"select,omitempty" json:"select,omitempty"`
	Parallel        *ParallelSpec   `yaml:"parallel,omitempty" json:"parallel,omitempty"`
}

// SelectSpec chooses the first matching case.
type SelectSpec struct {
	Cases   []SelectCase `yaml:"cases" json:"cases"`
	Default *SelectCase  `yaml:"default,omitempty" json:"default,omitempty"`
}

// SelectCase is one arm. Default arms omit If.
type SelectCase struct {
	Name     string  `yaml:"name,omitempty" json:"name,omitempty"`
	If       string  `yaml:"if,omitempty" json:"if,omitempty"`
	Pipeline []Stage `yaml:"pipeline" json:"pipeline"`
}

// ParallelSpec is an observe-only block. It does not join.
type ParallelSpec struct {
	TimeoutMs int     `yaml:"timeoutMs,omitempty" json:"timeoutMs,omitempty"`
	Stages    []Stage `yaml:"stages" json:"stages"`
}

func (d *Document) enabled() bool {
	if d == nil || d.Enabled == nil {
		return true
	}
	return *d.Enabled
}

func (d *Document) failureMode() string {
	if d == nil || d.FailureMode == "" {
		return "fail-open"
	}
	return d.FailureMode
}

// ParseYAML parses a rule document. It does not validate it.
func ParseYAML(script string) (*Document, error) {
	if len(script) > maxRuleFileBytes {
		return nil, fmt.Errorf("rule file exceeds 256 KiB")
	}
	var doc Document
	dec := yaml.NewDecoder(bytes.NewReader([]byte(script)))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("invalid YAML at %s", yamlLine(err))
	}
	return &doc, nil
}

func yamlUnmarshalGeneric(script string, dest *any) error {
	return yaml.Unmarshal([]byte(script), dest)
}

func yamlLine(err error) string {
	msg := err.Error()
	i := strings.Index(msg, "line ")
	if i < 0 {
		return "the file"
	}
	rest := msg[i:]
	end := strings.IndexAny(rest, ": ")
	if end < 0 {
		return rest
	}
	// "line 12:" — keep the line token only.
	field := rest
	if c := strings.Index(rest, ":"); c > 0 {
		field = rest[:c]
	}
	return field
}

func (m *RequestMatch) matches(method, host, path string) bool {
	if m == nil {
		return false
	}
	if m.Method != "" && !strings.EqualFold(m.Method, method) {
		return false
	}
	if m.Host != "" && !hostMatch(m.Host, host) {
		return false
	}
	if m.Path != "" && m.Path != path {
		return false
	}
	if m.PathPrefix != "" && !strings.HasPrefix(path, m.PathPrefix) {
		return false
	}
	return true
}

func hostMatch(pattern, host string) bool {
	host = stripPort(host)
	pattern = stripPort(pattern)
	if pattern == "" {
		return true
	}
	if strings.HasPrefix(pattern, "*.") {
		suf := strings.TrimPrefix(pattern, "*")
		return strings.HasSuffix(host, suf) && len(host) > len(suf)
	}
	return strings.EqualFold(pattern, host)
}

func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		// Leave bracketed IPv6 alone. A single colon is host:port.
		if strings.Count(host, ":") == 1 {
			return host[:i]
		}
	}
	return host
}
