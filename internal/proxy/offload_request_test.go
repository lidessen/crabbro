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
