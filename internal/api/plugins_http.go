package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/lidessen/chiling/internal/rule"
	"github.com/lidessen/chiling/internal/shared"
)

func (s *Server) handleRulesPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	var body struct {
		Method string `json:"method"`
		Host   string `json:"host"`
		Path   string `json:"path"`
	}
	if r.Method == http.MethodPost {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if s.RulesDir == nil {
		writeJSON(w, http.StatusOK, rule.Preview{Complete: true, Matched: []string{}, Disabled: []string{}})
		return
	}
	writeJSON(w, http.StatusOK, s.RulesDir.Preview(body.Method, body.Host, body.Path))
}

func (s *Server) handleRulesDiagnose(w http.ResponseWriter, r *http.Request) {
	if s.RulesDir == nil {
		writeJSON(w, http.StatusOK, map[string]any{"complete": true, "diagnostics": []any{}})
		return
	}
	files, complete, err := s.RulesDir.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, shared.ErrCodeInternal, "rules directory unreadable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"complete": complete, "files": files})
}

func (s *Server) handleRulesReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
		return
	}
	if s.RulesDir == nil || s.RuleStore == nil {
		writeJSON(w, http.StatusOK, map[string]any{"complete": true})
		return
	}
	diags, complete := s.RulesDir.Sync(s.RuleStore)
	writeJSON(w, http.StatusOK, map[string]any{"complete": complete, "diagnostics": diags})
}

func (s *Server) handleConfigStatus(w http.ResponseWriter, r *http.Request) {
	s.handleRulesDiagnose(w, r)
}

func (s *Server) handleConfigDraft(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if s.handleScopeDraft(w, r, id) {
		return
	}
	if s.RulesDir == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "no rules directory")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, ruleMaxBody))
	if err != nil || id == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "id and body are required")
		return
	}
	if err := s.RulesDir.Draft(id, body); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "draft rejected")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "draft": true})
}

func (s *Server) handleConfigValidate(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if r.URL.Query().Get("kind") == "scope" && s.Inspect != nil && s.Inspect.Durable != nil {
		diags := s.Inspect.Durable.ValidateFile(id)
		writeJSON(w, http.StatusOK, map[string]any{"ok": len(diags) == 0, "diagnostics": diags})
		return
	}
	if s.RulesDir == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "no rules directory")
		return
	}
	offline := r.URL.Query().Get("offline") != "false"
	diags := s.RulesDir.ValidateFile(id, offline, nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(diags) == 0, "diagnostics": diags})
}

func (s *Server) handleConfigCommit(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if r.URL.Query().Get("kind") == "scope" && s.Inspect != nil && s.Inspect.Durable != nil {
		if err := s.Inspect.Durable.Commit(id); err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "commit rejected")
			return
		}
		if s.Sessions != nil {
			s.Sessions.Note("scope_committed", "", "", "", "committed "+id)
		}
		if s.Inspect != nil {
			sid := ""
			if s.Sessions != nil {
				if cur := s.Sessions.Current(); cur != nil {
					sid = cur.ID
				}
			}
			s.emitScope(s.Inspect.Status(sid, false))
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "committed": true})
		return
	}
	if s.RulesDir == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "no rules directory")
		return
	}
	if err := s.RulesDir.Commit(id); err != nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "commit rejected")
		return
	}
	if s.RuleStore != nil {
		_, _ = s.RulesDir.Sync(s.RuleStore)
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "committed": true})
}

func (s *Server) handleConfigDiscard(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if r.URL.Query().Get("kind") == "scope" && s.Inspect != nil && s.Inspect.Durable != nil {
		if err := s.Inspect.Durable.Discard(id); err != nil {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "no draft")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "discarded": true})
		return
	}
	if s.RulesDir == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "no rules directory")
		return
	}
	if err := s.RulesDir.Discard(id); err != nil {
		writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "no draft")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "discarded": true})
}

const ruleMaxBody = 256 * 1024

func (s *Server) handlePluginHost(w http.ResponseWriter, r *http.Request) {
	if s.Plugins == nil {
		writeJSON(w, http.StatusOK, map[string]any{"running": false, "workers": 0})
		return
	}
	writeJSON(w, http.StatusOK, s.Plugins.Status())
}

func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	if s.Plugins == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "plugins are not configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"plugins": s.Plugins.List()})
	case http.MethodPost:
		var body struct {
			Path   string `json:"path"`
			Enable bool   `json:"enable"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "path is required")
			return
		}
		rec, err := s.Plugins.Install(body.Path, body.Enable)
		if err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "install failed")
			return
		}
		writeJSON(w, http.StatusOK, rec)
	default:
		writeError(w, http.StatusMethodNotAllowed, shared.ErrCodeBadRequest, "method not allowed")
	}
}

func (s *Server) handlePluginByID(w http.ResponseWriter, r *http.Request) {
	if s.Plugins == nil {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "plugins are not configured")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/plugins/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if id == "" {
		writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "plugin id is required")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		desc, err := s.Plugins.Describe(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "inspect failed")
			return
		}
		writeJSON(w, http.StatusOK, desc)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := s.Plugins.Uninstall(id); err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "uninstall failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": id})
		return
	}
	if len(parts) >= 2 && parts[1] == "tools" && r.Method == http.MethodGet {
		desc, err := s.Plugins.Describe(id)
		if err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "tools failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tools": desc["tools"]})
		return
	}
	if len(parts) >= 2 && parts[1] == "instances" {
		s.handlePluginInstances(w, r, id, parts[2:])
		return
	}
	if len(parts) >= 4 && parts[1] == "instances" {
		return
	}
	writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "unknown plugin route")
}

func (s *Server) handlePluginInstances(w http.ResponseWriter, r *http.Request, id string, rest []string) {
	if len(rest) == 0 && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"instances": s.Plugins.Instances(id)})
		return
	}
	if len(rest) == 0 && r.Method == http.MethodPost {
		var body struct {
			Name        string            `json:"name"`
			Config      map[string]any    `json:"config"`
			Credentials map[string]string `json:"credentials"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "instance name is required")
			return
		}
		if err := s.Plugins.AddInstance(id, body.Name, body.Config, body.Credentials); err != nil {
			writeError(w, http.StatusBadRequest, shared.ErrCodeBadRequest, "instance rejected")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"plugin": id, "instance": body.Name})
		return
	}
	if len(rest) >= 3 && rest[1] == "tools" && r.Method == http.MethodPost {
		var body struct {
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		res, err := s.Plugins.Invoke(r.Context(), rule.InvokeRequest{
			Plugin: id, Instance: rest[0], Capability: rest[2], Kind: "tool", With: body.Arguments,
		})
		if err != nil {
			writeError(w, http.StatusBadGateway, shared.ErrCodeInternal, "invoke failed")
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	if len(rest) == 2 && (rest[1] == "enable" || rest[1] == "disable") && r.Method == http.MethodPost {
		if err := s.Plugins.SetInstanceEnabled(id, rest[0], rest[1] == "enable"); err != nil {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "instance not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"instance": rest[0], "enabled": rest[1] == "enable"})
		return
	}
	if len(rest) == 1 && r.Method == http.MethodDelete {
		if err := s.Plugins.RemoveInstance(id, rest[0]); err != nil {
			writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "instance not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": rest[0]})
		return
	}
	writeError(w, http.StatusNotFound, shared.ErrCodeNotFound, "unknown instance route")
}
