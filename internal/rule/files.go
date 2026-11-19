package rule

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/shared"
)

// Dir is the rules directory. Drafts use a .draft suffix and are not loaded
// into the proxy. One bad file does not hide the others.
type Dir struct {
	Path string
}

func OpenDir(path string) *Dir {
	return &Dir{Path: path}
}

// FileStatus is one live file or draft.
type FileStatus struct {
	ID          string       `json:"id"`
	Live        bool         `json:"live"`
	Draft       bool         `json:"draft"`
	Enabled     bool         `json:"enabled"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// Status lists live files and drafts. complete is false when any live file
// failed to load.
func (d *Dir) Status() (files []FileStatus, complete bool, err error) {
	if d == nil || d.Path == "" {
		return nil, true, nil
	}
	entries, err := os.ReadDir(d.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true, nil
		}
		return nil, false, err
	}
	complete = true
	var total int64
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			complete = false
			files = append(files, FileStatus{ID: name, Diagnostics: []Diagnostic{{File: name, Path: "$", Message: "symlink"}}})
			continue
		}
		total += info.Size()
		switch {
		case strings.HasSuffix(name, ".yaml.draft"):
			id := strings.TrimSuffix(name, ".yaml.draft")
			diag := d.validateFile(filepath.Join(d.Path, name), id)
			files = append(files, FileStatus{ID: id, Draft: true, Diagnostics: diag})
		case strings.HasSuffix(name, ".yaml"):
			id := strings.TrimSuffix(name, ".yaml")
			diag := d.validateFile(filepath.Join(d.Path, name), id)
			enabled := true
			if doc, err := readDoc(filepath.Join(d.Path, name)); err == nil {
				enabled = doc.enabled()
			}
			if len(diag) > 0 {
				complete = false
			}
			files = append(files, FileStatus{ID: id, Live: true, Enabled: enabled, Diagnostics: diag})
		}
	}
	if total > maxRuleDirBytes || len(files) > maxRuleFiles {
		complete = false
	}
	return files, complete, nil
}

func (d *Dir) validateFile(path, id string) []Diagnostic {
	b, err := os.ReadFile(path)
	if err != nil {
		return []Diagnostic{{File: filepath.Base(path), Path: "$", Message: "unreadable"}}
	}
	return validateBytes(filepath.Base(path), id, b, true, nil)
}

// validateBytes checks one rule file's content. The caller reads the bytes
// once, so what is checked is what is used.
func validateBytes(name, id string, b []byte, offline bool, rt Runtime) []Diagnostic {
	if int64(len(b)) > maxRuleFileBytes {
		return []Diagnostic{{File: name, Path: "$", Message: "file exceeds 256 KiB"}}
	}
	doc, err := ParseYAML(string(b))
	if err != nil {
		return []Diagnostic{{File: name, Path: "$", Message: err.Error()}}
	}
	if doc.ID != id {
		return []Diagnostic{{File: name, Path: "$.id", Message: "id does not match the filename"}}
	}
	diags := Validate(string(b), offline, rt)
	for i := range diags {
		diags[i].File = name
	}
	return diags
}

func readDoc(path string) (*Document, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseYAML(string(b))
}

// Load reads live YAML files. Invalid files are reported and skipped.
func (d *Dir) Load() ([]*shared.Rule, []Diagnostic, bool) {
	rules, diags, complete, _ := d.loadAll()
	return rules, diags, complete
}

// loadAll is Load, and also the ids of live files that exist but could not
// be loaded, so a caller can tell a broken edit from a deleted file.
func (d *Dir) loadAll() (rules []*shared.Rule, diags []Diagnostic, complete bool, broken []string) {
	files, complete, err := d.Status()
	if err != nil {
		return nil, []Diagnostic{{Path: "$", Message: "unreadable"}}, false, nil
	}
	now := time.Now().UTC()
	var loaded int
	var loadedBytes int64
	for _, f := range files {
		if !f.Live {
			continue
		}
		if len(f.Diagnostics) > 0 {
			diags = append(diags, f.Diagnostics...)
			broken = append(broken, f.ID)
			continue
		}
		if loaded >= maxRuleFiles {
			complete = false
			diags = append(diags, Diagnostic{File: f.ID + ".yaml", Path: "$", Message: "rules directory exceeds the file limit"})
			broken = append(broken, f.ID)
			continue
		}
		path := filepath.Join(d.Path, f.ID+".yaml")
		b, err := os.ReadFile(path)
		if err != nil {
			complete = false
			diags = append(diags, Diagnostic{File: f.ID + ".yaml", Path: "$", Message: "unreadable"})
			broken = append(broken, f.ID)
			continue
		}
		if loadedBytes += int64(len(b)); loadedBytes > maxRuleDirBytes {
			complete = false
			diags = append(diags, Diagnostic{File: f.ID + ".yaml", Path: "$", Message: "rules directory exceeds the size limit"})
			broken = append(broken, f.ID)
			continue
		}
		doc, err := ParseYAML(string(b))
		if err != nil {
			complete = false
			broken = append(broken, f.ID)
			continue
		}
		loaded++
		pri := doc.Priority
		if pri == 0 {
			pri = shared.DefaultRulePriority
		}
		rules = append(rules, &shared.Rule{
			ID:          shared.RuleID(doc.ID),
			Name:        doc.Description,
			Priority:    pri,
			Enabled:     doc.enabled(),
			Script:      string(b),
			Source:      "file",
			Version:     1,
			FailureMode: doc.failureMode(),
			CreatedAt:   now,
			UpdatedAt:   now,
		})
	}
	return rules, diags, complete, broken
}

// Sync replaces file-backed rules in the store. API-created rules stay.
//
// A file that exists but no longer loads (a typo in an edit) keeps the
// version the store already has: a rule must not stop running, least of all
// a fail-closed one, because its file broke. The diagnostics say which file
// is stale. A file that is gone is dropped.
func (d *Dir) Sync(store *RuleStore) ([]Diagnostic, bool) {
	rules, diags, complete, broken := d.loadAll()
	if store == nil {
		return diags, complete
	}
	keep := map[shared.RuleID]struct{}{}
	for _, id := range broken {
		keep[shared.RuleID(id)] = struct{}{}
	}
	for _, r := range rules {
		keep[r.ID] = struct{}{}
		_ = store.UpsertFile(r)
	}
	store.DropFileRules(keep)
	return diags, complete
}

// Draft writes a draft beside the live file. It does not load it.
func (d *Dir) Draft(id string, body []byte) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid id")
	}
	if err := os.MkdirAll(d.Path, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(d.Path, id+".yaml.draft"), body, 0o600)
}

// ValidateFile checks the draft if present, otherwise the live file.
func (d *Dir) ValidateFile(id string, offline bool, rt Runtime) []Diagnostic {
	if !idPattern.MatchString(id) {
		return []Diagnostic{{Path: "$.id", Message: "invalid id"}}
	}
	draft := filepath.Join(d.Path, id+".yaml.draft")
	live := filepath.Join(d.Path, id+".yaml")
	path := live
	if _, err := os.Stat(draft); err == nil {
		path = draft
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return []Diagnostic{{File: filepath.Base(path), Path: "$", Message: "unreadable"}}
	}
	return validateBytes(filepath.Base(path), id, b, offline, rt)
}

// Commit promotes the draft. Other invalid files do not block this one. The
// draft is read once and that content is what is checked and written, and
// the live file is replaced by a rename, so a reload never sees half a file.
func (d *Dir) Commit(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid id")
	}
	draft := filepath.Join(d.Path, id+".yaml.draft")
	live := filepath.Join(d.Path, id+".yaml")
	b, err := os.ReadFile(draft)
	if err != nil {
		return fmt.Errorf("no draft")
	}
	if len(validateBytes(id+".yaml.draft", id, b, true, nil)) > 0 {
		return fmt.Errorf("draft is invalid")
	}
	tmp, err := os.CreateTemp(d.Path, "."+id+"-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, live); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Remove(draft)
}

// Discard deletes the draft.
func (d *Dir) Discard(id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid id")
	}
	err := os.Remove(filepath.Join(d.Path, id+".yaml.draft"))
	if os.IsNotExist(err) {
		return fmt.Errorf("no draft")
	}
	return err
}

// Preview reports which live rules match. It does not call plugins.
type Preview struct {
	Matched     []string     `json:"matched"`
	Disabled    []string     `json:"disabled"`
	Complete    bool         `json:"complete"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

func (d *Dir) Preview(method, host, path string) Preview {
	rules, diags, complete := d.Load()
	out := Preview{Complete: complete, Diagnostics: diags, Matched: []string{}, Disabled: []string{}}
	files, _, _ := d.Status()
	for _, f := range files {
		if f.Live && !f.Enabled && len(f.Diagnostics) == 0 {
			out.Disabled = append(out.Disabled, f.ID)
		}
	}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		doc, err := ParseYAML(r.Script)
		if err != nil || doc.On.Request == nil {
			continue
		}
		if doc.On.Request.matches(method, host, path) {
			out.Matched = append(out.Matched, string(r.ID))
		}
	}
	return out
}

// WriteBody copies r into a draft or returns the bytes.
func WriteBody(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("too large")
	}
	return b, nil
}
