package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// packageIDPattern is what a package id may be: an npm name, optionally
// scoped. It becomes a directory name, so nothing that climbs or is absolute
// can pass.
var packageIDPattern = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)

// knownPermissions are the grants a manifest may declare.
var knownPermissions = map[string]bool{
	"net": true, "net:private": true, "fs:data": true, "history:read": true, "secrets:read": true, "native": true,
}

// InstanceRecord is one named configuration of a package.
type InstanceRecord struct {
	Plugin      string            `json:"plugin"`
	Name        string            `json:"name"`
	Enabled     bool              `json:"enabled"`
	Config      map[string]any    `json:"config,omitempty"`
	Credentials map[string]string `json:"credentials,omitempty"`
}

type diskState struct {
	Packages  []PackageRecord  `json:"packages"`
	Instances []InstanceRecord `json:"instances"`
}

func (m *Manager) load() error {
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		if os.IsNotExist(err) {
			m.state = diskState{}
			return nil
		}
		return err
	}
	return json.Unmarshal(b, &m.state)
}

func (m *Manager) save() error {
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.statePath(), append(b, '\n'), 0o600)
}

func (m *Manager) statePath() string {
	return filepath.Join(m.Dir, "installed.json")
}

// Install copies a local package and runs npm ci --ignore-scripts.
// A package-lock.json is required. Lifecycle scripts are not run.
func (m *Manager) Install(src string, enable bool) (PackageRecord, error) {
	if _, err := exec.LookPath("npm"); err != nil {
		return PackageRecord{}, fmt.Errorf("npm is not installed")
	}
	lock := filepath.Join(src, "package-lock.json")
	if _, err := os.Stat(lock); err != nil {
		return PackageRecord{}, fmt.Errorf("package-lock.json is required")
	}
	meta, err := readPackage(src)
	if err != nil {
		return PackageRecord{}, err
	}
	if err := checkAPI(meta.API); err != nil {
		return PackageRecord{}, err
	}
	pkgRoot := filepath.Join(m.Dir, "packages")
	dest := filepath.Join(pkgRoot, meta.ID)
	if rel, err := filepath.Rel(pkgRoot, dest); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return PackageRecord{}, fmt.Errorf("invalid package id")
	}
	if err := os.RemoveAll(dest); err != nil {
		return PackageRecord{}, err
	}
	if err := copyTree(src, dest); err != nil {
		return PackageRecord{}, err
	}
	cmd := exec.Command("npm", "ci", "--ignore-scripts")
	cmd.Dir = dest
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "npm_config_ignore_scripts=true"}
	if _, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dest)
		return PackageRecord{}, fmt.Errorf("npm ci failed")
	}
	// Native addons live in dependencies, which exist only after npm ci, so
	// the scan runs on the installed tree.
	if err := rejectNative(dest, meta.Permissions); err != nil {
		_ = os.RemoveAll(dest)
		return PackageRecord{}, err
	}
	sum, _ := fileDigest(filepath.Join(dest, "package.json"))
	rec := PackageRecord{
		ID: meta.ID, Name: meta.Name, Version: meta.Version, Path: dest, Entry: meta.Entry,
		API: meta.API, Enabled: enable, Priority: 100, FailureMode: "fail-open",
		Isolation: meta.Isolation, Permissions: meta.Permissions, Activation: meta.Activation,
		Digest: sum, Match: meta.Match,
	}
	if hasPerm(meta.Permissions, "native") {
		rec.Isolation = "process"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	replaced := false
	for i := range m.state.Packages {
		if m.state.Packages[i].ID == rec.ID {
			m.state.Packages[i] = rec
			replaced = true
		}
	}
	if !replaced {
		m.state.Packages = append(m.state.Packages, rec)
	}
	if err := m.save(); err != nil {
		return PackageRecord{}, err
	}
	return rec, nil
}

// Uninstall removes a package and its instances.
func (m *Manager) Uninstall(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var path string
	next := m.state.Packages[:0]
	for _, p := range m.state.Packages {
		if p.ID == id {
			path = p.Path
			continue
		}
		next = append(next, p)
	}
	m.state.Packages = next
	inst := m.state.Instances[:0]
	for _, in := range m.state.Instances {
		if in.Plugin != id {
			inst = append(inst, in)
		}
	}
	m.state.Instances = inst
	if err := m.save(); err != nil {
		return err
	}
	if path != "" {
		_ = os.RemoveAll(path)
	}
	return nil
}

// AddInstance records a named instance. Credential values are file names, not secrets.
func (m *Manager) AddInstance(pluginID, name string, config map[string]any, creds map[string]string) error {
	if name == "" || pluginID == "" {
		return fmt.Errorf("plugin and instance are required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.packageByID(pluginID) == nil {
		return fmt.Errorf("plugin is not installed")
	}
	for _, v := range creds {
		if strings.Contains(v, "/") || strings.Contains(v, "..") {
			return fmt.Errorf("credential ref is a file name")
		}
	}
	rec := InstanceRecord{Plugin: pluginID, Name: name, Enabled: true, Config: config, Credentials: creds}
	for i := range m.state.Instances {
		if m.state.Instances[i].Plugin == pluginID && m.state.Instances[i].Name == name {
			m.state.Instances[i] = rec
			return m.save()
		}
	}
	m.state.Instances = append(m.state.Instances, rec)
	return m.save()
}

func (m *Manager) SetInstanceEnabled(pluginID, name string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.state.Instances {
		if m.state.Instances[i].Plugin == pluginID && m.state.Instances[i].Name == name {
			m.state.Instances[i].Enabled = enabled
			return m.save()
		}
	}
	return fmt.Errorf("instance not found")
}

func (m *Manager) RemoveInstance(pluginID, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.state.Instances[:0]
	found := false
	for _, in := range m.state.Instances {
		if in.Plugin == pluginID && in.Name == name {
			found = true
			continue
		}
		next = append(next, in)
	}
	if !found {
		return fmt.Errorf("instance not found")
	}
	m.state.Instances = next
	return m.save()
}

func (m *Manager) packageByID(id string) *PackageRecord {
	for i := range m.state.Packages {
		if m.state.Packages[i].ID == id {
			return &m.state.Packages[i]
		}
	}
	return nil
}

func (m *Manager) instanceByName(pluginID, name string) *InstanceRecord {
	for i := range m.state.Instances {
		if m.state.Instances[i].Plugin == pluginID && m.state.Instances[i].Name == name {
			return &m.state.Instances[i]
		}
	}
	return nil
}

type packageMeta struct {
	ID          string
	Name        string
	Version     string
	Entry       string
	API         string
	Isolation   string
	Permissions []string
	Activation  []string
	Match       Match
}

func readPackage(dir string) (packageMeta, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return packageMeta{}, fmt.Errorf("package.json is missing")
	}
	var raw struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Crabbro struct {
			ID          string   `json:"id"`
			API         string   `json:"api"`
			Entry       string   `json:"entry"`
			Isolation   string   `json:"isolation"`
			Permissions []string `json:"permissions"`
			Activation  []string `json:"activation"`
			Match       Match    `json:"match"`
		} `json:"crabbro"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return packageMeta{}, fmt.Errorf("package.json is not valid JSON")
	}
	id := raw.Crabbro.ID
	if id == "" {
		id = raw.Name
	}
	if !packageIDPattern.MatchString(id) {
		return packageMeta{}, fmt.Errorf("package id must be an npm package name")
	}
	for _, perm := range raw.Crabbro.Permissions {
		if !knownPermissions[perm] {
			return packageMeta{}, fmt.Errorf("unknown permission %q", perm)
		}
	}
	entry := raw.Crabbro.Entry
	if entry == "" || strings.Contains(entry, "..") || filepath.IsAbs(entry) {
		return packageMeta{}, fmt.Errorf("crabbro.entry is required and must be a relative path")
	}
	if _, err := os.Stat(filepath.Join(dir, entry)); err != nil {
		return packageMeta{}, fmt.Errorf("plugin entry is missing")
	}
	iso := raw.Crabbro.Isolation
	if iso == "" {
		iso = "worker"
	}
	act := raw.Crabbro.Activation
	if len(act) == 0 {
		act = []string{"onRequest"}
	}
	return packageMeta{
		ID: id, Name: raw.Name, Version: raw.Version, Entry: entry, API: raw.Crabbro.API,
		Isolation: iso, Permissions: raw.Crabbro.Permissions, Activation: act, Match: raw.Crabbro.Match,
	}, nil
}

func checkAPI(v string) error {
	parts := strings.Split(v, ".")
	if len(parts) < 2 || parts[0] != "1" {
		return fmt.Errorf("plugin api major must be 1")
	}
	return nil
}

func rejectNative(dir string, perms []string) error {
	if hasPerm(perms, "native") {
		return nil
	}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".node") {
			return fmt.Errorf("native addon refused")
		}
		return nil
	})
	return err
}

func hasPerm(perms []string, want string) bool {
	for _, p := range perms {
		if p == want {
			return true
		}
	}
	return false
}

func copyTree(src, dest string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dest, 0o700)
		}
		base := filepath.Base(path)
		if base == "node_modules" || base == ".git" || strings.HasPrefix(base, ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink refused")
		}
		target := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
