package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/decision"
	"github.com/lidessen/crabbro/internal/history"
	"github.com/lidessen/crabbro/internal/shared"
)

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.History == nil {
		writeJSON(w, http.StatusOK, history.Status{Enabled: false})
		return
	}
	writeJSON(w, http.StatusOK, s.History.Status())
}

func (s *Server) handleHistoryPrune(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.History == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "history is not enabled")
		return
	}
	n, err := s.History.Prune()
	if err != nil {
		writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pruned": n, "requests": s.History.Count()})
}

func (s *Server) collectRequests() []*shared.Request {
	seen := map[shared.RequestID]*shared.Request{}
	var order []shared.RequestID
	if s.History != nil {
		if rows, err := s.History.List(0); err == nil {
			for _, row := range rows {
				seen[row.ID] = row
				order = append(order, row.ID)
			}
		}
	}
	if s.RingBuffer != nil {
		for _, row := range s.RingBuffer.List(0) {
			if _, ok := seen[row.ID]; ok {
				seen[row.ID] = row
				continue
			}
			seen[row.ID] = row
			order = append(order, row.ID)
		}
	}
	out := make([]*shared.Request, 0, len(order))
	for _, id := range order {
		out = append(out, seen[id])
	}
	return out
}

func (s *Server) lookupRequest(id shared.RequestID) *shared.Request {
	if s.RingBuffer != nil {
		if req := s.RingBuffer.Resolve(id); req != nil {
			return req
		}
	}
	if s.History == nil {
		return nil
	}
	if req, _, _, err := s.History.Get(id); err == nil && req != nil {
		return req
	}
	rows, err := s.History.List(0)
	if err != nil {
		return nil
	}
	var match *shared.Request
	for _, row := range rows {
		if strings.HasPrefix(string(row.ID), string(id)) {
			if match != nil {
				return nil
			}
			match = row
		}
	}
	return match
}

func (s *Server) bodiesFor(req *shared.Request) ([]byte, []byte) {
	var rb, sb []byte
	if req == nil {
		return nil, nil
	}
	if s.BodyCache != nil {
		rb = s.BodyCache.Get(req.RequestBodyRef)
		sb = s.BodyCache.Get(req.ResponseBodyRef)
	}
	if s.History != nil && (len(rb) == 0 || len(sb) == 0) {
		hb, hs, err := s.History.Bodies(req.ID)
		if err == nil {
			if len(rb) == 0 {
				rb = hb
			}
			if len(sb) == 0 {
				sb = hs
			}
		}
	}
	return rb, sb
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if s.Sessions == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "sessions are not available")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"sessions": s.Sessions.List()})
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		sess := s.Sessions.Start(body.Name)
		writeJSON(w, http.StatusCreated, sess)
	default:
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
	}
}

func (s *Server) handleSessionCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Sessions == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "sessions are not available")
		return
	}
	cur := s.Sessions.Current()
	writeJSON(w, http.StatusOK, map[string]any{"session": cur})
}

func (s *Server) handleSessionByID(w http.ResponseWriter, r *http.Request) {
	if s.Sessions == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "sessions are not available")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	if path == "" || path == "current" {
		s.handleSessionCurrent(w, r)
		return
	}
	if strings.HasSuffix(path, "/stop") {
		id := strings.TrimSuffix(path, "/stop")
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
			return
		}
		sess, err := s.Sessions.Stop(id)
		if err != nil {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sess)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	sess, err := s.Sessions.Get(path)
	if err != nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Sessions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"activity": []shared.Activity{}})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items := s.Sessions.Activity(r.URL.Query().Get("session"), limit)
	writeJSON(w, http.StatusOK, map[string]any{"activity": items})
}

func (s *Server) handleTrajectory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	q := r.URL.Query()
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid limit")
			return
		}
		limit = n
	}
	if s.Sessions == nil {
		writeJSON(w, http.StatusOK, shared.TrajectoryPage{Items: []shared.Activity{}})
		return
	}
	items, next, err := s.Sessions.Trajectory(q.Get("session"), q.Get("cursor"), limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error())
		return
	}
	if items == nil {
		items = []shared.Activity{}
	}
	writeJSON(w, http.StatusOK, shared.TrajectoryPage{Items: items, NextCursor: next})
}

func (s *Server) handleVerifyReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Replay == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "replay is not available")
		return
	}
	var body shared.VerifyRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body")
		return
	}
	result, err := s.Replay(body)
	if err != nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleFind(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Decision == nil || !s.Decision.Status().Configured {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "decision API is not configured")
		return
	}
	var body struct {
		Query     string `json:"query"`
		Limit     int    `json:"limit"`
		Type      string `json:"type"`
		Initiator string `json:"initiator"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Query) == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "query is required")
		return
	}
	flows := s.flowSummaries(body.Limit, body.Type, body.Initiator)
	result, err := s.Decision.Find(r.Context(), body.Query, flows)
	if err != nil {
		if errors.Is(err, decision.ErrNotConfigured) {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, shared.ErrCodeInternal, err.Error())
		return
	}
	if s.Sessions != nil {
		s.Sessions.Note("find", "", "", "", body.Query)
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDecisionStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Decision == nil {
		writeJSON(w, http.StatusOK, decision.Status{Configured: false})
		return
	}
	writeJSON(w, http.StatusOK, s.Decision.Status())
}

func (s *Server) handleRulesDraft(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	var body struct {
		Description string `json:"description"`
		RequestID   string `json:"requestId"`
		Apply       bool   `json:"apply"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Description) == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "description is required")
		return
	}
	var flow *decision.Flow
	if body.RequestID != "" && s.RingBuffer != nil {
		req := s.RingBuffer.Resolve(shared.RequestID(body.RequestID))
		if req == nil {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "request not found")
			return
		}
		flow = s.flowFrom(req)
	}
	if s.Decision == nil || !s.Decision.Status().Configured {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "decision API is not configured")
		return
	}
	draft, err := s.Decision.Draft(r.Context(), body.Description, flow)
	if err != nil {
		writeError(w, http.StatusBadGateway, shared.ErrCodeInternal, err.Error())
		return
	}
	resp := map[string]any{"draft": draft}
	if body.Apply && s.RuleStore != nil {
		created, err := s.RuleStore.Create(draft.Name, shared.DefaultRulePriority, draft.Script)
		if err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error())
			return
		}
		resp["rule"] = created
		if s.Sessions != nil {
			s.Sessions.Note("rule_created", "", body.RequestID, string(created.ID), draft.Name)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) flowSummaries(limit int, typ, initiator string) []decision.Flow {
	if s.RingBuffer == nil {
		return nil
	}
	if limit <= 0 || limit > 32 {
		limit = 32
	}
	all := s.RingBuffer.List(0)
	out := make([]decision.Flow, 0, len(all))
	for _, req := range all {
		if req.Method == "CONNECT" || req.Hidden {
			continue
		}
		if typ != "" && (req.Attribution == nil || !strings.EqualFold(req.Attribution.ResourceType, typ)) {
			continue
		}
		if initiator != "" && (req.Attribution == nil || !strings.EqualFold(req.Attribution.InitiatorURL, initiator)) {
			continue
		}
		out = append(out, *s.flowFrom(req))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (s *Server) flowFrom(req *shared.Request) *decision.Flow {
	f := &decision.Flow{
		ID:     string(req.ID),
		Method: req.Method,
		URL:    req.URL,
		Status: req.Status,
	}
	if s.BodyCache != nil && req.RequestBodyRef != "" {
		if b := s.BodyCache.Get(req.RequestBodyRef); len(b) > 0 {
			f.RequestBody = previewText(b)
		}
	}
	if s.BodyCache != nil && req.ResponseBodyRef != "" {
		if b := s.BodyCache.Get(req.ResponseBodyRef); len(b) > 0 {
			f.ResponseBody = previewText(b)
		}
	}
	return f
}

func previewText(b []byte) string {
	const max = 500
	if len(b) > max {
		return string(b[:max])
	}
	return string(b)
}

// filterRequests applies agent-facing list filters. since is an ID cursor.
// sinceTime is RFC3339 or a Go duration such as 5m.
func filterRequests(all []*shared.Request, q map[string]string) ([]*shared.Request, error) {
	var sinceTime time.Time
	if raw := q["sinceTime"]; raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			sinceTime = t
		} else if d, err := time.ParseDuration(raw); err == nil {
			sinceTime = time.Now().Add(-d)
		} else {
			return nil, err
		}
	}
	method := strings.ToUpper(q["method"])
	host := strings.ToLower(q["host"])
	path := q["path"]
	status := 0
	if q["status"] != "" {
		n, err := strconv.Atoi(q["status"])
		if err != nil {
			return nil, err
		}
		status = n
	}
	needle := strings.ToLower(q["q"])
	tag := q["tag"]
	session := q["session"]
	kindFilter := strings.ToLower(q["kind"])
	out := make([]*shared.Request, 0, len(all))
	for _, req := range all {
		if kindFilter != "" {
			k := req.Kind
			if k == "" {
				k = "proxy"
			}
			if !strings.EqualFold(k, kindFilter) {
				continue
			}
		}
		if !sinceTime.IsZero() && req.StartedAt.Before(sinceTime) {
			continue
		}
		if method != "" && !strings.EqualFold(req.Method, method) {
			continue
		}
		if host != "" && !strings.Contains(strings.ToLower(req.Host), host) && !strings.Contains(strings.ToLower(req.URL), host) {
			continue
		}
		if path != "" && !strings.HasPrefix(req.Path, path) && !strings.Contains(req.URL, path) {
			continue
		}
		if status != 0 && req.Status != status {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(req.URL), needle) && !strings.Contains(strings.ToLower(req.Method), needle) {
			continue
		}
		if tag != "" && !hasTag(req.Tags, tag) {
			continue
		}
		if session != "" && req.SessionID != session {
			continue
		}
		if typ := q["type"]; typ != "" {
			got := ""
			if req.Attribution != nil {
				got = req.Attribution.ResourceType
			}
			if !strings.EqualFold(got, typ) {
				continue
			}
		}
		if init := q["initiator"]; init != "" {
			got := ""
			if req.Attribution != nil {
				got = req.Attribution.InitiatorURL
			}
			if !strings.EqualFold(got, init) {
				continue
			}
		}
		out = append(out, req)
	}
	return out, nil
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
