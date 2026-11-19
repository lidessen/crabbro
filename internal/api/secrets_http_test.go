package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/secret"
	"github.com/lidessen/crabbro/internal/shared"
)

func TestSecretsHTTP(t *testing.T) {
	s := testMITMServer(t)
	s.Secrets = secret.New(t.TempDir())

	// Create secret
	body, _ := json.Marshal(map[string]string{"name": "glm_key", "value": "sk-secret-123456"})
	req := httptest.NewRequest(http.MethodPost, "/v1/secrets", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST /v1/secrets = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-secret-123456") {
		t.Fatal("POST /v1/secrets echoed secret value")
	}

	// List secrets (never leaks value)
	req = httptest.NewRequest(http.MethodGet, "/v1/secrets", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/secrets = %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-secret-123456") {
		t.Fatal("GET /v1/secrets leaked secret value")
	}
	var list shared.SecretListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Secrets) != 1 || list.Secrets[0].Name != "glm_key" {
		t.Fatalf("list = %+v", list)
	}

	// Delete secret
	req = httptest.NewRequest(http.MethodDelete, "/v1/secrets/glm_key", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE /v1/secrets/glm_key = %d: %s", w.Code, w.Body.String())
	}
	if s.Secrets.Has("glm_key") {
		t.Fatal("secret still exists after delete")
	}
}
