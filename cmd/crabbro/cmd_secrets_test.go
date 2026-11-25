package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type secretsRoundTripper func(*http.Request) (*http.Response, error)

func (f secretsRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestSecretsSetArgumentOrder(t *testing.T) {
	t.Setenv("CRABBRO_TEST_SECRET_VALUE", "  test-value  ")
	tests := []struct {
		name string
		args []string
	}{
		{"name before flag", []string{"set", "my-key", "--from-env", "CRABBRO_TEST_SECRET_VALUE"}},
		{"name before flag with equals", []string{"set", "my-key", "--from-env=CRABBRO_TEST_SECRET_VALUE"}},
		{"flag before name", []string{"set", "--from-env", "CRABBRO_TEST_SECRET_VALUE", "my-key"}},
		{"stdin", []string{"set", "my-key"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, err := os.CreateTemp(t.TempDir(), "stdin")
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			stdinValue := "incorrect-stdin-value"
			if tt.name == "stdin" {
				stdinValue = "  test-value  "
			}
			if _, err := input.WriteString(stdinValue); err != nil {
				t.Fatal(err)
			}
			if _, err := input.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			originalStdin := os.Stdin
			os.Stdin = input
			defer func() { os.Stdin = originalStdin }()

			requests := 0
			originalTransport := http.DefaultTransport
			http.DefaultTransport = secretsRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/secrets" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if body["name"] != "my-key" || body["value"] != "test-value" {
					t.Errorf("unexpected request body: %v", body)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
			})
			defer func() { http.DefaultTransport = originalTransport }()
			if err := cmdSecrets(&clientConfig{TCP: "127.0.0.1:1", Token: "test"}, tt.args); err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Errorf("got %d requests, want 1", requests)
			}
		})
	}
}

func TestSecretsSetRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"set", "my-key", "--unknown"}, "flag provided but not defined"},
		{[]string{"set", "--unknown", "my-key"}, "flag provided but not defined"},
		{[]string{"set"}, "usage:"},
		{[]string{"set", "--from-env", "CRABBRO_TEST_SECRET_VALUE"}, "usage:"},
		{[]string{"set", "my-key", "extra"}, "usage:"},
		{[]string{"set", "--from-env", "CRABBRO_TEST_SECRET_VALUE", "my-key", "extra"}, "usage:"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			if err := cmdSecrets(&clientConfig{}, tt.args); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got error %v, want %q", err, tt.want)
			}
		})
	}
}
