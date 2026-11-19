// Package fixture provides file-based persistence for HTTP request fixtures.
//
// Fixtures are stored as individual JSON files under a configurable directory
// (defaulting to ~/.crabbro/fixtures/). Each fixture captures a complete HTTP
// request/response exchange with bodies inlined, making fixtures self-contained
// and portable across daemon restarts and machines.
package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// FixtureData represents a saved HTTP request fixture.
//
// Bodies are inlined so the fixture is self-contained and survives daemon
// restarts, body-cache evictions, and transfer between environments.
type FixtureData struct {
	Name      string         `json:"name"`
	CreatedAt time.Time      `json:"createdAt"`
	Request   FixtureRequest `json:"request"`
}

// FixtureRequest captures the HTTP request and response fields of an exchange.
type FixtureRequest struct {
	Method          string            `json:"method"`
	Scheme          string            `json:"scheme"`
	Host            string            `json:"host"`
	Path            string            `json:"path"`
	Query           string            `json:"query"`
	URL             string            `json:"url"`
	RequestHeaders  map[string]string `json:"requestHeaders"`
	RequestBody     string            `json:"requestBody,omitempty"`
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty"`
	ResponseBody    string            `json:"responseBody,omitempty"`
	Status          int               `json:"status,omitempty"`
	ClientAddr      string            `json:"clientAddr"`
}

// FixtureSummary is a lightweight view returned by List, containing enough
// metadata for a human or agent to select a fixture without loading its body.
type FixtureSummary struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Method    string    `json:"method"`
	Host      string    `json:"host"`
	Path      string    `json:"path"`
}

// ---------------------------------------------------------------------------
// FixtureStore
// ---------------------------------------------------------------------------

// FixtureStore persists HTTP request fixtures as individual JSON files.
//
// A zero-value FixtureStore is not usable; call New to obtain an initialised
// store rooted at a writable directory.
type FixtureStore struct {
	baseDir string
}

// New creates a FixtureStore rooted at baseDir.
//
// If baseDir is empty, the default ~/.crabbro/fixtures/ is used. The
// directory (and any missing parents) is created with 0755 permissions.
func New(baseDir string) (*FixtureStore, error) {
	if baseDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("fixture: cannot determine home directory: %w", err)
		}
		baseDir = filepath.Join(home, ".crabbro", "fixtures")
	}

	// Expand a leading ~ to $HOME for convenience.
	if strings.HasPrefix(baseDir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("fixture: cannot expand home directory: %w", err)
		}
		baseDir = filepath.Join(home, strings.TrimPrefix(baseDir, "~"))
	}

	// Ensure the storage directory exists.
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, fmt.Errorf("fixture: cannot create directory %s: %w", baseDir, err)
	}

	return &FixtureStore{baseDir: baseDir}, nil
}

// BaseDir returns the absolute path of the directory where fixture files are
// stored.
func (s *FixtureStore) BaseDir() string {
	return s.baseDir
}

// ---------------------------------------------------------------------------
// Public operations
// ---------------------------------------------------------------------------

// Save persists the fixture under the given name. If a fixture with the same
// name already exists it is silently overwritten.
//
// The name must not be empty and must not contain path separators (/ or \) or
// the parent-directory token (..) — these would allow filesystem traversal.
func (s *FixtureStore) Save(name string, f FixtureData) error {
	if err := validateName(name); err != nil {
		return err
	}

	f.Name = name
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now().UTC()
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("fixture: marshal %q: %w", name, err)
	}

	if err := os.WriteFile(s.fileName(name), data, 0o644); err != nil {
		return fmt.Errorf("fixture: write %q: %w", name, err)
	}
	return nil
}

// Get retrieves a fixture by name. Returns an error wrapping
// fs.ErrNotExist when the fixture is not found.
func (s *FixtureStore) Get(name string) (FixtureData, error) {
	if err := validateName(name); err != nil {
		return FixtureData{}, err
	}

	data, err := os.ReadFile(s.fileName(name))
	if err != nil {
		if os.IsNotExist(err) {
			return FixtureData{}, fmt.Errorf("fixture %q not found", name)
		}
		return FixtureData{}, fmt.Errorf("fixture: read %q: %w", name, err)
	}

	var f FixtureData
	if err := json.Unmarshal(data, &f); err != nil {
		return FixtureData{}, fmt.Errorf("fixture: unmarshal %q: %w", name, err)
	}
	return f, nil
}

// List returns a summary of every saved fixture, ordered alphabetically by
// name. Corrupt or unreadable files are silently skipped.
func (s *FixtureStore) List() ([]FixtureSummary, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fixture: list: %w", err)
	}

	summaries := make([]FixtureSummary, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		// Read just enough to extract the summary fields.
		data, err := os.ReadFile(filepath.Join(s.baseDir, entry.Name()))
		if err != nil {
			continue
		}

		var head struct {
			Name      string    `json:"name"`
			CreatedAt time.Time `json:"createdAt"`
			Request   struct {
				Method string `json:"method"`
				Host   string `json:"host"`
				Path   string `json:"path"`
			} `json:"request"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			continue
		}

		summaries = append(summaries, FixtureSummary{
			Name:      head.Name,
			CreatedAt: head.CreatedAt,
			Method:    head.Request.Method,
			Host:      head.Request.Host,
			Path:      head.Request.Path,
		})
	}

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].Name < summaries[j].Name
	})

	return summaries, nil
}

// Delete removes the fixture with the given name. Returns an error wrapping
// fs.ErrNotExist if the fixture does not exist.
func (s *FixtureStore) Delete(name string) error {
	if err := validateName(name); err != nil {
		return err
	}

	p := s.fileName(name)
	if _, err := os.Stat(p); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("fixture %q not found", name)
		}
		return fmt.Errorf("fixture: stat %q: %w", name, err)
	}

	if err := os.Remove(p); err != nil {
		return fmt.Errorf("fixture: delete %q: %w", name, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// fileName returns the absolute filesystem path for the given fixture name.
func (s *FixtureStore) fileName(name string) string {
	return filepath.Join(s.baseDir, name+".json")
}

// validateName rejects names that are empty or could escape the fixture
// directory via path traversal.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("fixture: name must not be empty")
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("fixture: name %q must not contain path separators", name)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("fixture: name %q must not contain parent-directory tokens", name)
	}
	return nil
}
