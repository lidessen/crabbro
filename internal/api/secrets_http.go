package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
)

func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if s.Secrets == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "secret store is not configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.Secrets.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, err.Error())
			return
		}
		out := make([]shared.SecretSummary, 0, len(items))
		for _, item := range items {
			out = append(out, shared.SecretSummary{
				Name:      item.Name,
				UpdatedAt: item.UpdatedAt,
				SizeBytes: item.SizeBytes,
			})
		}
		writeJSON(w, http.StatusOK, shared.SecretListResponse{Secrets: out})
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "invalid JSON body")
			return
		}
		if err := s.Secrets.Set(body.Name, body.Value); err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"name": body.Name, "saved": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
	}
}

func (s *Server) handleSecretByName(w http.ResponseWriter, r *http.Request) {
	if s.Secrets == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "secret store is not configured")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/secrets/")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "secret name is required")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if err := s.Secrets.Delete(name); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "secret not found")
			return
		}
		if errors.Is(err, secret.ErrInvalidRef) {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "deleted": true})
}
