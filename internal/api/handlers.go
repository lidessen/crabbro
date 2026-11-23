package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/crabbro/internal/fixture"
	"github.com/lidessen/crabbro/internal/shared"
)

// handleStatus responds to GET /v1/status.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}

	uptime := int64(time.Since(s.StartedAt()).Seconds())

	requestCount := int64(0)
	if s.RequestCountFn != nil {
		requestCount = s.RequestCountFn()
	}
	ruleCount := 0
	if s.RuleCountFn != nil {
		ruleCount = s.RuleCountFn()
	}

	executable, _ := os.Executable()
	resp := shared.StatusResponse{
		PID:           os.Getpid(),
		Executable:    executable,
		Version:       s.Version,
		UptimeSeconds: uptime,
		ProxyAddr:     s.ProxyAddr,
		RequestCount:  requestCount,
		RuleCount:     ruleCount,
	}

	if s.MITMHandler != nil {
		resp.MITMEnabled = s.MITMHandler.Enabled()
		resp.MITMCertInstalled = s.MITMHandler.CertInstalled()
	}
	if s.Gateway != nil {
		st := s.Gateway.Status()
		resp.GatewayEnabled = st.Enabled
		resp.GatewayAddr = st.ListenAddr
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleEvents responds to GET /v1/events via Server-Sent Events.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	after := int64(-1)
	if raw := r.URL.Query().Get("after"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid after cursor")
			return
		}
		after = n
	} else if raw := strings.TrimSpace(r.Header.Get("Last-Event-ID")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n >= 0 {
			after = n
		}
	}
	want := map[string]struct{}{}
	for _, raw := range r.URL.Query()["type"] {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				want[part] = struct{}{}
			}
		}
	}

	sub := s.EventHub.SubscribeAfter(after)
	defer s.EventHub.Unsubscribe(sub)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-sub:
			if !ok {
				return
			}
			if len(want) > 0 {
				if _, ok := want[string(event.Type)]; !ok {
					continue
				}
			}
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.ID, data)
			flusher.Flush()
		}
	}
}

// handleRequests responds to GET /v1/requests with optional pagination.
// Query params: since (cursor), limit (int), sinceTime (ISO 8601)
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}

	q := r.URL.Query()
	cursor := q.Get("since")
	// A duration or timestamp in --since is a time filter, not an ID cursor.
	if cursor != "" && !strings.HasPrefix(cursor, "req_") {
		if _, err := time.ParseDuration(cursor); err == nil {
			q.Set("sinceTime", cursor)
			cursor = ""
		} else if _, err := time.Parse(time.RFC3339, cursor); err == nil {
			q.Set("sinceTime", cursor)
			cursor = ""
		}
	}
	limitStr := q.Get("limit")

	limit := 0
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil || limit < 0 {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid limit parameter",
				shared.ErrorDetail{Field: "limit", Message: "must be a non-negative integer"})
			return
		}
	}

	requests := s.collectRequests()
	if cursor != "" {
		filtered := requests[:0]
		seen := false
		for _, req := range requests {
			if seen {
				filtered = append(filtered, req)
			}
			if string(req.ID) == cursor {
				seen = true
			}
		}
		requests = filtered
	}
	filtered, err := filterRequests(requests, map[string]string{
		"sinceTime": q.Get("sinceTime"),
		"method":    q.Get("method"),
		"host":      q.Get("host"),
		"path":      q.Get("path"),
		"status":    q.Get("status"),
		"query":     q.Get("query"),
		"tag":       q.Get("tag"),
		"session":   q.Get("session"),
		"type":      q.Get("type"),
		"initiator": q.Get("initiator"),
		"kind":      q.Get("kind"),
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid filter",
			shared.ErrorDetail{Field: "filter", Message: err.Error()})
		return
	}
	requests = filtered
	showAll := q.Get("all") == "1" || q.Get("all") == "true"
	hiddenCount := 0
	if !showAll {
		visible := requests[:0]
		for _, req := range requests {
			if req.Hidden {
				hiddenCount++
				continue
			}
			visible = append(visible, req)
		}
		requests = visible
	}
	if limit > 0 && len(requests) > limit {
		if cursor != "" {
			requests = requests[:limit]
		} else {
			requests = requests[len(requests)-limit:]
		}
	}

	// Convert to summaries.
	summaries := make([]shared.RequestSummary, 0, len(requests))
	for _, req := range requests {
		status := req.Status
		durationMs := req.DurationMs

		kind := req.Kind
		if kind == "" {
			kind = "proxy"
		}
		sum := shared.RequestSummary{
			ID:           req.ID,
			StartedAt:    req.StartedAt.Format(time.RFC3339Nano),
			Kind:         kind,
			Method:       req.Method,
			URL:          req.URL,
			Status:       status,
			DurationMs:   durationMs,
			Tags:         req.Tags,
			Hidden:       req.Hidden,
			HiddenReason: req.HiddenReason,
		}
		if req.Attribution != nil {
			sum.ResourceType = req.Attribution.ResourceType
			sum.InitiatorURL = req.Attribution.InitiatorURL
			sum.SourceID = req.Attribution.SourceID
		}
		if req.Gateway != nil {
			sum.GatewayPlan = req.Gateway.ChosenPlan
			sum.GatewayModel = req.Gateway.ChosenModel
			sum.GatewayRoute = req.Gateway.RouteID
		}
		summaries = append(summaries, sum)
	}

	// Calculate next cursor.
	var nextCursor string
	if limit > 0 && len(requests) == limit {
		lastReq := requests[len(requests)-1]
		nextCursor = string(lastReq.ID)
	}

	resp := shared.ListRequestsResponse{
		Requests:    summaries,
		NextCursor:  nextCursor,
		HiddenCount: hiddenCount,
		ScopeActive: s.scopeActive(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRequestDetail responds to GET /v1/requests/:id with detailed request info.
// Query param: include (comma-separated: headers, body, rule-trace)
func (s *Server) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}

	// Extract ID from path: /v1/requests/:id
	idStr := strings.TrimPrefix(r.URL.Path, "/v1/requests/")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "request ID is required")
		return
	}

	req := s.lookupRequest(shared.RequestID(idStr))
	if req == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "request not found",
			shared.ErrorDetail{Field: "id", Message: fmt.Sprintf("no request with ID %s", idStr)})
		return
	}

	include := parseIncludeParam(r.URL.Query().Get("include"))

	resp := shared.RequestDetailResponse{
		Request: *req,
	}

	if include["body"] {
		rb, sb := s.bodiesFor(req)
		if len(rb) > 0 {
			bodyStr := string(rb)
			resp.Body = &bodyStr
		}
		if len(sb) > 0 {
			bodyStr := string(sb)
			resp.ResponseBody = &bodyStr
		}
	}

	// Include headers if requested.
	if include["headers"] {
		headers := shared.Headers{}
		for k, v := range req.RequestHeaders {
			headers[k] = v
		}
		for k, v := range req.ResponseHeaders {
			responseKey := "response:" + k
			headers[responseKey] = v
		}
		resp.Headers = &headers
	}

	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Fixture handlers
// ---------------------------------------------------------------------------

// saveFixtureRequest is the JSON body for POST /v1/fixtures.
type saveFixtureRequest struct {
	RequestID string `json:"requestId"`
	Name      string `json:"name"`
}

// handleFixturesListOrSave dispatches to list or save based on HTTP method.
//   - GET  /v1/fixtures  returns a list of saved fixture summaries.
//   - POST /v1/fixtures  captures a request as a new fixture.
func (s *Server) handleFixturesListOrSave(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleFixturesList(w, r)
	case http.MethodPost:
		s.handleFixtureSave(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
	}
}

// handleFixturesList responds to GET /v1/fixtures.
func (s *Server) handleFixturesList(w http.ResponseWriter, r *http.Request) {
	summaries, err := s.FixtureStore.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, fmt.Sprintf("failed to list fixtures: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, summaries)
}

// handleFixtureSave responds to POST /v1/fixtures.
func (s *Server) handleFixtureSave(w http.ResponseWriter, r *http.Request) {
	var req saveFixtureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body",
			shared.ErrorDetail{Field: "body", Message: "expected JSON with requestId and name fields"})
		return
	}

	if req.RequestID == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "requestId is required",
			shared.ErrorDetail{Field: "requestId", Message: "must be a non-empty string"})
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "name is required",
			shared.ErrorDetail{Field: "name", Message: "must be a non-empty string"})
		return
	}

	// Look up the request from the ring buffer.
	rec := s.RingBuffer.GetByID(shared.RequestID(req.RequestID))
	if rec == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "request not found",
			shared.ErrorDetail{Field: "requestId", Message: fmt.Sprintf("no request with ID %s", req.RequestID)})
		return
	}

	// Build the fixture from the request record, resolving body references.
	f := fixture.FixtureData{
		Name:      req.Name,
		CreatedAt: time.Now().UTC(),
		Request: fixture.FixtureRequest{
			Method:          rec.Method,
			Scheme:          rec.Scheme,
			Host:            rec.Host,
			Path:            rec.Path,
			Query:           rec.Query,
			URL:             rec.URL,
			RequestHeaders:  rec.RequestHeaders,
			ResponseHeaders: rec.ResponseHeaders,
			Status:          rec.Status,
			ClientAddr:      rec.ClientAddr,
		},
	}

	// Resolve request body from the body cache.
	if rec.RequestBodyRef != "" {
		if bc := s.BodyCache.Get(rec.RequestBodyRef); bc != nil {
			f.Request.RequestBody = string(bc)
		}
	}

	// Resolve response body from the body cache.
	if rec.ResponseBodyRef != "" && f.Request.RequestBody == "" {
		if bc := s.BodyCache.Get(rec.ResponseBodyRef); bc != nil {
			f.Request.ResponseBody = string(bc)
		}
	} else if rec.ResponseBodyRef != "" {
		if bc := s.BodyCache.Get(rec.ResponseBodyRef); bc != nil {
			f.Request.ResponseBody = string(bc)
		}
	}

	if err := s.FixtureStore.Save(req.Name, f); err != nil {
		writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, fmt.Sprintf("failed to save fixture: %v", err))
		return
	}

	writeJSON(w, http.StatusCreated, f)
}

// handleFixtureByName dispatches to get or delete based on HTTP method.
//   - GET    /v1/fixtures/:name  returns fixture detail.
//   - DELETE /v1/fixtures/:name  deletes a fixture.
func (s *Server) handleFixtureByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v1/fixtures/")
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "fixture name is required")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleFixtureGet(w, r, name)
	case http.MethodDelete:
		s.handleFixtureDelete(w, r, name)
	default:
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
	}
}

// handleFixtureGet responds to GET /v1/fixtures/:name.
func (s *Server) handleFixtureGet(w http.ResponseWriter, r *http.Request, name string) {
	f, err := s.FixtureStore.Get(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, fmt.Sprintf("fixture %q not found", name),
				shared.ErrorDetail{Field: "name", Message: err.Error()})
		} else {
			writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, fmt.Sprintf("failed to get fixture: %v", err))
		}
		return
	}
	writeJSON(w, http.StatusOK, f)
}

// handleFixtureDelete responds to DELETE /v1/fixtures/:name.
func (s *Server) handleFixtureDelete(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.FixtureStore.Delete(name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, fmt.Sprintf("fixture %q not found", name),
				shared.ErrorDetail{Field: "name", Message: err.Error()})
		} else {
			writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, fmt.Sprintf("failed to delete fixture: %v", err))
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Rule handlers
// ---------------------------------------------------------------------------

// handleRules dispatches to list or create based on HTTP method.
//   - GET  /v1/rules  returns all rules.
//   - POST /v1/rules  creates a new rule.
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleRulesList(w, r)
	case http.MethodPost:
		s.handleRulesCreate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
	}
}

// handleRulesList responds to GET /v1/rules.
func (s *Server) handleRulesList(w http.ResponseWriter, r *http.Request) {
	rules := s.RuleStore.List()
	resp := shared.ListRulesResponse{Rules: rules}
	writeJSON(w, http.StatusOK, resp)
}

// handleRulesCreate responds to POST /v1/rules.
func (s *Server) handleRulesCreate(w http.ResponseWriter, r *http.Request) {
	var req shared.CreateRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body",
			shared.ErrorDetail{Field: "body", Message: "expected JSON with script field"})
		return
	}

	if req.Script == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "script is required",
			shared.ErrorDetail{Field: "script", Message: "must be a non-empty string"})
		return
	}

	rule, err := s.RuleStore.Create(req.Name, req.Priority, req.Script)
	if err != nil {
		// A script the store refuses is the caller's mistake — a rule
		// must be a YAML document — so it answers 400, not 500.
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error(),
			shared.ErrorDetail{Field: "script", Message: "must be a YAML rule v1 document"})
		return
	}
	if req.FailureMode != "" {
		if updated, err := s.RuleStore.SetFailureMode(rule.ID, req.FailureMode); err == nil {
			rule = updated
		} else {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error(),
				shared.ErrorDetail{Field: "failureMode", Message: "must be fail-open or fail-closed"})
			return
		}
	}
	if s.Sessions != nil {
		s.Sessions.Note("rule_created", "", "", string(rule.ID), rule.Name)
	}

	writeJSON(w, http.StatusCreated, rule)
}

// handleRulesByID dispatches based on path and method for /v1/rules/:id
// and /v1/rules/:id/toggle.
func (s *Server) handleRulesByID(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Check for toggle subpath.
	if strings.HasSuffix(path, "/toggle") {
		s.handleRulesToggle(w, r)
		return
	}

	// Extract rule ID from path: /v1/rules/:id
	idStr := strings.TrimPrefix(path, "/v1/rules/")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "rule ID is required")
		return
	}

	id := shared.RuleID(idStr)

	switch r.Method {
	case http.MethodGet:
		s.handleRulesGet(w, r, id)
	case http.MethodPut:
		s.handleRulesUpdate(w, r, id)
	case http.MethodDelete:
		s.handleRulesDelete(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
	}
}

// handleRulesGet responds to GET /v1/rules/:id.
func (s *Server) handleRulesGet(w http.ResponseWriter, r *http.Request, id shared.RuleID) {
	rule, err := s.RuleStore.Get(id)
	if err != nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, fmt.Sprintf("rule %q not found", id),
			shared.ErrorDetail{Field: "id", Message: err.Error()})
		return
	}
	resp := shared.GetRuleResponse{Rule: *rule}
	writeJSON(w, http.StatusOK, resp)
}

// handleRulesUpdate responds to PUT /v1/rules/:id.
func (s *Server) handleRulesUpdate(w http.ResponseWriter, r *http.Request, id shared.RuleID) {
	var req shared.UpdateRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body",
			shared.ErrorDetail{Field: "body", Message: "expected valid JSON"})
		return
	}

	updated, err := s.RuleStore.Update(id, req.Name, req.Priority, req.Script)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, fmt.Sprintf("rule %q not found", id),
				shared.ErrorDetail{Field: "id", Message: err.Error()})
		} else {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// handleRulesDelete responds to DELETE /v1/rules/:id.
func (s *Server) handleRulesDelete(w http.ResponseWriter, r *http.Request, id shared.RuleID) {
	if err := s.RuleStore.Delete(id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, fmt.Sprintf("rule %q not found", id),
				shared.ErrorDetail{Field: "id", Message: err.Error()})
		} else {
			writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, fmt.Sprintf("failed to delete rule: %v", err))
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRulesToggle responds to POST /v1/rules/:id/toggle.
func (s *Server) handleRulesToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}

	var req shared.ToggleRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body",
			shared.ErrorDetail{Field: "body", Message: "expected JSON with enabled field"})
		return
	}

	// Extract rule ID from path: /v1/rules/:id/toggle
	path := strings.TrimSuffix(r.URL.Path, "/toggle")
	idStr := strings.TrimPrefix(path, "/v1/rules/")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "rule ID is required")
		return
	}

	updated, err := s.RuleStore.SetEnabled(shared.RuleID(idStr), req.Enabled)
	if err != nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, fmt.Sprintf("rule %q not found", idStr),
			shared.ErrorDetail{Field: "id", Message: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, updated)
}

// ---------------------------------------------------------------------------
// Rule test handler
// ---------------------------------------------------------------------------

// handleRulesTest responds to POST /v1/rules/test.
func (s *Server) handleRulesTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}

	var req shared.TestRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body",
			shared.ErrorDetail{Field: "body", Message: "expected JSON with ruleScript and fixture reference"})
		return
	}

	if req.RuleScript == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "ruleScript is required",
			shared.ErrorDetail{Field: "ruleScript", Message: "must be a non-empty string"})
		return
	}

	// Resolve fixture into a shared.Request for the rule engine.
	reqData, body, err := s.resolveFixtureForTest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error())
		return
	}

	// Evaluate the rule.
	timeout := shared.DefaultRuleTimeout
	result := s.RuleEngine.Evaluate(req.RuleScript, reqData, body, timeout)

	// Build response.
	resp := shared.TestRuleResponse{
		Matched:     result.Matched,
		Actions:     result.Actions,
		DurationMs:  result.DurationMs,
		SchemaValid: result.SchemaValid,
		SchemaErrs:  result.SchemaErrs,
		Error:       result.Error,
		Trace: shared.RuleTrace{
			Matched:    result.Matched,
			Actions:    result.Actions,
			DurationMs: result.DurationMs,
			Error:      result.Error,
		},
	}

	writeJSON(w, http.StatusOK, resp)
}

// resolveFixtureForTest resolves a fixture reference from the request into a
// shared.Request plus its request body string.
func (s *Server) resolveFixtureForTest(req shared.TestRuleRequest) (*shared.Request, string, error) {
	var fData fixture.FixtureData
	var err error

	switch {
	case req.Fixture != "":
		fData, err = s.FixtureStore.Get(req.Fixture)
	case req.FixtureID != "":
		// Resolve from ring buffer.
		rec := s.RingBuffer.GetByID(shared.RequestID(req.FixtureID))
		if rec == nil {
			return nil, "", fmt.Errorf("request %q not found", req.FixtureID)
		}
		// Convert ring buffer record to fixture format for body resolution.
		body := ""
		if rec.RequestBodyRef != "" {
			if bc := s.BodyCache.Get(rec.RequestBodyRef); bc != nil {
				body = string(bc)
			}
		}
		return rec, body, nil
	default:
		return nil, "", fmt.Errorf("one of fixture, fixtureId, or fixtureFile is required")
	}
	if err != nil {
		return nil, "", fmt.Errorf("fixture %q not found: %v", req.Fixture, err)
	}

	// Convert fixture data to a shared.Request.
	rec := &shared.Request{
		Method:         fData.Request.Method,
		Scheme:         fData.Request.Scheme,
		Host:           fData.Request.Host,
		Path:           fData.Request.Path,
		Query:          fData.Request.Query,
		URL:            fData.Request.URL,
		ClientAddr:     fData.Request.ClientAddr,
		RequestHeaders: fData.Request.RequestHeaders,
	}
	if rec.RequestHeaders == nil {
		rec.RequestHeaders = make(shared.Headers)
	}

	return rec, fData.Request.RequestBody, nil
}

// parseIncludeParam parses the ?include= query parameter into a set of flags.
func parseIncludeParam(val string) map[string]bool {
	result := map[string]bool{
		"headers":    false,
		"body":       false,
		"rule-trace": false,
	}
	if val == "" {
		return result
	}
	for _, part := range strings.Split(val, ",") {
		part = strings.TrimSpace(part)
		switch part {
		case "headers", "body", "rule-trace":
			result[part] = true
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// MITM handlers (Milestone 3)
// ---------------------------------------------------------------------------

// handleMITMEnable responds to POST /v1/mitm/enable.
func (s *Server) handleMITMEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.MITMHandler == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "MITM not configured")
		return
	}
	s.MITMHandler.SetEnabled(true)
	s.publishMITM()
	s.EventHub.Emit(&shared.Event{
		Type:      shared.EventMITMEnabled,
		Timestamp: time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": true})
}

// handleMITMDisable responds to POST /v1/mitm/disable.
func (s *Server) handleMITMDisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.MITMHandler == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "MITM not configured")
		return
	}
	s.MITMHandler.SetEnabled(false)
	s.publishMITM()
	s.EventHub.Emit(&shared.Event{
		Type:      shared.EventMITMDisabled,
		Timestamp: time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": false})
}

// handleMITMStatus responds to GET /v1/mitm/status.
func (s *Server) handleMITMStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.MITMHandler == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "MITM not configured")
		return
	}
	resp := shared.MITMStatusResponse{
		Enabled:          s.MITMHandler.Enabled(),
		CertInstalled:    s.MITMHandler.CertInstalled(),
		AllowPatterns:    s.MITMHandler.Matcher.AllowPatterns(),
		DenyPatterns:     s.MITMHandler.Matcher.DenyPatterns(),
		EmptyAllowDenies: true,
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMITMCert responds to GET /v1/mitm/cert.
func (s *Server) handleMITMCert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.MITMHandler == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "MITM not configured")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", "attachment; filename=rootCA.pem")
	w.Write(s.MITMHandler.GetCertPEM())
}

// handleMITMAllow responds to POST/GET/DELETE /v1/mitm/allow.
func (s *Server) handleMITMAllow(w http.ResponseWriter, r *http.Request) {
	if s.MITMHandler == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "MITM not configured")
		return
	}

	switch r.Method {
	case http.MethodGet:
		patterns := s.MITMHandler.Matcher.AllowPatterns()
		if patterns == nil {
			patterns = []string{}
		}
		writeJSON(w, http.StatusOK, map[string][]string{"patterns": patterns})

	case http.MethodPost:
		var req shared.MITMDomainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body",
				shared.ErrorDetail{Field: "body", Message: "expected JSON with pattern field"})
			return
		}
		if req.Pattern == "" {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "pattern is required",
				shared.ErrorDetail{Field: "pattern", Message: "must be a non-empty string"})
			return
		}
		s.MITMHandler.Matcher.AddAllow(req.Pattern)
		s.publishMITM()
		writeJSON(w, http.StatusOK, map[string]string{"pattern": req.Pattern, "action": "added"})

	case http.MethodDelete:
		pattern := r.URL.Query().Get("pattern")
		if pattern == "" {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "pattern query parameter is required")
			return
		}
		s.MITMHandler.Matcher.RemoveAllow(pattern)
		s.publishMITM()
		writeJSON(w, http.StatusOK, map[string]string{"pattern": pattern, "action": "removed"})

	default:
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
	}
}

// handleMITMCertInstalled responds to POST /v1/mitm/cert-installed.
// Body: { "installed": bool }
// Notifies the daemon that the CA certificate was installed or uninstalled
// in the system trust store.
func (s *Server) handleMITMCertInstalled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.MITMHandler == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "MITM not configured")
		return
	}

	var req struct {
		Installed bool `json:"installed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body",
			shared.ErrorDetail{Field: "body", Message: "expected JSON with installed field"})
		return
	}

	s.MITMHandler.SetCertInstalled(req.Installed)
	writeJSON(w, http.StatusOK, map[string]bool{"certInstalled": req.Installed})
}

// handleMITMDeny responds to POST/GET/DELETE /v1/mitm/deny.
func (s *Server) handleMITMDeny(w http.ResponseWriter, r *http.Request) {
	if s.MITMHandler == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "MITM not configured")
		return
	}

	switch r.Method {
	case http.MethodGet:
		patterns := s.MITMHandler.Matcher.DenyPatterns()
		if patterns == nil {
			patterns = []string{}
		}
		writeJSON(w, http.StatusOK, map[string][]string{"patterns": patterns})

	case http.MethodPost:
		var req shared.MITMDomainRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body",
				shared.ErrorDetail{Field: "body", Message: "expected JSON with pattern field"})
			return
		}
		if req.Pattern == "" {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "pattern is required",
				shared.ErrorDetail{Field: "pattern", Message: "must be a non-empty string"})
			return
		}
		s.MITMHandler.Matcher.AddDeny(req.Pattern)
		s.publishMITM()
		writeJSON(w, http.StatusOK, map[string]string{"pattern": req.Pattern, "action": "added"})

	case http.MethodDelete:
		pattern := r.URL.Query().Get("pattern")
		if pattern == "" {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "pattern query parameter is required")
			return
		}
		s.MITMHandler.Matcher.RemoveDeny(pattern)
		s.publishMITM()
		writeJSON(w, http.StatusOK, map[string]string{"pattern": pattern, "action": "removed"})

	default:
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
	}
}
