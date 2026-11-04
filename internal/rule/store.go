package rule

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

// RuleStore provides in-memory CRUD for rules. It is safe for concurrent
// use by multiple goroutines (sync.RWMutex).
//
// Rules are lost on daemon restart — no file or database persistence.
type RuleStore struct {
	mu         sync.RWMutex
	rules      map[shared.RuleID]*shared.Rule
	nextID     atomic.Int64
	generation atomic.Uint64
}

// NewRuleStore creates an empty RuleStore.
func NewRuleStore() *RuleStore {
	return &RuleStore{
		rules: make(map[shared.RuleID]*shared.Rule),
	}
}

// Count returns the number of rules in the store.
func (s *RuleStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rules)
}

// List returns all rules, ordered by ID (insertion order approximation).
func (s *RuleStore) List() []*shared.Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*shared.Rule, 0, len(s.rules))
	// Collect by insertion order approximating via iteration.
	for _, r := range s.rules {
		result = append(result, r)
	}
	return result
}

// Get returns a single rule by ID. Returns an error if not found.
func (s *RuleStore) Get(id shared.RuleID) (*shared.Rule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	r, ok := s.rules[id]
	if !ok {
		return nil, fmt.Errorf("rule %q not found", id)
	}
	return r, nil
}

// Create inserts a new rule with the given fields and a generated ID.
// The script field is required.
func (s *RuleStore) Create(name string, priority int, script string) (*shared.Rule, error) {
	if script == "" {
		return nil, fmt.Errorf("rule script must not be empty")
	}

	if priority == 0 {
		priority = shared.DefaultRulePriority
	}

	now := time.Now().UTC()
	id := shared.RuleID(fmt.Sprintf("rule_%d", s.nextID.Add(1)))

	rule := &shared.Rule{
		ID:          id,
		Name:        name,
		Priority:    priority,
		Enabled:     true,
		Script:      script,
		Version:     1,
		FailureMode: "fail-open",
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	s.mu.Lock()
	s.rules[id] = rule
	s.generation.Add(1)
	s.mu.Unlock()

	return rule, nil
}

// Update modifies the fields of an existing rule. Empty/zero fields are not
// updated. The script must not be empty if provided.
func (s *RuleStore) Update(id shared.RuleID, name *string, priority *int, script *string) (*shared.Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.rules[id]
	if !ok {
		return nil, fmt.Errorf("rule %q not found", id)
	}

	if name != nil {
		r.Name = *name
	}
	if priority != nil {
		r.Priority = *priority
	}
	if script != nil {
		if *script == "" {
			return nil, fmt.Errorf("rule script must not be empty")
		}
		r.Script = *script
		r.Version++
	}

	r.UpdatedAt = time.Now().UTC()
	s.generation.Add(1)
	return r, nil
}

// Delete removes a rule by ID. Returns an error if not found.
func (s *RuleStore) Delete(id shared.RuleID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.rules[id]; !ok {
		return fmt.Errorf("rule %q not found", id)
	}

	delete(s.rules, id)
	s.generation.Add(1)
	return nil
}

// SetEnabled enables or disables a rule. Returns an error if not found.
func (s *RuleStore) SetEnabled(id shared.RuleID, enabled bool) (*shared.Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.rules[id]
	if !ok {
		return nil, fmt.Errorf("rule %q not found", id)
	}

	r.Enabled = enabled
	r.UpdatedAt = time.Now().UTC()
	s.generation.Add(1)
	return r, nil
}

// SetFailureMode sets fail-open or fail-closed. Unknown values are rejected.
func (s *RuleStore) SetFailureMode(id shared.RuleID, mode string) (*shared.Rule, error) {
	if mode != "fail-open" && mode != "fail-closed" {
		return nil, fmt.Errorf("failureMode must be fail-open or fail-closed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rules[id]
	if !ok {
		return nil, fmt.Errorf("rule %q not found", id)
	}
	r.FailureMode = mode
	r.UpdatedAt = time.Now().UTC()
	s.generation.Add(1)
	return r, nil
}

// Snapshot returns a stable copy of the enabled rules and the generation that
// produced them. Order is ascending priority, then rule ID. Callers must not
// share the slice with later mutations; the pointers are copies.
func (s *RuleStore) Snapshot() (uint64, []*shared.Rule) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*shared.Rule, 0, len(s.rules))
	for _, r := range s.rules {
		if !r.Enabled {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].ID < out[j].ID
	})
	return s.generation.Load(), out
}

// UpsertFile inserts or replaces a file-backed rule. The id is the document id.
func (s *RuleStore) UpsertFile(r *shared.Rule) error {
	if r == nil || r.ID == "" {
		return fmt.Errorf("rule id is required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.rules[r.ID]
	cp := *r
	cp.Source = "file"
	if ok {
		cp.CreatedAt = prev.CreatedAt
		if prev.Script != r.Script {
			cp.Version = prev.Version + 1
		} else {
			cp.Version = prev.Version
		}
	} else if cp.Version == 0 {
		cp.Version = 1
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	s.rules[r.ID] = &cp
	s.generation.Add(1)
	return nil
}

// DropFileRules removes file-backed rules whose ids are not in keep.
func (s *RuleStore) DropFileRules(keep map[shared.RuleID]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, r := range s.rules {
		if r.Source != "file" {
			continue
		}
		if _, ok := keep[id]; ok {
			continue
		}
		delete(s.rules, id)
		changed = true
	}
	if changed {
		s.generation.Add(1)
	}
}

// EnabledRules returns all currently enabled rules sorted by priority
// (ascending). Each returned rule is a pointer to the store's internal
// copy; callers must not mutate them.
func (s *RuleStore) EnabledRules() []*shared.Rule {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*shared.Rule, 0, len(s.rules))
	for _, r := range s.rules {
		if r.Enabled {
			result = append(result, r)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority < result[j].Priority
		}
		return result[i].ID < result[j].ID
	})
	return result
}
