// Package secret manages operator credentials on disk (mode 0600) and
// provides redaction helpers so secret values never enter captures, logs,
// traces, or SSE events.
package secret

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	// ErrInvalidRef is returned when a secret reference violates naming rules.
	ErrInvalidRef = errors.New("invalid secret ref")
	// ErrNotFound is returned when a requested secret file does not exist.
	ErrNotFound = os.ErrNotExist

	refPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Summary describes a stored secret without revealing its contents.
type Summary struct {
	Name      string
	UpdatedAt time.Time
	SizeBytes int64
}

// Store reads and writes mode-0600 credential files under Dir.
type Store struct {
	Dir string
}

// New creates a Store rooted at dir (typically ~/.crabbro/secrets).
func New(dir string) *Store {
	return &Store{Dir: dir}
}

// ValidRef checks that ref is a safe single-component credential name.
func ValidRef(ref string) error {
	if !refPattern.MatchString(ref) || strings.Contains(ref, "..") {
		return fmt.Errorf("%w: %q", ErrInvalidRef, ref)
	}
	return nil
}

func (s *Store) pathFor(ref string) (string, error) {
	if s == nil || strings.TrimSpace(s.Dir) == "" {
		return "", fmt.Errorf("secret store is not configured")
	}
	if err := ValidRef(ref); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, ref), nil
}

// Get reads the trimmed secret value for ref. Symlinks are rejected.
func (s *Store) Get(ref string) (string, error) {
	p, err := s.pathFor(ref)
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("secret %q is not a regular file", ref)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Set atomically writes value for ref with mode 0600 inside a mode-0700 directory.
func (s *Store) Set(ref, value string) error {
	p, err := s.pathFor(ref)
	if err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("secret value cannot be empty")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(p); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return fmt.Errorf("secret path %q is not a regular file", ref)
		}
	}
	tmp, err := os.CreateTemp(s.Dir, "."+ref+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(value); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, p); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// Delete removes the stored secret for ref.
func (s *Store) Delete(ref string) error {
	p, err := s.pathFor(ref)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return fmt.Errorf("secret %q is not a regular file", ref)
	}
	return os.Remove(p)
}

// Has reports whether ref exists as a regular file in the store.
func (s *Store) Has(ref string) bool {
	p, err := s.pathFor(ref)
	if err != nil {
		return false
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink == 0 && fi.Mode().IsRegular()
}

// List returns metadata for all stored secrets without reading values.
func (s *Store) List() ([]Summary, error) {
	if s == nil || strings.TrimSpace(s.Dir) == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Summary{}, nil
		}
		return nil, err
	}
	out := make([]Summary, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || ValidRef(name) != nil {
			continue
		}
		fi, err := os.Lstat(filepath.Join(s.Dir, name))
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, Summary{
			Name:      name,
			UpdatedAt: fi.ModTime().UTC(),
			SizeBytes: fi.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// AllValues returns all non-empty secret values currently in the store for scrubbing.
func (s *Store) AllValues() []string {
	list, err := s.List()
	if err != nil || len(list) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(list))
	out := make([]string, 0, len(list))
	for _, item := range list {
		v, err := s.Get(item.Name)
		if err != nil || v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
