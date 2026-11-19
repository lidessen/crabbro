package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/lidessen/crabbro/internal/shared"
)

// Store manages ~/.crabbro/gateway/{plans,routes}/*.yaml and publishes
// immutable snapshots atomically.
type Store struct {
	RootDir  string
	OnChange func(*Snapshot)

	mu              sync.Mutex
	gen             uint64
	lastValidPlans  map[string]*Plan
	lastValidRoutes map[string]*Route
	snap            atomic.Pointer[Snapshot]
}

// Open opens or initializes a gateway config Store at rootDir (~/.crabbro/gateway).
func Open(rootDir string) *Store {
	s := &Store{
		RootDir:         rootDir,
		lastValidPlans:  map[string]*Plan{},
		lastValidRoutes: map[string]*Route{},
	}
	empty := &Snapshot{
		Plans:    map[string]*Plan{},
		AllPlans: map[string]*Plan{},
	}
	s.snap.Store(empty)
	_, _ = s.Reload()
	return s
}

// Snapshot returns the current immutable configuration snapshot without locking.
func (s *Store) Snapshot() *Snapshot {
	if s == nil {
		return &Snapshot{Plans: map[string]*Plan{}, AllPlans: map[string]*Plan{}}
	}
	if cur := s.snap.Load(); cur != nil {
		return cur
	}
	return &Snapshot{Plans: map[string]*Plan{}, AllPlans: map[string]*Plan{}}
}

func (s *Store) dirForKind(kind string) (string, error) {
	if s == nil || strings.TrimSpace(s.RootDir) == "" {
		return "", fmt.Errorf("gateway config directory is not configured")
	}
	switch kind {
	case KindPlan:
		return filepath.Join(s.RootDir, "plans"), nil
	case KindRoute:
		return filepath.Join(s.RootDir, "routes"), nil
	default:
		return "", fmt.Errorf("unknown gateway config kind %q", kind)
	}
}

// Status reports live files, drafts, and per-file diagnostics for plans and routes.
func (s *Store) Status() ([]FileStatus, bool, error) {
	if s == nil || s.RootDir == "" {
		return nil, true, nil
	}
	s.mu.Lock()
	snap := s.Snapshot()
	s.mu.Unlock()

	var out []FileStatus
	complete := true

	for _, kind := range []string{KindPlan, KindRoute} {
		dir, _ := s.dirForKind(kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return out, false, err
		}
		byID := map[string]*FileStatus{}
		ensure := func(id string) *FileStatus {
			if st, ok := byID[id]; ok {
				return st
			}
			st := &FileStatus{Kind: kind, ID: id}
			byID[id] = st
			return st
		}
		for _, ent := range entries {
			name := ent.Name()
			if ent.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			info, err := os.Lstat(filepath.Join(dir, name))
			if err != nil {
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				complete = false
				st := ensure(name)
				st.Diagnostics = append(st.Diagnostics, shared.Diagnostic{File: name, Path: "$", Message: "symlink"})
				continue
			}
			switch {
			case strings.HasSuffix(name, ".yaml.draft"):
				id := strings.TrimSuffix(name, ".yaml.draft")
				st := ensure(id)
				st.Draft = true
				diags := s.ValidateFile(kind, id)
				st.Diagnostics = append(st.Diagnostics, diags...)
			case strings.HasSuffix(name, ".yaml"):
				id := strings.TrimSuffix(name, ".yaml")
				st := ensure(id)
				st.Live = true
				b, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					complete = false
					st.Diagnostics = append(st.Diagnostics, shared.Diagnostic{File: name, Path: "$", Message: "unreadable"})
					continue
				}
				if kind == KindPlan {
					p, diags := ValidatePlanBytes(name, id, b)
					if len(diags) > 0 {
						complete = false
						st.Diagnostics = append(st.Diagnostics, diags...)
					} else {
						st.Enabled = p.IsEnabled()
					}
				} else {
					r, diags := ValidateRouteBytes(name, id, b)
					if len(diags) > 0 {
						complete = false
						st.Diagnostics = append(st.Diagnostics, diags...)
					} else {
						st.Enabled = r.IsEnabled()
					}
				}
			}
		}
		// Attach cross-file diagnostics for live files from current snapshot.
		for _, d := range snap.Diagnostics {
			id := strings.TrimSuffix(d.File, ".yaml")
			if st, ok := byID[id]; ok && st.Live {
				st.Diagnostics = append(st.Diagnostics, d)
			}
		}
		ids := make([]string, 0, len(byID))
		for id := range byID {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			out = append(out, *byID[id])
		}
	}
	return out, complete && len(snap.Diagnostics) == 0, nil
}

// Reload scans plans/ and routes/, validates each file + cross-file links,
// preserves last-known-good definitions on broken edits, and publishes a new
// Snapshot atomically when valid.
func (s *Store) Reload() ([]shared.Diagnostic, bool) {
	if s == nil || s.RootDir == "" {
		return nil, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var diags []shared.Diagnostic
	complete := true

	nextPlans, planBroken, pDiags, pOK := s.loadPlansLocked()
	diags = append(diags, pDiags...)
	if !pOK {
		complete = false
	}
	// Keep last-known-good plan for files that still exist on disk but failed edit.
	for _, id := range planBroken {
		if prev, ok := s.lastValidPlans[id]; ok {
			nextPlans[id] = prev
		}
	}

	nextRoutes, routeBroken, rDiags, rOK := s.loadRoutesLocked()
	diags = append(diags, rDiags...)
	if !rOK {
		complete = false
	}
	for _, id := range routeBroken {
		if prev, ok := s.lastValidRoutes[id]; ok {
			nextRoutes[id] = prev
		}
	}

	crossDiags := ValidateCrossFile(nextPlans, nextRoutes)
	if len(crossDiags) > 0 {
		complete = false
		diags = append(diags, crossDiags...)
		// Keep previous active routing snapshot when cross-file validation fails,
		// while exposing the new diagnostics via Snapshot.Diagnostics.
		prev := s.Snapshot()
		if prev.Generation > 0 {
			cp := *prev
			cp.Diagnostics = append([]shared.Diagnostic(nil), diags...)
			s.snap.Store(&cp)
			return diags, false
		}
	}

	s.lastValidPlans = nextPlans
	s.lastValidRoutes = nextRoutes
	s.gen++

	enabledPlans := make(map[string]*Plan, len(nextPlans))
	allPlans := make(map[string]*Plan, len(nextPlans))
	for id, p := range nextPlans {
		allPlans[id] = p
		if p.IsEnabled() {
			enabledPlans[id] = p
		}
	}

	allRoutes := make([]*Route, 0, len(nextRoutes))
	enabledRoutes := make([]*Route, 0, len(nextRoutes))
	for _, r := range nextRoutes {
		allRoutes = append(allRoutes, r)
		if r.IsEnabled() {
			enabledRoutes = append(enabledRoutes, r)
		}
	}
	sortRoutes(allRoutes)
	sortRoutes(enabledRoutes)

	snap := &Snapshot{
		Generation:  s.gen,
		Plans:       enabledPlans,
		AllPlans:    allPlans,
		Routes:      enabledRoutes,
		AllRoutes:   allRoutes,
		Diagnostics: append([]shared.Diagnostic(nil), diags...),
	}
	s.snap.Store(snap)
	if s.OnChange != nil {
		s.OnChange(snap)
	}
	return diags, complete
}

func (s *Store) loadPlansLocked() (plans map[string]*Plan, broken []string, diags []shared.Diagnostic, complete bool) {
	plans = map[string]*Plan{}
	complete = true
	dir, _ := s.dirForKind(KindPlan)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return plans, nil, nil, true
		}
		return plans, nil, []shared.Diagnostic{{Path: "$", Message: "plans directory unreadable"}}, false
	}
	count := 0
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		id := strings.TrimSuffix(name, ".yaml")
		full := filepath.Join(dir, name)
		fi, err := os.Lstat(full)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			complete = false
			diags = append(diags, shared.Diagnostic{File: name, Path: "$", Message: "symlink"})
			broken = append(broken, id)
			continue
		}
		if count >= MaxPlans {
			complete = false
			diags = append(diags, shared.Diagnostic{File: name, Path: "$", Message: "plans directory exceeds limit"})
			broken = append(broken, id)
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil {
			complete = false
			diags = append(diags, shared.Diagnostic{File: name, Path: "$", Message: "unreadable"})
			broken = append(broken, id)
			continue
		}
		p, pDiags := ValidatePlanBytes(name, id, b)
		if len(pDiags) > 0 {
			complete = false
			diags = append(diags, pDiags...)
			broken = append(broken, id)
			continue
		}
		count++
		plans[id] = p
	}
	return plans, broken, diags, complete
}

func (s *Store) loadRoutesLocked() (routes map[string]*Route, broken []string, diags []shared.Diagnostic, complete bool) {
	routes = map[string]*Route{}
	complete = true
	dir, _ := s.dirForKind(KindRoute)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return routes, nil, nil, true
		}
		return routes, nil, []shared.Diagnostic{{Path: "$", Message: "routes directory unreadable"}}, false
	}
	count := 0
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		id := strings.TrimSuffix(name, ".yaml")
		full := filepath.Join(dir, name)
		fi, err := os.Lstat(full)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			complete = false
			diags = append(diags, shared.Diagnostic{File: name, Path: "$", Message: "symlink"})
			broken = append(broken, id)
			continue
		}
		if count >= MaxRoutes {
			complete = false
			diags = append(diags, shared.Diagnostic{File: name, Path: "$", Message: "routes directory exceeds limit"})
			broken = append(broken, id)
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil {
			complete = false
			diags = append(diags, shared.Diagnostic{File: name, Path: "$", Message: "unreadable"})
			broken = append(broken, id)
			continue
		}
		r, rDiags := ValidateRouteBytes(name, id, b)
		if len(rDiags) > 0 {
			complete = false
			diags = append(diags, rDiags...)
			broken = append(broken, id)
			continue
		}
		count++
		routes[id] = r
	}
	return routes, broken, diags, complete
}

// Draft writes a staged .yaml.draft file without affecting the live Snapshot.
func (s *Store) Draft(kind, id string, body []byte) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid id %q", id)
	}
	if int64(len(body)) > MaxFileBytes {
		return fmt.Errorf("file exceeds 256 KiB")
	}
	dir, err := s.dirForKind(kind)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, id+".yaml.draft"), body, 0o600)
}

// ValidateFile checks the draft if present, otherwise the live file, including cross-file checks.
func (s *Store) ValidateFile(kind, id string) []shared.Diagnostic {
	if !idPattern.MatchString(id) {
		return []shared.Diagnostic{{Path: "$.id", Message: "invalid id"}}
	}
	dir, err := s.dirForKind(kind)
	if err != nil {
		return []shared.Diagnostic{{Path: "$", Message: err.Error()}}
	}
	draft := filepath.Join(dir, id+".yaml.draft")
	live := filepath.Join(dir, id+".yaml")
	path := live
	if _, err := os.Stat(draft); err == nil {
		path = draft
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return []shared.Diagnostic{{File: filepath.Base(path), Path: "$", Message: "unreadable"}}
	}
	snap := s.Snapshot()
	plansCopy := make(map[string]*Plan, len(snap.AllPlans)+1)
	for k, v := range snap.AllPlans {
		plansCopy[k] = v
	}
	routesCopy := make(map[string]*Route, len(snap.AllRoutes)+1)
	for _, r := range snap.AllRoutes {
		routesCopy[r.ID] = r
	}

	switch kind {
	case KindPlan:
		p, diags := ValidatePlanBytes(filepath.Base(path), id, b)
		if len(diags) > 0 {
			return diags
		}
		plansCopy[id] = p
		return filterDiagsByFile(ValidateCrossFile(plansCopy, routesCopy), "")
	case KindRoute:
		r, diags := ValidateRouteBytes(filepath.Base(path), id, b)
		if len(diags) > 0 {
			return diags
		}
		routesCopy[id] = r
		return filterDiagsByFile(ValidateCrossFile(plansCopy, routesCopy), id+".yaml")
	default:
		return []shared.Diagnostic{{Path: "$", Message: "unknown config kind"}}
	}
}

func filterDiagsByFile(diags []shared.Diagnostic, file string) []shared.Diagnostic {
	if file == "" {
		return diags
	}
	var out []shared.Diagnostic
	for _, d := range diags {
		if d.File == file {
			out = append(out, d)
		}
	}
	return out
}

// Commit validates the draft, promotes it atomically over the live file, and reloads Snapshot.
func (s *Store) Commit(kind, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid id %q", id)
	}
	dir, err := s.dirForKind(kind)
	if err != nil {
		return err
	}
	draft := filepath.Join(dir, id+".yaml.draft")
	b, err := os.ReadFile(draft)
	if err != nil {
		return fmt.Errorf("no draft for %q", id)
	}
	if diags := s.ValidateFile(kind, id); len(diags) > 0 {
		return fmt.Errorf("%s: %s", diags[0].Path, diags[0].Message)
	}
	live := filepath.Join(dir, id+".yaml")
	tmp, err := os.CreateTemp(dir, "."+id+".commit-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, live); err != nil {
		return err
	}
	cleanup = false
	_ = os.Remove(draft)
	s.Reload()
	return nil
}

// Discard removes a staged draft without altering live config.
func (s *Store) Discard(kind, id string) error {
	if !idPattern.MatchString(id) {
		return fmt.Errorf("invalid id %q", id)
	}
	dir, err := s.dirForKind(kind)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, id+".yaml.draft"))
}

// SetSnapshot directly installs an in-memory snapshot (useful in unit tests).
func (s *Store) SetSnapshot(snap *Snapshot) {
	if s == nil || snap == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	snap.Generation = s.gen
	s.snap.Store(snap)
	if s.OnChange != nil {
		s.OnChange(snap)
	}
}

func sortRoutes(routes []*Route) {
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Priority != routes[j].Priority {
			return routes[i].Priority < routes[j].Priority
		}
		return routes[i].ID < routes[j].ID
	})
}
