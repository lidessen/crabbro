// Package decision asks a configured model to judge captured flows.
//
// Two providers share one Go result type:
//
//   - jev: TypeSafe's Jev model, POST {base}/v1/systemone, questions of type
//     noul or choice. This is the "typesafe jev" decision API.
//   - openai: OpenAI Chat Completions, or any compatible third-party base URL,
//     with a strict JSON schema. The response is unmarshaled into the same
//     structs and rejected when it does not match.
//
// The proxy works with no key configured. Find and Draft return
// ErrNotConfigured in that case.
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ErrNotConfigured is returned when no decision API key is configured.
var ErrNotConfigured = errors.New("decision API is not configured")

// Config selects a provider. Secrets come from the environment, not argv.
type Config struct {
	Provider string
	BaseURL  string
	APIKey   string
	Model    string
}

// Configured reports whether a request can be sent.
func (c Config) Configured() bool {
	return strings.TrimSpace(c.APIKey) != "" && (c.Provider == "jev" || c.Provider == "openai")
}

// FromEnv loads CHILING_DECISION_* with OPENAI_* and TYPESAFE_API_KEY fallbacks.
// Provider defaults to jev when only a TypeSafe key is present, otherwise openai.
func FromEnv() Config {
	cfg := Config{
		Provider: strings.ToLower(strings.TrimSpace(os.Getenv("CHILING_DECISION_PROVIDER"))),
		BaseURL:  strings.TrimSpace(os.Getenv("CHILING_DECISION_BASE_URL")),
		APIKey:   strings.TrimSpace(os.Getenv("CHILING_DECISION_API_KEY")),
		Model:    strings.TrimSpace(os.Getenv("CHILING_DECISION_MODEL")),
	}
	if cfg.Provider == "" {
		switch {
		case os.Getenv("TYPESAFE_API_KEY") != "" && os.Getenv("OPENAI_API_KEY") == "":
			cfg.Provider = "jev"
		default:
			cfg.Provider = "openai"
		}
	}
	if cfg.APIKey == "" {
		if cfg.Provider == "jev" {
			cfg.APIKey = strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY"))
		}
		if cfg.APIKey == "" {
			cfg.APIKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
		}
		if cfg.APIKey == "" {
			cfg.APIKey = strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY"))
		}
	}
	if cfg.BaseURL == "" {
		if cfg.Provider == "jev" {
			if v := strings.TrimSpace(os.Getenv("TYPESAFE_BASE_URL")); v != "" {
				cfg.BaseURL = v
			} else {
				cfg.BaseURL = "https://api.typesafe.ai"
			}
		} else if v := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); v != "" {
			cfg.BaseURL = v
		} else {
			cfg.BaseURL = "https://api.openai.com/v1"
		}
	}
	if cfg.Model == "" {
		if cfg.Provider == "jev" {
			cfg.Model = "jev-latest"
		} else {
			cfg.Model = "gpt-4o-mini"
		}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return cfg
}

// Flow is one captured exchange shown to the model. Bodies are previews.
type Flow struct {
	ID           string `json:"id"`
	Method       string `json:"method"`
	URL          string `json:"url"`
	Status       int    `json:"status,omitempty"`
	RequestBody  string `json:"requestBody,omitempty"`
	ResponseBody string `json:"responseBody,omitempty"`
}

// Hit is one flow the model judged relevant.
type Hit struct {
	RequestID string  `json:"requestId"`
	Score     float64 `json:"score"`
	Reason    string  `json:"reason"`
}

// FindResult is the validated search result.
type FindResult struct {
	Hits     []Hit  `json:"hits"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

// DraftResult is a rule scaffold the model selected or wrote.
type DraftResult struct {
	Name        string `json:"name"`
	Script      string `json:"script"`
	Explanation string `json:"explanation"`
	Action      string `json:"action"`
	Model       string `json:"model"`
	Provider    string `json:"provider"`
}

// Client calls the configured provider.
type Client struct {
	cfg  Config
	HTTP *http.Client
}

// New returns a client. It does not require the config to be complete.
func New(cfg Config) *Client {
	return &Client{
		cfg:  cfg,
		HTTP: &http.Client{Timeout: 60 * time.Second},
	}
}

// Config returns a copy of the configuration with the API key redacted.
func (c *Client) Config() Config {
	out := c.cfg
	if out.APIKey != "" {
		out.APIKey = "set"
	}
	return out
}

// Status is the public configuration view.
type Status struct {
	Configured bool   `json:"configured"`
	Provider   string `json:"provider"`
	BaseURL    string `json:"baseUrl"`
	Model      string `json:"model"`
}

// Status reports whether calls can be made, without the secret.
func (c *Client) Status() Status {
	return Status{
		Configured: c.cfg.Configured(),
		Provider:   c.cfg.Provider,
		BaseURL:    c.cfg.BaseURL,
		Model:      c.cfg.Model,
	}
}

// Find ranks flows against a natural-language query.
func (c *Client) Find(ctx context.Context, query string, flows []Flow) (FindResult, error) {
	if !c.cfg.Configured() {
		return FindResult{}, ErrNotConfigured
	}
	if strings.TrimSpace(query) == "" {
		return FindResult{}, fmt.Errorf("query is required")
	}
	if len(flows) == 0 {
		return FindResult{Model: c.cfg.Model, Provider: c.cfg.Provider}, nil
	}
	switch c.cfg.Provider {
	case "jev":
		return c.findJev(ctx, query, flows)
	case "openai":
		return c.findOpenAI(ctx, query, flows)
	default:
		return FindResult{}, fmt.Errorf("unknown decision provider %q", c.cfg.Provider)
	}
}

// Draft chooses or writes a rule for a description. flow may be nil.
func (c *Client) Draft(ctx context.Context, description string, flow *Flow) (DraftResult, error) {
	if !c.cfg.Configured() {
		return DraftResult{}, ErrNotConfigured
	}
	if strings.TrimSpace(description) == "" {
		return DraftResult{}, fmt.Errorf("description is required")
	}
	switch c.cfg.Provider {
	case "jev":
		return c.draftJev(ctx, description, flow)
	case "openai":
		return c.draftOpenAI(ctx, description, flow)
	default:
		return DraftResult{}, fmt.Errorf("unknown decision provider %q", c.cfg.Provider)
	}
}

func (c *Client) postJSON(ctx context.Context, url string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("decision API status %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decision API returned invalid JSON: %w", err)
	}
	return nil
}
