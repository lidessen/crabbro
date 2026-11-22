package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/quota"
	"github.com/lidessen/crabbro/internal/shared"
)

func (s *Server) handleGatewayStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Gateway == nil {
		writeJSON(w, http.StatusOK, shared.GatewayStatusResponse{Enabled: false})
		return
	}
	writeJSON(w, http.StatusOK, s.Gateway.Status())
}

func (s *Server) handleGatewayPlans(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Gateway == nil {
		writeJSON(w, http.StatusOK, shared.GatewayPlanListResponse{Plans: []shared.GatewayPlanStatus{}})
		return
	}
	writeJSON(w, http.StatusOK, s.Gateway.Plans())
}

func (s *Server) handleGatewayPlanAction(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/gateway/plans/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "reset-cooldown" {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "unknown gateway plan action")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Gateway == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "gateway is not enabled")
		return
	}
	if err := s.Gateway.ResetCooldown(parts[0]); err != nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan": parts[0], "reset": true})
}

func (s *Server) handleGatewayRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Gateway == nil {
		writeJSON(w, http.StatusOK, shared.GatewayRouteListResponse{Routes: []shared.GatewayRouteStatus{}})
		return
	}
	writeJSON(w, http.StatusOK, s.Gateway.Routes())
}

func (s *Server) handleGatewayUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	window := r.URL.Query().Get("window")
	if window == "" {
		window = "24h"
	}
	plan := r.URL.Query().Get("plan")
	if s.Gateway == nil {
		writeJSON(w, http.StatusOK, shared.GatewayUsageResponse{
			Window:  window,
			Buckets: []shared.GatewayUsageBucket{},
		})
		return
	}
	resp, err := s.Gateway.Usage(window, plan)
	if err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGatewayPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Gateway == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "gateway is not enabled")
		return
	}
	var req shared.GatewayPreviewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body")
		return
	}
	writeJSON(w, http.StatusOK, s.Gateway.Preview(r.Context(), req))
}

func (s *Server) handleGatewayReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Gateway == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "gateway is not enabled")
		return
	}
	diags, complete := s.Gateway.Reload()
	writeJSON(w, http.StatusOK, map[string]any{"complete": complete, "diagnostics": diags})
}

func (s *Server) handleGatewayQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Gateway == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "gateway is not enabled")
		return
	}
	s.Gateway.Quotas.Refresh(r.Context(), s.Gateway.Config.Snapshot().AllPlans)
	writeJSON(w, http.StatusOK, s.Gateway.Plans())
}

// handleGatewayQuotaReset redeems at most one reset coupon for one plan via
// POST /v1/gateway/quotas/reset. The response never contains provider coupon
// identifiers, only the selected window scope and its sanitized expiry.
func (s *Server) handleGatewayQuotaReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.Gateway == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "gateway is not enabled")
		return
	}
	var req shared.GatewayQuotaResetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Plan) == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body: plan is required")
		return
	}
	if err := quota.NormalizeResetScope(req.Scope); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error())
		return
	}
	out := s.Gateway.ResetQuota(r.Context(), req)
	if out.Error != "" && isResetPlanError(out.Error) {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, out.Error)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// isResetPlanError reports whether a redemption error is a missing-plan error.
func isResetPlanError(msg string) bool {
	return strings.Contains(msg, "not found")
}
