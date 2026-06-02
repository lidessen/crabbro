package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lidessen/chiling/internal/shared"
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

	resp := shared.StatusResponse{
		Version:       s.Version,
		UptimeSeconds: uptime,
		ProxyAddr:     s.ProxyAddr,
		RequestCount:  requestCount,
		RuleCount:     ruleCount,
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

	sub := s.EventHub.Subscribe()
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
			data, err := json.Marshal(event)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
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

	cursor := r.URL.Query().Get("since")
	limitStr := r.URL.Query().Get("limit")

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

	var requests []*shared.Request
	if cursor != "" {
		requests = s.RingBuffer.ListSince(cursor, limit)
	} else {
		requests = s.RingBuffer.List(limit)
	}

	// Convert to summaries.
	summaries := make([]shared.RequestSummary, 0, len(requests))
	for _, req := range requests {
		status := req.Status
		durationMs := req.DurationMs

		summaries = append(summaries, shared.RequestSummary{
			ID:         req.ID,
			StartedAt:  req.StartedAt.Format(time.RFC3339Nano),
			Method:     req.Method,
			URL:        req.URL,
			Status:     status,
			DurationMs: durationMs,
			Tags:       req.Tags,
		})
	}

	// Calculate next cursor.
	var nextCursor string
	if limit > 0 && len(requests) == limit {
		lastReq := requests[len(requests)-1]
		nextCursor = string(lastReq.ID)
	}

	resp := shared.ListRequestsResponse{
		Requests:   summaries,
		NextCursor: nextCursor,
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

	req := s.RingBuffer.GetByID(shared.RequestID(idStr))
	if req == nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "request not found",
			shared.ErrorDetail{Field: "id", Message: fmt.Sprintf("no request with ID %s", idStr)})
		return
	}

	include := parseIncludeParam(r.URL.Query().Get("include"))

	resp := shared.RequestDetailResponse{
		Request: *req,
	}

	// Include body if requested.
	if include["body"] && req.RequestBodyRef != "" {
		if bc := s.BodyCache.Get(req.RequestBodyRef); bc != nil {
			bodyStr := string(bc)
			resp.Body = &bodyStr
		}
	}
	if include["body"] && req.ResponseBodyRef != "" && resp.Body == nil {
		if bc := s.BodyCache.Get(req.ResponseBodyRef); bc != nil {
			bodyStr := string(bc)
			resp.Body = &bodyStr
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

// parseIncludeParam parses the ?include= query parameter into a set of flags.
func parseIncludeParam(val string) map[string]bool {
	result := map[string]bool{
		"headers":   false,
		"body":      false,
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

