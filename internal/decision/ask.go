package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/ir"
)

// Diagnostic is the safe record of one decision call. It never carries any
// credential, request headers, or upstream raw body.
type Diagnostic struct {
	Category               string `json:"category"`
	RequestMayHaveBeenSent bool   `json:"requestMayHaveBeenSent"`
	Status                 int    `json:"status,omitempty"`
	DurationMs             int64  `json:"durationMs,omitempty"`
}

// AskResult is a validated choice, noul, or score plus calibration metadata and diagnostics.
type AskResult struct {
	Choice     string     `json:"choice,omitempty"`
	Noul       *float64   `json:"noul,omitempty"`
	Score      *float64   `json:"score,omitempty"`
	Text       string     `json:"text,omitempty"`
	Calibrated bool       `json:"calibrated,omitempty"`
	Mode       string     `json:"mode,omitempty"`
	Diagnostic Diagnostic `json:"decisionDiagnostic"`
	Model      string     `json:"model,omitempty"`
	Provider   string     `json:"provider,omitempty"`
}

// Ask runs one decision question (noul | choice | score) through the gateway decision route.
func (c *Client) Ask(ctx context.Context, prompt, kind string, options []string) (AskResult, error) {
	st := c.Status()
	out := AskResult{Provider: st.Provider, Model: st.Model}
	if !st.Configured {
		out.Diagnostic = Diagnostic{Category: "not_configured"}
		return out, ErrNotConfigured
	}
	switch kind {
	case "":
		kind = ir.DecisionKindNoul
	case ir.DecisionKindNoul, ir.DecisionKindChoice, ir.DecisionKindScore:
	default:
		out.Diagnostic = Diagnostic{Category: "bad_question_type"}
		return out, fmt.Errorf("unsupported decision question kind %q", kind)
	}

	q := ir.DecisionQuestion{
		Type:         kind,
		Instructions: prompt,
	}
	switch kind {
	case ir.DecisionKindNoul:
		q.Criteria = map[string]string{
			"true":  "yes / matches the condition",
			"false": "no / does not match the condition",
		}
	case ir.DecisionKindChoice:
		if len(options) > 0 {
			m := make(map[string]string, len(options))
			for _, opt := range options {
				m[opt] = opt
			}
			q.Criteria = m
		}
	case ir.DecisionKindScore:
		if len(options) >= 2 {
			q.Criteria = options
		} else {
			q.Criteria = []string{"low: condition does not apply", "high: condition strongly applies"}
		}
	}

	start := time.Now()
	resp, err := c.invoker.InvokeDecision(ctx, c.routeID, &ir.DecisionRequest{
		Model: st.Model,
		State: map[string]any{"prompt": prompt, "options": options},
		Questions: map[string]ir.DecisionQuestion{
			"answer": q,
		},
	})
	durMs := time.Since(start).Milliseconds()
	if err != nil {
		out.Diagnostic = Diagnostic{
			Category:               "provider_error",
			RequestMayHaveBeenSent: true,
			DurationMs:             durMs,
		}
		return out, err
	}

	ans, ok := resp.Answers["answer"]
	if !ok {
		out.Diagnostic = Diagnostic{
			Category:               "invalid_response",
			RequestMayHaveBeenSent: true,
			DurationMs:             durMs,
		}
		return out, fmt.Errorf("decision response missing answer")
	}

	out.Choice = ans.Choice
	out.Noul = ans.Noul
	out.Score = ans.Score
	out.Calibrated = resp.Calibrated
	out.Mode = resp.Mode
	out.Model = firstNonEmpty(resp.Model, st.Model)
	out.Provider = providerWithMode(st.Provider, resp.Mode)
	out.Diagnostic = Diagnostic{
		Category:               "ok",
		RequestMayHaveBeenSent: true,
		Status:                 200,
		DurationMs:             durMs,
	}
	return out, nil
}

// Sink describes the bound decision route for diagnostics (never stores credentials).
type Sink struct {
	Instance      string `json:"instance"`
	PackageDigest string `json:"packageDigest"`
	Principal     string `json:"principal"`
	Endpoint      string `json:"endpoint"`
	Model         string `json:"model"`
	Provider      string `json:"provider"`
	Credential    string `json:"credential"`
}

// WriteSink writes the sink metadata mode 0600, refusing symlinks.
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

// ReadSink loads a sink metadata file, refusing symlinks.
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
