package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Diagnostic is the safe record of one decision call. It never carries the
// credential, request headers, or upstream body.
type Diagnostic struct {
	Category               string `json:"category"`
	RequestMayHaveBeenSent bool   `json:"requestMayHaveBeenSent"`
	Status                 int    `json:"status,omitempty"`
	DurationMs             int64  `json:"durationMs,omitempty"`
}

// AskResult is a validated choice or noul plus the diagnostic.
type AskResult struct {
	Choice     string     `json:"choice,omitempty"`
	Noul       *float64   `json:"noul,omitempty"`
	Text       string     `json:"text,omitempty"`
	Diagnostic Diagnostic `json:"decisionDiagnostic"`
	Model      string     `json:"model,omitempty"`
	Provider   string     `json:"provider,omitempty"`
}

// Ask performs one provider call. The caller must already have removed any
// endpoint, header, or credential the plugin tried to set. A timeout does not
// prove the provider did not charge, and this method does not retry.
func (c *Client) Ask(ctx context.Context, prompt, kind string, options []string) (AskResult, error) {
	out := AskResult{Provider: c.cfg.Provider, Model: c.cfg.Model}
	if c == nil || !c.cfg.Configured() {
		out.Diagnostic = Diagnostic{Category: "not_configured"}
		return out, ErrNotConfigured
	}
	if kind == "" {
		kind = "noul"
	}
	start := time.Now()
	sent := true
	var err error
	switch c.cfg.Provider {
	case "jev":
		out, err = c.askJev(ctx, prompt, kind, options)
	case "openai":
		out, err = c.askOpenAI(ctx, prompt, kind, options)
	default:
		out.Diagnostic = Diagnostic{Category: "bad_provider"}
		return out, fmt.Errorf("unknown decision provider")
	}
	out.Diagnostic.DurationMs = time.Since(start).Milliseconds()
	out.Diagnostic.RequestMayHaveBeenSent = sent
	if err != nil {
		if out.Diagnostic.Category == "" {
			out.Diagnostic.Category = "provider_error"
		}
		return out, err
	}
	if out.Diagnostic.Category == "" {
		out.Diagnostic.Category = "ok"
	}
	return out, nil
}

func (c *Client) askJev(ctx context.Context, prompt, kind string, options []string) (AskResult, error) {
	q := jevQuestion{Type: kind, Instructions: prompt}
	if kind == "choice" {
		q.Criteria = options
	}
	body := jevRequest{
		Model: c.cfg.Model,
		State: map[string]any{"prompt": prompt},
		Questions: map[string]jevQuestion{
			"answer": q,
		},
	}
	var resp jevResponse
	if err := c.postJSON(ctx, c.endpoint("/v1/systemone"), body, &resp); err != nil {
		return AskResult{Provider: "jev", Model: c.cfg.Model, Diagnostic: Diagnostic{Category: "provider_error", RequestMayHaveBeenSent: true}}, err
	}
	ans, ok := resp.Answers["answer"]
	if !ok {
		return AskResult{Provider: "jev", Diagnostic: Diagnostic{Category: "invalid_response", RequestMayHaveBeenSent: true}}, fmt.Errorf("decision response missing answer")
	}
	return AskResult{
		Choice: ans.Choice, Noul: ans.Noul, Provider: "jev", Model: resp.Model,
		Diagnostic: Diagnostic{Category: "ok", RequestMayHaveBeenSent: true, Status: 200},
	}, nil
}

func (c *Client) askOpenAI(ctx context.Context, prompt, kind string, options []string) (AskResult, error) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"choice": map[string]any{"type": "string"},
			"text":   map[string]any{"type": "string"},
		},
		"required":             []string{"choice", "text"},
		"additionalProperties": false,
	}
	var parsed struct {
		Choice string `json:"choice"`
		Text   string `json:"text"`
	}
	user, _ := json.Marshal(map[string]any{"prompt": prompt, "type": kind, "options": options})
	model, err := c.complete(ctx, "decision_ask", schema, "Answer with the schema only.", string(user), &parsed)
	if err != nil {
		return AskResult{Provider: "openai", Model: c.cfg.Model, Diagnostic: Diagnostic{Category: "provider_error", RequestMayHaveBeenSent: true}}, err
	}
	return AskResult{
		Choice: parsed.Choice, Text: parsed.Text, Provider: "openai", Model: model,
		Diagnostic: Diagnostic{Category: "ok", RequestMayHaveBeenSent: true, Status: 200},
	}, nil
}

func (c *Client) endpoint(path string) string {
	base := c.cfg.BaseURL
	if base == "" {
		return path
	}
	return stringsTrimRightSlash(base) + path
}

func stringsTrimRightSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Sink binds a decision instance without storing the credential.
type Sink struct {
	Instance      string `json:"instance"`
	PackageDigest string `json:"packageDigest"`
	Principal     string `json:"principal"`
	Endpoint      string `json:"endpoint"`
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	Credential    string `json:"credential"`
}

// WriteSink writes the sink mode 0600. It refuses a symlink.
func WriteSink(path string, sink Sink) error {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sink path is a symlink")
	}
	body, err := json.MarshalIndent(sink, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

// ReadSink loads a sink file. The credential value is not in this file.
func ReadSink(path string) (Sink, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Sink{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return Sink{}, fmt.Errorf("sink path is a symlink")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Sink{}, err
	}
	var sink Sink
	if err := json.Unmarshal(b, &sink); err != nil {
		return Sink{}, err
	}
	return sink, nil
}
