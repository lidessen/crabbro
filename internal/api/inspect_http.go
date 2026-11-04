package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lidessen/chiling/internal/shared"
)

func (s *Server) scopeActive() bool {
	if s.Inspect == nil {
		return false
	}
	st := s.Inspect.Status("", false)
	return len(st.Pick)+len(st.Omit)+len(st.Bypass)+len(st.ConfigDeny)+len(st.ConfigAllow) > 0
}

func (s *Server) emitScope(st shared.ScopeState) {
	if s == nil || s.EventHub == nil {
		return
	}
	s.EventHub.Emit(&shared.Event{
		Type:      shared.EventScopeChanged,
		Timestamp: time.Now().UTC(),
		Payload:   st,
	})
}

func (s *Server) handleSources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Inspect == nil || s.Inspect.Catalog == nil {
		writeJSON(w, http.StatusOK, map[string]any{"sources": []any{}})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/sources/")
	if id != "" && id != r.URL.Path {
		src, pages, ok := s.Inspect.Catalog.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "source not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"source": src, "pages": pages})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": s.Inspect.Catalog.List()})
}

func (s *Server) handleScope(w http.ResponseWriter, r *http.Request) {
	if s.Sessions == nil || s.Inspect == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "scope is not available")
		return
	}
	if r.Method == http.MethodGet {
		sess, _, implicit := s.Sessions.EnsureScope()
		id := ""
		if sess != nil {
			id = sess.ID
		}
		writeJSON(w, http.StatusOK, s.Inspect.Status(id, implicit))
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	var body struct {
		Op   string   `json:"op"`
		Expr []string `json:"expr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Op == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "op is required")
		return
	}
	sess, scope, implicit := s.Sessions.EnsureScope()
	var err error
	kind, summary := "", ""
	switch body.Op {
	case "pick":
		err = scope.ReplacePick(body.Expr)
		kind, summary = "scope_picked", "picked"
	case "omit", "off":
		err = scope.AddOmit(body.Expr)
		kind, summary = "scope_omitted", "omitted"
	case "on":
		if len(body.Expr) != 1 {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "on takes one expression")
			return
		}
		err = scope.Restore(body.Expr[0])
		kind, summary = "scope_restored", "restored"
	case "reset":
		scope.Reset()
		kind, summary = "scope_reset", "reset"
	case "bypass":
		err = scope.AddBypass(body.Expr)
		kind, summary = "scope_bypass", "bypass"
	default:
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "unknown scope op")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid expression")
		return
	}
	s.Sessions.Note(kind, sess.ID, "", "", summary)
	st := s.Inspect.Status(sess.ID, implicit)
	s.emitScope(st)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleScopeDraft(w http.ResponseWriter, r *http.Request, id string) bool {
	if s.Inspect == nil || s.Inspect.Durable == nil || r.URL.Query().Get("kind") != "scope" {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "draft rejected")
		return true
	}
	if err := s.Inspect.Durable.Draft(id, body); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "draft rejected")
		return true
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "draft": true})
	return true
}
