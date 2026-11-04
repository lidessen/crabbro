// Package session records named capture windows and a trajectory of what the
// agent did. A future UI can render this as a timeline.
//
// Activity.Kind is an open string. Inspection scope changes (docs/design/inspect.md)
// will use kinds such as scope_picked and scope_reset on the current session.
// This package does not interpret those kinds yet.
package session

import (
	"fmt"
	"sync"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

// Store is an in-memory session and activity log. It is safe for concurrent use.
type Store struct {
	mu       sync.Mutex
	next     int64
	sessions []*shared.Session
	current  *shared.Session
	activity []shared.Activity
	max      int
}

// New creates a store that keeps up to max activity entries (and the same
// number of historical sessions).
func New(max int) *Store {
	if max < 1 {
		max = 1024
	}
	return &Store{max: max}
}

// Start opens a recording session. An already-open session is stopped first.
func (s *Store) Start(name string) *shared.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil && s.current.Recording {
		s.stopLocked(s.current.ID)
	}
	s.next++
	now := time.Now().UTC()
	sess := &shared.Session{
		ID:        fmt.Sprintf("ses_%d", s.next),
		Name:      name,
		StartedAt: now,
		Recording: true,
	}
	s.sessions = append(s.sessions, sess)
	s.current = sess
	s.appendLocked(shared.Activity{
		At:        now,
		Kind:      "session_started",
		SessionID: sess.ID,
		Summary:   fmt.Sprintf("recording started (%s)", displayName(sess)),
	})
	return cloneSession(sess)
}

// Stop ends the session with the given ID, or the current session when id is empty.
func (s *Store) Stop(id string) (*shared.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" && s.current != nil {
		id = s.current.ID
	}
	sess := s.stopLocked(id)
	if sess == nil {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return cloneSession(sess), nil
}

func (s *Store) stopLocked(id string) *shared.Session {
	for _, sess := range s.sessions {
		if sess.ID != id {
			continue
		}
		if sess.Recording {
			now := time.Now().UTC()
			sess.Recording = false
			sess.StoppedAt = &now
			s.appendLocked(shared.Activity{
				At:        now,
				Kind:      "session_stopped",
				SessionID: sess.ID,
				Summary:   fmt.Sprintf("recording stopped (%s)", displayName(sess)),
			})
		}
		if s.current != nil && s.current.ID == id {
			s.current = nil
		}
		return sess
	}
	return nil
}

// Current returns the active recording session, or nil.
func (s *Store) Current() *shared.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || !s.current.Recording {
		return nil
	}
	return cloneSession(s.current)
}

// List returns sessions oldest first.
func (s *Store) List() []shared.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]shared.Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, *cloneSession(sess))
	}
	return out
}

// Get returns one session.
func (s *Store) Get(id string) (*shared.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ID == id {
			return cloneSession(sess), nil
		}
	}
	return nil, fmt.Errorf("session %q not found", id)
}

// NoteRequest attaches a captured request to the active session, if any.
func (s *Store) NoteRequest(req *shared.Request) {
	if req == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || !s.current.Recording {
		return
	}
	req.SessionID = s.current.ID
	s.current.RequestIDs = append(s.current.RequestIDs, string(req.ID))
	s.appendLocked(shared.Activity{
		At:        time.Now().UTC(),
		Kind:      "request_captured",
		SessionID: s.current.ID,
		RequestID: string(req.ID),
		Summary:   fmt.Sprintf("%s %s", req.Method, req.URL),
	})
}

// Note records a trajectory event. sessionID defaults to the active session.
func (s *Store) Note(kind, sessionID, requestID, ruleID, summary string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID == "" && s.current != nil {
		sessionID = s.current.ID
	}
	s.appendLocked(shared.Activity{
		At:        time.Now().UTC(),
		Kind:      kind,
		SessionID: sessionID,
		RequestID: requestID,
		RuleID:    ruleID,
		Summary:   summary,
	})
}

// Activity returns recent trajectory entries, optionally filtered by session.
func (s *Store) Activity(sessionID string, limit int) []shared.Activity {
	s.mu.Lock()
	defer s.mu.Unlock()
	var src []shared.Activity
	if sessionID == "" {
		src = s.activity
	} else {
		for _, a := range s.activity {
			if a.SessionID == sessionID {
				src = append(src, a)
			}
		}
	}
	if limit > 0 && len(src) > limit {
		src = src[len(src)-limit:]
	}
	out := make([]shared.Activity, len(src))
	copy(out, src)
	return out
}

func (s *Store) appendLocked(a shared.Activity) {
	s.next++
	a.ID = fmt.Sprintf("act_%d", s.next)
	s.activity = append(s.activity, a)
	if len(s.activity) > s.max {
		s.activity = s.activity[len(s.activity)-s.max:]
	}
}

func cloneSession(sess *shared.Session) *shared.Session {
	cp := *sess
	if sess.RequestIDs != nil {
		cp.RequestIDs = append([]string(nil), sess.RequestIDs...)
	}
	if sess.StoppedAt != nil {
		t := *sess.StoppedAt
		cp.StoppedAt = &t
	}
	return &cp
}

func displayName(sess *shared.Session) string {
	if sess.Name != "" {
		return sess.Name
	}
	return sess.ID
}
