package inspect

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed scope.v1.json
var scopeSchemaJSON []byte

// DurableSnapshot is the committed allow and deny lists.
type DurableSnapshot struct {
	Allow          []Expr
	Deny           []Expr
	Bypass         []Expr
	AllowRaw       []string
	DenyRaw        []string
	AllowRestricts bool
}

// Durable loads ~/.crabbro/scope. Drafts are not active.
type Durable struct {
	Dir string
	mu  sync.Mutex
	cur DurableSnapshot
}

func OpenDurable(dir string) *Durable {
	d := &Durable{Dir: dir}
	_ = d.Reload()
	return d
}

func (d *Durable) Snapshot() DurableSnapshot {
	if d == nil {
		return DurableSnapshot{}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cur
}

func (d *Durable) Reload() error {
	if d == nil || d.Dir == "" {
		return nil
	}
	allow, err := readList(filepath.Join(d.Dir, "allow.yaml"), "allow")
	if err != nil {
		return err
	}
	deny, err := readList(filepath.Join(d.Dir, "deny.yaml"), "deny")
	if err != nil {
		return err
	}
	snap := DurableSnapshot{AllowRestricts: len(allow.Entries) > 0}
	for _, e := range allow.Entries {
		ex, err := ParseExpr(e.Expr)
		if err != nil {
			continue
		}
		if e.Action == "bypass" {
			snap.Bypass = append(snap.Bypass, ex)
		} else {
			snap.Allow = append(snap.Allow, ex)
			snap.AllowRaw = append(snap.AllowRaw, e.Expr)
		}
	}
	for _, e := range deny.Entries {
		ex, err := ParseExpr(e.Expr)
		if err != nil {
			continue
		}
		if e.Action == "bypass" {
			snap.Bypass = append(snap.Bypass, ex)
		} else {
			snap.Deny = append(snap.Deny, ex)
			snap.DenyRaw = append(snap.DenyRaw, e.Expr)
		}
	}
	d.mu.Lock()
	d.cur = snap
	d.mu.Unlock()
	return nil
}

type scopeFile struct {
	Version int          `yaml:"version" json:"version"`
	ID      string       `yaml:"id" json:"id"`
	Entries []scopeEntry `yaml:"entries" json:"entries"`
}

type scopeEntry struct {
	Expr   string `yaml:"expr" json:"expr"`
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
}

func readList(path, id string) (scopeFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return scopeFile{Version: 1, ID: id}, nil
		}
		return scopeFile{}, err
	}
	var doc scopeFile
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return scopeFile{}, fmt.Errorf("invalid YAML")
	}
	if doc.ID == "" {
		doc.ID = id
	}
	return doc, nil
}

// ValidateFile checks the draft if present, otherwise the live file.
func (d *Durable) ValidateFile(id string) []string {
	if id != "allow" && id != "deny" {
		return []string{"$.id: id must be allow or deny"}
	}
	path := filepath.Join(d.Dir, id+".yaml")
	if _, err := os.Stat(path + ".draft"); err == nil {
		path += ".draft"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return []string{"$: unreadable"}
	}
	return validateScopeYAML(b, id)
}

func validateScopeYAML(b []byte, id string) []string {
	var doc scopeFile
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return []string{"$: invalid YAML"}
	}
	if doc.ID != "" && doc.ID != id {
		return []string{"$.id: id does not match the filename"}
	}
	doc.ID = id
	if doc.Version == 0 {
		doc.Version = 1
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return []string{"$: invalid YAML"}
	}
	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		return []string{"$: invalid YAML"}
	}
	sch, err := scopeSchema()
	if err != nil {
		return []string{"$: schema unavailable"}
	}
	if err := sch.Validate(inst); err != nil {
		return []string{"$: schema rejected the document"}
	}
	var msgs []string
	for i, e := range doc.Entries {
		if _, err := ParseExpr(e.Expr); err != nil {
			msgs = append(msgs, fmt.Sprintf("$.entries[%d].expr: invalid expression", i))
		}
		if e.Action != "" && e.Action != "hide" && e.Action != "bypass" {
			msgs = append(msgs, fmt.Sprintf("$.entries[%d].action: action must be hide or bypass", i))
		}
	}
	return msgs
}

var (
	schemaOnce sync.Once
	schema     *jsonschema.Schema
	schemaErr  error
)

func scopeSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		var doc any
		if err := json.Unmarshal(scopeSchemaJSON, &doc); err != nil {
			schemaErr = err
			return
		}
		c := jsonschema.NewCompiler()
		if err := c.AddResource("https://crabbro.dev/schemas/scope.v1.json", doc); err != nil {
			schemaErr = err
			return
		}
		schema, schemaErr = c.Compile("https://crabbro.dev/schemas/scope.v1.json")
	})
	return schema, schemaErr
}

// Draft writes a draft next to the live file.
func (d *Durable) Draft(id string, body []byte) error {
	if id != "allow" && id != "deny" {
		return fmt.Errorf("invalid id")
	}
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d.Dir, id+".yaml.draft"), body, 0o600)
}

// Commit promotes a valid draft.
func (d *Durable) Commit(id string) error {
	if msgs := d.ValidateFile(id); len(msgs) > 0 {
		return fmt.Errorf("draft is invalid")
	}
	draft := filepath.Join(d.Dir, id+".yaml.draft")
	b, err := os.ReadFile(draft)
	if err != nil {
		return fmt.Errorf("no draft")
	}
	if err := os.WriteFile(filepath.Join(d.Dir, id+".yaml"), b, 0o600); err != nil {
		return err
	}
	if err := os.Remove(draft); err != nil {
		return err
	}
	return d.Reload()
}

// Discard deletes the draft.
func (d *Durable) Discard(id string) error {
	err := os.Remove(filepath.Join(d.Dir, id+".yaml.draft"))
	if os.IsNotExist(err) {
		return fmt.Errorf("no draft")
	}
	return err
}

// Status lists live files and drafts.
func (d *Durable) Status() []map[string]any {
	if d == nil {
		return nil
	}
	var out []map[string]any
	for _, id := range []string{"allow", "deny"} {
		live := fileExists(filepath.Join(d.Dir, id+".yaml"))
		draft := fileExists(filepath.Join(d.Dir, id+".yaml.draft"))
		if !live && !draft {
			continue
		}
		out = append(out, map[string]any{"id": id, "live": live, "draft": draft, "diagnostics": d.ValidateFile(id)})
	}
	return out
}

func fileExists(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return true
}
