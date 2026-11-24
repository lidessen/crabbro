package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestBuildOffloadRequestIsolation(t *testing.T) {
	for _, tc := range []struct{ adapter, path, body, field string }{
		{"responses", "/v1/responses", `{"model":"subscription-model","input":"hello","stream":true,"user":"private-account","metadata":{"cookie":"private-cookie"}}`, "input"},
		{"anthropic-messages", "/v1/messages", `{"model":"subscription-model","messages":[],"max_tokens":10,"metadata":{"user_id":"private-account"}}`, "messages"},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			src, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://subscription.example/path?token=private-query", strings.NewReader(tc.body))
			src.Header.Set("Authorization", "private-auth")
			src.Header.Set("Cookie", "private-cookie")
			src.Header.Set("X-Account-ID", "private-account")
			req, err := buildOffloadRequest(src, tc.adapter, "plan/model", 4096)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(req.Body)
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(body, &obj); err != nil {
				t.Fatal(err)
			}
			if req.URL.Path != tc.path || req.URL.RawQuery != "" || req.Header.Get("Content-Type") != "application/json" || len(req.Header) != 1 || string(obj["model"]) != `"plan/model"` || obj[tc.field] == nil {
				t.Fatalf("unexpected clean request: %s %v %s", req.URL, req.Header, body)
			}
			for _, secret := range []string{"private-auth", "private-cookie", "private-account", "private-query", "subscription-model"} {
				if strings.Contains(req.URL.String()+string(body), secret) {
					t.Fatal("subscription value leaked")
				}
			}
			cancel()
			if req.Context().Err() != context.Canceled {
				t.Fatal("context cancellation was not preserved")
			}
		})
	}
}

func TestBuildOffloadRequestRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		adapter, body, model, encoding string
		limit                          int64
	}{
		{"unknown", `{}`, "route", "", 10},
		{"responses", `{}`, "", "", 10},
		{"responses", `[]`, "route", "", 10},
		{"responses", `{"input":"too long"}`, "route", "", 10},
		{"responses", `{}`, "route", "gzip", 10},
	} {
		src, _ := http.NewRequest(http.MethodPost, "http://subscription.example/", strings.NewReader(tc.body))
		src.Header.Set("Content-Encoding", tc.encoding)
		if _, err := buildOffloadRequest(src, tc.adapter, tc.model, tc.limit); err == nil {
			t.Fatalf("accepted invalid input: %+v", tc)
		}
	}
}

func TestOffloadNestedIsolation(t *testing.T) {
	src, _ := http.NewRequest(http.MethodPost, "https://subscription.example/v1/responses?token=synthetic-query", strings.NewReader(`{"input":[{"role":"user","content":"synthetic-auth synthetic-nested synthetic-query","metadata":{"access_token":"synthetic-nested"},"account_id":"synthetic-account"}],"tools":[{"type":"function","name":"example","parameters":{"properties":{"credential":{"value":"synthetic-credential"}}}}],"max_output_tokens":10}`))
	src.Header.Set("Authorization", "Bearer synthetic-auth")
	req, err := buildOffloadRequest(src, "responses", "route/model", 4096)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"synthetic-auth", "synthetic-nested", "synthetic-query", "synthetic-account", "synthetic-credential", "metadata", "account_id", "credential"} {
		if strings.Contains(string(body), value) {
			t.Fatalf("private value or field retained: %q", value)
		}
	}
	if !strings.Contains(string(body), `"max_output_tokens":10`) {
		t.Fatalf("protocol field missing: %s", body)
	}
}

func TestOffloadRedactsCredentialComponents(t *testing.T) {
	for _, tc := range []struct {
		adapter, body, field string
	}{
		{"responses", `{"input":"czNjcjN0 s3cr3t other=value query-secret"}`, "input"},
		{"anthropic-messages", `{"messages":[{"role":"user","content":"czNjcjN0 s3cr3t other=value query-secret"}]}`, "messages"},
	} {
		t.Run(tc.adapter, func(t *testing.T) {
			src, err := http.NewRequest(http.MethodPost, "https://subscription.example/?key=query-secret", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			src.Header.Set("Authorization", "Basic czNjcjN0")
			src.Header.Add("Cookie", "session=s3cr3t; second=other=value; empty=")
			req, err := buildOffloadRequest(src, tc.adapter, "route/model", 4096)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(body, &obj); err != nil {
				t.Fatal(err)
			}
			field := string(obj[tc.field])
			for _, secret := range []string{"czNjcjN0", "s3cr3t", "other=value", "query-secret"} {
				if strings.Contains(field, secret) {
					t.Errorf("credential component %q was retained in %s", secret, field)
				}
			}
			if strings.Count(field, "[REDACTED]") != 4 {
				t.Fatalf("unexpected sanitized field: %s", field)
			}
		})
	}
}

func TestOffloadPreservesAnthropicThinkingBudget(t *testing.T) {
	src, err := http.NewRequest(http.MethodPost, "https://subscription.example/", strings.NewReader(`{"messages":[],"thinking":{"type":"enabled","budget_tokens":4096}}`))
	if err != nil {
		t.Fatal(err)
	}
	req, err := buildOffloadRequest(src, "anthropic-messages", "route/model", 4096)
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.NewDecoder(req.Body).Decode(&obj); err != nil {
		t.Fatal(err)
	}
	if obj.Thinking.Type != "enabled" || obj.Thinking.BudgetTokens != 4096 {
		t.Fatalf("thinking was not preserved: %+v", obj.Thinking)
	}
}
