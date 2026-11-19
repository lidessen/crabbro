// Package capture owns the shared exchange observation pipeline (ring buffer,
// bounded body cache, SSE event hub, session trajectory, and SQLite history)
// used by both the forward proxy and the LLM gateway.
package capture

import (
	"time"

	"github.com/lidessen/crabbro/internal/history"
	"github.com/lidessen/crabbro/internal/ring"
	"github.com/lidessen/crabbro/internal/session"
	"github.com/lidessen/crabbro/internal/shared"
)

// ExchangeSink records live exchanges, bounded bodies, events, trajectory
// steps, and completed history rows for any data-plane subsystem.
type ExchangeSink interface {
	Start(rec *shared.Request) *shared.Request
	StoreBody(data []byte) shared.BodyRef
	GetBody(ref shared.BodyRef) []byte
	Emit(typ shared.EventType, id shared.RequestID, payload any)
	NoteActivity(kind, sessionID, reqID, ruleID, summary string)
	Complete(rec *shared.Request)
	Resolve(id shared.RequestID) *shared.Request
	List(limit int) []*shared.Request
	SessionStore() *session.Store
}

// DefaultSink wires RingBuffer, BodyCache, EventHub, Sessions, and History into
// one shared ExchangeSink.
type DefaultSink struct {
	RingBuffer *ring.RequestBuffer
	BodyCache  *BodyCache
	EventHub   *EventHub
	Sessions   *session.Store
	History    *history.Store
}

// NewDefaultSink constructs a DefaultSink with the supplied stores.
func NewDefaultSink(rb *ring.RequestBuffer, bc *BodyCache, hub *EventHub, sess *session.Store, hist *history.Store) *DefaultSink {
	return &DefaultSink{
		RingBuffer: rb,
		BodyCache:  bc,
		EventHub:   hub,
		Sessions:   sess,
		History:    hist,
	}
}

// Start registers rec in the ring buffer (assigning a stable req_<n> ID) and
// records it against any active recording session.
func (s *DefaultSink) Start(rec *shared.Request) *shared.Request {
	if s == nil || rec == nil {
		return rec
	}
	if s.RingBuffer != nil {
		stored := s.RingBuffer.Add(rec.Method, rec.Scheme, rec.Host, rec.Path, rec.Query, rec.URL, rec.ClientAddr)
		id := stored.ID
		startedAt := stored.StartedAt
		*stored = *rec
		stored.ID = id
		if stored.StartedAt.IsZero() {
			stored.StartedAt = startedAt
		}
		rec = stored
	}
	if s.Sessions != nil {
		s.Sessions.NoteRequest(rec)
	}
	return rec
}

// StoreBody caches a bounded body preview and returns its reference key.
func (s *DefaultSink) StoreBody(data []byte) shared.BodyRef {
	if s == nil || s.BodyCache == nil {
		return ""
	}
	return s.BodyCache.Store(data)
}

// GetBody retrieves a previously cached body preview.
func (s *DefaultSink) GetBody(ref shared.BodyRef) []byte {
	if s == nil || s.BodyCache == nil {
		return nil
	}
	return s.BodyCache.Get(ref)
}

// Emit publishes a structured lifecycle event to EventHub.
func (s *DefaultSink) Emit(typ shared.EventType, id shared.RequestID, payload any) {
	if s == nil || s.EventHub == nil {
		return
	}
	s.EventHub.Emit(&shared.Event{
		Type:      typ,
		RequestID: id,
		Timestamp: time.Now(),
		Payload:   payload,
	})
}

// NoteActivity appends a step to the session trajectory log.
func (s *DefaultSink) NoteActivity(kind, sessionID, reqID, ruleID, summary string) {
	if s == nil || s.Sessions == nil {
		return
	}
	s.Sessions.Note(kind, sessionID, reqID, ruleID, summary)
}

// Complete persists a finished exchange (and any cached body previews) to SQLite history.
func (s *DefaultSink) Complete(rec *shared.Request) {
	if s == nil || s.History == nil || rec == nil || rec.ID == "" {
		return
	}
	var rb, sb []byte
	if s.BodyCache != nil {
		if b := s.BodyCache.Get(rec.RequestBodyRef); len(b) > 0 {
			rb = append([]byte(nil), b...)
		}
		if b := s.BodyCache.Get(rec.ResponseBodyRef); len(b) > 0 {
			sb = append([]byte(nil), b...)
		}
	}
	_ = s.History.Save(rec, rb, sb)
}

// Resolve looks up a captured exchange by full ID or unique prefix.
func (s *DefaultSink) Resolve(id shared.RequestID) *shared.Request {
	if s == nil || s.RingBuffer == nil {
		return nil
	}
	return s.RingBuffer.Resolve(id)
}

// List returns up to limit captured exchanges from the ring buffer.
func (s *DefaultSink) List(limit int) []*shared.Request {
	if s == nil || s.RingBuffer == nil {
		return nil
	}
	return s.RingBuffer.List(limit)
}

// SessionStore exposes the underlying session store for inspect scope checks.
func (s *DefaultSink) SessionStore() *session.Store {
	if s == nil {
		return nil
	}
	return s.Sessions
}
