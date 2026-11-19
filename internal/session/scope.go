package session

import (
	"github.com/lidessen/crabbro/internal/inspect"
	"github.com/lidessen/crabbro/internal/shared"
)

// CurrentScope returns the scope for the open recording session, or nil.
func (s *Store) CurrentScope() *inspect.Scope {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || !s.current.Recording {
		return nil
	}
	return s.scopes[s.current.ID]
}

// EnsureScope returns the open session's scope, starting an implicit session
// named "scope" when nothing is recording. implicit is true when this call
// opened that session.
func (s *Store) EnsureScope() (*shared.Session, *inspect.Scope, bool) {
	if s == nil {
		return nil, nil, false
	}
	s.mu.Lock()
	recording := s.current != nil && s.current.Recording
	s.mu.Unlock()
	implicit := !recording
	if implicit {
		s.Start("scope")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scopes == nil {
		s.scopes = map[string]*inspect.Scope{}
	}
	sc := s.scopes[s.current.ID]
	if sc == nil {
		sc = &inspect.Scope{}
		s.scopes[s.current.ID] = sc
	}
	return cloneSession(s.current), sc, implicit
}
