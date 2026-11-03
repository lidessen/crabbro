package decision

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFromEnvUnconfigured(t *testing.T) {
	t.Setenv("CHILING_DECISION_API_KEY", "")
	t.Setenv("CHILING_DECISION_PROVIDER", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	cfg := FromEnv()
	if cfg.Configured() {
		t.Fatal("expected unconfigured client without a key")
	}
	c := New(cfg)
	_, err := c.Find(context.Background(), "login", []Flow{{ID: "req_1", Method: "POST", URL: "https://example.test/login"}})
	if err != ErrNotConfigured {
		t.Fatalf("Find err = %v, want ErrNotConfigured", err)
	}
}

func TestFindJev(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("missing bearer token")
		}
		var req jevRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "jev-latest" {
			t.Errorf("model = %s", req.Model)
		}
		if _, ok := req.Questions["m_req_1"]; !ok {
			t.Fatalf("questions = %#v", req.Questions)
		}
		noul := 0.91
		low := 0.1
		json.NewEncoder(w).Encode(jevResponse{
			Model: "jev-1.13.0",
			Answers: map[string]jevAnswer{
				"m_req_1": {Type: "noul", Noul: &noul},
				"m_req_2": {Type: "noul", Noul: &low},
			},
		})
	}))
	defer srv.Close()

	c := New(Config{Provider: "jev", BaseURL: srv.URL, APIKey: "test-key", Model: "jev-latest"})
	got, err := c.Find(context.Background(), "the login request that returns the user token", []Flow{
		{ID: "req_1", Method: "POST", URL: "https://api.example.test/login", ResponseBody: `{"token":"abc"}`},
		{ID: "req_2", Method: "GET", URL: "https://api.example.test/health"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 || got.Hits[0].RequestID != "req_1" {
		t.Fatalf("hits = %#v", got.Hits)
	}
	if got.Hits[0].Score < 0.9 || got.Hits[0].Reason == "" {
		t.Fatalf("hit = %#v", got.Hits[0])
	}
	if got.Model != "jev-1.13.0" || got.Provider != "jev" {
		t.Fatalf("result model/provider = %s %s", got.Model, got.Provider)
	}
}

func TestFindJevRejectsBadScore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bad := 1.4
		json.NewEncoder(w).Encode(jevResponse{
			Model:   "jev-latest",
			Answers: map[string]jevAnswer{"m_req_1": {Type: "noul", Noul: &bad}},
		})
	}))
	defer srv.Close()
	c := New(Config{Provider: "jev", BaseURL: srv.URL, APIKey: "k", Model: "jev-latest"})
	_, err := c.Find(context.Background(), "login", []Flow{{ID: "req_1", URL: "https://example.test/login"}})
	if err == nil {
		t.Fatal("expected validation error")
	}
}

func TestFindOpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "json_schema") {
			t.Fatalf("expected json_schema response format, body %s", body)
		}
		content, _ := json.Marshal(map[string]any{
			"hits": []map[string]any{{
				"requestId": "req_9",
				"score":     0.8,
				"reason":    "response body contains a user token",
			}},
		})
		json.NewEncoder(w).Encode(map[string]any{
			"model": "gpt-test",
			"choices": []map[string]any{{
				"message": map[string]any{"content": string(content)},
			}},
		})
	}))
	defer srv.Close()
	c := New(Config{Provider: "openai", BaseURL: srv.URL, APIKey: "test", Model: "gpt-4o-mini"})
	got, err := c.Find(context.Background(), "login token", []Flow{
		{ID: "req_9", Method: "POST", URL: "https://api.example.test/login"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hits) != 1 || got.Hits[0].Reason == "" {
		t.Fatalf("hits = %#v", got.Hits)
	}
}

func TestFindOpenAIRejectsUnknownID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content := `{"hits":[{"requestId":"nope","score":0.9,"reason":"no"}]}`
		json.NewEncoder(w).Encode(map[string]any{
			"model": "gpt-test",
			"choices": []map[string]any{{
				"message": map[string]any{"content": content},
			}},
		})
	}))
	defer srv.Close()
	c := New(Config{Provider: "openai", BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := c.Find(context.Background(), "q", []Flow{{ID: "req_1", URL: "https://example.test/"}})
	if err == nil || !strings.Contains(err.Error(), "unknown request id") {
		t.Fatalf("err = %v", err)
	}
}

func TestDraftJevScaffoldsScript(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conf := 0.8
		json.NewEncoder(w).Encode(jevResponse{
			Model: "jev-latest",
			Answers: map[string]jevAnswer{
				"action": {Type: "choice", Choice: "mockResponse", Confidence: &conf},
			},
		})
	}))
	defer srv.Close()
	c := New(Config{Provider: "jev", BaseURL: srv.URL, APIKey: "k", Model: "jev-latest"})
	got, err := c.Draft(context.Background(), "mock the login response", &Flow{
		ID: "req_1", Method: "POST", URL: "https://api.example.test/v1/login",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Action != "mockResponse" || !strings.Contains(got.Script, "mockResponse") {
		t.Fatalf("draft = %#v", got)
	}
	if !strings.Contains(got.Script, "/v1/login") {
		t.Fatalf("script = %s", got.Script)
	}
}
