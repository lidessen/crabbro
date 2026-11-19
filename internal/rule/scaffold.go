package rule

import (
	"fmt"
	"strings"

	"github.com/lidessen/crabbro/internal/shared"
	"gopkg.in/yaml.v3"
)

// Scaffold builds a YAML rule that matches a captured request.
// action is one of mock, mockResponse, block, header, setRequestHeader,
// rewrite, rewriteURL, or tag.
func Scaffold(req *shared.Request, action, responseBody, contentType string) string {
	return scaffold(req, action, responseBody, contentType, "scaffolded from a captured exchange")
}

// ScaffoldWithNote is Scaffold with a description that does not include secrets.
func ScaffoldWithNote(req *shared.Request, action, responseBody, contentType, note string) string {
	return scaffold(req, action, responseBody, contentType, note)
}

func scaffold(req *shared.Request, action, responseBody, contentType, note string) string {
	if req == nil {
		req = &shared.Request{}
	}
	if contentType == "" {
		contentType = "application/json"
	}
	if strings.TrimSpace(note) == "" {
		note = "scaffolded from a captured exchange"
	}
	doc := Document{
		Version:     1,
		ID:          "from-capture",
		Description: note,
		On:          On{Request: &RequestMatch{}},
	}
	if req.Method != "" {
		doc.On.Request.Method = req.Method
	}
	if req.Host != "" {
		doc.On.Request.Host = stripPort(req.Host)
	}
	if req.Path != "" {
		doc.On.Request.Path = req.Path
	}
	stage := Stage{}
	switch action {
	case "block":
		stage.Uses = "crabbro.block"
		stage.With = map[string]any{"status": 403, "body": "blocked"}
	case "header", "setRequestHeader":
		stage.Uses = "crabbro.set-request-header"
		stage.With = map[string]any{"name": "X-Crabbro", "value": "1"}
	case "rewrite", "rewriteURL":
		target := req.URL
		if target == "" {
			target = "https://example.test/rewritten"
		}
		stage.Uses = "crabbro.rewrite-url"
		stage.With = map[string]any{"url": target}
	case "tag":
		stage.Uses = "crabbro.tag"
		stage.With = map[string]any{"tags": []any{"from-capture"}}
	default:
		body := responseBody
		if body == "" {
			body = `{"ok":true}`
		}
		stage.Uses = "crabbro.mock-response"
		stage.With = map[string]any{
			"status":  statusOr(req.Status, 200),
			"headers": map[string]any{"content-type": contentType},
			"body":    body,
		}
	}
	doc.Request = &PhaseDoc{Pipeline: []Stage{stage}}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return ""
	}
	_ = enc.Close()
	return b.String()
}

// Migrate wraps a legacy JavaScript rule as one run stage on each phase.
// It does not recover uses from the script. The script text is copied literally.
func Migrate(id, script string) (string, error) {
	if strings.TrimSpace(script) == "" {
		return "", fmt.Errorf("script is empty")
	}
	if id == "" {
		id = "migrated"
	}
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("invalid id")
	}
	run := &RunSpec{Runtime: "goja", Script: script}
	stage := Stage{ID: "legacy", Run: run}
	doc := Document{
		Version:     1,
		ID:          id,
		Description: "migrated JavaScript rule",
		On:          On{Request: &RequestMatch{}},
		Request:     &PhaseDoc{Pipeline: []Stage{stage}},
		Response:    &PhaseDoc{Pipeline: []Stage{stage}},
		Frame:       &PhaseDoc{Pipeline: []Stage{stage}},
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", err
	}
	_ = enc.Close()
	return b.String(), nil
}

func statusOr(status, fallback int) int {
	if status < 100 || status > 599 {
		return fallback
	}
	return status
}
